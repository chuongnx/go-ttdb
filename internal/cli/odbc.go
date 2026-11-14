package cli

/*
#include "cli.h"
*/
import "C"

import (
	"strings"
	"sync"
	"unsafe"

	"github.com/chuongnx/go-timesten/tt"
)

// Return codes and constants re-exported for the other files of this package.
const (
	sqlNoData   = C.SQL_NO_DATA
	sqlNullData = C.SQL_NULL_DATA
)

func ok(rc C.SQLRETURN) bool { return rc == C.SQL_SUCCESS || rc == C.SQL_SUCCESS_WITH_INFO }

// diag collects all diagnostic records of a handle into a *tt.Error.
func diag(op string, ht C.SQLSMALLINT, h C.SQLHANDLE) *tt.Error {
	var diags []tt.Diag
	var state [6]C.SQLCHAR
	var native C.SQLINTEGER
	var msg [C.SQL_MAX_MESSAGE_LENGTH + 1]C.SQLCHAR
	var msgLen C.SQLSMALLINT
	for rec := C.SQLSMALLINT(1); rec < 64; rec++ {
		rc := C.SQLGetDiagRec(ht, h, rec, &state[0], &native, &msg[0], C.SQLSMALLINT(len(msg)), &msgLen)
		if !ok(rc) {
			break
		}
		n := int(msgLen)
		if n > len(msg)-1 {
			n = len(msg) - 1
		}
		diags = append(diags, tt.Diag{
			SQLState: C.GoStringN((*C.char)(unsafe.Pointer(&state[0])), 5),
			Code:     int(native),
			Message:  strings.TrimSpace(C.GoStringN((*C.char)(unsafe.Pointer(&msg[0])), C.int(n))),
		})
	}
	return tt.New(op, diags)
}

// ---------------------------------------------------------------------------------------------
// Environment

// Env is the process-wide ODBC environment handle.
type Env struct{ h C.SQLHENV }

var (
	envOnce sync.Once
	envVal  *Env
	envErr  error
)

// SharedEnv returns the process-wide environment, creating it on first use (ODBC 3 behaviour).
func SharedEnv() (*Env, error) {
	envOnce.Do(func() {
		var h C.SQLHANDLE
		if rc := C.SQLAllocHandle(C.SQL_HANDLE_ENV, nil, &h); !ok(rc) {
			envErr = tt.New("SQLAllocHandle(ENV)", []tt.Diag{{Message: "cannot allocate ODBC environment"}})
			return
		}
		env := C.SQLHENV(h)
		if rc := C.gtt_set_env_int(env, C.SQL_ATTR_ODBC_VERSION, C.SQL_OV_ODBC3); !ok(rc) {
			envErr = diag("SQLSetEnvAttr(ODBC_VERSION)", C.SQL_HANDLE_ENV, h)
			C.SQLFreeHandle(C.SQL_HANDLE_ENV, h)
			return
		}
		envVal = &Env{h: env}
	})
	return envVal, envErr
}

// ---------------------------------------------------------------------------------------------
// Connection

// Conn is one ODBC connection handle.
type Conn struct {
	h      C.SQLHDBC
	closed bool
}

// Connect opens a connection with a TimesTen connection string ("DSN=...;UID=...;PWD=...").
func (e *Env) Connect(connStr string) (*Conn, error) {
	var h C.SQLHANDLE
	if rc := C.SQLAllocHandle(C.SQL_HANDLE_DBC, C.SQLHANDLE(e.h), &h); !ok(rc) {
		return nil, diag("SQLAllocHandle(DBC)", C.SQL_HANDLE_ENV, C.SQLHANDLE(e.h))
	}
	cs := C.CString(connStr)
	defer C.free(unsafe.Pointer(cs))
	var outLen C.SQLSMALLINT
	rc := C.SQLDriverConnect(C.SQLHDBC(h), nil, (*C.SQLCHAR)(unsafe.Pointer(cs)), C.SQL_NTS,
		nil, 0, &outLen, C.SQL_DRIVER_NOPROMPT)
	if !ok(rc) {
		err := diag("SQLDriverConnect", C.SQL_HANDLE_DBC, h)
		C.SQLFreeHandle(C.SQL_HANDLE_DBC, h)
		return nil, err
	}
	return &Conn{h: C.SQLHDBC(h)}, nil
}

