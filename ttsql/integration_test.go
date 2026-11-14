//go:build integration

package ttsql_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chuongnx/go-timesten/tt"
	"github.com/chuongnx/go-timesten/ttsql"
	"github.com/chuongnx/go-timesten/ttxtest"
)

func TestPingAndDual(t *testing.T) {
	db := ttxtest.DB(t)
	var n int
	if err := db.QueryRow("SELECT 1 + ? FROM DUAL", 41).Scan(&n); err != nil || n != 42 {
		t.Fatalf("got %d, %v", n, err)
	}
}

const allTypes = `ID TT_INTEGER NOT NULL PRIMARY KEY,
	C_TINY TT_TINYINT, C_SMALL TT_SMALLINT, C_INT TT_INTEGER, C_BIG TT_BIGINT,
	C_N10 NUMBER(10), C_N3810 NUMBER(38,10), C_NUM NUMBER, C_BF BINARY_FLOAT, C_BD BINARY_DOUBLE,
	C_DATE DATE, C_TS TIMESTAMP(9), C_TIME TIME,
	C_CHAR CHAR(10), C_VC VARCHAR2(200), C_NCHAR NCHAR(10), C_NVC NVARCHAR2(200),
	C_BIN BINARY(4), C_VBIN VARBINARY(100), C_CLOB CLOB, C_BLOB BLOB, C_TTTS TT_TIMESTAMP`

func TestTypesRoundTrip(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, allTypes)
	loc := time.Local
	ts := time.Date(2026, 11, 14, 9, 30, 15, 123456789, loc)
	date := time.Date(2026, 1, 31, 23, 59, 58, 0, loc)
	big := strings.Repeat("x", 100_000)
	_, err := db.Exec("INSERT INTO "+tbl+" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		1, 255, -32768, -2147483648, int64(-9223372036854775808),
		int64(9999999999), "1234567890123456789012345678.0123456789", "3.14159265358979323846", float32(1.5), 2.25,
		date, ts, "13:45:59",
		"abc", "Tiếng Việt có dấu ✓", "Xin chào", "日本語テキスト",
		[]byte{1, 2, 3, 4}, []byte{0, 255, 7}, big, bytes.Repeat([]byte{0xAB}, 70_000), ts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO " + tbl + " (ID) VALUES (2)"); err != nil {
		t.Fatal(err)
	}

	var (
		tiny, small, i32, i64, n10 int64
		n3810, num                 string
		bf, bd                     float64
		gotDate, gotTS, gotTime    time.Time
		ch, vc, nch, nvc, clob     string
		bin, vbin, blob            []byte
	)
	err = db.QueryRow("SELECT C_TINY,C_SMALL,C_INT,C_BIG,C_N10,C_N3810,C_NUM,C_BF,C_BD,C_DATE,C_TS,C_TIME,"+
		"C_CHAR,C_VC,C_NCHAR,C_NVC,C_BIN,C_VBIN,C_CLOB,C_BLOB FROM "+tbl+" WHERE ID=1").Scan(
		&tiny, &small, &i32, &i64, &n10, &n3810, &num, &bf, &bd, &gotDate, &gotTS, &gotTime,
		&ch, &vc, &nch, &nvc, &bin, &vbin, &clob, &blob)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, got, want any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("tiny", tiny, 255)
	check("small", small, -32768)
	check("int", i32, -2147483648)
	check("bigint", i64, int64(-9223372036854775808))
	check("number(10)", n10, 9999999999)
	check("number(38,10)", n3810, "1234567890123456789012345678.0123456789")
	check("number", num, "3.14159265358979323846")
	check("binary_float", bf, 1.5)
	check("binary_double", bd, 2.25)
	if !gotDate.Equal(date) {
		t.Errorf("date = %v, want %v", gotDate, date)
	}
	if !gotTS.Equal(ts) {
		t.Errorf("timestamp = %v, want %v", gotTS, ts)
	}
	check("time", gotTime.Format("15:04:05"), "13:45:59")
	check("char", ch, "abc       ")
	check("varchar2", vc, "Tiếng Việt có dấu ✓")
	check("nchar", strings.TrimRight(nch, " "), "Xin chào")
	check("nvarchar2", nvc, "日本語テキスト")
	check("binary", bin, []byte{1, 2, 3, 4})
	check("varbinary", vbin, []byte{0, 255, 7})
	if clob != big {
		t.Errorf("clob length %d, want %d", len(clob), len(big))
	}
	if !bytes.Equal(blob, bytes.Repeat([]byte{0xAB}, 70_000)) {
		t.Errorf("blob length %d", len(blob))
	}

	var ttts time.Time
	if err := db.QueryRow("SELECT C_TTTS FROM " + tbl + " WHERE ID=1").Scan(&ttts); err != nil || !ttts.Equal(ts.Truncate(time.Microsecond)) {
		t.Errorf("tt_timestamp = %v, want %v (%v)", ttts, ts.Truncate(time.Microsecond), err)
	}

	// NULLs
	var ns sql.NullString
	var ni sql.NullInt64
	var nt sql.NullTime
	var nb []byte
	if err := db.QueryRow("SELECT C_VC, C_INT, C_TS, C_VBIN FROM "+tbl+" WHERE ID=2").Scan(&ns, &ni, &nt, &nb); err != nil {
		t.Fatal(err)
	}
	if ns.Valid || ni.Valid || nt.Valid || nb != nil {
		t.Errorf("expected NULLs, got %v %v %v %v", ns, ni, nt, nb)
	}
	// NULL parameter in a predicate on a typed column
	var cnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM "+tbl+" WHERE C_INT IS NULL OR C_INT = ?", nil).Scan(&cnt); err != nil || cnt != 1 {
		t.Errorf("null param: %d %v", cnt, err)
	}
}

