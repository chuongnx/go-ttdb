# go-ttdb

[![Go Reference](https://pkg.go.dev/badge/github.com/chuongnx/go-ttdb.svg)](https://pkg.go.dev/github.com/chuongnx/go-ttdb)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**Go access to Oracle TimesTen In-Memory Database**: a `database/sql` driver and a
Transaction Log API (XLA) reader for change data capture. Both run on TimesTen's native ODBC and XLA
C APIs through cgo, so no JVM is needed.

[Tiếng Việt](README.vi.md)

| Package | What it is |
|---|---|
| [`ttsql`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/ttsql) | `database/sql` driver: pooling, transactions, prepared statements, every TimesTen data type, PL/SQL with OUT / IN OUT parameters and REF CURSORs, array batch execution, durable commit, context cancellation |
| [`xla`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/xla) | Change data capture from the transaction log: committed INSERT / UPDATE / DELETE / TRUNCATE / DDL in commit order, before and after images, persistent bookmarks, acknowledge and rewind |
| [`xla/admin`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/xla/admin) | Bookmarks, subscriptions and log holds through SQL built-in procedures |
| [`tt`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/tt) | Error type (`*tt.Error`: native code, SQLSTATE, all diagnostics) and classification (`IsTransient`, `IsConnectionLost`) |
| [`ttxtest`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/ttxtest) | Helpers for integration tests against a real database |

## Features

- Works with direct and client/server connections. On macOS, it builds against the client libraries only.
- Every TimesTen column type, including LOBs. NUMBER is returned as exact decimal text, so no precision
  is lost.
- PL/SQL support:
  - OUT and IN OUT parameters of every scalar type, through `sql.Out`.
  - Function results.
  - Several REF CURSORs from one call.
- `ExecBatch` uses ODBC parameter arrays: one round trip per 1024 rows, and per-row errors.
- Behaviour for production use, covered by tests:
  - Statements invalidated by DDL are re-prepared transparently.
  - After a network failure, dead pooled connections are replaced without extra errors.
  - Deadlocks and lock timeouts are classified, so you can retry them.
  - Cancellation through `context`.
  - No memory leaks over long runs.
- XLA reader:
  - Records are decoded into typed Go values.
  - The list of changed columns for each UPDATE.
  - TRUNCATE and DDL records.
  - Survives a daemon restart without losing or repeating acknowledged records.

## Requirements

- **Oracle TimesTen 22.1** (client or server installation) on the build and run host. This module
  does not include or redistribute any part of TimesTen.
- Go 1.22 or later, with cgo enabled and a C compiler (gcc or clang).
- For XLA, a direct connection (the program runs on the database host) and a user with the `XLA`
  privilege.

| Platform | `ttsql` direct | `ttsql` client/server | `xla` | Status |
|---|---|---|---|---|
| Linux x86-64 | ✓ | ✓ | ✓ | tested |
| Linux arm64 | ✓ | ✓ | ✓ | tested |
| macOS (arm64 / x86-64) | – | ✓ | – | tested; TimesTen ships only the client on macOS |
| AIX 7.2+ (POWER) | ✓ | ✓ | ✓ | builds with gcc ≥ 10; not yet verified, see [testing on AIX](docs/testing.md#testing-on-aix) |

## Install

```bash
go get github.com/chuongnx/go-ttdb@latest
```

Point cgo at your TimesTen installation (details for every platform in
[docs/installation.md](docs/installation.md)):

```bash
export CGO_ENABLED=1
export CGO_CFLAGS="-I$TIMESTEN_HOME/install/include"
export CGO_LDFLAGS="-L$TIMESTEN_HOME/install/lib"
export LD_LIBRARY_PATH="$TIMESTEN_HOME/install/lib"   # macOS: -Wl,-rpath in CGO_LDFLAGS; AIX: LIBPATH
```

## Quick start

### Query with database/sql

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"github.com/chuongnx/go-ttdb/ttsql"
)

func main() {
	ctx := context.Background()
	// Direct: "DSN=sampledb;UID=app;PWD=secret"
	// Client/server: "TTC_SERVER=dbhost/6625;TTC_SERVER_DSN=sampledb;UID=app;PWD=secret"
	db, err := sql.Open(ttsql.DriverName, "DSN=sampledb;UID=app;PWD=secret")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SELECT id, customer, amount FROM orders WHERE amount > ?", 100)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var customer, amount string // NUMBER(p,s) as exact decimal text
		if err := rows.Scan(&id, &customer, &amount); err != nil {
			log.Fatal(err)
		}
		fmt.Println(id, customer, amount)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
}
```

### Call PL/SQL: function result and REF CURSOR

```go
conn, _ := db.Conn(ctx) // a cursor is read on the connection that opened it
defer conn.Close()

var count int64
_, err := conn.ExecContext(ctx, "BEGIN ? := order_pkg.order_count; END;", sql.Out{Dest: &count})

var cur ttsql.Cursor // SYS_REFCURSOR returned by a function
_, err = conn.ExecContext(ctx, "BEGIN ? := order_pkg.orders_of(?); END;", sql.Out{Dest: &cur}, "alice")
defer cur.Close()
for cur.Next() {
	var id int64
	var amount float64
	cur.Scan(&id, &amount)
}
```

### Capture changes with XLA

```go
r, err := xla.Open(ctx, "DSN=sampledb;UID=xlauser;PWD=secret", "orders_cdc") // create or reuse the bookmark
if err != nil {
	log.Fatal(err)
}
defer r.Close()
if err := r.Subscribe(ctx, "APP", "ORDERS"); err != nil {
	log.Fatal(err)
}
for {
	recs, err := r.Next(ctx, xla.Batch(1000), xla.Wait(2*time.Second))
	if err != nil {
		log.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Op.IsDML() {
			amount, _ := rec.NewValue("AMOUNT")
			fmt.Println(rec.Op, rec.QualifiedName(), rec.Key(), amount)
		}
	}
	// store or forward recs durably first, then:
	if err := r.Ack(); err != nil {
		log.Fatal(err)
	}
}
```

Runnable programs are in [examples/sql](examples/sql) and [examples/xla-tail](examples/xla-tail). The
second prints changes as JSON lines.

## Documentation

| Guide | Contents |
|---|---|
| [Installation](docs/installation.md) | Linux, macOS, AIX; cgo flags, library paths, build tags |
| [Connecting](docs/connecting.md) | Connection strings, driver options, direct vs client/server, pool settings, `Connector` |
| [SQL guide](docs/sql-guide.md) | CRUD, transactions, prepared statements, `ExecBatch`, durable commit, cancellation |
| [PL/SQL](docs/plsql.md) | Packages, procedures and functions, OUT / IN OUT parameters of every type, REF CURSOR |
| [Data types](docs/data-types.md) | TimesTen ↔ Go mapping for columns, parameters and XLA; value rules |
| [XLA (change data capture)](docs/xla.md) | Bookmarks, reading and acknowledging, record format, DDL, administration |
| [Production notes](docs/production.md) | Errors and retries, lost connections, DDL, timeouts, memory, known driver defects |
| [Testing](docs/testing.md) | Dev container, make targets, the integration test suite, testing on a host or AIX |
| [Troubleshooting](docs/troubleshooting.md) | Common build and runtime errors and their fixes |

API reference: [pkg.go.dev/github.com/chuongnx/go-ttdb](https://pkg.go.dev/github.com/chuongnx/go-ttdb).

## Status

The API is pre-1.0 (`v0.x`) and may still change in minor versions. Changes are listed in the
[CHANGELOG](CHANGELOG.md). The driver is tested against TimesTen 22.1.1.36 and 22.1.1.37 (Classic).
TimesTen Scaleout is not tested.

## Contributing

Bug reports and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) explains how to set up
the dev container and run the tests. Report security issues as described in [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). See [NOTICE](NOTICE).

Oracle and TimesTen are registered trademarks of Oracle and/or its affiliates. go-ttdb is an
independent open-source project. It is not affiliated with, endorsed by or supported by Oracle.
