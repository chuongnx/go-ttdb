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

### Client/server connections

A client/server connection needs no database on the local host — only the TimesTen client. Name the
server in the connection string, or use a client DSN from `sys.odbc.ini` / `sys.ttconnect.ini`:

```go
// DSN-less: host/port of the TimesTen server and the server-side DSN
db, err := sql.Open("timesten", "TTC_SERVER=dbhost/6625;TTC_SERVER_DSN=sampledb;UID=app;PWD=secret")

// or a client DSN ([sampledbCS] TTC_SERVER=sampledb_CS, TTC_SERVER_DSN=sampledb)
db, err := sql.Open("timesten", "DSN=sampledbCS;UID=app;PWD=secret")
```

Everything in [database/sql](#databasesql) works the same over client/server (queries, transactions,
batches, PL/SQL, REF CURSORs, cancellation); only XLA needs a direct connection. To run the integration
tests from a client-only host (e.g. macOS) against a server:

```bash
export TIMESTEN_DSN="TTC_SERVER=dbhost/6625;TTC_SERVER_DSN=gotest;UID=gotest;PWD=gotest"
go test -tags integration -count=1 ./ttsql/
```

The dev container's server listens on 6625 but does not publish it; forward it to the host with
`docker run -d --rm --name tt-cs-forward --network go-timesten-dev_default -p 127.0.0.1:16625:16625
alpine/socat tcp-listen:16625,fork,reuseaddr tcp-connect:go-timesten-dev:6625` and use
`TTC_SERVER=localhost/16625`.

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
| NUMBER(p,s), NUMBER, FLOAT | `string` | exact decimal text with a leading zero (`0.25`, `-0.5`; TimesTen itself sends `.25`); scans into `float64`/`int64` too |
| BINARY_FLOAT / BINARY_DOUBLE | `float64` | |
| DATE, TIMESTAMP, TT_DATE, TT_TIMESTAMP | `time.Time` | TT_TIMESTAMP has microsecond resolution |
| TIME | `time.Time` (0000-01-01) | |
| CHAR / VARCHAR2 / NCHAR / NVARCHAR2 / CLOB / NCLOB | `string` | UTF-8 (`ConnectionCharacterSet=AL32UTF8`) |
| BINARY / VARBINARY / BLOB | `[]byte` | |
| ROWID | `string` | |

> **⚠️ CLOB over client/server.** The TimesTen client driver (`libttclient`, verified with 22.1.1.36 and
> 22.1.1.37) garbles CLOB text that contains multibyte characters and is longer than 4 KB. It sends the
> value in 4 KB pages, pads a page with NULs when a character does not fit, and loses bytes. The stored
> data is correct, and direct connections, NCLOB and VARCHAR2 read it correctly. The driver detects
> the damage and returns an error wrapping `tt.ErrClientCLOB` rather than corrupted text. For
> Vietnamese or other non-ASCII text read over client/server, use **NCLOB** or **VARCHAR2** (up to
> 4 MB in TimesTen) instead of CLOB (`TestClientCLOBWorkarounds`). `Conn.IsClientServer()` tells which
> kind of connection you have.

Value rules worth knowing (all covered by `TestTypeEdgeCases` and related tests):

- `""` is stored as NULL (Oracle semantics). Code that tells `""` from NULL must not rely on the database.
- NUMBER(19) and wider can exceed `int64`: `Scan` into `int64` then fails with "value out of range".
  Scan into `string` for exact values. A `uint64` above `math.MaxInt64` is rejected by `database/sql`;
  pass it as a string.
- `bool` is sent as 1 / 0. BINARY_FLOAT/DOUBLE keep NaN and ±Inf.
- `time.Time` values are converted to `go_location` before they are stored, and DATE drops fractional
  seconds. In a zone with daylight saving time, a wall-clock time that does not exist (the hour skipped in
  spring) is read back shifted by Go's normalization. Prefer `UTC` or a zone without DST for timestamps.
- TimesTen has no INTERVAL columns. `date1 - date2` is a NUMBER of days.
- Strings and `[]byte` over 32 KB are bound as LOB types, so they reach PL/SQL CLOB/BLOB variables
  intact. The PL/SQL VARCHAR2 limit would otherwise reject them with ORA-06502.
