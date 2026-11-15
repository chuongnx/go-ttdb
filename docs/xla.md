# XLA: change data capture

The TimesTen Transaction Log API (XLA) gives committed changes in commit order. With it you can:

- Replicate data to another system.
- Feed a message queue.
- Invalidate caches.
- Keep an audit trail.

Package `xla` reads it, and package `xla/admin` manages bookmarks and subscriptions with plain SQL.

- [Requirements](#requirements)
- [Reading changes](#reading-changes)
- [Bookmarks and acknowledgement](#bookmarks-and-acknowledgement)
- [Records](#records)
- [DDL and ALTER TABLE](#ddl-and-alter-table)
- [Restarts and failures](#restarts-and-failures)
- [Administration (xla/admin)](#administration-xlaadmin)
- [Limits](#limits)

## Requirements

- **Direct connection.** The program must run on the database host. XLA is not available over
  client/server, so the `xla` package is not built on macOS or with `-tags tt_client`.
- **XLA privilege.** The user needs it: `GRANT XLA TO xlauser`.
- **Log retention.** The database keeps transaction log files until every bookmark has acknowledged
  them, so a stalled reader makes the logs grow.

## Reading changes

```go
r, err := xla.Open(ctx, "DSN=sampledb;UID=xlauser;PWD=secret", "orders_cdc")
if err != nil {
	return err
}
defer r.Close()

if err := r.Subscribe(ctx, "APP", "ORDERS"); err != nil { // persistent; also "APP.ORDERS" with owner ""
	return err
}

for ctx.Err() == nil {
	recs, err := r.Next(ctx, xla.Batch(1000), xla.Wait(2*time.Second))
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		continue
	}
	for _, rec := range recs {
		switch {
		case rec.Op.IsDML():
			handleRow(rec)
		case rec.Op == xla.OpTruncate:
			handleTruncate(rec.Owner, rec.Name)
		default:
			// DDL: OpCreateTable, OpAddColumns, OpDropTable, ...
		}
	}
	if err := sink.Flush(); err != nil { // make the records durable downstream first
		return err
	}
	if err := r.Ack(); err != nil {
		return err
	}
}
```

| Call | What it does |
|---|---|
| `xla.Open(ctx, connStr, bookmark, opts...)` | Connects and opens the bookmark |
| `xla.WithMode(xla.CreateOrReuse)` | Default: reuse the bookmark if it exists, otherwise create it |
| `xla.WithMode(xla.CreateOnly)` / `xla.WithMode(xla.ReuseOnly)` | Fail if the bookmark already exists / does not exist |
| `xla.Replicated()` | Create a replicated bookmark (for active standby pairs) |
| `xla.WithLocation(loc)` | Time zone for DATE / TIMESTAMP values (default `time.Local`) |
| `r.Next(ctx, xla.Batch(n), xla.Wait(d))` | Up to `n` records (1–1000, default 1000). Waits up to `d` when none are available (default 0: return at once). Cancellation is noticed within about a second. |
| `r.Subscribe` / `Unsubscribe` / `Subscribed` | Table subscriptions of this bookmark. They persist in the database. |
| `r.Ack()` | Persists the position after the last record returned by `Next` |
| `r.Position()` / `r.Rewind(p)` | Re-read records without reconnecting (not before the last `Ack`) |
| `r.DeleteBookmark()` | Deletes the bookmark and its subscriptions, and closes the reader |
| `r.Close()` | Closes the connection. The bookmark and its acknowledged position stay. |

A `Reader` is safe for concurrent use, but its calls are serialized. Use one reader per bookmark.

## Bookmarks and acknowledgement

A bookmark is a named, persistent read position:

- `Next` advances the position in memory.
- `Ack` stores it. Acknowledging is comparatively expensive, so do it once per batch, not once per
  record.

After a crash or reconnect, reading resumes after the last `Ack`. Records that were read but not
acknowledged are delivered again, which gives **at-least-once** delivery. Make the consumer
idempotent, for example by upserting by primary key, or by de-duplicating on `(TxnID, LSN)`.

`Position` and `Rewind` let you retry a downstream failure without reconnecting:

```go
p, _ := r.Position()
recs, _ := r.Next(ctx)
if err := sink.Write(recs); err != nil {
	r.Rewind(p) // the same records come back from the next Next
}
```

## Records

```go
type Record struct {
	Op             Op     // OpInsert, OpUpdate, OpDelete, OpTruncate, OpCreateTable, ...
	Flags          Flags
	TxnID          uint64 // groups the records of one transaction
	ConnID         uint64
	LSN            LSN    // log position (ordering only)
	SysTableID     uint64
	Owner, Name    string // affected table
	Table          *Table // table definition, for DML records
	Old, New       []Value
	UpdatedColumns []int  // UPDATE: indexes into Table.Columns of the changed columns
}
```

| Op | `Old` | `New` |
|---|---|---|
| `OpInsert` | nil | the inserted row |
| `OpUpdate` | row before | row after; `UpdatedColumns` lists what changed |
| `OpDelete` | the deleted row | nil |

Helpers:

- `rec.NewValue("COL")` / `rec.OldValue("COL")` return a column by name. Names are upper case unless
  the column was created quoted.
- `rec.Key()` returns the primary key values. For a table without a primary key it is empty, and the
  before image identifies the row.
- `rec.FirstInTxn()` / `rec.EndsTxn()` mark transaction boundaries. Records of one transaction can
  share an `LSN`.
- `rec.Table.Columns` holds the name, type, size, precision, scale, nullability and primary-key flag.
- Flags tell how a change was made: `FlagReplicated` (it came from replication),
  `FlagCascadeDelete`, `FlagAging`.

Value types are listed in [data-types.md](data-types.md#xla-values). LOB columns are reported as
`KindLOB` without content: XLA does not deliver LOB data. Read it from the table by key if you need it.

## DDL and ALTER TABLE

DDL records of **all** tables reach every bookmark, not only those of subscribed tables, so filter
them by `rec.Owner` / `rec.Name`. This covers CREATE / DROP TABLE, ADD / DROP COLUMNS, indexes,
views, sequences and synonyms.

**ALTER TABLE on a subscribed table:**

- A reader that is up to date receives the `OpAddColumns` / `OpDropColumns` record, with the table's
  owner and name, and decodes later rows with the new definition.
- A reader that has **not yet read** rows logged before the ALTER cannot decode them. Every `Next`
  fails with TT8037 (`tt.ErrXlaTableDiff`), and reopening does not help.
- Safe procedure:
  1. Stop the writers.
  2. Let every reader catch up: `Next` returns nothing, then `Ack`.
  3. Alter the table.
  4. Restart the writers.

A subscribed table cannot be dropped. Call `admin.UnsubscribeAll(ctx, db, owner, table)` first.

## Restarts and failures

After a database or daemon restart, `Next` fails with a connection error (`tt.IsConnectionLost`).
Close the reader and reopen the bookmark in a retry loop:

```go
for {
	r, err = xla.Open(ctx, connStr, bookmark, xla.WithMode(xla.ReuseOnly))
	if err == nil {
		break
	}
	time.Sleep(time.Second)
}
```

Reading resumes after the last `Ack`. Unacknowledged records arrive again; acknowledged ones do not.
This is tested by `TestReaderSurvivesDaemonRestart`.

Direct connections need one more step:

- A process keeps the old instance's shared memory attached while **any** of its connections to it
  stays open. That includes idle connections in a `*sql.DB` pool that have not noticed the restart.
- Until those connections are closed, new connections fail with **TT8521** ("Cannot attach PL/SQL
  shared memory").
- So close the reader and flush the pool (`db.SetMaxIdleConns(0)`, then restore the value) before you
  reopen.

## Administration (xla/admin)

These functions take a `*sql.DB` opened with the `ttsql` driver, over a direct or client/server
connection, so you can manage XLA from any host:

```go
holds, _ := admin.LogHolds(ctx, db)              // every log hold: XLA, checkpoint, replication, backup
names, _ := admin.Bookmarks(ctx, db)             // XLA bookmark names
subs, _ := admin.Subscriptions(ctx, db, "")      // all subscriptions ("" = every bookmark)
admin.CreateBookmark(ctx, db, "orders_cdc", false)
admin.Subscribe(ctx, db, "APP.ORDERS", "orders_cdc")
admin.Unsubscribe(ctx, db, "APP.ORDERS", "orders_cdc")
admin.UnsubscribeAll(ctx, db, "APP", "ORDERS")   // before DROP TABLE
admin.DeleteBookmark(ctx, db, "orders_cdc")      // only when no reader has it open
```

Monitor `LogHolds`: the hold of a bookmark that is not acknowledged stays at an old log file, and
the log directory grows. Delete bookmarks that are no longer used.

## Limits

- At most 64 bookmarks per database.
- Bookmark names are 1–31 characters (`xla.MaxBookmarkLen`).
- One open reader per bookmark at a time. A second one fails with `tt.ErrXlaBookmarkInUse` (8029).
- A connection used for XLA cannot run SQL (`tt.ErrXlaDedicatedConn`). The reader opens its own
  connection.

## Try it

[`examples/xla-tail`](../examples/xla-tail) prints changes as JSON lines:

```bash
go run ./examples/xla-tail -dsn "DSN=sampledb;UID=xlauser;PWD=secret" -bookmark tail1 APP.ORDERS
# -delete removes the bookmark on exit, so it does not keep holding log files
```