func TestColumnTypes(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID NUMBER(9) NOT NULL PRIMARY KEY, AMT NUMBER(12,2), NAME VARCHAR2(50)")
	rows, err := db.Query("SELECT ID, AMT, NAME FROM " + tbl)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cts, _ := rows.ColumnTypes()
	if cts[0].DatabaseTypeName() != "NUMBER" || cts[2].DatabaseTypeName() != "VARCHAR2" {
		t.Errorf("type names: %s %s", cts[0].DatabaseTypeName(), cts[2].DatabaseTypeName())
	}
	if p, s, ok := cts[1].DecimalSize(); !ok || p != 12 || s != 2 {
		t.Errorf("decimal size %d,%d,%v", p, s, ok)
	}
	if n, ok := cts[0].Nullable(); !ok || n {
		t.Errorf("ID nullable=%v ok=%v", n, ok)
	}
	if l, ok := cts[2].Length(); !ok || l != 50 {
		t.Errorf("length %d %v", l, ok)
	}
}

func TestManyRowsFetch(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, V VARCHAR2(20)")
	ctx := context.Background()
	conn, _ := db.Conn(ctx)
	defer conn.Close()
	rows := make([][]any, 5000)
	for i := range rows {
		rows[i] = []any{i, fmt.Sprintf("v%d", i)}
	}
	if n, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?, ?)", rows); err != nil || n != 5000 {
		t.Fatalf("batch: %d %v", n, err)
	}
	r, err := db.Query("SELECT ID, V FROM " + tbl + " ORDER BY ID")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	i := 0
	for r.Next() {
		var id int
		var v string
		if err := r.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		if id != i || v != fmt.Sprintf("v%d", i) {
			t.Fatalf("row %d: %d %s", i, id, v)
		}
		i++
	}
	if r.Err() != nil || i != 5000 {
		t.Fatalf("rows=%d err=%v", i, r.Err())
	}
}

func TestTransactions(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY")
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO "+tbl+" VALUES (?)", 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO "+tbl+" VALUES (?)", 2); err != nil {
		t.Fatal(err)
	}
	if err := ttsql.DurableCommit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var ids []int
	r, err := db.Query("SELECT ID FROM " + tbl)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var id int
		if err := r.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if fmt.Sprint(ids) != "[2]" {
		t.Fatalf("ids = %v", ids)
	}
	if _, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err == nil {
		t.Error("read-only tx should be rejected")
	}
	// autocommit is back on after commit: a plain insert is visible to another connection
	db.Exec("INSERT INTO " + tbl + " VALUES (3)")
	var c int
	db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&c)
	if c != 2 {
		t.Errorf("count=%d", c)
	}
}

