package ttsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/chuongnx/go-timesten/internal/cli"
	"github.com/chuongnx/go-timesten/tt"
)

// Stmt is a statement prepared with database/sql's Prepare.
type Stmt struct {
	c        *Conn
	st       *cli.Stmt
	query    string
	numInput int
	closed   bool
}

var (
	_ driver.Stmt             = (*Stmt)(nil)
	_ driver.StmtExecContext  = (*Stmt)(nil)
	_ driver.StmtQueryContext = (*Stmt)(nil)
)

// NumInput returns the number of parameter markers (-1 when unknown).
func (s *Stmt) NumInput() int { return s.numInput }

// Close frees the statement handle.
func (s *Stmt) Close() error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.st.Free()
	}
	return nil
}

// Exec implements driver.Stmt.
func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}

// Query implements driver.Stmt.
func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}

// ExecContext executes the prepared statement.
func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if err := s.c.usable(); err != nil {
		return nil, err
	}
	res, err := s.c.execStmt(ctx, s.st, args)
	return res, s.c.note(err)
}

// QueryContext executes the prepared statement and returns its rows.
func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if err := s.c.usable(); err != nil {
		return nil, err
	}
	rows, err := s.c.queryStmt(ctx, s.st, args, false)
	return rows, s.c.note(err)
}

func namedValues(args []driver.Value) []driver.NamedValue {
	nv := make([]driver.NamedValue, len(args))
	for i, a := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return nv
}

type result struct{ affected int64 }

func (r result) LastInsertId() (int64, error) {
	return 0, fmt.Errorf("ttsql: LastInsertId is not supported; use a sequence (SELECT seq.NEXTVAL FROM DUAL)")
}
func (r result) RowsAffected() (int64, error) { return r.affected, nil }

// outParam remembers where an OUT value goes.
type outParam struct {
	index int // 0-based parameter index
	dest  any
}

// toParams converts database/sql arguments (positional) into cli parameters.
func toParams(args []driver.NamedValue) ([]cli.Param, []outParam, error) {
	ps := make([]cli.Param, len(args))
	var outs []outParam
	for i, a := range args {
		idx := i
		if a.Ordinal > 0 {
			idx = a.Ordinal - 1
		}
		if idx >= len(ps) {
			return nil, nil, fmt.Errorf("ttsql: argument ordinal %d out of range", a.Ordinal)
		}
		o, isOut := a.Value.(sql.Out)
		if !isOut {
			ps[idx] = cli.Param{Dir: cli.In, Value: a.Value}
			continue
		}
		kind, err := outKind(o.Dest)
		if err != nil {
			return nil, nil, fmt.Errorf("ttsql: OUT parameter %d: %w", idx+1, err)
		}
		p := cli.Param{Dir: cli.Out, Kind: kind}
		if o.In {
			if kind == cli.KindCursor {
				return nil, nil, fmt.Errorf("ttsql: REF CURSOR parameter %d cannot be IN OUT", idx+1)
			}
			p.Dir = cli.InOut
			in, err := driver.DefaultParameterConverter.ConvertValue(reflect.ValueOf(o.Dest).Elem().Interface())
			if err != nil {
				return nil, nil, fmt.Errorf("ttsql: IN OUT parameter %d: %w", idx+1, err)
			}
			if kind == cli.KindFloat {
				if iv, isInt := in.(int64); isInt {
					in = float64(iv)
				}
			}
			p.Value = in
		}
		ps[idx] = p
		outs = append(outs, outParam{index: idx, dest: o.Dest})
	}
	return ps, outs, nil
}

func outKind(dest any) (cli.Kind, error) {
	switch dest.(type) {
	case **Cursor, *Cursor:
		return cli.KindCursor, nil
	case *string, *sql.NullString:
		return cli.KindString, nil
	case *[]byte:
		return cli.KindBytes, nil
	case *int64, *int, *int32, *int16, *int8, *uint64, *uint32, *uint16, *uint8, *uint,
		*bool, *sql.NullInt64, *sql.NullInt32, *sql.NullInt16, *sql.NullByte, *sql.NullBool:
		return cli.KindInt, nil
	case *float64, *float32, *sql.NullFloat64:
		return cli.KindFloat, nil
	case *time.Time, *sql.NullTime:
		return cli.KindTime, nil
	case *any:
		return cli.KindString, nil
	case nil:
		return 0, fmt.Errorf("sql.Out.Dest is nil")
	}
	return 0, fmt.Errorf("unsupported destination %T", dest)
}

// bindAndExecute binds ps and executes st. A statement invalidated by DDL since it was prepared
// (cached statements, db.Prepare) fails with TimesTen error 946 before anything runs: it is
// prepared again and executed once more.
func (c *Conn) bindAndExecute(ctx context.Context, st *cli.Stmt, ps []cli.Param) error {
	exec := func() error {
		if err := st.BindParams(ps, c.cfg.Location); err != nil {
			return err
		}
		return run(ctx, st, st.Execute)
	}
	err := exec()
	if isInvalidated(err) && ctx.Err() == nil {
		if perr := st.Reprepare(); perr != nil {
			return perr
		}
		err = exec()
	}
	return err
}

func isInvalidated(err error) bool {
	var te *tt.Error
	return errors.As(err, &te) && te.Has(tt.ErrCommandInvalid)
}

