# go-timesten

Go access to Oracle TimesTen In-Memory Database (Classic, 22.1):

| Package | What it is |
|---|---|
| `ttsql` | `database/sql` driver: pooling, transactions, prepared statements, all TimesTen types, PL/SQL with `sql.Out` (OUT / IN OUT / function results / REF CURSOR), batch execution with parameter arrays, durable commit, context cancellation |
| `xla` | Transaction Log API reader: committed INSERT / UPDATE (before + after image, changed columns) / DELETE / TRUNCATE / DDL in commit order, persistent bookmarks, acknowledge, rewind |
| `xla/admin` | Bookmarks, subscriptions and log holds through SQL built-ins |
| `tt` | Error type (`*tt.Error` with native code, SQLSTATE, all diagnostics) and classification (`IsTransient`, `IsConnectionLost`) |
| `ttxtest` | Helpers for integration tests against a real database |

It calls the TimesTen ODBC and XLA C APIs through cgo — the same native layer TimesTen's own
JMS/XLA Java library uses — without a JVM.

## Requirements

- TimesTen 22.1 client or server installation on the build and run host (headers + libraries; not
  redistributed with this module).
- cgo and a C compiler. Linux x86-64 / arm64; AIX (POWER, AIX ≥ 7.2) builds natively on AIX with gcc ≥ 10.
- XLA only: a **direct** connection (the program runs on the database host), a user with the `XLA`
  privilege.

```bash
export CGO_ENABLED=1
export CGO_CFLAGS="-I$TIMESTEN_HOME/install/include"
export CGO_LDFLAGS="-L$TIMESTEN_HOME/install/lib"
export LD_LIBRARY_PATH="$TIMESTEN_HOME/install/lib"   # AIX: LIBPATH
go build ./...
```

Linked library (build tags):

| Tag | Library | Direct | Client/server | XLA |
|---|---|---|---|---|
| *(default)* | `libttdrvmgr` (TimesTen Driver Manager) | ✓ | ✓ | ✓ (direct connections) |
| `tt_direct` | `libtten` | ✓ | – | ✓ |
| `tt_client` | `libttclient` | – | ✓ | – |

macOS: TimesTen ships only the client there (no `tt_xla.h`, no `libttdrvmgr`), so darwin builds link
`libttclient` and leave out XLA (`xla`, `examples/xla-tail`) automatically. `LD_LIBRARY_PATH` is not used
on macOS; add an rpath instead:
`export CGO_LDFLAGS="-L$TIMESTEN_HOME/install/lib -Wl,-rpath,$TIMESTEN_HOME/install/lib"`.

## database/sql

```go
import (
	"database/sql"
	"github.com/chuongnx/go-timesten/ttsql"
)

db, err := sql.Open("timesten", "DSN=sampledb;UID=app;PWD=secret;go_location=Asia/Ho_Chi_Minh")
db.SetMaxOpenConns(16)            // pooling is database/sql's
db.SetConnMaxIdleTime(5 * time.Minute)

rows, err := db.QueryContext(ctx, "SELECT id, amount FROM orders WHERE customer = ?", "c1")

// PL/SQL: OUT, IN OUT, function result, REF CURSOR (keep one connection while reading the cursor)
conn, _ := db.Conn(ctx)
var name string
var cur ttsql.Cursor
_, err = conn.ExecContext(ctx, "BEGIN pkg.find(?, ?, ?); END;", 42, sql.Out{Dest: &name}, sql.Out{Dest: &cur})
for cur.Next() { cur.Scan(&id, &amount) }
cur.Close()

// batch: one round trip per 1024 rows (go_batch_rows)
n, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO orders VALUES (?, ?, ?)", rows)

// durable commit on DurableCommits=0 databases
tx, _ := conn.BeginTx(ctx, nil)
...
ttsql.DurableCommit(ctx, tx)
tx.Commit()
```

DSN options (removed before the string reaches TimesTen): `go_location` (zone of DATE/TIMESTAMP, default
Local), `go_stmt_cache` (prepared statements cached per connection for Exec, default 32),
`go_fetch_rows` (rows per fetch, default 256), `go_batch_rows` (rows per array execution, default 1024).

Value mapping:

| TimesTen | Go (Scan default) | Notes |
|---|---|---|
| TT_TINYINT / SMALLINT / INTEGER / BIGINT, NUMBER(p≤18,0) | `int64` | |
| NUMBER(p,s), NUMBER, FLOAT | `string` | exact decimal text; scans into `float64`/`int64` too |
| BINARY_FLOAT / BINARY_DOUBLE | `float64` | |
| DATE, TIMESTAMP, TT_DATE, TT_TIMESTAMP | `time.Time` | TT_TIMESTAMP has microsecond resolution |
| TIME | `time.Time` (0000-01-01) | |
| CHAR / VARCHAR2 / NCHAR / NVARCHAR2 / CLOB / NCLOB | `string` | UTF-8 (`ConnectionCharacterSet=AL32UTF8`) |
| BINARY / VARBINARY / BLOB | `[]byte` | |
| ROWID | `string` | |

Errors are `*tt.Error`; `tt.IsTransient(err)` (lock conflict 6001, deadlock 6002, lock timeout 6003,
connection lost) tells when retrying the transaction may succeed. A cancelled context aborts the running
statement (`SQLCancel`, TimesTen error 6113) and the error wraps both `ctx.Err()` and the `*tt.Error`.
Connections that saw a connection-level error are dropped by the pool.

## XLA

```go
r, err := xla.Open(ctx, "DSN=sampledb;UID=xlauser;PWD=secret", "orders_cdc") // create or reuse
defer r.Close()
r.Subscribe(ctx, "APP", "ORDERS")
for {
	recs, err := r.Next(ctx, xla.Batch(1000), xla.Wait(2*time.Second))
	for _, rec := range recs {
		switch rec.Op {
		case xla.OpInsert, xla.OpUpdate, xla.OpDelete:
			amount, _ := rec.NewValue("AMOUNT") // xla.Value: Kind + Int/Float/Text/Bytes/Time
			key := rec.Key()                    // primary key values
		case xla.OpTruncate:
			// rec.Owner, rec.Name
		}
	}
	// forward recs somewhere durable, then:
	r.Ack()
}
```

Semantics worth knowing:

- Records arrive in commit order; `FirstInTxn()` / `EndsTxn()` mark transaction boundaries, `TxnID`
  groups them. Records of one transaction may share an `LSN`.
- `Ack` persists the bookmark at the last record returned by `Next`. Unacknowledged records are delivered
  again after a reconnect; `Position` / `Rewind` re-read without reconnecting.
- DDL records (CREATE / DROP / TRUNCATE …) of **all** tables reach every bookmark — filter by table.
- LOB columns are reported (`KindLOB`) but XLA does not deliver their contents.
- A bookmark holds transaction log files until acknowledged; delete unused bookmarks
  (`DeleteBookmark`, `admin.DeleteBookmark`). A subscribed table cannot be dropped
  (`admin.UnsubscribeAll` first). At most 64 bookmarks per database, names ≤ 31 characters, one open
  reader per bookmark (`tt.ErrXlaBookmarkInUse`).
- A `Reader` serializes its calls; open one reader per bookmark and per goroutine that reads.

`examples/xla-tail` prints changes as JSON lines; `examples/sql` exercises the driver.

## Development

Everything runs in a container with TimesTen, Go and gcc and its own database (`gotest`):

```bash
make dev-up    # once
make test      # unit tests
make itest     # integration tests (real database)
make race      # integration tests with -race and cgocheck
make bench     # ExecBatch / XLA read throughput
make faults    # restarts the container's TimesTen daemon: pool recovery
```

`dev/Dockerfile` builds on an existing TimesTen image (`TT_IMAGE`).

Behaviour verified against TimesTen 22.1.1.36 (see tests) that differs from plain ODBC expectations:

- NUMBER without precision and FLOAT(n) are described as `SQL_DOUBLE` → fetched as text to keep every digit.
- With SERIALIZABLE isolation an array fetch can return `SQL_NO_DATA` together with the last rows.
- REF CURSOR binding takes the second statement handle itself as the data pointer.
- XLA: `ttXlaConvertCharType` rejects NCHAR/NVARCHAR2 (decoded from UTF-16 here); NCHAR sizes are in
  bytes; `ttXlaTimeStampToODBCCType` reports TT_TIMESTAMP fractions in microseconds.
