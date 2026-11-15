# PL/SQL

TimesTen runs PL/SQL: anonymous blocks, procedures, functions and packages. This page shows how to call
them from Go, with OUT and IN OUT parameters of every type and REF CURSORs. Everything here is tested
by `ttsql/plsql_integration_test.go`.

- [Create a package](#create-a-package)
- [Call procedures and functions](#call-procedures-and-functions)
- [OUT parameters of every type](#out-parameters-of-every-type)
- [REF CURSOR](#ref-cursor)
- [BOOLEAN, RECORD and collections](#boolean-record-and-collections)
- [Errors from PL/SQL](#errors-from-plsql)

## Create a package

Send the package specification and the body as two statements, each with `db.ExecContext`.

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

The full package is in `ttsql/plsql_integration_test.go`. Drop it with `DROP PACKAGE order_pkg`.
Standalone `CREATE PROCEDURE` / `CREATE FUNCTION` work the same way.

## Call procedures and functions

Wrap calls in an anonymous block: `BEGIN ...; END;`.

| Parameter mode | Go argument |
|---|---|
| IN | the value itself |
| OUT | `sql.Out{Dest: &v}` |
| IN OUT | `sql.Out{Dest: &v, In: true}`; the current value of `v` is sent and then overwritten |
| function result | the first `?` of `BEGIN ? := pkg.fn(...); END;`, as `sql.Out{Dest: &v}` |

```go
// DML through the package
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

Calls made through a `*sql.Tx` belong to that transaction, so `tx.Rollback()` undoes the DML the
procedures did. TimesTen built-in procedures that return rows (`CALL ttLogHolds`, …) are read with
`db.QueryContext(ctx, "CALL ttLogHolds")`.

## OUT parameters of every type

| PL/SQL OUT type | Go `Dest` | NULL-able `Dest` |
|---|---|---|
| VARCHAR2, NVARCHAR2, CHAR, NCHAR | `*string` | `*sql.NullString` |
| NUMBER (exact) | `*string` (decimal text) | `*sql.NullString` |
| NUMBER (integer), PLS_INTEGER, BINARY_INTEGER | `*int64`, `*int`, `*int32`, … | `*sql.NullInt64`, `*sql.NullInt32` |
| NUMBER, BINARY_DOUBLE, BINARY_FLOAT | `*float64`, `*float32` | `*sql.NullFloat64` |
| DATE, TIMESTAMP | `*time.Time` | `*sql.NullTime` |
| RAW | `*[]byte` | `*[]byte` (nil for NULL) |
| CLOB / BLOB | `*string` / `*[]byte` | `*sql.NullString` / `*[]byte` |
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

- **Buffer size.** String and RAW OUT buffers are 32767 bytes, which is the PL/SQL VARCHAR2 limit. A
  longer value fails with "OUT parameter truncated". For IN OUT parameters the buffer is at least the
  size of the input.
- **Leading zero.** A NUMBER OUT read into `*string` keeps TimesTen's text, without the leading zero
  (`.25`, `-.5`). TimesTen describes every PL/SQL block parameter as VARCHAR, so the driver cannot
  tell a NUMBER OUT from a VARCHAR2 OUT. Read NUMBER OUT parameters into `*float64` / `*int64`, or
  return exact decimals through a REF CURSOR, where they are normalized like any column (`0.25`).
- **CHAR padding.** An unconstrained PL/SQL `CHAR` OUT parameter is blank-padded to the buffer size.
  Trim it with `strings.TrimRight(code, " ")`, or declare it `VARCHAR2`.
- **Long IN values.** IN strings and `[]byte` longer than 32 KB are bound as LOBs, so they reach
  PL/SQL CLOB/BLOB parameters intact.

## REF CURSOR

A cursor is read on the connection that opened it:

- Use a `*sql.Conn` or a `*sql.Tx`, never the `*sql.DB` directly.
- Close the cursor before releasing the connection.

A cursor can come from an OUT parameter or from a function result, and one call can return several.

```go
conn, err := db.Conn(ctx)
if err != nil {
	return err
}
defer conn.Close()

// function returning SYS_REFCURSOR
var cur ttsql.Cursor
_, err = conn.ExecContext(ctx, "BEGIN ? := order_pkg.orders_of(?); END;", sql.Out{Dest: &cur}, "alice")
if err != nil {
	return err
}
cols, _ := cur.Columns() // [ID AMOUNT CREATED]
for cur.Next() {
	var id int64
	var amount float64
	var created time.Time
	if err := cur.Scan(&id, &amount, &created); err != nil {
		return err
	}
}
if err := cur.Err(); err != nil {
	return err
}
cur.Close()

// a scalar OUT and two cursors from one call
var total float64
var orders, customers ttsql.Cursor
_, err = conn.ExecContext(ctx, "BEGIN order_pkg.report(?, ?, ?, ?); END;", 100,
	sql.Out{Dest: &total}, sql.Out{Dest: &orders}, sql.Out{Dest: &customers})
defer orders.Close()
defer customers.Close()
for orders.Next() { /* ... */ }
for customers.Next() { /* ... */ }
```

How a cursor behaves:

- **Scan.** `Cursor.Scan` uses the same conversions as `rows.Scan`, including the `sql.Null*` types.
- **Empty cursor.** An empty cursor returns `false` from the first `Next`, and `Err` is nil.
- **Closing.** A cursor left open is closed with its connection. Close cursors promptly anyway, because
  each one holds a statement handle on the server.
- **Inline open.** You can also open a cursor directly: `BEGIN OPEN ? FOR SELECT ...; END;`.

## BOOLEAN, RECORD and collections

These PL/SQL-only types have no ODBC binding: `BOOLEAN`, `RECORD`, and collections such as
`TABLE OF … INDEX BY`. Binding them directly fails with `TT8503: ORA-06550` (wrong number or types of
arguments). Convert them to scalars in an anonymous block instead:

```go
// BOOLEAN → 1 / 0
db.ExecContext(ctx, "DECLARE v BOOLEAN; BEGIN pkg.is_big(?, v); ? := CASE WHEN v THEN 1 ELSE 0 END; END;",
	42, sql.Out{Dest: &big})

// RECORD → one OUT per field
db.ExecContext(ctx, "DECLARE r pkg.rec_t; BEGIN pkg.get_rec(r); ? := r.a; ? := r.b; END;",
	sql.Out{Dest: &a}, sql.Out{Dest: &b})

// collection → elements / COUNT (or OPEN a cursor over the data instead)
db.ExecContext(ctx, "DECLARE c pkg.num_tab; BEGIN pkg.get_tab(c); ? := c(1); ? := c.COUNT; END;",
	sql.Out{Dest: &first}, sql.Out{Dest: &n})
```

To pass a collection in, send a delimited string or several scalars and build the collection inside
the block. For large inputs, insert into a global temporary table with `ExecBatch` and read it in PL/SQL.

## Errors from PL/SQL

Errors raised in PL/SQL come back as `*tt.Error`. The message contains the Oracle code and text,
for example `ORA-20001: order 999 not found` from `RAISE_APPLICATION_ERROR`.

If calls start failing with **TT8521** ("Cannot attach PL/SQL shared memory") after a database restart,
see [troubleshooting.md](troubleshooting.md#tt8521-cannot-attach-plsql-shared-memory).
