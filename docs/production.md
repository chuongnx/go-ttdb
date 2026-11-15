# Production notes

This page describes how the driver behaves when things go wrong, and what your application should
do about it. Each behaviour is verified by a test:

- `ttsql/robust_integration_test.go`
- `ttsql/netfault_integration_test.go`, which puts a TCP proxy between client and server and breaks the
  link.
- The fault-injection test, which restarts the daemon.
- The soak test.

Contents:

- [Error classes and retries](#error-classes-and-retries)
- [Lock conflicts and deadlocks](#lock-conflicts-and-deadlocks)
- [Lost connections](#lost-connections)
- [Connect timeouts](#connect-timeouts)
- [DDL while statements are cached](#ddl-while-statements-are-cached)
- [Cancellation](#cancellation)
- [CLOB over client/server](#clob-over-clientserver)
- [Memory](#memory)
- [Cursors](#cursors)
- [Checklist](#checklist)

## Error classes and retries

| Check | True for | Action |
|---|---|---|
| `tt.IsTransient(err)` | lock conflict 6001, deadlock 6002, lock timeout 6003, connection lost | retry the whole transaction |
| `tt.IsConnectionLost(err)` | SQLSTATE 08xxx, TimesTen 846, 994, 47137, 47138 | the connection is already discarded; retry on a new one |
| `errors.Is(err, context.Canceled / DeadlineExceeded)` | cancelled statements (6113) | do not retry |
| `tt.Code(err) == tt.ErrKeyExists` | unique key violation (907) | application logic |

## Lock conflicts and deadlocks

A row lock that cannot be granted within the lock wait time fails with 6003. A transaction chosen as
a deadlock victim fails with 6002. Both are transient, so retry the **whole** transaction, not only
the failed statement:

```go
func withRetry(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := func() error {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := fn(tx); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err == nil || !tt.IsTransient(err) || attempt == 3 {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
}
```

The lock wait time is set per connection: `CALL ttLockWait(seconds)`. The default is 10 seconds. Set
it in `Config.InitSQL` so every pooled connection has the same value
([connecting.md](connecting.md#connector-and-config)).

## Lost connections

A network failure or a server restart is handled like this:

1. The statement that hits the failure returns an error with `tt.IsConnectionLost(err)` true, and
   that connection is discarded.
2. The driver records the time of the failure. Every other pooled connection opened before it is
   checked before its next use, and dead ones are replaced silently (`driver.ErrBadConn`).
3. So one failure costs the application **one** error, not one error per pooled connection.
4. A transaction interrupted this way is rolled back by the server. Its outcome is never "maybe
   committed", except when the failure hit during `Commit` itself. In that case, check whether the
   transaction was applied before you retry.

These cases are tested: a dead idle pool, a failure during a query, during a transaction and during a
fetch, the server going down, and the server hanging.

## Connect timeouts

Over client/server, a refused connection fails at once. A server that accepts TCP but does not answer
blocks until `TTC_Timeout` seconds have passed (default 60). Set `TTC_Timeout=10` in the connection
string.

`SQLDriverConnect` cannot be cancelled. A context deadline makes `Connect` return, but the attempt
finishes in the background, and its connection is closed when it arrives.

## DDL while statements are cached

After `ALTER TABLE`, `CREATE INDEX` or `DROP` + `CREATE` of a table, statements prepared earlier fail
inside TimesTen with TT0946 ("SQL command is no longer valid"). The driver prepares them again and
retries once. Cached `Exec` statements, `db.Prepare` statements and `ExecBatch` all keep working, and
the application never sees TT0946.

For XLA readers and ALTER TABLE, see [xla.md](xla.md#ddl-and-alter-table).

## Cancellation

Cancelling a context stops a running query, a fetch in progress, a PL/SQL block or a batch. The driver
uses `SQLCancel` for this, and TimesTen reports error 6113. The connection stays usable.

In autocommit mode, the `ExecBatch` rows that ran before the cancel stay committed: whole chunks
(`go_batch_rows` rows each) and the part of the chunk that was running. The returned count covers
them, and the error is a `*BatchError` that also wraps `ctx.Err()`. To get all-or-nothing, run the batch inside a transaction.

## CLOB over client/server

> **Warning:** the TimesTen client driver (`libttclient`, seen in 22.1.1.36 and 22.1.1.37) garbles
> **CLOB** text that contains multibyte characters and is longer than 4 KB.

- **What happens.** The client driver sends the value in 4 KB pages. It pads a page with NUL bytes when
  a character does not fit, and loses bytes.
- **What is not affected.** The stored data is correct. Direct connections, NCLOB and VARCHAR2 read it
  correctly.
- **What the driver does.** It detects the damage and returns an error wrapping `tt.ErrClientCLOB`
  instead of corrupted text.
- **What to do.** For Vietnamese, Chinese or other non-ASCII text read over client/server, use
  **NCLOB**, or **VARCHAR2** (up to 4 MB in TimesTen), instead of CLOB.

`TestClientCLOBWorkarounds` verifies this. `Conn.IsClientServer()` tells which kind of connection you
have.

## Memory

A 15-minute soak test ran 3.7 million mixed operations: queries, transactions, batches, LOBs and
PL/SQL. The Go heap, the goroutine count and the pool size stayed flat, and there was no leak.

Private memory of LOB-heavy workloads on Linux creeps up and then levels off. That is glibc heap
fragmentation from large buffers freed on different threads. For services that move many LOBs, set:

```bash
MALLOC_ARENA_MAX=2
MALLOC_MMAP_THRESHOLD_=32768   # only if still needed
```

Run the soak test on your own platform with `make soak SOAK=30m` ([testing.md](testing.md)).

## Cursors

Cursors left open are closed with their connection. Returning a connection to the pool with an open
cursor is safe, but close cursors promptly, because each one holds a statement handle on the server.

## Checklist

- `ConnectionCharacterSet=AL32UTF8` in the connection string.
- `go_location=UTC`, or another explicit zone, and time zone data in the binary (`import _ "time/tzdata"`).
- Limit the pool: `SetMaxOpenConns`, `SetConnMaxIdleTime`.
- Client/server: `TTC_Timeout`.
- `CALL ttLockWait(n)` in `InitSQL`, and retry transient errors.
- No CLOB for non-ASCII text over client/server.
- XLA: acknowledge per batch, make the consumer idempotent, and watch `admin.LogHolds`.
- XLA: catch up readers before you `ALTER` a subscribed table.
- LOB-heavy services on glibc: `MALLOC_ARENA_MAX=2`.