- `ExecBatch` cannot insert into CLOB/BLOB/NCLOB columns ("Batch mode not supported for LOBs"). Batch the
  other columns, then write the LOBs row by row. Every value in one batch column must be the same kind
  (integers may mix with floats). Send exact decimals as strings.

Errors are `*tt.Error`; `tt.IsTransient(err)` (lock conflict 6001, deadlock 6002, lock timeout 6003,
connection lost) tells when retrying the transaction may succeed. A cancelled context aborts the running
statement (`SQLCancel`, TimesTen error 6113) and the error wraps both `ctx.Err()` and the `*tt.Error`.
Connections that saw a connection-level error are dropped by the pool.

### Production behaviour

These behaviours are verified by `ttsql/robust_integration_test.go` and `ttsql/netfault_integration_test.go`:

- **DDL while statements are cached.** After ALTER TABLE, CREATE INDEX or DROP + CREATE of a table,
  statements prepared earlier fail inside TimesTen with TT0946 ("SQL command is no longer valid"). The
  driver prepares them again and retries once, so cached `Exec` statements, `db.Prepare` statements and
  `ExecBatch` keep working without the application seeing the error.
- **Lock conflicts and deadlocks.** These return 6003 (lock wait timeout) or 6002 (deadlock victim), and
  `tt.IsTransient` is true. Retry the whole transaction:

  ```go
  for attempt := 0; ; attempt++ {
  	err := runTx(ctx, db) // BeginTx … Commit
  	if err == nil || !tt.IsTransient(err) || attempt == 3 {
  		return err
  	}
  	time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
  }
  ```

  The lock wait is a per-connection setting (`CALL ttLockWait(seconds)`). Set it in `Config.InitSQL` so
  every pooled connection has the same value.
- **Lost connections** (network failure, server restart). The statement that hits the failure returns an
  error with `tt.IsConnectionLost(err)` true (SQLSTATE 08xxx), and that connection is discarded. The
  driver then checks every other pooled connection opened before the failure before reusing it, and
  replaces the dead ones silently. One failure therefore costs the application one error, not one per
  pooled connection. A transaction interrupted this way is rolled back by the server.
- **Connect timeouts (client/server).** A refused connection fails at once. A server that accepts TCP but
  does not answer blocks until `TTC_Timeout` seconds pass (default 60). Set it in the connection string,
  e.g. `TTC_Timeout=10`. `SQLDriverConnect` cannot be cancelled, so a context deadline only abandons the
  wait.
- **Cancellation.** Cancelling stops a running query, a fetch in progress, a PL/SQL block or a batch. In
  autocommit mode, the `ExecBatch` chunks (of `go_batch_rows` rows) completed before the cancel stay
  committed, and the returned count says how many. Run the batch inside a transaction for all-or-nothing.
- **Memory.** A 15-minute soak test (3.7 million mixed operations) shows a flat Go heap and no leak.
  Private memory of LOB-heavy workloads still creeps up and then levels off. That is glibc fragmentation
  from large buffers freed across threads: with `MALLOC_ARENA_MAX=2` (and, if needed,
  `MALLOC_MMAP_THRESHOLD_=32768`) it stays flat. Set these for services that move many LOBs.
- **Cursors** left open are closed with their connection. Returning a connection to the pool with an
  open cursor is safe, but close cursors promptly: each one holds a statement handle on the server.

## Guide: tables, CRUD and PL/SQL packages

The examples below come from `ttsql/plsql_integration_test.go`, which runs them against a real database.

### Create a table, INSERT / SELECT / UPDATE / DELETE

Parameters are `?` placeholders; `RowsAffected` reports the affected row count.

