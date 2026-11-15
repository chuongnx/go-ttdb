//go:build integration

package ttsql_test

// End-to-end scenarios: create a table and a PL/SQL package, then SELECT / INSERT / UPDATE / DELETE
// both with plain SQL and through package procedures, read OUT parameters of every supported type,
// NULL outputs, IN OUT parameters, functions, and REF CURSORs (OUT parameter, function result,
// several cursors from one call). Works over direct and client/server connections.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chuongnx/go-ttdb/tt"
	"github.com/chuongnx/go-ttdb/ttsql"
	"github.com/chuongnx/go-ttdb/ttxtest"
)

const ordersColumns = `ID NUMBER(10) NOT NULL PRIMARY KEY,
	CUSTOMER VARCHAR2(40) NOT NULL, NOTE NVARCHAR2(100), CODE CHAR(3),
	AMOUNT NUMBER(12,2), QTY TT_INTEGER, BIG TT_BIGINT, RATIO BINARY_DOUBLE, BF BINARY_FLOAT,
	DAY DATE, CREATED TIMESTAMP(6), RAWV VARBINARY(16)`

// orderPackage creates a table and a package over it, both dropped when the test ends.
func orderPackage(t *testing.T, db *sql.DB) (tbl, pkg string) {
	t.Helper()
	tbl = ttxtest.Table(t, db, ordersColumns)
	pkg = ttxtest.Name("PKG")
	spec := fmt.Sprintf(`CREATE OR REPLACE PACKAGE %[1]s AS
	PROCEDURE add_order(p_id IN NUMBER, p_customer IN VARCHAR2, p_amount IN NUMBER, p_created IN TIMESTAMP);
	PROCEDURE set_amount(p_id IN NUMBER, p_amount IN NUMBER, o_rows OUT NUMBER);
	PROCEDURE remove_order(p_id IN NUMBER, o_rows OUT NUMBER);
	PROCEDURE get_order(p_id IN NUMBER,
		o_customer OUT VARCHAR2, o_note OUT NVARCHAR2, o_code OUT CHAR, o_amount OUT NUMBER,
		o_qty OUT PLS_INTEGER, o_big OUT NUMBER, o_ratio OUT BINARY_DOUBLE, o_bf OUT BINARY_FLOAT,
		o_day OUT DATE, o_created OUT TIMESTAMP, o_raw OUT RAW);
	PROCEDURE bump(p_counter IN OUT NUMBER, p_text IN OUT VARCHAR2);
	FUNCTION order_count RETURN NUMBER;
	FUNCTION customer_total(p_customer IN VARCHAR2) RETURN NUMBER;
	FUNCTION orders_of(p_customer IN VARCHAR2) RETURN SYS_REFCURSOR;
	PROCEDURE report(p_min IN NUMBER, o_total OUT NUMBER, o_orders OUT SYS_REFCURSOR,
		o_customers OUT SYS_REFCURSOR);
END %[1]s;`, pkg)
	body := fmt.Sprintf(`CREATE OR REPLACE PACKAGE BODY %[1]s AS
	PROCEDURE add_order(p_id IN NUMBER, p_customer IN VARCHAR2, p_amount IN NUMBER, p_created IN TIMESTAMP) IS
	BEGIN
		INSERT INTO %[2]s (ID, CUSTOMER, AMOUNT, CREATED) VALUES (p_id, p_customer, p_amount, p_created);
	END;

	PROCEDURE set_amount(p_id IN NUMBER, p_amount IN NUMBER, o_rows OUT NUMBER) IS
	BEGIN
		UPDATE %[2]s SET AMOUNT = p_amount WHERE ID = p_id;
		o_rows := SQL%%ROWCOUNT;
	END;

	PROCEDURE remove_order(p_id IN NUMBER, o_rows OUT NUMBER) IS
	BEGIN
		DELETE FROM %[2]s WHERE ID = p_id;
		o_rows := SQL%%ROWCOUNT;
	END;

	PROCEDURE get_order(p_id IN NUMBER,
		o_customer OUT VARCHAR2, o_note OUT NVARCHAR2, o_code OUT CHAR, o_amount OUT NUMBER,
		o_qty OUT PLS_INTEGER, o_big OUT NUMBER, o_ratio OUT BINARY_DOUBLE, o_bf OUT BINARY_FLOAT,
		o_day OUT DATE, o_created OUT TIMESTAMP, o_raw OUT RAW) IS
	BEGIN
		SELECT CUSTOMER, NOTE, CODE, AMOUNT, QTY, BIG, RATIO, BF, DAY, CREATED, RAWV
		  INTO o_customer, o_note, o_code, o_amount, o_qty, o_big, o_ratio, o_bf, o_day, o_created, o_raw
		  FROM %[2]s WHERE ID = p_id;
	EXCEPTION
		WHEN NO_DATA_FOUND THEN
			RAISE_APPLICATION_ERROR(-20001, 'order ' || p_id || ' not found');
	END;

	PROCEDURE bump(p_counter IN OUT NUMBER, p_text IN OUT VARCHAR2) IS
	BEGIN
		p_counter := p_counter + 1;
		p_text := UPPER(p_text) || '!';
	END;

	FUNCTION order_count RETURN NUMBER IS
		n NUMBER;
	BEGIN
		SELECT COUNT(*) INTO n FROM %[2]s;
		RETURN n;
	END;

	FUNCTION customer_total(p_customer IN VARCHAR2) RETURN NUMBER IS
		s NUMBER;
	BEGIN
		SELECT SUM(AMOUNT) INTO s FROM %[2]s WHERE CUSTOMER = p_customer;
		RETURN s;
	END;

	FUNCTION orders_of(p_customer IN VARCHAR2) RETURN SYS_REFCURSOR IS
		c SYS_REFCURSOR;
	BEGIN
		OPEN c FOR SELECT ID, AMOUNT, CREATED FROM %[2]s WHERE CUSTOMER = p_customer ORDER BY ID;
		RETURN c;
	END;

	PROCEDURE report(p_min IN NUMBER, o_total OUT NUMBER, o_orders OUT SYS_REFCURSOR,
		o_customers OUT SYS_REFCURSOR) IS
	BEGIN
		SELECT NVL(SUM(AMOUNT), 0) INTO o_total FROM %[2]s WHERE AMOUNT >= p_min;
		OPEN o_orders FOR SELECT ID, CUSTOMER, AMOUNT FROM %[2]s WHERE AMOUNT >= p_min ORDER BY ID;
		OPEN o_customers FOR SELECT CUSTOMER, COUNT(*), SUM(AMOUNT) FROM %[2]s
			GROUP BY CUSTOMER ORDER BY CUSTOMER;
	END;
END %[1]s;`, pkg, tbl)
	ttxtest.Exec(t, db, spec, body)
	t.Cleanup(func() { db.Exec("DROP PACKAGE " + pkg) })
	return tbl, pkg
}

