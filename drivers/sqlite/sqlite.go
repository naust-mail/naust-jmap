// Package sqlite is a Backend implementation over a single SQLite
// database file. It exists in its own Go module so the core runtime
// stays free of third-party dependencies; embedders who want SQLite
// import this module, everyone else never downloads it.
//
// Layout is one key-value table. All JMAP structure (collections,
// indexes, the change log) is built above the Backend interface by
// objectdb, so nothing here knows about the protocol.
//
// Concurrency model, matching the backend contract: batches need
// atomicity but not isolation from readers, because the runtime
// serializes writes per account through leases. Writes go through a
// single connection with immediate transactions; reads run on a
// separate pool, which WAL mode lets proceed without blocking the
// writer.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	_ "modernc.org/sqlite"
)

// Store implements backend.Backend on a SQLite file.
type Store struct {
	r *sql.DB // read pool
	w *sql.DB // single write connection
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	// busy_timeout covers the moments WAL still needs the lock (e.g.
	// checkpoints); synchronous=NORMAL is the standard WAL durability
	// point (safe against crashes, fsyncs on checkpoint not per-commit).
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	// BLOB keys compare by memcmp in SQLite, which is exactly the
	// bytes.Compare ordering the Backend contract requires. WITHOUT
	// ROWID stores rows in key order, so range scans are sequential.
	if _, err := w.ExecContext(context.Background(),
		`CREATE TABLE IF NOT EXISTS kv (k BLOB PRIMARY KEY, v BLOB NOT NULL) WITHOUT ROWID`); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	return &Store{r: r, w: w}, nil
}

// Get returns the value at key, or backend.ErrNotFound.
func (s *Store) Get(ctx context.Context, key []byte) ([]byte, error) {
	var v []byte
	err := s.r.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, backend.ErrNotFound
	}
	return v, err
}

// Scan visits keys in [start, end) in order, stopping early when fn
// returns false. Nil bounds mean unbounded on that side.
func (s *Store) Scan(ctx context.Context, start, end []byte, reverse bool, fn func(key, value []byte) bool) error {
	q := `SELECT k, v FROM kv`
	var args []any
	switch {
	case len(start) > 0 && len(end) > 0:
		q += ` WHERE k >= ? AND k < ?`
		args = []any{start, end}
	case len(start) > 0:
		q += ` WHERE k >= ?`
		args = []any{start}
	case len(end) > 0:
		q += ` WHERE k < ?`
		args = []any{end}
	}
	if reverse {
		q += ` ORDER BY k DESC`
	} else {
		q += ` ORDER BY k`
	}
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		if !fn(k, v) {
			return nil
		}
	}
	return rows.Err()
}

// WriteBatch applies all ops in one immediate transaction: either every
// op takes effect or none does. A failing Assert aborts with
// backend.ErrAssertFailed; a malformed counter aborts an Add.
func (s *Store) WriteBatch(ctx context.Context, b *backend.Batch) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, op := range b.Ops {
		switch op.Kind {
		case backend.OpSet:
			_, err = tx.ExecContext(ctx,
				`INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`,
				op.Key, op.Value)
		case backend.OpDelete:
			_, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE k = ?`, op.Key)
		case backend.OpAdd:
			err = add(ctx, tx, op.Key, op.Delta)
		case backend.OpAssert:
			err = assert(ctx, tx, op.Key, op.Value)
		default:
			err = fmt.Errorf("sqlite: unknown op kind %d", op.Kind)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// add implements OpAdd inside the batch transaction. The read sees
// earlier ops in the same batch, so multiple Adds to one key
// accumulate as the contract requires.
func add(ctx context.Context, tx *sql.Tx, key []byte, delta int64) error {
	var cur int64
	var v []byte
	err := tx.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Absent: the counter is created at delta.
	case err != nil:
		return err
	default:
		if cur, err = backend.DecodeInt64(v); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`,
		key, backend.EncodeInt64(cur+delta))
	return err
}

// assert implements OpAssert: expect nil means the key must be absent.
func assert(ctx context.Context, tx *sql.Tx, key, expect []byte) error {
	var v []byte
	err := tx.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if expect == nil {
			return nil
		}
		return backend.ErrAssertFailed
	}
	if err != nil {
		return err
	}
	if expect == nil || !bytes.Equal(v, expect) {
		return backend.ErrAssertFailed
	}
	return nil
}

// Close releases both connection pools.
func (s *Store) Close() error {
	rErr := s.r.Close()
	wErr := s.w.Close()
	if rErr != nil {
		return rErr
	}
	return wErr
}
