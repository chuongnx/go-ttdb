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

// Kind is the Go-side representation of a bound value.
type Kind int

const (
	KindNull      Kind = iota
	KindInt            // int64        ↔ SQL_C_SBIGINT
	KindFloat          // float64      ↔ SQL_C_DOUBLE
	KindString         // string       ↔ SQL_C_CHAR (connection character set, AL32UTF8 → UTF-8)
	KindBytes          // []byte       ↔ SQL_C_BINARY
	KindTime           // time.Time    ↔ SQL_C_TYPE_TIMESTAMP
	KindCursor         // *Stmt        ↔ SQL_C_REFCURSOR (output only)
	KindDecimal        // string with a decimal number (NUMBER/DECIMAL fetched as text)
	KindTimeOfDay      // time.Time on 0000-01-01 ↔ SQL_C_TYPE_TIME
)

// Dir is the direction of a parameter.
type Dir int

const (
	In Dir = iota
	Out
	InOut
)

// Param describes one parameter marker.
type Param struct {
	Dir   Dir
	Value any  // input value: nil, int64, float64, bool, string, []byte, time.Time
	Kind  Kind // for Out/InOut: kind of the output buffer (input kind is taken from Value)
	Size  int  // output buffer size in bytes for KindString/KindBytes (default 32767)
}

// DefaultOutSize is the output buffer for string/bytes OUT parameters (PL/SQL VARCHAR2 limit).
const DefaultOutSize = 32767

type paramBuf struct {
	kind   Kind
	dir    Dir
	buf    unsafe.Pointer
	size   int
	ind    *C.SQLLEN
	cursor *Stmt // KindCursor: statement receiving the REF CURSOR, until handed out
}

func (p *paramBuf) free() {
	if p.buf != nil {
		C.free(p.buf)
		p.buf = nil
	}
	if p.ind != nil {
		C.free(unsafe.Pointer(p.ind))
		p.ind = nil
	}
	if p.cursor != nil {
		p.cursor.Free()
		p.cursor = nil
	}
}

func kindOf(v any) (Kind, error) {
	switch v.(type) {
	case nil:
		return KindNull, nil
	case int64, bool:
		return KindInt, nil
	case float64:
		return KindFloat, nil
	case string:
		return KindString, nil
	case []byte:
		return KindBytes, nil
	case time.Time:
		return KindTime, nil
	}
	return 0, fmt.Errorf("timesten: unsupported parameter type %T", v)
}

func cmalloc(n int) unsafe.Pointer {
	if n < 1 {
		n = 1
	}
	p := C.malloc(C.size_t(n))
	if p == nil {
		panic("timesten: out of C memory")
	}
	return p
}

func newInd(v C.SQLLEN) *C.SQLLEN {
	p := (*C.SQLLEN)(cmalloc(int(unsafe.Sizeof(C.SQLLEN(0)))))
	*p = v
	return p
}

func putTimestamp(dst unsafe.Pointer, t time.Time, loc *time.Location) {
	t = t.In(loc)
	ts := (*C.TIMESTAMP_STRUCT)(dst)
	ts.year = C.SQLSMALLINT(t.Year())
	ts.month = C.SQLUSMALLINT(t.Month())
	ts.day = C.SQLUSMALLINT(t.Day())
	ts.hour = C.SQLUSMALLINT(t.Hour())
	ts.minute = C.SQLUSMALLINT(t.Minute())
	ts.second = C.SQLUSMALLINT(t.Second())
	ts.fraction = C.SQLUINTEGER(t.Nanosecond())
}

func getTimestamp(src unsafe.Pointer, loc *time.Location) time.Time {
	ts := (*C.TIMESTAMP_STRUCT)(src)
	return time.Date(int(ts.year), time.Month(ts.month), int(ts.day), int(ts.hour), int(ts.minute),
		int(ts.second), int(ts.fraction), loc)
}