// TestCRUDPlainSQL: INSERT / SELECT / UPDATE / DELETE with parameters and RowsAffected.
func TestCRUDPlainSQL(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, ordersColumns)
	ctx := context.Background()
	created := time.Date(2026, 11, 14, 8, 0, 0, 123456000, time.Local)

	// INSERT
	ins := "INSERT INTO " + tbl + " (ID, CUSTOMER, NOTE, AMOUNT, QTY, CREATED) VALUES (?, ?, ?, ?, ?, ?)"
	for i := 1; i <= 3; i++ {
		res, err := db.ExecContext(ctx, ins, i, fmt.Sprintf("cust-%d", i%2), "ghi chú "+fmt.Sprint(i), float64(i)*10.5, i, created)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("insert %d: RowsAffected=%d", i, n)
		}
	}

	// SELECT one row
	var (
		customer, note string
		amount         float64
		qty            int
		gotCreated     time.Time
	)
	err := db.QueryRowContext(ctx, "SELECT CUSTOMER, NOTE, AMOUNT, QTY, CREATED FROM "+tbl+" WHERE ID = ?", 2).
		Scan(&customer, &note, &amount, &qty, &gotCreated)
	if err != nil {
		t.Fatal(err)
	}
	if customer != "cust-0" || note != "ghi chú 2" || amount != 21 || qty != 2 || !gotCreated.Equal(created) {
		t.Errorf("row 2 = %q %q %v %d %v", customer, note, amount, qty, gotCreated)
	}

	// SELECT no row
	if err := db.QueryRowContext(ctx, "SELECT CUSTOMER FROM "+tbl+" WHERE ID = ?", 99).Scan(&customer); err != sql.ErrNoRows {
		t.Errorf("missing row: err = %v, want sql.ErrNoRows", err)
	}

	// UPDATE
	res, err := db.ExecContext(ctx, "UPDATE "+tbl+" SET AMOUNT = AMOUNT * 2, NOTE = NULL WHERE CUSTOMER = ?", "cust-1")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("update RowsAffected=%d, want 2", n)
	}
	var nullNote sql.NullString
	db.QueryRowContext(ctx, "SELECT AMOUNT, NOTE FROM "+tbl+" WHERE ID = 3").Scan(&amount, &nullNote)
	if amount != 63 || nullNote.Valid {
		t.Errorf("after update: amount=%v note=%v", amount, nullNote)
	}

	// DELETE
	res, err = db.ExecContext(ctx, "DELETE FROM "+tbl+" WHERE ID IN (?, ?)", 1, 99)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("delete RowsAffected=%d, want 1", n)
	}

	// SELECT many rows
	rows, err := db.QueryContext(ctx, "SELECT ID, AMOUNT FROM "+tbl+" ORDER BY ID")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id int
		var amt string
		if err := rows.Scan(&id, &amt); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d=%s", id, amt))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[2=21 3=63]" {
		t.Errorf("rows = %v", got)
	}
}

