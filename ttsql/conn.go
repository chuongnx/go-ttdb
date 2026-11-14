package ttsql

import (
	"container/list"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chuongnx/go-timesten/internal/cli"
	"github.com/chuongnx/go-timesten/tt"
)

// Conn is one TimesTen connection. Besides the database/sql driver interfaces it exposes
// TimesTen-specific operations, reachable with sql.Conn.Raw:
//
//	err := conn.Raw(func(dc any) error {
//		n, err := dc.(*ttsql.Conn).ExecBatch(ctx, "INSERT INTO t VALUES (?, ?)", rows)
//		...
//	})
type Conn struct {
	mu      sync.Mutex
	cc      *cli.Conn
	cfg     *Config
	inTx    bool
	bad     bool
	closed  bool
	isoSet  bool
	cache   *stmtCache
	cursors map[*Cursor]struct{}

	conr      *Connector // nil for connections not made by a Connector
	created   int64      // unix ns
	checkedAt int64      // unix ns of the last successful liveness check
}

var (
	_ driver.Conn               = (*Conn)(nil)
	_ driver.ConnBeginTx        = (*Conn)(nil)
	_ driver.ConnPrepareContext = (*Conn)(nil)
	_ driver.ExecerContext      = (*Conn)(nil)
	_ driver.QueryerContext     = (*Conn)(nil)
	_ driver.Pinger             = (*Conn)(nil)
	_ driver.Validator          = (*Conn)(nil)
	_ driver.SessionResetter    = (*Conn)(nil)
	_ driver.NamedValueChecker  = (*Conn)(nil)
)

func newConn(cc *cli.Conn, cfg *Config) *Conn {
	return &Conn{cc: cc, cfg: cfg, cache: newStmtCache(cfg.StmtCacheSize), cursors: map[*Cursor]struct{}{}}
}

// note records a connection-level failure so the pool discards the connection.
func (c *Conn) note(err error) error {
	if err != nil && tt.IsConnectionLost(err) {
		c.bad = true
		if c.conr != nil {
			c.conr.lostAt.Store(time.Now().UnixNano())
		}
	}
	return err
}

func (c *Conn) usable() error {
	if c.closed {
		return driver.ErrBadConn
	}
	if c.bad {
		return driver.ErrBadConn
	}
	return nil
}

// run executes fn, calling SQLCancel on st if ctx ends first.
func run(ctx context.Context, st *cli.Stmt, fn func() error) error {
	if ctx.Done() == nil {
		return fn()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			st.Cancel()
		case <-done:
		}
	}()
	err := fn()
	close(done)
	wg.Wait()
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("%w: %w", ctx.Err(), err)
	}
	return err
}

// Prepare implements driver.Conn.
func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext prepares a statement on this connection.
func (c *Conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return nil, err
	}
	st, err := c.cc.NewStmt()
	if err != nil {
		return nil, c.note(err)
	}
	if err := st.Prepare(query); err != nil {
		st.Free()
		return nil, c.note(err)
	}
	n, err := st.NumParams()
	if err != nil {
		n = -1
	}
	return &Stmt{c: c, st: st, query: query, numInput: n}, nil
}

// Close closes the connection and every statement and cursor opened on it.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	for cu := range c.cursors {
		cu.closeLocked()
	}
	c.cache.purge()
	return c.cc.Close()
}

// Begin implements driver.Conn.
func (c *Conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

// BeginTx starts a transaction. TimesTen supports READ COMMITTED (default) and SERIALIZABLE.
func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return nil, err
	}
	if c.inTx {
		return nil, errors.New("ttsql: transaction already in progress")
	}
	if opts.ReadOnly {
		return nil, errors.New("ttsql: read-only transactions are not supported by TimesTen")
	}
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault, sql.LevelReadCommitted:
		if c.isoSet {
			if err := c.cc.SetIsolation(cli.IsolationReadCommitted); err != nil {
				return nil, c.note(err)
			}
			c.isoSet = false
		}
	case sql.LevelSerializable:
		if err := c.cc.SetIsolation(cli.IsolationSerializable); err != nil {
			return nil, c.note(err)
		}
		c.isoSet = true
	default:
		return nil, fmt.Errorf("ttsql: isolation level %v is not supported by TimesTen", sql.IsolationLevel(opts.Isolation))
	}
	if err := c.cc.SetAutoCommit(false); err != nil {
		return nil, c.note(err)
	}
	c.inTx = true
	return &tx{c: c}, nil
}