func TestDuplicateKeyError(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY")
	db.Exec("INSERT INTO " + tbl + " VALUES (1)")
	_, err := db.Exec("INSERT INTO " + tbl + " VALUES (1)")
	var te *tt.Error
	if !errors.As(err, &te) || te.Code != tt.ErrKeyExists || tt.IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestBatchErrorRow(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR2(10)")
	ctx := context.Background()
	conn, _ := db.Conn(ctx)
	defer conn.Close()
	db.Exec("INSERT INTO " + tbl + " VALUES (7, 'x')")
	rows := [][]any{{1, "a"}, {2, nil}, {7, "dup"}, {8, "b"}}
	_, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?, ?)", rows)
	var be *ttsql.BatchError
	if !errors.As(err, &be) || be.Row != 2 || tt.Code(err) != tt.ErrKeyExists {
		t.Fatalf("err = %v (row %v)", err, be)
	}
	// mixed kinds in a column are rejected before execution
	if _, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?, ?)", [][]any{{1, "a"}, {"x", "b"}}); err == nil {
		t.Error("mixed kinds accepted")
	}
}

func TestProceduresAndRefCursor(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR2(30)")
	proc, fn := ttxtest.Name("P"), ttxtest.Name("F")
	ttxtest.Exec(t, db,
		"INSERT INTO "+tbl+" VALUES (1, 'one')", "INSERT INTO "+tbl+" VALUES (2, 'two')",
		fmt.Sprintf(`CREATE OR REPLACE PROCEDURE %s(p_id IN NUMBER, p_name OUT VARCHAR2, p_cnt IN OUT NUMBER,
			p_ts OUT TIMESTAMP, p_cur OUT SYS_REFCURSOR) AS
		BEGIN
			SELECT name INTO p_name FROM %s WHERE id = p_id;
			p_cnt := p_cnt * 10;
			p_ts := TIMESTAMP '2026-11-14 01:02:03.5';
			OPEN p_cur FOR SELECT id, name FROM %s ORDER BY id;
		END;`, proc, tbl, tbl),
		fmt.Sprintf("CREATE OR REPLACE FUNCTION %s(a NUMBER, b NUMBER) RETURN NUMBER AS BEGIN RETURN a + b; END;", fn))
	t.Cleanup(func() { db.Exec("DROP PROCEDURE " + proc); db.Exec("DROP FUNCTION " + fn) })

	ctx := context.Background()
	conn, _ := db.Conn(ctx)
	defer conn.Close()
	var name string
	cnt := int64(4)
	var ts time.Time
	var cur ttsql.Cursor
	_, err := conn.ExecContext(ctx, "BEGIN "+proc+"(?, ?, ?, ?, ?); END;", 2,
		sql.Out{Dest: &name}, sql.Out{Dest: &cnt, In: true}, sql.Out{Dest: &ts}, sql.Out{Dest: &cur})
	if err != nil {
		t.Fatal(err)
	}
	if name != "two" || cnt != 40 || ts.Format("15:04:05.0") != "01:02:03.5" {
		t.Errorf("outs: name=%q cnt=%d ts=%v", name, cnt, ts)
	}
	var got []string
	for cur.Next() {
		var id int
		var n string
		if err := cur.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%s", id, n))
	}
	if cur.Err() != nil || fmt.Sprint(got) != "[1:one 2:two]" {
		t.Errorf("cursor rows %v err %v", got, cur.Err())
	}
	cur.Close()

	var sum float64
	if _, err := conn.ExecContext(ctx, "BEGIN ? := "+fn+"(?, ?); END;", sql.Out{Dest: &sum}, 1.25, 2); err != nil || sum != 3.25 {
		t.Errorf("function: %v %v", sum, err)
	}
	// built-in procedure returning a result set
	r, err := conn.QueryContext(ctx, "CALL ttLogHolds")
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := r.Columns()
	r.Close()
	if len(cols) != 4 {
		t.Errorf("ttLogHolds columns %v", cols)
	}
}

