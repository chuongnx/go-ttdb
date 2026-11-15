# Connecting

```go
import (
	"database/sql"

	"github.com/chuongnx/go-ttdb/ttsql" // registers the driver "timesten"
)

db, err := sql.Open(ttsql.DriverName, "DSN=sampledb;UID=app;PWD=secret")
```

`sql.Open` does not connect. Call `db.PingContext(ctx)` at startup to fail fast on wrong settings.

- [Connection strings](#connection-strings)
- [Direct connections](#direct-connections)
- [Client/server](#client-server)
- [Driver options (go_*)](#driver-options-go_)
- [Connector and Config](#connector-and-config)
- [Pool settings](#pool-settings)
- [Which connection do I have?](#which-connection-do-i-have)

## Connection strings

The data source name is an ordinary TimesTen ODBC connection string: `KEY=value` pairs separated by
`;`. Any TimesTen connection attribute can be used (`DSN`, `UID`, `PWD`, `ConnectionCharacterSet`,
`TTC_SERVER`, `TTC_Timeout`, `PLSQL_TIMEOUT`, …). Values containing `;` must be enclosed in braces:
`PWD={se;cret}`.

Keys starting with `go_` are [driver options](#driver-options-go_). The driver removes them before it
passes the string to TimesTen.

## Direct connections

A direct connection attaches the database's shared memory in your process. It is the fastest mode,
and the only one that supports XLA. The program must run on the database host, as an OS user that
may access the instance.

```go
// DSN defined in $TIMESTEN_HOME/conf/sys.odbc.ini
db, err := sql.Open("timesten", "DSN=sampledb;UID=app;PWD=secret")
```

A typical `sys.odbc.ini` entry:

```ini
[sampledb]
Driver=/path/to/instance/install/lib/libtten.so
DataStore=/path/to/datastore/sampledb
DatabaseCharacterSet=AL32UTF8
ConnectionCharacterSet=AL32UTF8
PermSize=1024
```

## Client/server

A client/server connection needs only the TimesTen client on the local host. The database runs on
a TimesTen server, which listens on port 6625 by default. Everything in `ttsql` works over
client/server: queries, transactions, batches, PL/SQL, REF CURSORs and cancellation. XLA does not.

```go
// Without a client DSN: server host/port and the server-side DSN
db, err := sql.Open("timesten", "TTC_SERVER=dbhost/6625;TTC_SERVER_DSN=sampledb;UID=app;PWD=secret")

// With a client DSN from sys.odbc.ini, e.g.
//   [sampledbCS]
//   TTC_SERVER=dbhost/6625
//   TTC_SERVER_DSN=sampledb
db, err := sql.Open("timesten", "DSN=sampledbCS;UID=app;PWD=secret")
```

Recommended attributes:

| Attribute | Why |
|---|---|
| `TTC_Timeout=10` | Seconds to wait for a server that accepts TCP but does not answer. The default is 60. `SQLDriverConnect` cannot be cancelled, so a context deadline only abandons the wait. |
| `ConnectionCharacterSet=AL32UTF8` | UTF-8 text in and out |

> **Note:** do not use CLOB columns for non-ASCII text that you read over client/server. See
> [production.md](production.md#clob-over-clientserver).

## Driver options (go_*)

| Option | Default | Meaning |
|---|---|---|
| `go_location` | `Local` | Time zone used for DATE / TIMESTAMP values, for example `UTC` or `Asia/Ho_Chi_Minh`. Needs time zone data ([installation.md](installation.md#time-zone-data)). |
| `go_stmt_cache` | `32` | Prepared statements cached per connection (LRU) for `Exec` and `ExecBatch` with the same SQL text. `0` turns the cache off. Queries are prepared per call; use `db.Prepare` to reuse a query. |
| `go_fetch_rows` | `256` | Rows fetched per round trip (array fetch) |
| `go_batch_rows` | `1024` | Rows per array execution in `ExecBatch` |

An unknown `go_` key is an error, which catches typos early.

```go
db, err := sql.Open("timesten",
	"DSN=sampledb;UID=app;PWD=secret;go_location=UTC;go_fetch_rows=1000")
```

## Connector and Config

Use a `Connector` to set options from code, or to run statements on every new connection:

```go
cfg := ttsql.DefaultConfig("TTC_SERVER=dbhost/6625;TTC_SERVER_DSN=sampledb;UID=app;PWD=secret")
cfg.Location = time.UTC
cfg.InitSQL = []string{
	"CALL ttLockWait(5)", // lock wait timeout in seconds, per connection
}
conr, err := ttsql.NewConnector(cfg)
if err != nil {
	log.Fatal(err)
}
db := sql.OpenDB(conr)
```

`ttsql.ParseDSN(dsn)` returns the `Config` for a DSN string, including its `go_` options. Start
from it when you want both.

## Pool settings

Pooling comes from `database/sql`. Each pooled connection is one TimesTen connection. Connections
are limited by the database's `Connections` attribute, and every connection holds server
resources, so limit the pool:

```go
db.SetMaxOpenConns(16)
db.SetMaxIdleConns(16)
db.SetConnMaxIdleTime(5 * time.Minute)
db.SetConnMaxLifetime(time.Hour)
```

The driver works with the pool. When a statement fails with a connection-level error (SQLSTATE 08xxx,
TimesTen 846/994/47137/47138), the driver:

- Discards that connection.
- Checks every other pooled connection opened before the failure, the next time it is reused.

Dead connections are replaced silently. See [production.md](production.md#lost-connections).

## Which connection do I have?

```go
conn, _ := db.Conn(ctx)
defer conn.Close()
conn.Raw(func(dc any) error {
	fmt.Println("client/server:", dc.(*ttsql.Conn).IsClientServer())
	return nil
})
```
