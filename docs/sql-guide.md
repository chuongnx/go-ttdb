# SQL guide

This guide covers plain SQL through `database/sql`. For PL/SQL see [plsql.md](plsql.md). The examples
come from `ttsql/plsql_integration_test.go` and `ttsql/integration_test.go`, which run them against a
real database.

- [Placeholders](#placeholders)
- [Create a table, INSERT / SELECT / UPDATE / DELETE](#create-a-table-insert--select--update--delete)
- [NULL values](#null-values)
- [Prepared statements](#prepared-statements)
- [Transactions](#transactions)
- [Batch execution (ExecBatch)](#batch-execution-execbatch)
- [Durable commit](#durable-commit)
- [Cancellation and timeouts](#cancellation-and-timeouts)
- [Errors](#errors)

## Placeholders

Parameters are positional. Write `?`, or `:name`. Named placeholders are still bound **by
position**, so pass the arguments in the order they appear in the statement. The name of a
`sql.Named` argument is ignored.

```go
db.QueryContext(ctx, "SELECT * FROM orders WHERE customer = ? AND amount > ?", "alice", 100)
db.QueryContext(ctx, "SELECT * FROM orders WHERE customer = :c AND amount > :a", "alice", 100)
```

## Create a table, INSERT / SELECT / UPDATE / DELETE

```go
_, err := db.ExecContext(ctx, `CREATE TABLE orders (
	id       NUMBER(10)    NOT NULL PRIMARY KEY,
	customer VARCHAR2(40)  NOT NULL,
	note     NVARCHAR2(100),
	amount   NUMBER(12,2),
	created  TIMESTAMP(6))`)

res, err := db.ExecContext(ctx,
	"INSERT INTO orders (id, customer, amount, created) VALUES (?, ?, ?, ?)",
	1, "alice", 10.5, time.Now())

var customer string
var amount float64
err = db.QueryRowContext(ctx, "SELECT customer, amount FROM orders WHERE id = ?", 1).
	Scan(&customer, &amount)
if errors.Is(err, sql.ErrNoRows) {
	// not found
}

res, err = db.ExecContext(ctx, "UPDATE orders SET amount = amount * 2, note = NULL WHERE customer = ?", "alice")
n, _ := res.RowsAffected()

res, err = db.ExecContext(ctx, "DELETE FROM orders WHERE id = ?", 1)

rows, err := db.QueryContext(ctx, "SELECT id, amount FROM orders ORDER BY id")
if err != nil {
	return err
}
defer rows.Close()
for rows.Next() {
	var id int64
	var amount string // NUMBER as exact decimal text, e.g. "0.25"
	if err := rows.Scan(&id, &amount); err != nil {
		return err
	}
}
return rows.Err()
```

`LastInsertId` is not supported, because TimesTen has no auto-increment columns. Use a sequence:
`SELECT seq.NEXTVAL FROM dual`, then insert.

`rows.ColumnTypes()` reports the database type name, length, precision/scale and nullability of
each column.

## NULL values

Scan NULL-able columns into `sql.NullString`, `sql.NullInt64`, `sql.NullFloat64`, `sql.NullTime`,
`sql.Null[T]`, a pointer, or `[]byte` (which is nil for NULL). Pass `nil` to insert NULL.

TimesTen follows Oracle semantics: **`""` is stored as NULL**. See [data-types.md](data-types.md) for
the other value rules.

## Prepared statements

`Exec` and `ExecBatch` statements are cached per connection automatically (`go_stmt_cache`, 32 by
default). For a query run many times, prepare it once:

```go
stmt, err := db.PrepareContext(ctx, "SELECT amount FROM orders WHERE id = ?")
defer stmt.Close()
for _, id := range ids {
	var amount float64
	if err := stmt.QueryRowContext(ctx, id).Scan(&amount); err != nil { ... }
}
```

DDL invalidates prepared statements inside TimesTen (TT0946). The driver prepares them again and
retries once, so cached and prepared statements keep working after `ALTER TABLE`, `CREATE INDEX` or
`DROP` + `CREATE`.

## Transactions

Outside a transaction, every statement is committed on its own (autocommit). Use `BeginTx` to group
statements:

```go
tx, err := db.BeginTx(ctx, nil) // READ COMMITTED
if err != nil {
	return err
}
defer tx.Rollback() // no-op after Commit

if _, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance - ? WHERE id = ?", 100, 1); err != nil {
	return err
}
if _, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance + ? WHERE id = ?", 100, 2); err != nil {
	return err
}
return tx.Commit()
```

| `sql.TxOptions` | Supported |
|---|---|
| `Isolation: sql.LevelDefault` or `sql.LevelReadCommitted` | yes (TimesTen default) |
| `Isolation: sql.LevelSerializable` | yes |
| other isolation levels | error |
| `ReadOnly: true` | error (TimesTen has no read-only transactions) |

Lock conflicts and deadlocks are reported as transient errors. Retry the whole transaction, as shown
in [production.md](production.md#lock-conflicts-and-deadlocks).

## Batch execution (ExecBatch)

`ExecBatch` runs one statement for many rows with ODBC parameter arrays. Rows are sent
`go_batch_rows` (default 1024) at a time, one round trip per chunk, instead of one round trip per row.

```go
conn, err := db.Conn(ctx)
if err != nil {
	return err
}
defer conn.Close()

rows := [][]any{
	{1, "alice", "10.50"},
	{2, "bob", "20.00"},
	// ...
}
n, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO orders (id, customer, amount) VALUES (?, ?, ?)", rows)
var be *ttsql.BatchError
if errors.As(err, &be) {
	log.Printf("row %d failed: %v", be.Row, be.Err) // be.Row is the index into rows
}
```

Rules:

- **Kinds:** every value in one column must have the same kind (string, int64, float64, time.Time,
  []byte, bool). Integers may be mixed with floats, and `nil` is allowed anywhere. Send exact
  decimals as strings.
- **No LOB columns:** TimesTen does not support array binding for CLOB / BLOB / NCLOB ("Batch mode not
  supported for LOBs"). Batch the other columns, then update the LOBs row by row.
- **Errors:** on the first failing row, `ExecBatch` returns a `*BatchError` (`Row`, and `Err`, which
  wraps `*tt.Error`).
- **Partial results:** the rows before the failing row stay applied — TimesTen does not undo them, even
  inside the failing chunk. If row 1500 fails, rows 0–1499 are applied, and `n` is their affected
  row count. Without a transaction they are already committed. Over client/server, the failing
  chunk's share of `n` is the number of input rows applied rather than the rows they affected. The two
  differ only for an UPDATE or DELETE where one input row matches several rows.
- **All-or-nothing:** start a transaction on the same `*sql.Conn` first:

  ```go
  tx, err := conn.BeginTx(ctx, nil)
  if err != nil {
  	return err
  }
  defer tx.Rollback()
  if _, err := ttsql.ExecBatch(ctx, conn, query, rows); err != nil {
  	return err
  }
  return tx.Commit()
  ```

- **Raw access:** `ExecBatch` is also available on the driver connection,
  `conn.Raw(func(dc any) error { dc.(*ttsql.Conn).ExecBatch(...) })`.

## Durable commit

On databases with `DurableCommits=0`, which is the TimesTen default and the fastest setting, a commit
is durable only after the log buffer is flushed. To make one important transaction durable at
commit time:

```go
tx, _ := db.BeginTx(ctx, nil)
// ... statements ...
if err := ttsql.DurableCommit(ctx, tx); err != nil { // CALL ttDurableCommit
	return err
}
return tx.Commit()
```

## Cancellation and timeouts

Every `...Context` method honours its context. When the context ends:

- The driver calls `SQLCancel` on the running statement. This works for a running query, a fetch in
  progress, a PL/SQL block or a batch.
- The error wraps both `ctx.Err()` and the TimesTen error (6113, "statement cancelled"). Check it
  with `errors.Is(err, context.DeadlineExceeded)`.

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()
_, err := db.ExecContext(ctx, "CALL long_running_proc()")
if errors.Is(err, context.DeadlineExceeded) { ... }
```

Connecting cannot be cancelled (`SQLDriverConnect`). Over client/server, limit it with `TTC_Timeout`.

## Errors

Every database error is a `*tt.Error`:

```go
var e *tt.Error
if errors.As(err, &e) {
	fmt.Println(e.Code, e.SQLState, e.Message) // first diagnostic; all of them are in e.Diags
}
switch {
case tt.Code(err) == tt.ErrKeyExists: // 907 unique constraint violated
case tt.IsTransient(err):            // 6001/6002/6003 or connection lost: retry the transaction
case tt.IsConnectionLost(err):       // the connection was discarded
}
```

Constants for common codes are in package [`tt`](https://pkg.go.dev/github.com/chuongnx/go-ttdb/tt):
`ErrKeyExists`, `ErrNotFound`, `ErrLockConflict`, `ErrDeadlock`, `ErrLockTimeout`,
`ErrStatementCancelled`, `ErrCommandInvalid`, and more.
