package cli

/*
#include "cli.h"
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"
)

// Column describes one result column.
type Column struct {
	Name     string
	SQLType  int // ODBC SQL type code as reported by SQLDescribeCol
	TypeName string
	Size     int64 // column size (precision for numerics, characters/bytes for strings)
	Digits   int   // decimal digits (scale)
	Nullable int   // 0 = no, 1 = yes, 2 = unknown
	Kind     Kind  // Go representation

	cType C.SQLSMALLINT
	elem  int  // bytes per row in the bound buffer
	long  bool // fetched with SQLGetData
}

const (
	maxBoundElem   = 32 * 1024       // larger columns are fetched with SQLGetData
	maxRowsetBytes = 4 * 1024 * 1024 // cap on one rowset's buffers
	getDataChunk   = 64 * 1024
)

var sqlTypeNames = map[int]string{
	C.SQL_CHAR: "CHAR", C.SQL_VARCHAR: "VARCHAR2", C.SQL_LONGVARCHAR: "CLOB",
	C.SQL_WCHAR: "NCHAR", C.SQL_WVARCHAR: "NVARCHAR2", C.SQL_WLONGVARCHAR: "NCLOB",
	C.SQL_DECIMAL: "NUMBER", C.SQL_NUMERIC: "NUMBER",
	C.SQL_TINYINT: "TT_TINYINT", C.SQL_SMALLINT: "TT_SMALLINT", C.SQL_INTEGER: "TT_INTEGER",
	C.SQL_BIGINT: "TT_BIGINT", C.SQL_REAL: "BINARY_FLOAT", C.SQL_FLOAT: "BINARY_DOUBLE",
	C.SQL_DOUBLE: "NUMBER", C.SQL_TYPE_DATE: "DATE", C.SQL_TYPE_TIME: "TIME",
	C.SQL_TYPE_TIMESTAMP: "TIMESTAMP", C.SQL_DATE: "DATE", C.SQL_TIME: "TIME",
	C.SQL_TIMESTAMP: "TIMESTAMP", C.SQL_BINARY: "BINARY", C.SQL_VARBINARY: "VARBINARY",
	C.SQL_LONGVARBINARY: "BLOB", C.SQL_ROWID: "ROWID",
}

func planColumn(c *Column) {
	c.TypeName = sqlTypeNames[c.SQLType]
	switch c.SQLType {
	case C.SQL_TINYINT, C.SQL_SMALLINT, C.SQL_INTEGER, C.SQL_BIGINT, C.SQL_BIT:
		c.Kind, c.cType, c.elem = KindInt, C.SQL_C_SBIGINT, 8
	case C.SQL_DECIMAL, C.SQL_NUMERIC:
		if c.Digits == 0 && c.Size > 0 && c.Size <= 18 {
			c.Kind, c.cType, c.elem = KindInt, C.SQL_C_SBIGINT, 8
		} else {
			c.Kind, c.cType, c.elem = KindDecimal, C.SQL_C_CHAR, 64
		}
	case C.SQL_REAL, C.SQL_FLOAT: // BINARY_FLOAT, BINARY_DOUBLE
		c.Kind, c.cType, c.elem = KindFloat, C.SQL_C_DOUBLE, 8
	case C.SQL_DOUBLE: // TimesTen reports Oracle NUMBER without precision and FLOAT(n) as SQL_DOUBLE:
		// decimal floating point — fetch as text to keep every digit
		c.Kind, c.cType, c.elem = KindDecimal, C.SQL_C_CHAR, 64
	case C.SQL_TYPE_DATE, C.SQL_TYPE_TIMESTAMP, C.SQL_DATE, C.SQL_TIMESTAMP:
		c.Kind, c.cType, c.elem = KindTime, C.SQL_C_TYPE_TIMESTAMP, int(unsafe.Sizeof(C.TIMESTAMP_STRUCT{}))
	case C.SQL_TYPE_TIME, C.SQL_TIME:
		c.Kind, c.cType, c.elem = KindTimeOfDay, C.SQL_C_TYPE_TIME, int(unsafe.Sizeof(C.TIME_STRUCT{}))
	case C.SQL_BINARY, C.SQL_VARBINARY:
		c.Kind, c.cType, c.elem = KindBytes, C.SQL_C_BINARY, int(max(c.Size, 1))
	case C.SQL_LONGVARBINARY:
		c.Kind, c.cType, c.long = KindBytes, C.SQL_C_BINARY, true
	case C.SQL_LONGVARCHAR, C.SQL_WLONGVARCHAR:
		c.Kind, c.cType, c.long = KindString, C.SQL_C_CHAR, true
	case C.SQL_ROWID:
		c.Kind, c.cType, c.elem = KindString, C.SQL_C_CHAR, 32
	default: // CHAR, VARCHAR2, NCHAR, NVARCHAR2 and anything unknown: text in the connection charset
		c.Kind, c.cType = KindString, C.SQL_C_CHAR
		c.elem = int(max(c.Size, 1))*4 + 1
	}
	if c.elem > maxBoundElem {
		c.long = true
	}
}

// Rowset fetches the result set of an executed statement, many rows per driver call.
type Rowset struct {
	s       *Stmt
	cols    []Column
	loc     *time.Location
	n       int // rows per fetch
	bufs    []unsafe.Pointer
	inds    []unsafe.Pointer
	fetched *C.SQLULEN
	long    bool // SQLGetData mode (one row per fetch, no bound buffers)
	pos     int
	count   int
	eof     bool // the driver signalled the end; rows still buffered may remain
	done    bool
	closed  bool
}

// DescribeColumns returns the result columns of an executed (or prepared) statement.
func (s *Stmt) DescribeColumns() ([]Column, error) {
	n, err := s.NumCols()
	if err != nil || n == 0 {
		return nil, err
	}
	cols := make([]Column, n)
	var name [512]C.SQLCHAR
	for i := range cols {
		var nameLen, typ, digits, nullable C.SQLSMALLINT
		var size C.SQLULEN
		rc := C.SQLDescribeCol(s.h, C.SQLUSMALLINT(i+1), &name[0], C.SQLSMALLINT(len(name)), &nameLen,
			&typ, &size, &digits, &nullable)
		if !ok(rc) {
			return nil, s.err("SQLDescribeCol")
		}
		c := &cols[i]
		c.Name = C.GoStringN((*C.char)(unsafe.Pointer(&name[0])), C.int(min(int(nameLen), len(name)-1)))
		c.SQLType, c.Size, c.Digits, c.Nullable = int(typ), int64(size), int(digits), int(nullable)
		planColumn(c)
	}
	return cols, nil
}

// OpenRowset prepares fetching. maxRows is the rowset size (rows per driver round trip).
func (s *Stmt) OpenRowset(maxRows int, loc *time.Location) (*Rowset, error) {
	cols, err := s.DescribeColumns()
	if err != nil {
		return nil, err
	}
	r := &Rowset{s: s, cols: cols, loc: loc}
	width := 0
	for _, c := range cols {
		if c.long {
			r.long = true
		}
		width += c.elem + int(unsafe.Sizeof(C.SQLLEN(0)))
	}
	if r.long || len(cols) == 0 {
		r.n = 1
		return r, nil
	}
	r.n = max(1, min(maxRows, maxRowsetBytes/max(width, 1)))
	if r.n > 1 {
		r.fetched = (*C.SQLULEN)(cmalloc(int(unsafe.Sizeof(C.SQLULEN(0)))))
		if rc := C.gtt_set_stmt_int(s.h, C.SQL_ATTR_ROW_BIND_TYPE, C.SQL_BIND_BY_COLUMN); !ok(rc) {
			r.Close()
			return nil, s.err("SQLSetStmtAttr(ROW_BIND_TYPE)")
		}
		if rc := C.gtt_set_stmt_int(s.h, C.SQL_ATTR_ROW_ARRAY_SIZE, C.SQLLEN(r.n)); !ok(rc) {
			r.Close()
			return nil, s.err("SQLSetStmtAttr(ROW_ARRAY_SIZE)")
		}
		if rc := C.gtt_set_stmt_ptr(s.h, C.SQL_ATTR_ROWS_FETCHED_PTR, unsafe.Pointer(r.fetched)); !ok(rc) {
			r.Close()
			return nil, s.err("SQLSetStmtAttr(ROWS_FETCHED_PTR)")
		}
	}
	indSize := int(unsafe.Sizeof(C.SQLLEN(0)))
	for i, c := range cols {
		buf := cmalloc(c.elem * r.n)
		ind := cmalloc(indSize * r.n)
		r.bufs = append(r.bufs, buf)
		r.inds = append(r.inds, ind)
		rc := C.SQLBindCol(s.h, C.SQLUSMALLINT(i+1), c.cType, C.SQLPOINTER(buf), C.SQLLEN(c.elem), (*C.SQLLEN)(ind))
		if !ok(rc) {
			r.Close()
			return nil, s.err("SQLBindCol")
		}
	}
	return r, nil
}

// Columns returns the result columns.
func (r *Rowset) Columns() []Column { return r.cols }

// Next fills dest (len == number of columns) with the next row. It returns false at the end.
func (r *Rowset) Next(dest []any) (bool, error) {
	if r.closed || r.done {
		return false, nil
	}
	if r.pos >= r.count {
		if r.eof {
			r.done = true
			return false, nil
		}
		if r.fetched != nil {
			*r.fetched = 0
		}
		rc := C.SQLFetch(r.s.h)
		r.count, r.pos = 0, 0
		switch {
		case rc == sqlNoData:
			// TimesTen may return SQL_NO_DATA together with a final partial rowset (observed with
			// SERIALIZABLE isolation): use the rows it reports, then stop.
			r.eof = true
			if r.fetched != nil {
				r.count = int(*r.fetched)
			}
		case ok(rc):
			r.count = 1
			if r.fetched != nil {
				r.count = int(*r.fetched)
			}
		default:
			return false, r.s.err("SQLFetch")
		}
		if r.count == 0 {
			r.done = true
			return false, nil
		}
	}
	var err error
	if r.long {
		err = r.getDataRow(dest)
	} else {
		err = r.boundRow(dest, r.pos)
	}
	r.pos++
	return err == nil, err
}

func (r *Rowset) boundRow(dest []any, row int) error {
	indSize := uintptr(unsafe.Sizeof(C.SQLLEN(0)))
	for i, c := range r.cols {
		ind := *(*C.SQLLEN)(unsafe.Add(r.inds[i], uintptr(row)*indSize))
		if ind == sqlNullData {
			dest[i] = nil
			continue
		}
		p := unsafe.Add(r.bufs[i], row*c.elem)
		switch c.Kind {
		case KindInt:
			dest[i] = int64(*(*C.SQLBIGINT)(p))
		case KindFloat:
			dest[i] = float64(*(*C.SQLDOUBLE)(p))
		case KindTime:
			dest[i] = getTimestamp(p, r.loc)
		case KindTimeOfDay:
			dest[i] = getTimeOfDay(p, r.loc)
		case KindBytes:
			if int(ind) > c.elem {
				return fmt.Errorf("timesten: column %s truncated", c.Name)
			}
			dest[i] = C.GoBytes(p, C.int(ind))
		default: // string / decimal text
			if int(ind) > c.elem-1 {
				return fmt.Errorf("timesten: column %s truncated (%d bytes)", c.Name, ind)
			}
			dest[i] = string(C.GoBytes(p, C.int(ind)))
		}
	}
	return nil
}

func (r *Rowset) getDataRow(dest []any) error {
	for i, c := range r.cols {
		v, err := r.s.getData(i+1, &c, r.loc)
		if err != nil {
			return err
		}
		dest[i] = v
	}
	return nil
}

// getData reads one column of the current row with SQLGetData (any length).
func (s *Stmt) getData(col int, c *Column, loc *time.Location) (any, error) {
	var ind C.SQLLEN
	switch c.Kind {
	case KindInt, KindFloat, KindTime, KindTimeOfDay:
		buf := cmalloc(32)
		defer C.free(buf)
		cType := c.cType
		rc := C.SQLGetData(s.h, C.SQLUSMALLINT(col), cType, C.SQLPOINTER(buf), 32, &ind)
		if !ok(rc) {
			return nil, s.err("SQLGetData")
		}
		if ind == sqlNullData {
			return nil, nil
		}
		switch c.Kind {
		case KindInt:
			return int64(*(*C.SQLBIGINT)(buf)), nil
		case KindFloat:
			return float64(*(*C.SQLDOUBLE)(buf)), nil
		case KindTime:
			return getTimestamp(buf, loc), nil
		default:
			return getTimeOfDay(buf, loc), nil
		}
	}
	// variable length: read in chunks
	buf := cmalloc(getDataChunk)
	defer C.free(buf)
	text := c.cType == C.SQL_C_CHAR
	var out []byte
	for {
		rc := C.SQLGetData(s.h, C.SQLUSMALLINT(col), c.cType, C.SQLPOINTER(buf), getDataChunk, &ind)
		if rc == sqlNoData {
			break
		}
		if !ok(rc) {
			return nil, s.err("SQLGetData")
		}
		if ind == sqlNullData {
			return nil, nil
		}
		n := getDataChunk
		if text {
			n-- // terminator
		}
		if ind != C.SQL_NO_TOTAL && int(ind) <= n {
			n = int(ind)
		}
		out = append(out, C.GoBytes(buf, C.int(n))...)
		if rc == C.SQL_SUCCESS {
			break
		}
	}
	if out == nil {
		out = []byte{}
	}
	if text {
		return string(out), nil
	}
	return out, nil
}

// Close unbinds the buffers, resets the rowset attributes and closes the cursor.
func (r *Rowset) Close() {
	if r.closed {
		return
	}
	r.closed = true
	C.SQLFreeStmt(r.s.h, C.SQL_CLOSE)
	if len(r.bufs) > 0 {
		C.SQLFreeStmt(r.s.h, C.SQL_UNBIND)
	}
	if r.fetched != nil {
		C.gtt_set_stmt_ptr(r.s.h, C.SQL_ATTR_ROWS_FETCHED_PTR, nil)
		C.gtt_set_stmt_int(r.s.h, C.SQL_ATTR_ROW_ARRAY_SIZE, 1)
		C.free(unsafe.Pointer(r.fetched))
		r.fetched = nil
	}
	for _, p := range r.bufs {
		C.free(p)
	}
	for _, p := range r.inds {
		C.free(p)
	}
	r.bufs, r.inds = nil, nil
}