// TestPackageCRUD: INSERT / UPDATE / DELETE through package procedures, counts from functions.
func TestPackageCRUD(t *testing.T) {
	db := ttxtest.DB(t)
	tbl, pkg := orderPackage(t, db)
	ctx := context.Background()
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.Local)

	for i, c := range []string{"alice", "bob", "alice"} {
		if _, err := db.ExecContext(ctx, "BEGIN "+pkg+".add_order(?, ?, ?, ?); END;", i+1, c, 100*(i+1), now); err != nil {
			t.Fatal(err)
		}
	}

	var count int64
	if _, err := db.ExecContext(ctx, "BEGIN ? := "+pkg+".order_count; END;", sql.Out{Dest: &count}); err != nil || count != 3 {
		t.Fatalf("order_count = %d, %v", count, err)
	}
	var total float64
	if _, err := db.ExecContext(ctx, "BEGIN ? := "+pkg+".customer_total(?); END;", sql.Out{Dest: &total}, "alice"); err != nil || total != 400 {
		t.Fatalf("customer_total = %v, %v", total, err)
	}

	var updated int
	if _, err := db.ExecContext(ctx, "BEGIN "+pkg+".set_amount(?, ?, ?); END;", 2, 250.75, sql.Out{Dest: &updated}); err != nil || updated != 1 {
		t.Fatalf("set_amount rows = %d, %v", updated, err)
	}
	var amt string
	db.QueryRowContext(ctx, "SELECT AMOUNT FROM "+tbl+" WHERE ID = 2").Scan(&amt)
	if amt != "250.75" {
		t.Errorf("amount after set_amount = %s", amt)
	}
	if _, err := db.ExecContext(ctx, "BEGIN "+pkg+".set_amount(?, ?, ?); END;", 42, 1, sql.Out{Dest: &updated}); err != nil || updated != 0 {
		t.Errorf("set_amount on missing row = %d, %v", updated, err)
	}

	var deleted int
	if _, err := db.ExecContext(ctx, "BEGIN "+pkg+".remove_order(?, ?); END;", 1, sql.Out{Dest: &deleted}); err != nil || deleted != 1 {
		t.Fatalf("remove_order rows = %d, %v", deleted, err)
	}
	db.ExecContext(ctx, "BEGIN ? := "+pkg+".order_count; END;", sql.Out{Dest: &count})
	if count != 2 {
		t.Errorf("count after delete = %d", count)
	}

	// procedure calls inside a transaction are rolled back together
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "BEGIN "+pkg+".add_order(?, ?, ?, ?); END;", 10, "carol", 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "BEGIN "+pkg+".remove_order(?, ?); END;", 2, sql.Out{Dest: &deleted}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var ids []int
	rows, err := db.QueryContext(ctx, "SELECT ID FROM "+tbl+" ORDER BY ID")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil || fmt.Sprint(ids) != "[2 3]" {
		t.Errorf("ids after rollback = %v (%v), want [2 3]", ids, rows.Err())
	}
}