```go
db.ExecContext(ctx, `CREATE TABLE orders (
	id NUMBER(10) NOT NULL PRIMARY KEY, customer VARCHAR2(40) NOT NULL, note NVARCHAR2(100),
	amount NUMBER(12,2), created TIMESTAMP(6))`)

res, err := db.ExecContext(ctx, "INSERT INTO orders (id, customer, amount, created) VALUES (?, ?, ?, ?)",
	1, "alice", 10.5, time.Now())

var customer string
var amount float64
err = db.QueryRowContext(ctx, "SELECT customer, amount FROM orders WHERE id = ?", 1).Scan(&customer, &amount)
// no row → sql.ErrNoRows

res, err = db.ExecContext(ctx, "UPDATE orders SET amount = amount * 2, note = NULL WHERE customer = ?", "alice")
n, _ := res.RowsAffected()

res, err = db.ExecContext(ctx, "DELETE FROM orders WHERE id = ?", 1)

rows, err := db.QueryContext(ctx, "SELECT id, amount FROM orders ORDER BY id")
defer rows.Close()
for rows.Next() {
	var id int
	var amount string // NUMBER as exact decimal text
	rows.Scan(&id, &amount)
}
err = rows.Err()
```

Scan NULLable columns into `sql.NullString`, `sql.NullInt64`, `sql.NullFloat64`, `sql.NullTime` or
`[]byte` (nil for NULL).

### Create a package

Send the package specification and the body as two statements. `ttxtest.Exec` / `db.Exec` take the
whole `CREATE OR REPLACE PACKAGE ...` text; `SQL%ROWCOUNT` gives the rows touched by DML inside PL/SQL.

```sql
CREATE OR REPLACE PACKAGE order_pkg AS
	PROCEDURE add_order(p_id IN NUMBER, p_customer IN VARCHAR2, p_amount IN NUMBER, p_created IN TIMESTAMP);
	PROCEDURE set_amount(p_id IN NUMBER, p_amount IN NUMBER, o_rows OUT NUMBER);
	PROCEDURE remove_order(p_id IN NUMBER, o_rows OUT NUMBER);
	PROCEDURE get_order(p_id IN NUMBER, o_customer OUT VARCHAR2, o_note OUT NVARCHAR2, o_code OUT CHAR,
		o_amount OUT NUMBER, o_qty OUT PLS_INTEGER, o_big OUT NUMBER, o_ratio OUT BINARY_DOUBLE,
		o_bf OUT BINARY_FLOAT, o_day OUT DATE, o_created OUT TIMESTAMP, o_raw OUT RAW);
	PROCEDURE bump(p_counter IN OUT NUMBER, p_text IN OUT VARCHAR2);
	FUNCTION order_count RETURN NUMBER;
	FUNCTION orders_of(p_customer IN VARCHAR2) RETURN SYS_REFCURSOR;
	PROCEDURE report(p_min IN NUMBER, o_total OUT NUMBER, o_orders OUT SYS_REFCURSOR,
		o_customers OUT SYS_REFCURSOR);
END order_pkg;
```

```sql
CREATE OR REPLACE PACKAGE BODY order_pkg AS
	PROCEDURE set_amount(p_id IN NUMBER, p_amount IN NUMBER, o_rows OUT NUMBER) IS
	BEGIN
		UPDATE orders SET amount = p_amount WHERE id = p_id;
		o_rows := SQL%ROWCOUNT;
	END;
	PROCEDURE get_order(...) IS
	BEGIN
		SELECT ... INTO o_customer, ... FROM orders WHERE id = p_id;
	EXCEPTION
		WHEN NO_DATA_FOUND THEN RAISE_APPLICATION_ERROR(-20001, 'order ' || p_id || ' not found');
	END;
	FUNCTION orders_of(p_customer IN VARCHAR2) RETURN SYS_REFCURSOR IS
		c SYS_REFCURSOR;
	BEGIN
		OPEN c FOR SELECT id, amount, created FROM orders WHERE customer = p_customer ORDER BY id;
		RETURN c;
	END;
	-- ... the other procedures and functions
END order_pkg;
```

Drop it with `DROP PACKAGE order_pkg`.

### Call procedures and functions

Wrap calls in an anonymous block, `BEGIN ...; END;`. Pass IN values directly, OUT parameters as
`sql.Out{Dest: &v}`, IN OUT parameters as `sql.Out{Dest: &v, In: true}`. A function result is the first
`?` of `BEGIN ? := pkg.fn(...); END;`.

