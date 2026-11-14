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

// BatchError reports the first row of an array execution that failed.
type BatchError struct {
	Row int // 0-based index into the rows passed to ExecArray
	Err error
}

func (e *BatchError) Error() string { return fmt.Sprintf("timesten: batch row %d: %v", e.Row, e.Err) }
func (e *BatchError) Unwrap() error { return e.Err }

type arrayCol struct {
	kind  Kind
	elem  int
	buf   unsafe.Pointer
	ind   unsafe.Pointer
	ctype C.SQLSMALLINT
	stype C.SQLSMALLINT
	size  C.SQLULEN
	dig   C.SQLSMALLINT
}

// ExecArray executes the prepared statement once for every row using ODBC parameter arrays
// (SQL_ATTR_PARAMSET_SIZE), binding by column. Values must already be normalized to nil, int64,
// float64, bool, string, []byte or time.Time; one column must not mix kinds (int64 is widened to
// float64 in a float column). It returns the total affected row count.
func (s *Stmt) ExecArray(rows [][]any, loc *time.Location) (int64, error) {
	n := len(rows)
	if n == 0 {
		return 0, nil
	}
	ncols := len(rows[0])
	for i, r := range rows {
		if len(r) != ncols {
			return 0, fmt.Errorf("timesten: batch row %d has %d values, want %d", i, len(r), ncols)
		}
	}
	s.ResetParams()
	cols := make([]arrayCol, ncols)
	defer func() {
		for _, c := range cols {
			if c.buf != nil {
				C.free(c.buf)
			}
			if c.ind != nil {
				C.free(c.ind)
			}
		}
	}()

	// 1. plan each column: kind and element size
	for j := range cols {
		c := &cols[j]
		c.kind = KindNull
		maxLen := 0
		for i, r := range rows {
			k, err := kindOf(r[j])
			if err != nil {
				return 0, &BatchError{Row: i, Err: fmt.Errorf("column %d: %w", j+1, err)}
			}
			switch {
			case k == KindNull:
			case c.kind == KindNull:
				c.kind = k
			case c.kind == KindFloat && k == KindInt:
			case c.kind == KindInt && k == KindFloat:
				c.kind = KindFloat
			case c.kind != k:
				return 0, &BatchError{Row: i, Err: fmt.Errorf("column %d mixes %T with kind %d", j+1, r[j], c.kind)}
			}
			switch v := r[j].(type) {
			case string:
				maxLen = max(maxLen, len(v))
			case []byte:
				maxLen = max(maxLen, len(v))
			}
		}
		switch c.kind {
		case KindNull:
			c.stype, c.size, c.dig = s.describeParamType(j + 1)
			c.ctype, c.elem = C.SQL_C_CHAR, 1
		case KindInt:
			c.ctype, c.stype, c.size, c.elem = C.SQL_C_SBIGINT, C.SQL_BIGINT, 19, 8
		case KindFloat:
			c.ctype, c.stype, c.size, c.elem = C.SQL_C_DOUBLE, C.SQL_DOUBLE, 15, 8
		case KindTime:
			c.ctype, c.stype, c.size, c.dig = C.SQL_C_TYPE_TIMESTAMP, C.SQL_TYPE_TIMESTAMP, 29, 9
			c.elem = int(unsafe.Sizeof(C.TIMESTAMP_STRUCT{}))
		case KindString:
			c.ctype, c.stype, c.size, c.elem = C.SQL_C_CHAR, C.SQL_VARCHAR, C.SQLULEN(max(maxLen, 1)), maxLen+1
		case KindBytes:
			c.ctype, c.stype, c.size, c.elem = C.SQL_C_BINARY, C.SQL_VARBINARY, C.SQLULEN(max(maxLen, 1)), max(maxLen, 1)
		}
		c.buf = cmalloc(c.elem * n)
		c.ind = cmalloc(int(unsafe.Sizeof(C.SQLLEN(0))) * n)
	}

	// 2. fill the arrays
	indSize := int(unsafe.Sizeof(C.SQLLEN(0)))
	for j := range cols {
		c := &cols[j]
		for i, r := range rows {
			p := unsafe.Add(c.buf, i*c.elem)
			ind := (*C.SQLLEN)(unsafe.Add(c.ind, i*indSize))
			v := r[j]
			if v == nil {
				*ind = sqlNullData
				continue
			}
			switch c.kind {
			case KindInt:
				var x int64
				switch t := v.(type) {
				case int64:
					x = t
				case bool:
					if t {
						x = 1
					}
				}
				*(*C.SQLBIGINT)(p) = C.SQLBIGINT(x)
				*ind = 8
			case KindFloat:
				var x float64
				switch t := v.(type) {
				case float64:
					x = t
				case int64:
					x = float64(t)
				}
				*(*C.SQLDOUBLE)(p) = C.SQLDOUBLE(x)
				*ind = 8
			case KindTime:
				putTimestamp(p, v.(time.Time), loc)
				*ind = C.SQLLEN(c.elem)
			case KindString, KindBytes:
				var b []byte
				if sv, isStr := v.(string); isStr {
					b = []byte(sv)
				} else {
					b = v.([]byte)
				}
				if len(b) > 0 {
					C.memcpy(p, unsafe.Pointer(&b[0]), C.size_t(len(b)))
				}
				*ind = C.SQLLEN(len(b))
			}
		}
		rc := C.SQLBindParameter(s.h, C.SQLUSMALLINT(j+1), C.SQL_PARAM_INPUT, c.ctype, c.stype, c.size, c.dig,
			C.SQLPOINTER(c.buf), C.SQLLEN(c.elem), (*C.SQLLEN)(c.ind))
		if !ok(rc) {
			C.SQLFreeStmt(s.h, C.SQL_RESET_PARAMS)
			return 0, s.err(fmt.Sprintf("SQLBindParameter(%d)", j+1))
		}
	}

	// 3. execute with PARAMSET_SIZE = n
	// No SQL_ATTR_PARAM_STATUS_PTR: TimesTen 22.1 frees an application-supplied status array itself when
	// a row of the array fails (double free / heap corruption, reproduced by
	// TestBatchTruncationErrorRepeated). The failing row is the last one processed.
	processed := (*C.SQLULEN)(cmalloc(int(unsafe.Sizeof(C.SQLULEN(0)))))
	defer func() {
		C.gtt_set_stmt_ptr(s.h, C.SQL_ATTR_PARAMS_PROCESSED_PTR, nil)
		C.gtt_set_stmt_int(s.h, C.SQL_ATTR_PARAMSET_SIZE, 1)
		C.SQLFreeStmt(s.h, C.SQL_RESET_PARAMS)
		C.free(unsafe.Pointer(processed))
	}()
	*processed = 0
	for _, a := range []struct {
		attr C.SQLINTEGER
		v    C.SQLLEN
	}{{C.SQL_ATTR_PARAM_BIND_TYPE, C.SQL_PARAM_BIND_BY_COLUMN}, {C.SQL_ATTR_PARAMSET_SIZE, C.SQLLEN(n)}} {
		if rc := C.gtt_set_stmt_int(s.h, a.attr, a.v); !ok(rc) {
			return 0, s.err("SQLSetStmtAttr(PARAMSET)")
		}
	}
	if rc := C.gtt_set_stmt_ptr(s.h, C.SQL_ATTR_PARAMS_PROCESSED_PTR, unsafe.Pointer(processed)); !ok(rc) {
		return 0, s.err("SQLSetStmtAttr(PARAMS_PROCESSED_PTR)")
	}
	if err := s.Execute(); err != nil {
		row := -1
		if p := int(*processed); p > 0 && p <= n {
			row = p - 1
		}
		return 0, &BatchError{Row: row, Err: err}
	}
	affected, err := s.RowCount()
	s.CloseCursor()
	return affected, err
}