// TestPackageOutputTypes: one OUT parameter per data type, then the same call on a row of NULLs.
func TestPackageOutputTypes(t *testing.T) {
	db := ttxtest.DB(t)
	tbl, pkg := orderPackage(t, db)
	ctx := context.Background()
	day := time.Date(2026, 1, 31, 0, 0, 0, 0, time.Local)
	created := time.Date(2026, 11, 14, 9, 30, 15, 123456000, time.Local)
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" (ID, CUSTOMER) VALUES (2, 'empty')")
	_, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		1, "Nguyễn Văn A", "Ghi chú tiếng Việt ✓", "VIP", "1234567890.12", 42, int64(9007199254740993),
		0.125, float32(2.5), day, created, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	if err != nil {
		t.Fatal(err)
	}

	call := "BEGIN " + pkg + ".get_order(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?); END;"
	var (
		customer, note, code, amount string
		qty                          int32
		big                          int64
		ratio                        float64
		bf                           float32
		gotDay, gotCreated           time.Time
		raw                          []byte
	)
	_, err = db.ExecContext(ctx, call, 1,
		sql.Out{Dest: &customer}, sql.Out{Dest: &note}, sql.Out{Dest: &code}, sql.Out{Dest: &amount},
		sql.Out{Dest: &qty}, sql.Out{Dest: &big}, sql.Out{Dest: &ratio}, sql.Out{Dest: &bf},
		sql.Out{Dest: &gotDay}, sql.Out{Dest: &gotCreated}, sql.Out{Dest: &raw})
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, got, want any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("VARCHAR2", customer, "Nguyễn Văn A")
	check("NVARCHAR2", note, "Ghi chú tiếng Việt ✓")
	// an unconstrained PL/SQL CHAR OUT parameter is blank-padded to the bind buffer size
	check("CHAR", strings.TrimRight(code, " "), "VIP")
	check("NUMBER as string", amount, "1234567890.12")
	check("PLS_INTEGER", qty, 42)
	check("NUMBER as int64", big, int64(9007199254740993))
	check("BINARY_DOUBLE", ratio, 0.125)
	check("BINARY_FLOAT", bf, 2.5)
	if !gotDay.Equal(day) {
		t.Errorf("DATE = %v, want %v", gotDay, day)
	}
	if !gotCreated.Equal(created) {
		t.Errorf("TIMESTAMP = %v, want %v", gotCreated, created)
	}
	check("RAW", fmt.Sprintf("%X", raw), "DEADBEEF")

	// NUMBER into float64, and every OUT as NULL through sql.Null* / []byte
	var (
		nCustomer, nNote, nCode sql.NullString
		nAmount                 sql.NullFloat64
		nQty, nBig              sql.NullInt64
		nRatio, nBf             sql.NullFloat64
		nDay, nCreated          sql.NullTime
		nRaw                    []byte
	)
	_, err = db.ExecContext(ctx, call, 2,
		sql.Out{Dest: &nCustomer}, sql.Out{Dest: &nNote}, sql.Out{Dest: &nCode}, sql.Out{Dest: &nAmount},
		sql.Out{Dest: &nQty}, sql.Out{Dest: &nBig}, sql.Out{Dest: &nRatio}, sql.Out{Dest: &nBf},
		sql.Out{Dest: &nDay}, sql.Out{Dest: &nCreated}, sql.Out{Dest: &nRaw})
	if err != nil {
		t.Fatal(err)
	}
	if !nCustomer.Valid || nCustomer.String != "empty" {
		t.Errorf("customer = %v", nCustomer)
	}
	if nNote.Valid || nCode.Valid || nAmount.Valid || nQty.Valid || nBig.Valid || nRatio.Valid || nBf.Valid ||
		nDay.Valid || nCreated.Valid || nRaw != nil {
		t.Errorf("expected NULL outputs: %v %v %v %v %v %v %v %v %v %x",
			nNote, nCode, nAmount, nQty, nBig, nRatio, nBf, nDay, nCreated, nRaw)
	}

	// error raised by the package (RAISE_APPLICATION_ERROR)
	_, err = db.ExecContext(ctx, call, 999,
		sql.Out{Dest: &customer}, sql.Out{Dest: &note}, sql.Out{Dest: &code}, sql.Out{Dest: &amount},
		sql.Out{Dest: &qty}, sql.Out{Dest: &big}, sql.Out{Dest: &ratio}, sql.Out{Dest: &bf},
		sql.Out{Dest: &gotDay}, sql.Out{Dest: &gotCreated}, sql.Out{Dest: &raw})
	if err == nil || !strings.Contains(err.Error(), "ORA-20001") || !strings.Contains(err.Error(), "order 999 not found") {
		t.Errorf("missing order: err = %v", err)
	}
	if tt.Code(err) == 0 {
		t.Errorf("missing order: no TimesTen error code in %v", err)
	}
}

