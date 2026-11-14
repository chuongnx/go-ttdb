// Package tt holds the error type shared by the TimesTen driver (package ttsql) and the
// transaction log reader (package xla). It has no cgo dependency.
package tt

import (
	"errors"
	"fmt"
	"strings"
)

// Diag is one diagnostic record returned by the ODBC driver or the XLA API.
type Diag struct {
	SQLState string // five-character ODBC state, e.g. "08S01" (empty for XLA errors)
	Code     int    // TimesTen native error number, e.g. 6002
	Message  string
}

// Error is returned for every failure reported by TimesTen. The first diagnostic record is
// promoted to the top-level fields; all records are kept in Diags.
type Error struct {
	Op       string // what the library was doing, e.g. "SQLExecute", "ttXlaPersistOpen"
	SQLState string
	Code     int
	Message  string
	Diags    []Diag
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("timesten: ")
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.Code != 0 {
		fmt.Fprintf(&b, "[%d] ", e.Code)
	}
	if e.SQLState != "" {
		fmt.Fprintf(&b, "(%s) ", e.SQLState)
	}
	b.WriteString(strings.TrimSpace(e.Message))
	for _, d := range e.Diags[min(1, len(e.Diags)):] {
		fmt.Fprintf(&b, "; [%d] %s", d.Code, strings.TrimSpace(d.Message))
	}
	return b.String()
}

// New builds an Error from diagnostic records (the first one becomes the primary).
func New(op string, diags []Diag) *Error {
	e := &Error{Op: op, Diags: diags}
	if len(diags) > 0 {
		e.SQLState, e.Code, e.Message = diags[0].SQLState, diags[0].Code, diags[0].Message
	} else {
		e.Message = "unknown error (no diagnostic records)"
	}
	return e
}

// Selected TimesTen native error numbers. See the TimesTen Error Messages reference.
const (
	ErrLockConflict       = 6001  // lock request denied because of a conflict
	ErrDeadlock           = 6002  // deadlock: transaction chosen as victim
	ErrLockTimeout        = 6003  // lock request timed out
	ErrKeyExists          = 907   // unique constraint violated / object exists
	ErrNotFound           = 911   // key not found
	ErrConnInvalid        = 846   // database connection invalid
	ErrConnTerminated     = 994   // database connection terminated
	ErrClientFailover     = 47137 // client handle invalid after failover
	ErrClientNoConnection = 47138 // client connection lost
	ErrXlaBookmarkInUse   = 8029  // XLA bookmark is in use by another connection
	ErrXlaTableDiff       = 8037  // table definition changed
	ErrXlaDedicatedConn   = 8047  // connection is dedicated to XLA
	ErrStatementCancelled = 6113  // statement cancelled (SQLCancel / context cancellation)
	ErrCommandInvalid     = 946   // prepared statement invalidated by DDL on an object it uses
)

// Has reports whether any diagnostic record carries the native error code.
func (e *Error) Has(code int) bool {
	if e.Code == code {
		return true
	}
	for _, d := range e.Diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

// IsTransient reports whether retrying the same work (usually the whole transaction) may succeed:
// lock conflicts, deadlocks and lock timeouts.
func (e *Error) IsTransient() bool {
	return e.Has(ErrLockConflict) || e.Has(ErrDeadlock) || e.Has(ErrLockTimeout) || e.IsConnectionLost()
}

// IsConnectionLost reports whether the connection can no longer be used.
func (e *Error) IsConnectionLost() bool {
	if strings.HasPrefix(e.SQLState, "08") {
		return true
	}
	for _, c := range []int{ErrConnInvalid, ErrConnTerminated, ErrClientFailover, ErrClientNoConnection} {
		if e.Has(c) {
			return true
		}
	}
	return false
}

// ErrClientCLOB is wrapped by the error returned for a CLOB value that the TimesTen client driver
// delivered corrupted: multibyte text over 4 KB read over a client/server connection. Store such text
// as NCLOB or VARCHAR2 (up to 4 MB), or read it over a direct connection.
var ErrClientCLOB = errors.New("CLOB corrupted by the TimesTen client driver")

// Code returns the native TimesTen error number of err, or 0 when err is not a *Error.
func Code(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// IsTransient reports whether err is a TimesTen error worth retrying.
func IsTransient(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.IsTransient()
}

// IsConnectionLost reports whether err means the connection is unusable.
func IsConnectionLost(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.IsConnectionLost()
}