func getTimeOfDay(src unsafe.Pointer, loc *time.Location) time.Time {
	tm := (*C.TIME_STRUCT)(src)
	return time.Date(0, 1, 1, int(tm.hour), int(tm.minute), int(tm.second), 0, loc)
}

// describeParamType returns the SQL type of parameter i (1-based), or SQL_VARCHAR when the driver
// cannot describe it.
func (s *Stmt) describeParamType(i int) (C.SQLSMALLINT, C.SQLULEN, C.SQLSMALLINT) {
	var typ, digits, nullable C.SQLSMALLINT
	var size C.SQLULEN
	if rc := C.SQLDescribeParam(s.h, C.SQLUSMALLINT(i), &typ, &size, &digits, &nullable); ok(rc) {
		return typ, size, digits
	}
	return C.SQL_VARCHAR, 1, 0
}

// BindParams binds all parameters; buffers stay allocated until ResetParams or Free.
func (s *Stmt) BindParams(ps []Param, loc *time.Location) error {
	s.ResetParams()
	for i, p := range ps {
		pb, err := s.bindOne(i+1, p, loc)
		if pb != nil {
			s.params = append(s.params, pb)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Stmt) bindOne(n int, p Param, loc *time.Location) (*paramBuf, error) {
	inKind, err := kindOf(p.Value)
	if err != nil {
		return nil, fmt.Errorf("parameter %d: %w", n, err)
	}
	kind := inKind
	if p.Dir != In {
		kind = p.Kind
		if p.Dir == InOut && inKind != KindNull && inKind != kind {
			return nil, fmt.Errorf("timesten: parameter %d: IN OUT value is %T but output kind is %d", n, p.Value, kind)
		}
	}
	ioType := C.SQLSMALLINT(C.SQL_PARAM_INPUT)
	switch p.Dir {
	case Out:
		ioType = C.SQL_PARAM_OUTPUT
	case InOut:
		ioType = C.SQL_PARAM_INPUT_OUTPUT
	}

	pb := &paramBuf{kind: kind, dir: p.Dir}
	var cType, sqlType, digits C.SQLSMALLINT
	var colSize C.SQLULEN
	var bufLen C.SQLLEN

	switch kind {
	case KindNull:
		sqlType, colSize, digits = s.describeParamType(n)
		cType = C.SQL_C_CHAR
		pb.buf = cmalloc(1)
		pb.ind = newInd(sqlNullData)
	case KindInt:
		cType, sqlType, colSize = C.SQL_C_SBIGINT, C.SQL_BIGINT, 19
		pb.buf = cmalloc(8)
		var v int64
		switch x := p.Value.(type) {
		case int64:
			v = x
		case bool:
			if x {
				v = 1
			}
		}
		*(*C.SQLBIGINT)(pb.buf) = C.SQLBIGINT(v)
		pb.ind = newInd(0)
		bufLen = 8
	case KindFloat:
		cType, sqlType, colSize = C.SQL_C_DOUBLE, C.SQL_DOUBLE, 15
		pb.buf = cmalloc(8)
		if x, isF := p.Value.(float64); isF {
			*(*C.SQLDOUBLE)(pb.buf) = C.SQLDOUBLE(x)
		}
		pb.ind = newInd(0)
		bufLen = 8
	case KindTime:
		cType, sqlType, colSize, digits = C.SQL_C_TYPE_TIMESTAMP, C.SQL_TYPE_TIMESTAMP, 29, 9
		sz := int(unsafe.Sizeof(C.TIMESTAMP_STRUCT{}))
		pb.buf = cmalloc(sz)
		if x, isT := p.Value.(time.Time); isT {
			putTimestamp(pb.buf, x, loc)
		}
		pb.ind = newInd(0)
		bufLen = C.SQLLEN(sz)
	case KindString, KindBytes:
		var data []byte
		switch x := p.Value.(type) {
		case string:
			data = []byte(x)
		case []byte:
			data = x
		}
		size := len(data)
		if p.Dir != In {
			out := p.Size
			if out <= 0 {
				out = DefaultOutSize
			}
			if out > size {
				size = out
			}
		}
		if kind == KindString {
			cType, sqlType = C.SQL_C_CHAR, C.SQL_VARCHAR
			pb.size = size + 1 // room for the terminator the driver writes on output
		} else {
			cType, sqlType = C.SQL_C_BINARY, C.SQL_VARBINARY
			pb.size = size
		}
		pb.buf = cmalloc(pb.size)
		if len(data) > 0 {
			C.memcpy(pb.buf, unsafe.Pointer(&data[0]), C.size_t(len(data)))
		}
		colSize = C.SQLULEN(max(size, 1))
		bufLen = C.SQLLEN(pb.size)
		if p.Dir == Out || inKind == KindNull {
			pb.ind = newInd(sqlNullData)
		} else {
			pb.ind = newInd(C.SQLLEN(len(data)))
		}
	case KindCursor:
		if p.Dir != Out {
			return nil, fmt.Errorf("timesten: parameter %d: REF CURSOR must be an OUT parameter", n)
		}
		cur, err := s.conn.NewStmt()
		if err != nil {
			return nil, err
		}
		pb.cursor = cur
		// TimesTen: "use an allocated second statement handle as rgbValue" — the handle itself;
		// length and indicator arguments are ignored for REF CURSORs.
		rc := C.SQLBindParameter(s.h, C.SQLUSMALLINT(n), C.SQL_PARAM_OUTPUT, C.SQL_C_REFCURSOR, C.SQL_REFCURSOR,
			0, 0, C.SQLPOINTER(cur.h), 0, nil)
		if !ok(rc) {
			return pb, s.err(fmt.Sprintf("SQLBindParameter(%d, REFCURSOR)", n))
		}
		return pb, nil
	default:
		return nil, fmt.Errorf("timesten: parameter %d: unsupported output kind %d", n, kind)
	}
	if p.Dir != In && inKind == KindNull && kind != KindCursor && kind != KindString && kind != KindBytes {
		*pb.ind = sqlNullData
	}

	rc := C.SQLBindParameter(s.h, C.SQLUSMALLINT(n), ioType, cType, sqlType, colSize, digits,
		C.SQLPOINTER(pb.buf), bufLen, pb.ind)
	if !ok(rc) {
		return pb, s.err(fmt.Sprintf("SQLBindParameter(%d)", n))
	}
	return pb, nil
}

// OutValue returns the value of OUT/IN OUT parameter i (0-based) after Execute. A REF CURSOR is
// returned as a *Stmt owned by the caller (call Free when done).
func (s *Stmt) OutValue(i int, loc *time.Location) (any, error) {
	if i < 0 || i >= len(s.params) {
		return nil, fmt.Errorf("timesten: no parameter %d", i)
	}
	p := s.params[i]
	if p.kind == KindCursor {
		cur := p.cursor
		p.cursor = nil
		return cur, nil
	}
	ind := *p.ind
	if ind == sqlNullData {
		return nil, nil
	}
	switch p.kind {
	case KindInt:
		return int64(*(*C.SQLBIGINT)(p.buf)), nil
	case KindFloat:
		return float64(*(*C.SQLDOUBLE)(p.buf)), nil
	case KindTime:
		return getTimestamp(p.buf, loc), nil
	case KindString, KindBytes:
		n := int(ind)
		limit := p.size
		if p.kind == KindString {
			limit--
		}
		if n > limit || n == C.SQL_NO_TOTAL {
			return nil, fmt.Errorf("timesten: OUT parameter %d truncated (%d bytes, buffer %d)", i+1, n, limit)
		}
		b := C.GoBytes(p.buf, C.int(n))
		if p.kind == KindString {
			return string(b), nil
		}
		return b, nil
	}
	return nil, nil
}