// TestPackageInOut: IN OUT parameters carry a value in and the new value back.
func TestPackageInOut(t *testing.T) {
	db := ttxtest.DB(t)
	_, pkg := orderPackage(t, db)
	counter := int64(41)
	text := "xin chào"
	if _, err := db.Exec("BEGIN "+pkg+".bump(?, ?); END;", sql.Out{Dest: &counter, In: true}, sql.Out{Dest: &text, In: true}); err != nil {
		t.Fatal(err)
	}
	if counter != 42 || text != "XIN CHÀO!" {
		t.Errorf("bump: counter=%d text=%q", counter, text)
	}
}

// TestPackageCursors: REF CURSOR as a function result and several cursors from one procedure call.
func TestPackageCursors(t *testing.T) {
	db := ttxtest.DB(t)
	_, pkg := orderPackage(t, db)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
	for i, c := range []string{"alice", "bob", "alice", "carol", "bob"} {
		if _, err := db.ExecContext(ctx, "BEGIN "+pkg+".add_order(?, ?, ?, ?); END;",
			i+1, c, 50*(i+1), base.AddDate(0, 0, i)); err != nil {
			t.Fatal(err)
		}
	}

	// a cursor is read on the connection that opened it: use a *sql.Conn (or *sql.Tx)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// function returning SYS_REFCURSOR
	var cur ttsql.Cursor
	if _, err := conn.ExecContext(ctx, "BEGIN ? := "+pkg+".orders_of(?); END;", sql.Out{Dest: &cur}, "alice"); err != nil {
		t.Fatal(err)
	}
	cols, err := cur.Columns()
	if err != nil || strings.Join(cols, ",") != "ID,AMOUNT,CREATED" {
		t.Errorf("cursor columns %v %v", cols, err)
	}
	var got []string
	for cur.Next() {
		var id int
		var amount float64
		var created time.Time
		if err := cur.Scan(&id, &amount, &created); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%g:%s", id, amount, created.Format("01-02")))
	}
	if err := cur.Err(); err != nil {
		t.Fatal(err)
	}
	cur.Close()
	if fmt.Sprint(got) != "[1:50:11-01 3:150:11-03]" {
		t.Errorf("orders_of(alice) = %v", got)
	}

	// procedure with a scalar OUT and two REF CURSOR OUT parameters
	var total float64
	var orders, customers ttsql.Cursor
	if _, err := conn.ExecContext(ctx, "BEGIN "+pkg+".report(?, ?, ?, ?); END;", 100,
		sql.Out{Dest: &total}, sql.Out{Dest: &orders}, sql.Out{Dest: &customers}); err != nil {
		t.Fatal(err)
	}
	defer orders.Close()
	defer customers.Close()
	if total != 700 {
		t.Errorf("report total = %v, want 700", total)
	}
	got = nil
	for orders.Next() {
		var id int
		var customer string
		var amount int64
		if err := orders.Scan(&id, &customer, &amount); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%s:%d", id, customer, amount))
	}
	if orders.Err() != nil || fmt.Sprint(got) != "[2:bob:100 3:alice:150 4:carol:200 5:bob:250]" {
		t.Errorf("orders cursor = %v (%v)", got, orders.Err())
	}
	got = nil
	for customers.Next() {
		var customer string
		var n int
		var sum sql.NullFloat64
		if err := customers.Scan(&customer, &n, &sum); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s:%d:%g", customer, n, sum.Float64))
	}
	if customers.Err() != nil || fmt.Sprint(got) != "[alice:2:200 bob:2:350 carol:1:200]" {
		t.Errorf("customers cursor = %v (%v)", got, customers.Err())
	}

	// an empty cursor
	var empty ttsql.Cursor
	if _, err := conn.ExecContext(ctx, "BEGIN ? := "+pkg+".orders_of(?); END;", sql.Out{Dest: &empty}, "nobody"); err != nil {
		t.Fatal(err)
	}
	if empty.Next() || empty.Err() != nil {
		t.Errorf("empty cursor: next=true or err=%v", empty.Err())
	}
	empty.Close()
}