```go
// INSERT / UPDATE / DELETE through the package
db.ExecContext(ctx, "BEGIN order_pkg.add_order(?, ?, ?, ?); END;", 1, "alice", 100, time.Now())

var updated int
db.ExecContext(ctx, "BEGIN order_pkg.set_amount(?, ?, ?); END;", 1, 250.75, sql.Out{Dest: &updated})

var deleted int
db.ExecContext(ctx, "BEGIN order_pkg.remove_order(?, ?); END;", 1, sql.Out{Dest: &deleted})

// function result
var count int64
db.ExecContext(ctx, "BEGIN ? := order_pkg.order_count; END;", sql.Out{Dest: &count})

// IN OUT
counter, text := int64(41), "xin chào"
db.ExecContext(ctx, "BEGIN order_pkg.bump(?, ?); END;",
	sql.Out{Dest: &counter, In: true}, sql.Out{Dest: &text, In: true}) // 42, "XIN CHÀO!"
```

Calls made through a `*sql.Tx` belong to that transaction: `tx.Rollback()` undoes the DML the
procedures did.

### OUT parameters of every type

| PL/SQL OUT type | Go `Dest` | NULL-able `Dest` |
|---|---|---|
| VARCHAR2, NVARCHAR2, CHAR, NCHAR | `*string` | `*sql.NullString` |
| NUMBER (exact) | `*string` (decimal text) | `*sql.NullString` |
| NUMBER (integer), PLS_INTEGER, BINARY_INTEGER | `*int64`, `*int`, `*int32`, … | `*sql.NullInt64`, `*sql.NullInt32` |
| NUMBER, BINARY_DOUBLE, BINARY_FLOAT | `*float64`, `*float32` | `*sql.NullFloat64` |
| DATE, TIMESTAMP | `*time.Time` | `*sql.NullTime` |
| RAW | `*[]byte` | `*[]byte` (nil for NULL) |
| SYS_REFCURSOR | `*ttsql.Cursor` | – |

```go
var (
	customer, note, code, amount string
	qty                          int32
	big                          int64
	ratio                        float64
	bf                           float32
	day, created                 time.Time
	raw                          []byte
)
_, err := db.ExecContext(ctx, "BEGIN order_pkg.get_order(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?); END;", 1,
	sql.Out{Dest: &customer}, sql.Out{Dest: &note}, sql.Out{Dest: &code}, sql.Out{Dest: &amount},
	sql.Out{Dest: &qty}, sql.Out{Dest: &big}, sql.Out{Dest: &ratio}, sql.Out{Dest: &bf},
	sql.Out{Dest: &day}, sql.Out{Dest: &created}, sql.Out{Dest: &raw})
```

Notes:

- String and RAW OUT buffers are 32767 bytes. A longer value fails with "OUT parameter truncated".
  For IN OUT parameters the buffer is at least the size of the input.
- A NUMBER OUT read into `*string` keeps the text exactly as TimesTen produces it, without the leading
  zero (`.25`, `-.5`). TimesTen describes every PL/SQL block parameter as VARCHAR, so the driver cannot
  tell a NUMBER OUT from a VARCHAR2 OUT. Read NUMBER OUT parameters into `*float64` / `*int64`, or
  return an exact decimal through a REF CURSOR, where it is normalized like any column.
- An unconstrained PL/SQL `CHAR` OUT parameter is blank-padded to the buffer size. Trim it with
  `strings.TrimRight(code, " ")`, or declare it `VARCHAR2`.
- Errors raised in PL/SQL come back as `*tt.Error`. The message contains the Oracle code and text,
  for example `ORA-20001: order 999 not found` from `RAISE_APPLICATION_ERROR`.
- PL/SQL-only types (BOOLEAN, RECORD, collections such as `TABLE OF ... INDEX BY`) have no ODBC
  binding. Binding them directly fails with `TT8503: ORA-06550` (wrong number or types of arguments).
  Convert them to scalars in an anonymous block instead (`TestPLSQLOnlyTypes`):

  ```go
  // BOOLEAN → 1/0
  db.Exec("DECLARE v BOOLEAN; BEGIN pkg.is_big(?, v); ? := CASE WHEN v THEN 1 ELSE 0 END; END;", 42, sql.Out{Dest: &big})
  // RECORD → one OUT per field
  db.Exec("DECLARE r pkg.rec_t; BEGIN pkg.get_rec(r); ? := r.a; ? := r.b; END;", sql.Out{Dest: &a}, sql.Out{Dest: &b})
  // collection → elements / COUNT (or OPEN a cursor over the data instead)
  db.Exec("DECLARE c pkg.num_tab; BEGIN pkg.get_tab(c); ? := c(1); ? := c.COUNT; END;", sql.Out{Dest: &first}, sql.Out{Dest: &n})
  ```

