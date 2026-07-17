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
	"time"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	_ "modernc.org/sqlite"
)

// The statements every batch is built from. They are prepared once and reused
// rather than passed as strings to Exec, which re-parses and re-plans the same
// SQL on every key written. A delivery commits ~20 keys, so that parse cost is
// paid 20 times per message for no reason: measured on a 20-key commit, 1.2
// ms/op re-parsing against 0.5 ms/op prepared.
const (
	sqlSet = `INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`
	sqlDel = `DELETE FROM kv WHERE k = ?`
	sqlGet = `SELECT v FROM kv WHERE k = ?`
)

// Store implements backend.Backend on a SQLite file.
type Store struct {
	r *sql.DB // read pool
	w *sql.DB // single write connection

	// Prepared against their own pool. A statement prepared on w must not be
	// used on r, or database/sql would open a second write connection to serve
	// it - which is exactly the serialization the write pool exists to enforce.
	wSet, wDel, wGet *sql.Stmt
	rGet             *sql.Stmt

	// The background checkpointer's lifecycle (see checkpointLoop).
	stop chan struct{}
	done chan struct{}
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	// Bootstrap the file and the table through a short-lived plain connection
	// before the WAL pools open. This exists for page_size: it must be fixed
	// before the file's first write and can never change once the pools
	// switch the file to WAL - and it cannot ride the pools' DSN, because the
	// driver applies _pragma parameters in lexicographic order, running
	// journal_mode(WAL) before page_size would get its turn. On an existing
	// file both the pragma and the CREATE TABLE are no-ops.
	//
	// 16 KiB pages, because blob content makes this table's big rows live in
	// overflow-page chains (see the CREATE TABLE comment), paid for page by
	// page - a quarter the chain length of the 4 KiB default.
	//
	// BLOB keys compare by memcmp in SQLite, which is exactly the
	// bytes.Compare ordering the Backend contract requires.
	//
	// Deliberately NOT a WITHOUT ROWID table. That would store each value
	// inline in the primary-key b-tree, which suits small rows and is why it
	// is tempting here - but this table also holds blob content, and a
	// megabyte row inlined into the b-tree makes every insert pay to split and
	// rewrite interior pages. Measured on a 1.4 MB value: 36 ms/op WITHOUT
	// ROWID against 19 ms/op with a rowid, where the value lives in an
	// overflow chain the index never walks. Range scans stay in key order
	// either way, since the primary-key index is what Scan reads.
	boot, err := sql.Open("sqlite", "file:"+path+"?_pragma=page_size(16384)")
	if err != nil {
		return nil, err
	}
	_, err = boot.ExecContext(context.Background(),
		`CREATE TABLE IF NOT EXISTS kv (k BLOB PRIMARY KEY, v BLOB NOT NULL)`)
	if cErr := boot.Close(); err == nil {
		err = cErr
	}
	if err != nil {
		return nil, err
	}

	// busy_timeout covers the moments WAL still needs the lock (e.g.
	// checkpoints); synchronous=NORMAL is the standard WAL durability
	// point (safe against crashes, fsyncs on checkpoint not per-commit).
	// mmap_size serves reads straight from the OS page cache instead of
	// through read() into SQLite's own cache; a build without mmap support
	// ignores it.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=mmap_size(268435456)"
	// The writer never checkpoints: with blob content riding the WAL, the
	// default autocheckpoint (~4 MB) would fire inside a commit every few
	// messages, and under concurrent readers it can fail to advance and grow
	// the WAL anyway. checkpointLoop does the same work off the commit path.
	w, err := sql.Open("sqlite", dsn+"&_pragma=wal_autocheckpoint(0)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	s := &Store{r: r, w: w, stop: make(chan struct{}), done: make(chan struct{})}
	go s.checkpointLoop()
	if err := s.prepare(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// checkpointLoop moves WAL pages back into the database off the commit path.
// The writer runs with autocheckpoint off, so a commit never pays checkpoint
// time; this loop does that work on its own schedule instead. PASSIVE copies
// as much as the readers' snapshots allow without blocking anyone, so under
// load the WAL hovers around a tick's worth of writes instead of being
// reclaimed inside deliveries. Errors are ignored: a checkpoint that cannot
// run now runs on a later tick, and Close's final checkpoint (implicit in
// closing the last connection) covers shutdown.
func (s *Store) checkpointLoop() {
	defer close(s.done)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			_, _ = s.r.ExecContext(context.Background(), `PRAGMA wal_checkpoint(PASSIVE)`)
		}
	}
}

// prepare readies the statements the hot paths reuse. They must be prepared
// after the table exists, since preparing parses and plans the SQL.
func (s *Store) prepare() error {
	var err error
	if s.wSet, err = s.w.Prepare(sqlSet); err != nil {
		return err
	}
	if s.wDel, err = s.w.Prepare(sqlDel); err != nil {
		return err
	}
	if s.wGet, err = s.w.Prepare(sqlGet); err != nil {
		return err
	}
	s.rGet, err = s.r.Prepare(sqlGet)
	return err
}

// Get returns the value at key, or backend.ErrNotFound.
func (s *Store) Get(ctx context.Context, key []byte) ([]byte, error) {
	var v []byte
	err := s.rGet.QueryRowContext(ctx, key).Scan(&v)
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
	// Bind the prepared statements to this transaction, so the batch reuses one
	// parsed plan per statement kind instead of re-parsing per op.
	set := tx.StmtContext(ctx, s.wSet)
	del := tx.StmtContext(ctx, s.wDel)
	get := tx.StmtContext(ctx, s.wGet)
	for _, op := range b.Ops {
		switch op.Kind {
		case backend.OpSet:
			// A key-only entry - an index entry, a reference marker - carries a
			// nil value: the key IS the data. database/sql binds a nil []byte as
			// SQL NULL rather than as a zero-length blob, which the NOT NULL
			// column rejects, so normalize it. Empty and nil are the same value
			// to the Backend contract; NULL is not a value at all.
			val := op.Value
			if val == nil {
				val = []byte{}
			}
			_, err = set.ExecContext(ctx, op.Key, val)
		case backend.OpDelete:
			_, err = del.ExecContext(ctx, op.Key)
		case backend.OpAdd:
			err = add(ctx, get, set, op.Key, op.Delta)
		case backend.OpAssert:
			err = assert(ctx, get, op.Key, op.Value)
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
func add(ctx context.Context, get, set *sql.Stmt, key []byte, delta int64) error {
	var cur int64
	var v []byte
	err := get.QueryRowContext(ctx, key).Scan(&v)
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
	_, err = set.ExecContext(ctx, key, backend.EncodeInt64(cur+delta))
	return err
}

// assert implements OpAssert: expect nil means the key must be absent.
func assert(ctx context.Context, get *sql.Stmt, key, expect []byte) error {
	var v []byte
	err := get.QueryRowContext(ctx, key).Scan(&v)
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

// Close releases the prepared statements and both connection pools. Closing a
// pool would release its statements anyway, but doing it explicitly keeps the
// order honest and lets a partially constructed Store (a failed prepare) be
// closed safely - hence the nil checks.
func (s *Store) Close() error {
	close(s.stop)
	<-s.done
	for _, st := range []*sql.Stmt{s.wSet, s.wDel, s.wGet, s.rGet} {
		if st != nil {
			st.Close()
		}
	}
	rErr := s.r.Close()
	wErr := s.w.Close()
	if rErr != nil {
		return rErr
	}
	return wErr
}