// TestPLSQLOnlyTypes: BOOLEAN, RECORD and collection parameters have no ODBC binding (TT8503 /
// ORA-06550); an anonymous block converts them to bindable scalars.
func TestPLSQLOnlyTypes(t *testing.T) {
	db := ttxtest.DB(t)
	pkg := ttxtest.Name("PKG")
	ttxtest.Exec(t, db, `CREATE OR REPLACE PACKAGE `+pkg+` AS
		TYPE num_tab IS TABLE OF NUMBER INDEX BY PLS_INTEGER;
		TYPE rec_t IS RECORD (a NUMBER, b VARCHAR2(10));
		PROCEDURE is_big(n IN NUMBER, o OUT BOOLEAN);
		PROCEDURE get_tab(o OUT num_tab);
		PROCEDURE get_rec(o OUT rec_t);
	END;`, `CREATE OR REPLACE PACKAGE BODY `+pkg+` AS
		PROCEDURE is_big(n IN NUMBER, o OUT BOOLEAN) IS BEGIN o := n > 10; END;
		PROCEDURE get_tab(o OUT num_tab) IS BEGIN o(1) := 10; o(2) := 20; END;
		PROCEDURE get_rec(o OUT rec_t) IS BEGIN o.a := 1; o.b := 'x'; END;
	END;`)
	t.Cleanup(func() { db.Exec("DROP PACKAGE " + pkg) })

	// direct binding fails
	var b bool
	var s string
	for _, q := range []struct {
		name string
		call string
		out  any
	}{
		{"BOOLEAN", "BEGIN " + pkg + ".is_big(?, ?); END;", &b},
		{"collection", "BEGIN " + pkg + ".get_tab(?); END;", &s},
		{"record", "BEGIN " + pkg + ".get_rec(?); END;", &s},
	} {
		args := []any{sql.Out{Dest: q.out}}
		if q.name == "BOOLEAN" {
			args = append([]any{42}, args...)
		}
		_, err := db.Exec(q.call, args...)
		if tt.Code(err) != 8503 || !strings.Contains(err.Error(), "ORA-06550") {
			t.Errorf("%s bound directly: err = %v, want TT8503 ORA-06550", q.name, err)
		}
	}

	// converted in an anonymous block
	var big int
	if _, err := db.Exec("DECLARE v BOOLEAN; BEGIN "+pkg+".is_big(?, v); ? := CASE WHEN v THEN 1 ELSE 0 END; END;",
		42, sql.Out{Dest: &big}); err != nil || big != 1 {
		t.Errorf("BOOLEAN via block: %d %v", big, err)
	}
	var a int
	if _, err := db.Exec("DECLARE r "+pkg+".rec_t; BEGIN "+pkg+".get_rec(r); ? := r.a; ? := r.b; END;",
		sql.Out{Dest: &a}, sql.Out{Dest: &s}); err != nil || a != 1 || s != "x" {
		t.Errorf("record via block: %d %q %v", a, s, err)
	}
	var first, count int
	if _, err := db.Exec("DECLARE c "+pkg+".num_tab; BEGIN "+pkg+".get_tab(c); ? := c(1); ? := c.COUNT; END;",
		sql.Out{Dest: &first}, sql.Out{Dest: &count}); err != nil || first != 10 || count != 2 {
		t.Errorf("collection via block: %d %d %v", first, count, err)
	}
}