### REF CURSOR

A cursor is read on the connection that opened it. Use a `*sql.Conn` or a `*sql.Tx`, and close the
cursor before releasing the connection. A cursor can come from an OUT parameter or from a function
result, and one call can return several cursors.

```go
conn, _ := db.Conn(ctx)
defer conn.Close()

// function returning SYS_REFCURSOR
var cur ttsql.Cursor
_, err := conn.ExecContext(ctx, "BEGIN ? := order_pkg.orders_of(?); END;", sql.Out{Dest: &cur}, "alice")
cols, _ := cur.Columns() // [ID AMOUNT CREATED]
for cur.Next() {
	var id int
	var amount float64
	var created time.Time
	if err := cur.Scan(&id, &amount, &created); err != nil { ... }
}
err = cur.Err()
cur.Close()

// a scalar OUT and two cursors from one call
var total float64
var orders, customers ttsql.Cursor
_, err = conn.ExecContext(ctx, "BEGIN order_pkg.report(?, ?, ?, ?); END;", 100,
	sql.Out{Dest: &total}, sql.Out{Dest: &orders}, sql.Out{Dest: &customers})
defer orders.Close()
defer customers.Close()
for orders.Next() { ... }
for customers.Next() { ... }
```

`Cursor.Scan` uses the same conversions as `rows.Scan`, including the `sql.Null*` types. An empty
cursor returns `false` from the first `Next`, and `Err` is nil.

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
- **ALTER TABLE on a subscribed table.** A reader that keeps up receives the `OpAddColumns` /
  `OpDropColumns` record, with the table's owner and name, and decodes later rows with the new definition.
  A reader that has **not yet read** rows logged before the ALTER cannot decode them: every `Next` fails
  with TT8037 (`tt.ErrXlaTableDiff`), and reopening does not help. Before altering a subscribed table,
  stop the writers, let every reader catch up (`Next` returns nothing, then `Ack`), and then alter.
- Tables without a primary key are delivered; `Key()` is empty, and the before image identifies the row.
- After a server restart, `Next` fails with a connection error. Reopen the bookmark: reading resumes
  after the last `Ack`, so unacknowledged records arrive again and acknowledged ones do not.
  Direct connections need one more step. A process keeps the old instance's shared memory attached while
  **any** of its connections to it stays open, including idle connections in a `*sql.DB` pool that have
  not noticed the restart. Until those are closed, new connections fail with TT8521 ("Cannot attach
  PL/SQL shared memory"). Close the reader, flush the pool (`db.SetMaxIdleConns(0)`, then restore the
  value), and reopen in a retry loop (`TestReaderSurvivesDaemonRestart`).

`examples/xla-tail` prints changes as JSON lines; `examples/sql` exercises the driver.

## Development

Everything runs in a container with TimesTen, Go and gcc and its own database (`gotest`):

```bash
make dev-up      # once
make test        # unit tests
make itest       # integration tests (real database, direct connection)
make race        # integration tests with -race and cgocheck
make itest-cs    # ttsql integration tests over client/server
make race-cs     # the same with -race and cgocheck
make netfaults   # client/server network failures (in-test TCP proxy)
make bench       # ExecBatch / XLA read throughput (bench-cs: over client/server)
make faults      # restarts the container's TimesTen daemon: pool and XLA reader recovery
make soak        # mixed workload for SOAK=30m, watching private memory, goroutines, pool
make ci          # lint + race + race-cs + netfaults
```

`make faults` and `make soak` must not run at the same time: `faults` restarts the database.

`dev/Dockerfile` builds on an existing TimesTen image (`TT_IMAGE`).

Integration tests by area (`go test -tags integration -run <name> ./ttsql/`):

