// Package cli is the thin cgo layer over the TimesTen ODBC (CLI) and XLA C APIs.
//
// Which TimesTen library is linked is chosen with build tags:
//
//	(default)   -lttdrvmgr  TimesTen Driver Manager: direct and client/server connections, XLA on direct
//	tt_direct   -ltten      direct driver only
//	tt_client   -lttclient  client/server only (no XLA)
//
// macOS has only the TimesTen client, so darwin builds link -lttclient and exclude XLA by default.
//
// Header and library directories come from the environment, e.g.
//
//	CGO_CFLAGS=-I$TIMESTEN_HOME/install/include CGO_LDFLAGS=-L$TIMESTEN_HOME/install/lib
//
// Memory rule: every buffer that ODBC keeps a pointer to after a call returns (SQLBindParameter,
// SQLBindCol, statement attributes that take pointers) is allocated with C.malloc and freed by this
// package — never Go memory.
//
// Concurrency: handles are not safe for concurrent use; callers serialize access per connection.
// The only call made concurrently by design is Stmt.Cancel.
package cli

/*
#cgo !tt_direct,!tt_client,!darwin LDFLAGS: -lttdrvmgr
#cgo tt_direct LDFLAGS: -ltten
#cgo tt_client LDFLAGS: -lttclient
#cgo darwin,!tt_direct,!tt_client LDFLAGS: -lttclient
#include "cli.h"
*/
import "C"