func TestContextCancel(t *testing.T) {
	db := ttxtest.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	// a deliberately slow query: large cartesian product
	_, err := db.ExecContext(ctx, `SELECT COUNT(*) FROM SYS.COLUMNS a, SYS.COLUMNS b, SYS.COLUMNS c`)
	if err == nil {
		t.Skip("query finished before the deadline; machine too fast for this check")
	}
	if !errors.Is(err, context.DeadlineExceeded) || tt.Code(err) != tt.ErrStatementCancelled {
		t.Fatalf("err = %v", err)
	}
	t.Logf("cancelled after %v: %v", time.Since(start), err)
	// the pool still works afterwards
	var one int
	if err := db.QueryRow("SELECT 1 FROM DUAL").Scan(&one); err != nil {
		t.Fatal(err)
	}
}

func TestPoolConcurrency(t *testing.T) {
	db := ttxtest.DB(t)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, W TT_INTEGER")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if _, err := db.Exec("INSERT INTO "+tbl+" VALUES (?, ?)", w*1000+i, w); err != nil {
					errs <- err
					return
				}
				var c int
				if err := db.QueryRow("SELECT COUNT(*) FROM "+tbl+" WHERE W = ?", w).Scan(&c); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var c int
	db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&c)
	if c != 1600 {
		t.Fatalf("count=%d", c)
	}
	if s := db.Stats(); s.OpenConnections > 4 {
		t.Fatalf("pool exceeded max: %+v", s)
	}
}

func TestPreparedStatementReuse(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, V NUMBER(10,2)")
	st, err := db.Prepare("INSERT INTO " + tbl + " VALUES (?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 50; i++ {
		if _, err := st.Exec(i, float64(i)+0.25); err != nil {
			t.Fatal(err)
		}
	}
	var sum string
	db.QueryRow("SELECT SUM(V) FROM " + tbl).Scan(&sum)
	if sum != "1237.5" {
		t.Fatalf("sum=%s", sum)
	}
}

func BenchmarkExecBatch10k(b *testing.B) {
	db := ttxtest.DB(b)
	tbl := ttxtest.Table(b, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR2(40), AMT NUMBER(12,2), TS TIMESTAMP")
	ctx := context.Background()
	conn, _ := db.Conn(ctx)
	defer conn.Close()
	now := time.Now()
	rows := make([][]any, 10_000)
	for i := range rows {
		rows[i] = []any{i, fmt.Sprintf("name-%d", i), float64(i) / 3, now}
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		conn.ExecContext(ctx, "DELETE FROM "+tbl)
		b.StartTimer()
		if _, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?, ?, ?, ?)", rows); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(10_000*b.N)/b.Elapsed().Seconds(), "rows/s")
}

// A row rejected inside an array execution (value wider than the column) must yield a BatchError and
// leave memory intact — run many times to catch heap corruption.
func TestBatchTruncationErrorRepeated(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, V VARCHAR2(5)")
	ctx := context.Background()
	conn, _ := db.Conn(ctx)
	defer conn.Close()
	q := "MERGE INTO " + tbl + " T USING (SELECT ? AS ID, ? AS V FROM DUAL) S ON (T.ID = S.ID) WHEN MATCHED THEN UPDATE SET T.V = S.V WHEN NOT MATCHED THEN INSERT (ID, V) VALUES (S.ID, S.V)"
	for i := 0; i < 300; i++ {
		_, err := ttsql.ExecBatch(ctx, conn, q, [][]any{{1, "ok"}, {2, "value too long"}})
		var be *ttsql.BatchError
		if !errors.As(err, &be) {
			t.Fatalf("iteration %d: err = %v", i, err)
		}
		if i == 0 {
			t.Logf("row %d: %v", be.Row, be.Err)
		}
	}
}