| Test | Covers |
|---|---|
| `TestTypesRoundTrip`, `TestColumnTypes` | every column type in and out, NULLs, column metadata |
| `TestCRUDPlainSQL` | INSERT / SELECT / UPDATE / DELETE, `RowsAffected`, `sql.ErrNoRows` |
| `TestPackageCRUD` | DML through package procedures, function results, rollback of procedure calls |
| `TestPackageOutputTypes` | an OUT parameter of each type, NULL OUT values, `RAISE_APPLICATION_ERROR` |
| `TestPackageInOut` | IN OUT parameters |
| `TestPackageCursors` | REF CURSOR from a function, several cursors from one call, empty cursor |
| `TestDecimalLeadingZero` (ttsql), `TestDecodeDecimalLeadingZero` (xla) | NUMBER text form in columns, cursors, OUT parameters and XLA |
| `TestPLSQLOnlyTypes` | BOOLEAN / RECORD / collection: direct binding fails, conversion in a block works |
| `TestProceduresAndRefCursor` | standalone procedure and function, built-in `CALL` returning rows |
| `TestTransactions`, `TestBatchErrorRow`, `TestContextCancel`, `TestPoolConcurrency` | transactions, batches, cancellation, pool |
| `TestTypeEdgeCases`, `TestNCharKeys` | extreme NUMBERs, NaN/Inf, `""` = NULL, 4-byte UTF-8, CHAR semantics, years 1–9999, ROWID |
| `TestLongMultibyteText`, `TestLongInputs`, `TestLOBMultiRowFetch` | text and LOBs over 64 KB with multibyte characters, inputs over 32 KB, LOB rows mixed with plain columns |
| `TestClientCLOBWorkarounds` | client/server CLOB defect is reported; NCLOB / VARCHAR2 hold the same text correctly |
| `TestFetchBoundaries`, `TestExecBatchAllKinds` | result sizes around the fetch size, batches of every kind (incl. the LOB limitation) |
| `TestOutLOBAndTruncation`, `TestLocationOption` | CLOB/BLOB OUT, OUT size limit, `go_location` |
| `TestStmtCacheAfterDDL` | cached / prepared statements and batches across ALTER, CREATE INDEX, DROP + CREATE |
| `TestLockTimeoutIsTransient`, `TestDeadlockIsTransient` | real lock conflicts and deadlocks are transient |
| `TestCursorsLeftOpen`, `TestCancelVariants`, `TestConnectErrors` | abandoned cursors, cancellation while fetching / in PL/SQL / in a batch, bad credentials |
| `TestNetFault*` (client/server) | dead idle pool, failure during query / transaction / fetch, server down, server hanging (`TTC_Timeout`) |
| `TestSoak` (`-tags soak`) | long mixed workload; private memory, goroutines and pool stay flat |
| xla: `TestNoPrimaryKey`, `TestAlterTableInSync`, `TestAlterTableWhileLagging`, `TestLogHoldFollowsAck`, `TestHighVolume`, `TestLOBColumnsReported`, `TestReaderSurvivesDaemonRestart` (`-tags faults`) | XLA edge cases and recovery |

Behaviour verified against TimesTen 22.1.1.36 (see tests) that differs from plain ODBC expectations:

- NUMBER without precision and FLOAT(n) are described as `SQL_DOUBLE` → fetched as text to keep every digit.
- With SERIALIZABLE isolation an array fetch can return `SQL_NO_DATA` together with the last rows.
- REF CURSOR binding takes the second statement handle itself as the data pointer.
- `SQLGetData` on long text returns pieces that end on a whole character, so a piece can be shorter than
  the buffer, and LOBs report `SQL_NO_TOTAL` instead of a length. Piece lengths are taken from the
  terminator position.
- Every parameter of a PL/SQL block is described as `VARCHAR(4000)`, whatever its PL/SQL type.
- The client driver returns `SQL_SUCCESS` for a long-text piece cut short at a character boundary
  although more data follows, so pieces are read until `SQL_NO_DATA`.
- `DBMS_LOB` is available in PL/SQL only, not in TimesTen SQL.
- XLA: `ttXlaConvertCharType` rejects NCHAR/NVARCHAR2 (decoded from UTF-16 here); NCHAR sizes are in
  bytes; `ttXlaTimeStampToODBCCType` reports TT_TIMESTAMP fractions in microseconds.
