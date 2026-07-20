// Package postgres is a Backend implementation over a single Postgres
// database, one kv table. All JMAP structure (collections, indexes, the
// change log) is built above the Backend interface by objectdb, so nothing
// here knows about the protocol - same split as drivers/sqlite.
//
// Uses pgx natively (pgxpool), not the database/sql stdlib adapter: Batch
// (a run of ops meant to commit atomically) maps directly onto pgx's
// pipelining, which database/sql has no equivalent for, and Postgres
// LISTEN/NOTIFY - the natural fit for a future cluster Notifier - is a
// first-class pgx feature but awkward to reach through the stdlib driver
// interface.
//
// Concurrency model, matching the backend contract: batches need atomicity
// but not isolation from readers, because the runtime serializes writes per
// account through leases. Unlike drivers/sqlite, there is no need for a
// single dedicated write connection - Postgres's MVCC handles concurrent
// writers on its own, and the account-level lease already means at most one
// writer per account is active at a time.
package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// The statements every batch is built from. Reused as pgx.Batch queue
// entries so a run of Set/Delete ops pipelines as one round trip rather than
// one per op (see WriteBatch).
const (
	sqlSet = `INSERT INTO kv (k, v) VALUES ($1, $2) ON CONFLICT (k) DO UPDATE SET v = excluded.v`
	sqlDel = `DELETE FROM kv WHERE k = $1`
	sqlGet = `SELECT v FROM kv WHERE k = $1`
)

// Store implements backend.Backend on a Postgres database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the Postgres database at dsn and ensures the kv table
// exists.
//
// BYTEA keys compare byte-by-byte, unsigned - exactly the bytes.Compare
// ordering the Backend contract requires, so Scan's ORDER BY k needs no
// special handling.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS kv (k BYTEA PRIMARY KEY, v BYTEA NOT NULL)`); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Get returns the value at key, or backend.ErrNotFound.
func (s *Store) Get(ctx context.Context, key []byte) ([]byte, error) {
	var v []byte
	err := s.pool.QueryRow(ctx, sqlGet, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
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
		q += ` WHERE k >= $1 AND k < $2`
		args = []any{start, end}
	case len(start) > 0:
		q += ` WHERE k >= $1`
		args = []any{start}
	case len(end) > 0:
		q += ` WHERE k < $1`
		args = []any{end}
	}
	if reverse {
		q += ` ORDER BY k DESC`
	} else {
		q += ` ORDER BY k`
	}
	rows, err := s.pool.Query(ctx, q, args...)
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

// WriteBatch applies all ops in one transaction: either every op takes
// effect or none does. A failing Assert aborts with backend.ErrAssertFailed;
// a malformed counter aborts an Add.
//
// Ops are applied in order, as the contract requires (e.g. a Set followed by
// an Assert on the same key must see the Set). Contiguous runs of Set/Delete
// don't need to observe each other's results to decide what to write, so
// they pipeline as one round trip via pgx.Batch; Add and Assert read before
// deciding what to write (or whether to abort), so they run sequentially and
// break the pipeline where they occur.
func (s *Store) WriteBatch(ctx context.Context, b *backend.Batch) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for i := 0; i < len(b.Ops); {
		switch b.Ops[i].Kind {
		case backend.OpSet, backend.OpDelete:
			j, err := runSetsAndDeletes(ctx, tx, b.Ops, i)
			if err != nil {
				return err
			}
			i = j
		case backend.OpAdd:
			if err := add(ctx, tx, b.Ops[i].Key, b.Ops[i].Delta); err != nil {
				return err
			}
			i++
		case backend.OpAssert:
			if err := assertOp(ctx, tx, b.Ops[i].Key, b.Ops[i].Value); err != nil {
				return err
			}
			i++
		default:
			return fmt.Errorf("postgres: unknown op kind %d", b.Ops[i].Kind)
		}
	}
	return tx.Commit(ctx)
}

// runSetsAndDeletes pipelines the contiguous run of Set/Delete ops starting
// at i as one batch round trip, and returns the index of the first op after
// the run (an Add, Assert, or len(ops)).
func runSetsAndDeletes(ctx context.Context, tx pgx.Tx, ops []backend.Op, i int) (int, error) {
	batch := &pgx.Batch{}
	j := i
	for j < len(ops) && (ops[j].Kind == backend.OpSet || ops[j].Kind == backend.OpDelete) {
		op := ops[j]
		if op.Kind == backend.OpSet {
			// A key-only entry carries a nil value; empty and nil are the
			// same value to the Backend contract, so normalize before
			// binding (pgx would otherwise send SQL NULL, which the NOT
			// NULL column rejects).
			val := op.Value
			if val == nil {
				val = []byte{}
			}
			batch.Queue(sqlSet, op.Key, val)
		} else {
			batch.Queue(sqlDel, op.Key)
		}
		j++
	}
	br := tx.SendBatch(ctx, batch)
	for k := i; k < j; k++ {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return 0, err
		}
	}
	return j, br.Close()
}

// add implements OpAdd inside the batch transaction. The read sees earlier
// ops in the same batch, so multiple Adds to one key accumulate as the
// contract requires.
func add(ctx context.Context, tx pgx.Tx, key []byte, delta int64) error {
	var cur int64
	var v []byte
	err := tx.QueryRow(ctx, sqlGet, key).Scan(&v)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Absent: the counter is created at delta.
	case err != nil:
		return err
	default:
		if cur, err = backend.DecodeInt64(v); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, sqlSet, key, backend.EncodeInt64(cur+delta))
	return err
}

// assertOp implements OpAssert: expect nil means the key must be absent.
func assertOp(ctx context.Context, tx pgx.Tx, key, expect []byte) error {
	var v []byte
	err := tx.QueryRow(ctx, sqlGet, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
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

// Close releases the connection pool.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}