// TestDecimalLeadingZero: NUMBER text always has a leading zero ("0.25", "-0.5"; TimesTen itself
// returns ".25", "-.5") in columns, REF CURSOR rows and string OUT parameters of NUMBER type.
func TestDecimalLeadingZero(t *testing.T) {
	db := ttxtest.DB(t)
	ctx := context.Background()
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, AMT NUMBER(12,2), N NUMBER, F FLOAT(10), N3810 NUMBER(38,10)")
	ttxtest.Exec(t, db,
		"INSERT INTO "+tbl+" VALUES (1, 0.25, -0.5, 0.125, -0.0000000001)",
		"INSERT INTO "+tbl+" VALUES (2, 12.5, 0, -12, 1E-10)")
	want := map[int]string{1: "0.25 -0.5 0.125 -0.0000000001", 2: "12.5 0 -12 0.0000000001"}

	// columns
	rows, err := db.QueryContext(ctx, "SELECT ID, AMT, N, F, N3810 FROM "+tbl+" ORDER BY ID")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int
		var a, n, f, x string
		if err := rows.Scan(&id, &a, &n, &f, &x); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join([]string{a, n, f, x}, " "); got != want[id] {
			t.Errorf("row %d = %q, want %q", id, got, want[id])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()

	// expressions, and the default Go value (any) is the normalized string
	var expr any
	var div string
	if err := db.QueryRowContext(ctx, "SELECT CAST(-0.75 AS NUMBER), 1/4 FROM DUAL").Scan(&expr, &div); err != nil {
		t.Fatal(err)
	}
	if expr != "-0.75" || div != "0.25" {
		t.Errorf("expressions = %v %q, want -0.75 0.25", expr, div)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// REF CURSOR rows
	var cur ttsql.Cursor
	if _, err := conn.ExecContext(ctx, "BEGIN OPEN ? FOR SELECT AMT, N FROM "+tbl+" WHERE ID = 1; END;", sql.Out{Dest: &cur}); err != nil {
		t.Fatal(err)
	}
	var a, n string
	if !cur.Next() {
		t.Fatalf("cursor empty: %v", cur.Err())
	}
	if err := cur.Scan(&a, &n); err != nil || a != "0.25" || n != "-0.5" {
		t.Errorf("cursor = %q %q %v", a, n, err)
	}
	cur.Close()

	// OUT parameters: TimesTen describes every PL/SQL block parameter as VARCHAR, so the driver
	// cannot tell a NUMBER OUT from a VARCHAR2 OUT and leaves string OUT values as PL/SQL produced
	// them. A NUMBER OUT read into a string keeps TimesTen's form; read it into a number type instead.
	var o1 string
	var o2 float64
	if _, err := conn.ExecContext(ctx, "DECLARE x NUMBER := 0.25; BEGIN ? := x; ? := -x * 2; END;",
		sql.Out{Dest: &o1}, sql.Out{Dest: &o2}); err != nil {
		t.Fatal(err)
	}
	if o1 != ".25" || o2 != -0.5 {
		t.Errorf("OUT NUMBER = %q (string) %v (float64), want .25 -0.5", o1, o2)
	}
}