func (c *Conn) err(op string) error { return diag(op, C.SQL_HANDLE_DBC, C.SQLHANDLE(c.h)) }

// SetAutoCommit switches autocommit on or off.
func (c *Conn) SetAutoCommit(on bool) error {
	v := C.SQLLEN(C.SQL_AUTOCOMMIT_OFF)
	if on {
		v = C.SQL_AUTOCOMMIT_ON
	}
	if rc := C.gtt_set_conn_int(c.h, C.SQL_ATTR_AUTOCOMMIT, v); !ok(rc) {
		return c.err("SQLSetConnectAttr(AUTOCOMMIT)")
	}
	return nil
}

// Isolation levels supported by TimesTen.
const (
	IsolationReadCommitted = int(C.SQL_TXN_READ_COMMITTED)
	IsolationSerializable  = int(C.SQL_TXN_SERIALIZABLE)
)

// SetIsolation sets the transaction isolation level for the next transaction.
func (c *Conn) SetIsolation(level int) error {
	if rc := C.gtt_set_conn_int(c.h, C.SQL_ATTR_TXN_ISOLATION, C.SQLLEN(level)); !ok(rc) {
		return c.err("SQLSetConnectAttr(TXN_ISOLATION)")
	}
	return nil
}

// Isolation returns the current transaction isolation level.
func (c *Conn) Isolation() (int, error) {
	var v C.SQLUINTEGER
	if rc := C.gtt_get_conn_uint(c.h, C.SQL_ATTR_TXN_ISOLATION, &v); !ok(rc) {
		return 0, c.err("SQLGetConnectAttr(TXN_ISOLATION)")
	}
	return int(v), nil
}

// SetConnAttr sets a numeric connection attribute (ODBC or TimesTen-specific, e.g. TT_PREFETCH_COUNT).
func (c *Conn) SetConnAttr(attr int, v int64) error {
	if rc := C.gtt_set_conn_int(c.h, C.SQLINTEGER(attr), C.SQLLEN(v)); !ok(rc) {
		return c.err("SQLSetConnectAttr")
	}
	return nil
}

// Commit commits the current transaction.
func (c *Conn) Commit() error {
	if rc := C.SQLEndTran(C.SQL_HANDLE_DBC, C.SQLHANDLE(c.h), C.SQL_COMMIT); !ok(rc) {
		return c.err("SQLEndTran(COMMIT)")
	}
	return nil
}

// Rollback rolls back the current transaction.
func (c *Conn) Rollback() error {
	if rc := C.SQLEndTran(C.SQL_HANDLE_DBC, C.SQLHANDLE(c.h), C.SQL_ROLLBACK); !ok(rc) {
		return c.err("SQLEndTran(ROLLBACK)")
	}
	return nil
}

// Close rolls back any open transaction, disconnects and frees the handle. Safe to call twice.
func (c *Conn) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	C.SQLEndTran(C.SQL_HANDLE_DBC, C.SQLHANDLE(c.h), C.SQL_ROLLBACK)
	var err error
	if rc := C.SQLDisconnect(c.h); !ok(rc) {
		err = c.err("SQLDisconnect")
	}
	C.SQLFreeHandle(C.SQL_HANDLE_DBC, C.SQLHANDLE(c.h))
	return err
}

// NewStmt allocates a statement handle on the connection.
func (c *Conn) NewStmt() (*Stmt, error) {
	var h C.SQLHANDLE
	if rc := C.SQLAllocHandle(C.SQL_HANDLE_STMT, C.SQLHANDLE(c.h), &h); !ok(rc) {
		return nil, c.err("SQLAllocHandle(STMT)")
	}
	return &Stmt{conn: c, h: C.SQLHSTMT(h)}, nil
}

// ---------------------------------------------------------------------------------------------
// Statement