func (c *Conn) execStmt(ctx context.Context, st *cli.Stmt, args []driver.NamedValue) (driver.Result, error) {
	ps, outs, err := toParams(args)
	if err != nil {
		return nil, err
	}
	defer st.ResetParams()
	if err := c.bindAndExecute(ctx, st, ps); err != nil {
		st.CloseCursor()
		return nil, err
	}
	n, err := st.RowCount()
	if err != nil {
		n = 0
	}
	for _, o := range outs {
		v, err := st.OutValue(o.index, c.cfg.Location)
		if err != nil {
			st.CloseCursor()
			return nil, err
		}
		if cur, isCur := v.(*cli.Stmt); isCur {
			c.attachCursor(o.dest, cur)
			continue
		}
		if err := assign(o.dest, v); err != nil {
			st.CloseCursor()
			return nil, fmt.Errorf("ttsql: OUT parameter %d: %w", o.index+1, err)
		}
	}
	st.CloseCursor()
	return result{affected: n}, nil
}

func (c *Conn) queryStmt(ctx context.Context, st *cli.Stmt, args []driver.NamedValue, owned bool) (driver.Rows, error) {
	ps, outs, err := toParams(args)
	if err != nil {
		return nil, err
	}
	if len(outs) > 0 {
		return nil, fmt.Errorf("ttsql: OUT parameters are only supported with Exec")
	}
	err = c.bindAndExecute(ctx, st, ps)
	st.ResetParams()
	if err != nil {
		st.CloseCursor()
		return nil, err
	}
	rs, err := st.OpenRowset(c.cfg.FetchRows, c.cfg.Location)
	if err != nil {
		st.CloseCursor()
		return nil, err
	}
	return &Rows{c: c, st: st, rs: rs, owned: owned, buf: make([]any, len(rs.Columns()))}, nil
}

// ---------------------------------------------------------------------------------------------
// Rows

// Rows is a result set.
type Rows struct {
	c      *Conn
	st     *cli.Stmt
	rs     *cli.Rowset
	owned  bool // the statement was created for this query and is freed on Close
	buf    []any
	closed bool
}

var (
	_ driver.Rows                           = (*Rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*Rows)(nil)
	_ driver.RowsColumnTypeNullable         = (*Rows)(nil)
	_ driver.RowsColumnTypeLength           = (*Rows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*Rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*Rows)(nil)
)

// Columns returns the column names.
func (r *Rows) Columns() []string { return columnNames(r.rs.Columns()) }

func columnNames(cols []cli.Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

// Close closes the result set.
func (r *Rows) Close() error {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.c.closed {
		return nil
	}
	r.rs.Close()
	if r.owned {
		r.st.Free()
	}
	return nil
}

// Next fetches the next row.
func (r *Rows) Next(dest []driver.Value) error {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if r.closed || r.c.closed {
		return io.EOF
	}
	ok, err := r.rs.Next(r.buf)
	if err != nil {
		return r.c.note(err)
	}
	if !ok {
		return io.EOF
	}
	for i := range dest {
		dest[i] = r.buf[i]
	}
	return nil
}

// ColumnTypeDatabaseTypeName returns the TimesTen type name (e.g. NUMBER, VARCHAR2).
func (r *Rows) ColumnTypeDatabaseTypeName(i int) string { return r.rs.Columns()[i].TypeName }

// ColumnTypeNullable reports nullability.
func (r *Rows) ColumnTypeNullable(i int) (nullable, ok bool) {
	switch r.rs.Columns()[i].Nullable {
	case 0:
		return false, true
	case 1:
		return true, true
	}
	return false, false
}

// ColumnTypeLength returns the declared length of character and binary columns.
func (r *Rows) ColumnTypeLength(i int) (int64, bool) {
	c := r.rs.Columns()[i]
	if c.Kind == cli.KindString || c.Kind == cli.KindBytes {
		return c.Size, true
	}
	return 0, false
}

// ColumnTypePrecisionScale returns precision and scale of numeric columns.
func (r *Rows) ColumnTypePrecisionScale(i int) (int64, int64, bool) {
	c := r.rs.Columns()[i]
	if c.Kind == cli.KindDecimal || (c.Kind == cli.KindInt && c.TypeName == "NUMBER") {
		return c.Size, int64(c.Digits), true
	}
	return 0, 0, false
}

var (
	typeInt64   = reflect.TypeOf(int64(0))
	typeFloat64 = reflect.TypeOf(float64(0))
	typeString  = reflect.TypeOf("")
	typeBytes   = reflect.TypeOf([]byte(nil))
	typeTime    = reflect.TypeOf(time.Time{})
	typeAny     = reflect.TypeOf((*any)(nil)).Elem()
)

// ColumnTypeScanType returns the Go type values of the column are delivered as.
func (r *Rows) ColumnTypeScanType(i int) reflect.Type {
	switch r.rs.Columns()[i].Kind {
	case cli.KindInt:
		return typeInt64
	case cli.KindFloat:
		return typeFloat64
	case cli.KindString, cli.KindDecimal:
		return typeString
	case cli.KindBytes:
		return typeBytes
	case cli.KindTime, cli.KindTimeOfDay:
		return typeTime
	}
	return typeAny
}