type tx struct{ c *Conn }

func (t *tx) Commit() error   { return t.c.endTx(true) }
func (t *tx) Rollback() error { return t.c.endTx(false) }

func (c *Conn) endTx(commit bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.inTx {
		return errors.New("ttsql: no transaction in progress")
	}
	c.inTx = false
	var err error
	if commit {
		err = c.cc.Commit()
	} else {
		err = c.cc.Rollback()
	}
	if err != nil {
		c.note(err)
		if commit {
			c.cc.Rollback()
		}
	}
	if aerr := c.cc.SetAutoCommit(true); aerr != nil && err == nil {
		err = c.note(aerr)
	}
	return err
}

// ExecContext executes a statement without returning rows. Statements are prepared once and cached
// per connection (Config.StmtCacheSize).
func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return nil, err
	}
	st, release, err := c.cachedStmt(query)
	if err != nil {
		return nil, c.note(err)
	}
	defer release()
	res, err := c.execStmt(ctx, st, args)
	return res, c.note(err)
}

// QueryContext executes a query and returns its rows.
func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return nil, err
	}
	st, err := c.cc.NewStmt()
	if err != nil {
		return nil, c.note(err)
	}
	if err := st.Prepare(query); err != nil {
		st.Free()
		return nil, c.note(err)
	}
	rows, err := c.queryStmt(ctx, st, args, true)
	if err != nil {
		st.Free()
		return nil, c.note(err)
	}
	return rows, nil
}

// Ping checks the connection with a trivial query.
func (c *Conn) Ping(ctx context.Context) error {
	rows, err := c.QueryContext(ctx, "SELECT 1 FROM DUAL", nil)
	if err != nil {
		if tt.IsConnectionLost(err) {
			return driver.ErrBadConn
		}
		return err
	}
	return rows.Close()
}

// IsValid lets the pool drop connections that saw a connection-level error.
func (c *Conn) IsValid() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.bad && !c.closed
}

// ResetSession is called before a pooled connection is reused.
func (c *Conn) ResetSession(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bad || c.closed {
		return driver.ErrBadConn
	}
	if c.inTx { // database/sql always ends transactions; be defensive
		c.cc.Rollback()
		c.cc.SetAutoCommit(true)
		c.inTx = false
	}
	// Another connection of the pool lost its connection after this one was opened (network
	// failure, server restart): check this one before handing it out. ErrBadConn makes database/sql
	// discard it and use another connection, without an error reaching the application.
	if c.conr != nil {
		if lost := c.conr.lostAt.Load(); lost > c.created && lost > c.checkedAt {
			if err := c.alive(); err != nil {
				c.bad = true
				return driver.ErrBadConn
			}
			c.checkedAt = time.Now().UnixNano()
		}
	}
	return nil
}

// alive runs a trivial query on the connection.
func (c *Conn) alive() error {
	st, err := c.cc.NewStmt()
	if err != nil {
		return err
	}
	defer st.Free()
	if err := st.Prepare("SELECT 1 FROM DUAL"); err != nil {
		return err
	}
	if err := st.Execute(); err != nil {
		return err
	}
	st.CloseCursor()
	return nil
}

// CheckNamedValue lets sql.Out through; other values use database/sql's default conversion.
func (c *Conn) CheckNamedValue(nv *driver.NamedValue) error {
	if _, ok := nv.Value.(sql.Out); ok {
		return nil
	}
	return driver.ErrSkip
}

// IsClientServer reports whether the connection goes through the TimesTen client driver
// (client/server) rather than a direct connection.
func (c *Conn) IsClientServer() bool { return c.cc.IsClientServer() }

// InTx reports whether a transaction started with BeginTx is open.
func (c *Conn) InTx() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inTx
}