// Stmt is one ODBC statement handle plus the C buffers bound to it.
type Stmt struct {
	conn     *Conn
	h        C.SQLHSTMT
	params   []*paramBuf
	cancelMu sync.Mutex // guards h against Cancel racing with Free
	freed    bool
}

func (s *Stmt) err(op string) error { return diag(op, C.SQL_HANDLE_STMT, C.SQLHANDLE(s.h)) }

// Prepare prepares a statement for repeated execution.
func (s *Stmt) Prepare(query string) error {
	q := C.CString(query)
	defer C.free(unsafe.Pointer(q))
	if rc := C.SQLPrepare(s.h, (*C.SQLCHAR)(unsafe.Pointer(q)), C.SQL_NTS); !ok(rc) {
		return s.err("SQLPrepare")
	}
	return nil
}

// Execute runs a prepared statement. A statement that returns no rows to delete/update returns
// nil (SQL_NO_DATA is not an error).
func (s *Stmt) Execute() error {
	rc := C.SQLExecute(s.h)
	if ok(rc) || rc == sqlNoData {
		return nil
	}
	return s.err("SQLExecute")
}

// ExecDirect prepares and runs a statement in one call.
func (s *Stmt) ExecDirect(query string) error {
	q := C.CString(query)
	defer C.free(unsafe.Pointer(q))
	rc := C.SQLExecDirect(s.h, (*C.SQLCHAR)(unsafe.Pointer(q)), C.SQL_NTS)
	if ok(rc) || rc == sqlNoData {
		return nil
	}
	return s.err("SQLExecDirect")
}

// RowCount returns the number of rows affected by the last INSERT/UPDATE/DELETE/MERGE.
func (s *Stmt) RowCount() (int64, error) {
	var n C.SQLLEN
	if rc := C.SQLRowCount(s.h, &n); !ok(rc) {
		return 0, s.err("SQLRowCount")
	}
	return int64(n), nil
}

// NumParams returns the number of parameter markers of a prepared statement.
func (s *Stmt) NumParams() (int, error) {
	var n C.SQLSMALLINT
	if rc := C.SQLNumParams(s.h, &n); !ok(rc) {
		return 0, s.err("SQLNumParams")
	}
	return int(n), nil
}

// NumCols returns the number of result columns (0 for statements without a result set).
func (s *Stmt) NumCols() (int, error) {
	var n C.SQLSMALLINT
	if rc := C.SQLNumResultCols(s.h, &n); !ok(rc) {
		return 0, s.err("SQLNumResultCols")
	}
	return int(n), nil
}

// SetQueryTimeout sets the statement timeout in seconds (0 = none). TimesTen applies it best-effort.
func (s *Stmt) SetQueryTimeout(seconds int) error {
	if rc := C.gtt_set_stmt_int(s.h, C.SQL_ATTR_QUERY_TIMEOUT, C.SQLLEN(seconds)); !ok(rc) {
		return s.err("SQLSetStmtAttr(QUERY_TIMEOUT)")
	}
	return nil
}

// Cancel asks the driver to cancel the statement running on another goroutine. It is the only
// method that may be called concurrently with other methods of the same Stmt.
func (s *Stmt) Cancel() {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if !s.freed {
		C.SQLCancel(s.h)
	}
}

// CloseCursor closes an open result set, keeping the prepared statement.
func (s *Stmt) CloseCursor() {
	C.SQLFreeStmt(s.h, C.SQL_CLOSE)
}

// ResetParams unbinds parameters and frees their buffers.
func (s *Stmt) ResetParams() {
	C.SQLFreeStmt(s.h, C.SQL_RESET_PARAMS)
	for _, p := range s.params {
		p.free()
	}
	s.params = s.params[:0]
}

// Free releases the statement handle and every buffer bound to it. Safe to call twice.
func (s *Stmt) Free() {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.freed {
		return
	}
	s.freed = true
	C.SQLFreeStmt(s.h, C.SQL_CLOSE)
	C.SQLFreeStmt(s.h, C.SQL_RESET_PARAMS)
	for _, p := range s.params {
		p.free()
	}
	s.params = nil
	C.SQLFreeHandle(C.SQL_HANDLE_STMT, C.SQLHANDLE(s.h))
}
