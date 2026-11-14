package ttsql

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chuongnx/go-timesten/internal/cli"
)

// Cursor receives a PL/SQL REF CURSOR OUT parameter:
//
//	var cur ttsql.Cursor
//	_, err := conn.ExecContext(ctx, "BEGIN OPEN :1 FOR SELECT id, name FROM t; END;", sql.Out{Dest: &cur})
//	defer cur.Close()
//	for cur.Next() { err = cur.Scan(&id, &name) }
//	err = cur.Err()
//
// The cursor lives on the connection that executed the statement: use a *sql.Conn or *sql.Tx so
// the connection is not handed to another goroutine by the pool while the cursor is read, and
// close the cursor before releasing it. Closing the connection closes its cursors.
type Cursor struct {
	c      *Conn
	st     *cli.Stmt
	rs     *cli.Rowset
	row    []any
	err    error
	closed bool
}

func (c *Conn) attachCursor(dest any, st *cli.Stmt) {
	var cu *Cursor
	switch d := dest.(type) {
	case *Cursor:
		cu = d
	case **Cursor:
		if *d == nil {
			*d = &Cursor{}
		}
		cu = *d
	}
	*cu = Cursor{c: c, st: st}
	c.cursors[cu] = struct{}{}
}

func (cu *Cursor) open() bool {
	if cu.rs != nil {
		return true
	}
	if cu.st == nil {
		cu.err = errors.New("ttsql: cursor is not open")
		return false
	}
	rs, err := cu.st.OpenRowset(cu.c.cfg.FetchRows, cu.c.cfg.Location)
	if err != nil {
		cu.err = err
		return false
	}
	cu.rs = rs
	cu.row = make([]any, len(rs.Columns()))
	return true
}

// Columns returns the column names of the cursor.
func (cu *Cursor) Columns() ([]string, error) {
	if cu.c == nil {
		return nil, errors.New("ttsql: cursor is not open")
	}
	cu.c.mu.Lock()
	defer cu.c.mu.Unlock()
	if cu.closed || !cu.open() {
		return nil, cu.Err()
	}
	return columnNames(cu.rs.Columns()), nil
}

// Next advances to the next row.
func (cu *Cursor) Next() bool {
	if cu.c == nil {
		return false
	}
	cu.c.mu.Lock()
	defer cu.c.mu.Unlock()
	if cu.closed || cu.err != nil || cu.c.closed || !cu.open() {
		return false
	}
	ok, err := cu.rs.Next(cu.row)
	if err != nil {
		cu.err = cu.c.note(err)
		return false
	}
	return ok
}

// Scan copies the current row into dest, with database/sql conversion rules.
func (cu *Cursor) Scan(dest ...any) error {
	if cu.row == nil {
		return errors.New("ttsql: Scan called without Next")
	}
	if len(dest) != len(cu.row) {
		return fmt.Errorf("ttsql: Scan expected %d destinations, got %d", len(cu.row), len(dest))
	}
	for i, d := range dest {
		if err := assign(d, cu.row[i]); err != nil {
			return fmt.Errorf("ttsql: column %d: %w", i+1, err)
		}
	}
	return nil
}

// Err returns the error that stopped iteration, if any.
func (cu *Cursor) Err() error { return cu.err }

// Close releases the cursor.
func (cu *Cursor) Close() error {
	if cu.c == nil {
		return nil
	}
	cu.c.mu.Lock()
	defer cu.c.mu.Unlock()
	cu.closeLocked()
	return nil
}

func (cu *Cursor) closeLocked() {
	if cu.closed {
		return
	}
	cu.closed = true
	if cu.rs != nil {
		cu.rs.Close()
	}
	if cu.st != nil {
		cu.st.Free()
	}
	delete(cu.c.cursors, cu)
}

// assign stores a driver value in dest using database/sql's conversion rules (via sql.Null[T]).
func assign(dest, v any) error {
	if sc, ok := dest.(sql.Scanner); ok {
		return sc.Scan(v)
	}
	switch d := dest.(type) {
	case *any:
		*d = v
		return nil
	case *string:
		return scanInto(d, v)
	case *[]byte:
		if v == nil {
			*d = nil
			return nil
		}
		return scanInto(d, v)
	case *int64:
		return scanInto(d, v)
	case *int:
		return scanInto(d, v)
	case *int32:
		return scanInto(d, v)
	case *int16:
		return scanInto(d, v)
	case *int8:
		return scanInto(d, v)
	case *uint64:
		return scanInto(d, v)
	case *uint32:
		return scanInto(d, v)
	case *uint16:
		return scanInto(d, v)
	case *uint8:
		return scanInto(d, v)
	case *uint:
		return scanInto(d, v)
	case *bool:
		return scanInto(d, v)
	case *float64:
		return scanInto(d, v)
	case *float32:
		return scanInto(d, v)
	case *time.Time:
		return scanInto(d, v)
	}
	return fmt.Errorf("unsupported destination %T", dest)
}

func scanInto[T any](d *T, v any) error {
	var n sql.Null[T]
	if err := n.Scan(v); err != nil {
		return err
	}
	if n.Valid {
		*d = n.V
	} else {
		var zero T
		*d = zero
	}
	return nil
}