// ExecBatch executes query once per row using ODBC parameter arrays (one driver round trip per
// Config.BatchRows rows) and returns the total number of affected rows. It runs in the
// connection's current transaction state. On failure the error is a *BatchError naming the row.
func (c *Conn) ExecBatch(ctx context.Context, query string, rows [][]any) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.usable(); err != nil {
		return 0, err
	}
	norm := make([][]any, len(rows))
	for i, r := range rows {
		nr := make([]any, len(r))
		for j, v := range r {
			nv, err := driver.DefaultParameterConverter.ConvertValue(v)
			if err != nil {
				return 0, &BatchError{Row: i, Err: fmt.Errorf("column %d: %w", j+1, err)}
			}
			nr[j] = nv
		}
		norm[i] = nr
	}
	st, release, err := c.cachedStmt(query)
	if err != nil {
		return 0, c.note(err)
	}
	defer release()
	var total int64
	reprepared := false
	for start := 0; start < len(norm); start += c.cfg.BatchRows {
		end := min(start+c.cfg.BatchRows, len(norm))
		var n int64
		err := run(ctx, st, func() error {
			var e error
			n, e = st.ExecArray(norm[start:end], c.cfg.Location)
			return e
		})
		if err != nil && start == 0 && !reprepared && isInvalidated(err) && ctx.Err() == nil {
			// cached statement invalidated by DDL: nothing ran; prepare again and redo the chunk
			reprepared = true
			if perr := st.Reprepare(); perr != nil {
				return total, c.note(perr)
			}
			start -= c.cfg.BatchRows
			continue
		}
		if err != nil {
			var be *cli.BatchError
			if errors.As(err, &be) {
				berr := be.Err
				if ctx.Err() != nil {
					berr = fmt.Errorf("%w: %w", ctx.Err(), be.Err)
				}
				return total, c.note(&BatchError{Row: start + be.Row, Err: berr})
			}
			return total, c.note(err)
		}
		total += n
	}
	return total, nil
}

// DurableCommit makes the current transaction durable on commit (CALL ttDurableCommit), for
// databases running with DurableCommits=0.
func (c *Conn) DurableCommit(ctx context.Context) error {
	_, err := c.ExecContext(ctx, "CALL ttDurableCommit", nil)
	return err
}

// BatchError reports the first failing row of ExecBatch.
type BatchError struct {
	Row int // 0-based index into the rows passed to ExecBatch
	Err error
}

func (e *BatchError) Error() string { return fmt.Sprintf("ttsql: batch row %d: %v", e.Row, e.Err) }
func (e *BatchError) Unwrap() error { return e.Err }

// ---------------------------------------------------------------------------------------------
// statement cache (LRU) for ExecContext / ExecBatch

type stmtCache struct {
	size  int
	ll    *list.List
	items map[string]*list.Element
}

type cacheEntry struct {
	query string
	st    *cli.Stmt
	busy  bool
}

func newStmtCache(size int) *stmtCache {
	return &stmtCache{size: size, ll: list.New(), items: map[string]*list.Element{}}
}

// cachedStmt returns a prepared statement for query and a release function. A statement that is
// already in use (re-entrant call) or a disabled cache yields a one-off statement.
func (c *Conn) cachedStmt(query string) (*cli.Stmt, func(), error) {
	sc := c.cache
	if el, ok := sc.items[query]; ok && !el.Value.(*cacheEntry).busy {
		e := el.Value.(*cacheEntry)
		sc.ll.MoveToFront(el)
		e.busy = true
		return e.st, func() { e.busy = false }, nil
	}
	st, err := c.cc.NewStmt()
	if err != nil {
		return nil, nil, err
	}
	if err := st.Prepare(query); err != nil {
		st.Free()
		return nil, nil, err
	}
	if sc.size <= 0 || sc.items[query] != nil {
		return st, st.Free, nil
	}
	e := &cacheEntry{query: query, st: st, busy: true}
	sc.items[query] = sc.ll.PushFront(e)
	for sc.ll.Len() > sc.size {
		old := sc.ll.Back()
		oe := old.Value.(*cacheEntry)
		if oe.busy {
			break
		}
		sc.ll.Remove(old)
		delete(sc.items, oe.query)
		oe.st.Free()
	}
	return st, func() { e.busy = false }, nil
}

func (sc *stmtCache) purge() {
	for _, el := range sc.items {
		el.Value.(*cacheEntry).st.Free()
	}
	sc.items = map[string]*list.Element{}
	sc.ll.Init()
}
