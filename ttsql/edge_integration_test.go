//go:build integration

package ttsql_test

// Data-type edge cases: extreme values, special floats, Unicode, large text and LOBs across fetch
// boundaries, batches of every kind, OUT LOBs and truncation, the go_location option.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/chuongnx/go-ttdb/tt"
	"github.com/chuongnx/go-ttdb/ttsql"
	"github.com/chuongnx/go-ttdb/ttxtest"
)

// roundTrip inserts v into col of a fresh row and scans it back into dest.
func roundTrip(t *testing.T, db *sql.DB, tbl, col string, id int, v, dest any) error {
	t.Helper()
	if _, err := db.Exec("INSERT INTO "+tbl+" (ID, "+col+") VALUES (?, ?)", id, v); err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return db.QueryRow("SELECT "+col+" FROM "+tbl+" WHERE ID = ?", id).Scan(dest)
}

func TestTypeEdgeCases(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, `ID TT_INTEGER NOT NULL PRIMARY KEY,
		N NUMBER, N19 NUMBER(19), NNEG NUMBER(5,-2), FL FLOAT(10), BF BINARY_FLOAT, BD BINARY_DOUBLE,
		VC VARCHAR2(4000), VCC VARCHAR2(5 CHAR), BIGVC VARCHAR2(100000), NCL NCLOB, VB VARBINARY(10),
		TS TIMESTAMP(9), D DATE, TTD TT_DATE, TTT TT_TIME, TINY TT_TINYINT`)
	id := 0
	next := func() int { id++; return id }

	t.Run("number", func(t *testing.T) {
		for _, c := range []struct{ in, want string }{
			{"1e-130", "1E-130"},
			{"9.99e125", "9.99E+125"},
			{"1234567890123456789012345678901234567890", "1234567890123456789012345678901234567890"},
			{"-0.001", "-0.001"},
			{"0", "0"},
		} {
			var s string
			if err := roundTrip(t, db, tbl, "N", next(), c.in, &s); err != nil || s != c.want {
				t.Errorf("NUMBER %s = %q, %v; want %q", c.in, s, err, c.want)
			}
		}
		var s string
		if err := roundTrip(t, db, tbl, "NNEG", next(), 12345, &s); err != nil || s != "12300" {
			t.Errorf("NUMBER(5,-2) rounds: %q %v", s, err)
		}
		if err := roundTrip(t, db, tbl, "FL", next(), 1.0/3, &s); err != nil || s != "0.3333" {
			t.Errorf("FLOAT(10) 1/3 = %q %v", s, err)
		}
		var i64 int64
		if err := roundTrip(t, db, tbl, "N19", next(), int64(math.MaxInt64), &i64); err != nil || i64 != math.MaxInt64 {
			t.Errorf("NUMBER(19) MaxInt64 = %d %v", i64, err)
		}
		// beyond int64: scanning into int64 fails, a string keeps every digit
		if err := roundTrip(t, db, tbl, "N19", next(), "9999999999999999999", &i64); err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Errorf("NUMBER(19) > int64 into int64: err = %v", err)
		}
		if err := db.QueryRow("SELECT N19 FROM "+tbl+" WHERE ID = ?", id).Scan(&s); err != nil || s != "9999999999999999999" {
			t.Errorf("NUMBER(19) > int64 into string = %q %v", s, err)
		}
		// uint64 with the high bit set is rejected by database/sql; pass it as a string
		if _, err := db.Exec("INSERT INTO "+tbl+" (ID, N) VALUES (?, ?)", next(), uint64(math.MaxUint64)); err == nil {
			t.Error("uint64 > MaxInt64 accepted")
		}
		if err := roundTrip(t, db, tbl, "N", next(), "18446744073709551615", &s); err != nil || s != "18446744073709551615" {
			t.Errorf("max uint64 as string = %q %v", s, err)
		}
		var b int
		if err := roundTrip(t, db, tbl, "N", next(), true, &b); err != nil || b != 1 {
			t.Errorf("bool true = %d %v", b, err)
		}
		// TT_TINYINT is unsigned 0..255
		if _, err := db.Exec("INSERT INTO "+tbl+" (ID, TINY) VALUES (?, ?)", next(), 256); err == nil {
			t.Error("TT_TINYINT 256 accepted")
		}
		if _, err := db.Exec("INSERT INTO "+tbl+" (ID, TINY) VALUES (?, ?)", next(), -1); err == nil {
			t.Error("TT_TINYINT -1 accepted")
		}
	})

	t.Run("float", func(t *testing.T) {
		for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64, math.SmallestNonzeroFloat64, -0.0} {
			var f float64
			if err := roundTrip(t, db, tbl, "BD", next(), v, &f); err != nil {
				t.Errorf("BINARY_DOUBLE %v: %v", v, err)
				continue
			}
			if !(f == v || (math.IsNaN(v) && math.IsNaN(f))) {
				t.Errorf("BINARY_DOUBLE %v = %v", v, f)
			}
		}
		var f float32
		if err := roundTrip(t, db, tbl, "BF", next(), float32(0.1), &f); err != nil || f != float32(0.1) {
			t.Errorf("BINARY_FLOAT 0.1 = %v %v", f, err)
		}
		if err := roundTrip(t, db, tbl, "BF", next(), float32(math.Inf(-1)), &f); err != nil || !math.IsInf(float64(f), -1) {
			t.Errorf("BINARY_FLOAT -Inf = %v %v", f, err)
		}
	})

	t.Run("text", func(t *testing.T) {
		var s string
		var ns sql.NullString
		// Oracle semantics: '' is NULL
		if err := roundTrip(t, db, tbl, "VC", next(), "", &ns); err != nil || ns.Valid {
			t.Errorf("empty string = %v %v, want NULL", ns, err)
		}
		for _, v := range []string{"😀🇻🇳 4-byte UTF-8", "tab\tnew\nline\r\x00nul", strings.Repeat("ạ", 1333)} {
			if err := roundTrip(t, db, tbl, "VC", next(), v, &s); err != nil || s != v {
				t.Errorf("VARCHAR2 %q... = %q... %v", v[:min(len(v), 12)], s[:min(len(s), 12)], err)
			}
		}
		// CHAR length semantics: 5 characters of 3 bytes each
		if err := roundTrip(t, db, tbl, "VCC", next(), "ẤẦẨẪẬ", &s); err != nil || s != "ẤẦẨẪẬ" {
			t.Errorf("VARCHAR2(5 CHAR) = %q %v", s, err)
		}
		if _, err := db.Exec("INSERT INTO "+tbl+" (ID, VCC) VALUES (?, ?)", next(), "ẤẦẨẪẬẬ"); err == nil {
			t.Error("VARCHAR2(5 CHAR) accepted 6 characters")
		}
		big := strings.Repeat("ệ", 30000) // 90,000 bytes: fetched with SQLGetData
		if err := roundTrip(t, db, tbl, "BIGVC", next(), big, &s); err != nil || s != big {
			t.Errorf("VARCHAR2(100000): len %d %v", len(s), err)
		}
		nclob := strings.Repeat("Ữ", 350000) // ~1 MB
		if err := roundTrip(t, db, tbl, "NCL", next(), nclob, &s); err != nil || s != nclob {
			t.Errorf("NCLOB 1MB: len %d %v", len(s), err)
		}
	})

	t.Run("binary", func(t *testing.T) {
		var b []byte
		if err := roundTrip(t, db, tbl, "VB", next(), []byte{}, &b); err != nil || b == nil || len(b) != 0 {
			t.Errorf("empty VARBINARY = %v (nil=%v) %v", b, b == nil, err)
		}
		if _, err := db.Exec("INSERT INTO "+tbl+" (ID, VB) VALUES (?, ?)", next(), make([]byte, 11)); err == nil {
			t.Error("VARBINARY(10) accepted 11 bytes")
		}
	})

	t.Run("time", func(t *testing.T) {
		for _, v := range []time.Time{
			time.Date(1, 1, 1, 0, 0, 0, 0, time.Local),
			time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.Local),
			time.Date(1960, 6, 1, 1, 2, 3, 4, time.Local),
			time.Date(2024, 2, 29, 12, 0, 0, 0, time.Local),
		} {
			var got time.Time
			if err := roundTrip(t, db, tbl, "TS", next(), v, &got); err != nil || !got.Equal(v) {
				t.Errorf("TIMESTAMP %v = %v %v", v, got, err)
			}
		}
		// a value in another zone is stored as the same instant expressed in go_location
		jst := time.Date(2026, 3, 1, 9, 0, 0, 0, time.FixedZone("JST", 9*3600))
		var got time.Time
		if err := roundTrip(t, db, tbl, "TS", next(), jst, &got); err != nil || !got.Equal(jst) || got.Location() != time.Local {
			t.Errorf("TIMESTAMP from JST = %v %v", got, err)
		}
		// DATE has no fractional seconds
		if err := roundTrip(t, db, tbl, "D", next(), time.Date(2026, 3, 1, 9, 0, 0, 500, time.Local), &got); err != nil ||
			!got.Equal(time.Date(2026, 3, 1, 9, 0, 0, 0, time.Local)) {
			t.Errorf("DATE with nanoseconds = %v %v", got, err)
		}
		if err := roundTrip(t, db, tbl, "TTD", next(), time.Date(2026, 3, 1, 0, 0, 0, 0, time.Local), &got); err != nil || got.Format("2006-01-02") != "2026-03-01" {
			t.Errorf("TT_DATE = %v %v", got, err)
		}
		if err := roundTrip(t, db, tbl, "TTT", next(), "23:59:58", &got); err != nil || got.Format("15:04:05") != "23:59:58" {
			t.Errorf("TT_TIME = %v %v", got, err)
		}
		// strings in the default format are converted by TimesTen
		if err := roundTrip(t, db, tbl, "TS", next(), "2026-03-01 10:11:12", &got); err != nil || got.Format("2006-01-02 15:04:05") != "2026-03-01 10:11:12" {
			t.Errorf("TIMESTAMP from string = %v %v", got, err)
		}
		// date arithmetic returns NUMBER (days)
		var days string
		if err := db.QueryRow("SELECT TO_DATE('2026-01-02 12:00:00','YYYY-MM-DD HH24:MI:SS') - TO_DATE('2026-01-01','YYYY-MM-DD') FROM DUAL").Scan(&days); err != nil || days != "1.5" {
			t.Errorf("date difference = %q %v", days, err)
		}
	})

	t.Run("rowid", func(t *testing.T) {
		var rowid string
		if err := db.QueryRow("SELECT ROWID FROM " + tbl + " WHERE ID = 1").Scan(&rowid); err != nil || rowid == "" {
			t.Fatalf("ROWID = %q %v", rowid, err)
		}
		var got int
		if err := db.QueryRow("SELECT ID FROM "+tbl+" WHERE ROWID = ?", rowid).Scan(&got); err != nil || got != 1 {
			t.Errorf("ROWID as parameter: %d %v", got, err)
		}
	})
}

func TestNCharKeys(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "K NVARCHAR2(20) NOT NULL PRIMARY KEY, C NCHAR(4), V NUMBER")
	keys := []string{"Hà Nội", "Đà Nẵng", "東京", "😀"}
	for i, k := range keys {
		if _, err := db.Exec("INSERT INTO "+tbl+" VALUES (?, ?, ?)", k, "ab", i); err != nil {
			t.Fatal(err)
		}
	}
	for i, k := range keys {
		var v int
		if err := db.QueryRow("SELECT V FROM "+tbl+" WHERE K = ?", k).Scan(&v); err != nil || v != i {
			t.Errorf("lookup %q = %d %v", k, v, err)
		}
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM "+tbl+" WHERE C = ?", "ab").Scan(&n); err != nil || n != 4 {
		t.Errorf("NCHAR comparison with blank padding: %d %v", n, err)
	}
}

// Rows with LOBs (fetched one row at a time with SQLGetData) next to plain columns.
func TestLOBMultiRowFetch(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR2(20), C CLOB, B BLOB, AMT NUMBER(10,2)")
	const rows = 300
	val := func(i int) (string, []byte) {
		if i%7 == 0 {
			return "", nil
		}
		return strings.Repeat(fmt.Sprintf("c%d-", i), i*10), bytes.Repeat([]byte{byte(i)}, i*100)
	}
	ctx := context.Background()
	conn, _ := db.Conn(ctx)
	defer conn.Close()
	for i := 0; i < rows; i++ {
		c, b := val(i)
		var cv, bv any = c, b
		if c == "" {
			cv, bv = nil, nil
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES (?, ?, ?, ?, ?)", i, fmt.Sprint("n", i), cv, bv, float64(i)+0.5); err != nil {
			t.Fatal(err)
		}
	}
	r, err := db.Query("SELECT ID, NAME, C, B, AMT FROM " + tbl + " ORDER BY ID")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	i := 0
	for r.Next() {
		var id int
		var name string
		var c sql.NullString
		var b []byte
		var amt float64
		if err := r.Scan(&id, &name, &c, &b, &amt); err != nil {
			t.Fatal(err)
		}
		wc, wb := val(i)
		if id != i || name != fmt.Sprint("n", i) || c.String != wc || c.Valid != (wc != "") || !bytes.Equal(b, wb) || amt != float64(i)+0.5 {
			t.Fatalf("row %d: id=%d name=%s clob %d/%v blob %d amt %v", i, id, name, len(c.String), c.Valid, len(b), amt)
		}
		i++
	}
	if r.Err() != nil || i != rows {
		t.Fatalf("rows=%d err=%v", i, r.Err())
	}
}

// Result sizes around the array-fetch size, with NULLs in the last row of each block.
func TestFetchBoundaries(t *testing.T) {
	for _, fetch := range []int{1, 2, 256} {
		db, err := sql.Open("timesten", ttxtest.DSN(t)+fmt.Sprintf(";go_fetch_rows=%d", fetch))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, V VARCHAR2(10), N NUMBER(10,2)")
		ctx := context.Background()
		conn, _ := db.Conn(ctx)
		rowsIn := make([][]any, 0, 520)
		for i := 0; i < 520; i++ {
			if (i+1)%fetch == 0 || i%5 == 0 {
				rowsIn = append(rowsIn, []any{i, nil, nil})
			} else {
				rowsIn = append(rowsIn, []any{i, fmt.Sprint("v", i), float64(i) / 4})
			}
		}
		if _, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?, ?, ?)", rowsIn); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		for _, limit := range []int{0, 1, fetch - 1, fetch, fetch + 1, 2*fetch - 1, 2 * fetch, 2*fetch + 1, 520} {
			if limit < 0 {
				continue
			}
			r, err := db.Query("SELECT ID, V, N FROM "+tbl+" WHERE ID < ? ORDER BY ID", limit)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for r.Next() {
				var id int
				var v sql.NullString
				var num sql.NullFloat64
				if err := r.Scan(&id, &v, &num); err != nil {
					t.Fatal(err)
				}
				want := rowsIn[n]
				if id != n || v.Valid != (want[1] != nil) || (v.Valid && v.String != want[1]) || (num.Valid && num.Float64 != want[2]) {
					t.Fatalf("fetch=%d limit=%d row %d: %d %v %v", fetch, limit, n, id, v, num)
				}
				n++
			}
			r.Close()
			if r.Err() != nil || n != limit {
				t.Errorf("fetch=%d limit=%d: got %d rows, err %v", fetch, limit, n, r.Err())
			}
		}
	}
}

func TestExecBatchAllKinds(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, `ID TT_INTEGER NOT NULL PRIMARY KEY, I TT_BIGINT, F BINARY_DOUBLE, N NUMBER(12,3),
		S VARCHAR2(50), U NVARCHAR2(20), B VARBINARY(20), TS TIMESTAMP(9), D DATE, C CLOB, BL BLOB`)
	ctx := context.Background()
	conn, _ := db.Conn(ctx)
	defer conn.Close()
	ts := time.Date(2026, 11, 14, 1, 2, 3, 456789000, time.Local)
	rows := [][]any{
		{1, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil}, // NULLs first: the kind comes from later rows
		// a column holds one kind (ints may mix with floats); exact decimals go as strings
		{2, int64(math.MinInt64), 1.5, "123456789.123", "Tiếng Việt", "日本", []byte{0, 1}, ts, ts, strings.Repeat("x", 5000), bytes.Repeat([]byte{7}, 5000)},
		{3, 7, 2, "0.5", "", "", []byte{}, ts.Add(time.Hour), ts, "", []byte{}},
		{4, int64(math.MaxInt64), math.Inf(1), "-1", "s", "u", []byte{255}, ts, ts, "c", []byte{1}},
	}
	// TimesTen has no array binding for LOB columns: the batch fails as a whole, nothing is inserted
	_, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?,?,?,?,?,?,?,?,?,?,?)", rows)
	if err == nil || !strings.Contains(err.Error(), "Batch mode not supported for LOBs") {
		t.Fatalf("batch into LOB columns: err = %v", err)
	}
	// without the LOB columns, then the LOBs row by row
	noLOB := make([][]any, len(rows))
	for k, r := range rows {
		noLOB[k] = r[:9]
	}
	n, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" (ID,I,F,N,S,U,B,TS,D) VALUES (?,?,?,?,?,?,?,?,?)", noLOB)
	if err != nil || n != 4 {
		t.Fatalf("batch: %d %v", n, err)
	}
	for _, r := range rows {
		if _, err := conn.ExecContext(ctx, "UPDATE "+tbl+" SET C = ?, BL = ? WHERE ID = ?", r[9], r[10], r[0]); err != nil {
			t.Fatal(err)
		}
	}
	var (
		i    sql.NullInt64
		f    sql.NullFloat64
		num  sql.NullString
		s, u sql.NullString
		b    []byte
		gts  sql.NullTime
		d    sql.NullTime
		c    sql.NullString
		bl   []byte
	)
	scan := func(id int) {
		t.Helper()
		if err := db.QueryRow("SELECT I,F,N,S,U,B,TS,D,C,BL FROM "+tbl+" WHERE ID=?", id).Scan(&i, &f, &num, &s, &u, &b, &gts, &d, &c, &bl); err != nil {
			t.Fatal(err)
		}
	}
	scan(1)
	if i.Valid || f.Valid || num.Valid || s.Valid || u.Valid || b != nil || gts.Valid || d.Valid || c.Valid || bl != nil {
		t.Errorf("row 1 not all NULL")
	}
	scan(2)
	if i.Int64 != math.MinInt64 || f.Float64 != 1.5 || num.String != "123456789.123" || s.String != "Tiếng Việt" || u.String != "日本" ||
		!bytes.Equal(b, []byte{0, 1}) || !gts.Time.Equal(ts) || !d.Time.Equal(ts.Truncate(time.Second)) || len(c.String) != 5000 || len(bl) != 5000 {
		t.Errorf("row 2: %v %v %v %q %q %v %v %v %d %d", i, f, num, s.String, u.String, b, gts.Time, d.Time, len(c.String), len(bl))
	}
	scan(3)
	if i.Int64 != 7 || f.Float64 != 2 || num.String != "0.5" || s.Valid || u.Valid {
		t.Errorf("row 3 (int into float column, '' as NULL): %v %v %v %v %v", i, f, num, s, u)
	}
	scan(4)
	if i.Int64 != math.MaxInt64 || !math.IsInf(f.Float64, 1) || num.String != "-1" {
		t.Errorf("row 4: %v %v %v", i, f, num)
	}

	// an empty batch is a no-op
	if n, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" (ID) VALUES (?)", nil); err != nil || n != 0 {
		t.Errorf("empty batch: %d %v", n, err)
	}
	// more rows than one array execution (go_batch_rows default 1024), error in the second chunk
	big := make([][]any, 2500)
	for k := range big {
		big[k] = []any{100 + k}
	}
	big[1500] = []any{2} // duplicate key
	_, err = ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" (ID) VALUES (?)", big)
	var be *ttsql.BatchError
	if !errors.As(err, &be) || be.Row != 1500 || tt.Code(err) != tt.ErrKeyExists {
		t.Errorf("chunked batch error: %v", err)
	}
}

func TestOutLOBAndTruncation(t *testing.T) {
	db := ttxtest.DB(t)
	ctx := context.Background()
	// CLOB / BLOB OUT parameters arrive as string / []byte
	var c string
	var b []byte
	if _, err := db.ExecContext(ctx, "DECLARE c CLOB := RPAD('x', 20000, 'y'); b BLOB := HEXTORAW('DEADBEEF'); BEGIN ? := c; ? := b; END;",
		sql.Out{Dest: &c}, sql.Out{Dest: &b}); err != nil {
		t.Fatal(err)
	}
	if len(c) != 20000 || c[:2] != "xy" || fmt.Sprintf("%X", b) != "DEADBEEF" {
		t.Errorf("LOB OUT: clob %d bytes, blob %X", len(c), b)
	}
	// up to 32767 bytes fit; PL/SQL VARCHAR2 cannot hold more
	var s string
	if _, err := db.ExecContext(ctx, "BEGIN ? := RPAD('a', 32767, 'a'); END;", sql.Out{Dest: &s}); err != nil || len(s) != 32767 {
		t.Errorf("32767-byte OUT: %d %v", len(s), err)
	}
	// a CLOB longer than the OUT buffer is reported, not silently cut
	_, err := db.ExecContext(ctx, "DECLARE c CLOB; BEGIN c := RPAD('a', 32767, 'a'); c := c || c; ? := c; END;", sql.Out{Dest: &s})
	if err == nil {
		t.Errorf("65534-byte CLOB OUT returned %d bytes without error", len(s))
	} else {
		t.Logf("oversized OUT: %v", err)
	}
	// IN OUT keeps a buffer at least as large as the input
	in := strings.Repeat("é", 20000) // 40,000 bytes: bound as a LOB (over the PL/SQL VARCHAR limit)
	if _, err := db.ExecContext(ctx, "DECLARE v CLOB; BEGIN v := ?; ? := LENGTH(v); END;", in, sql.Out{Dest: &s}); err != nil || s != "20000" {
		t.Errorf("40 KB IN into CLOB: %q %v", s, err)
	}
}

func TestLocationOption(t *testing.T) {
	hcm, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	open := func(loc string) *sql.DB {
		db, err := sql.Open("timesten", ttxtest.DSN(t)+";go_location="+loc)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	vn, us, utc := open("Asia/Ho_Chi_Minh"), open("America/New_York"), open("UTC")
	tbl := ttxtest.Table(t, vn, "ID TT_INTEGER NOT NULL PRIMARY KEY, TS TIMESTAMP(6)")

	// wall clock 2026-11-14 09:00 in Ho Chi Minh City is stored as 09:00
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, hcm)
	if _, err := vn.Exec("INSERT INTO "+tbl+" VALUES (1, ?)", at); err != nil {
		t.Fatal(err)
	}
	// a UTC instant is converted to the connection zone before storing: 02:00Z → 09:00
	if _, err := vn.Exec("INSERT INTO "+tbl+" VALUES (2, ?)", at.UTC()); err != nil {
		t.Fatal(err)
	}
	var text string
	vn.QueryRow("SELECT TO_CHAR(TS, 'YYYY-MM-DD HH24:MI') FROM " + tbl + " WHERE ID = 2").Scan(&text)
	if text != "2026-11-14 09:00" {
		t.Errorf("stored wall clock = %s", text)
	}
	for _, c := range []struct {
		db   *sql.DB
		loc  *time.Location
		name string
	}{{vn, hcm, "Asia/Ho_Chi_Minh"}, {us, ny, "America/New_York"}, {utc, time.UTC, "UTC"}} {
		var got time.Time
		if err := c.db.QueryRow("SELECT TS FROM " + tbl + " WHERE ID = 1").Scan(&got); err != nil {
			t.Fatal(err)
		}
		// the stored wall clock is read as a time in the connection's zone
		if got.Location().String() != c.name || got.Format("15:04") != "09:00" {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	// DST gap in New York: 2026-03-08 02:30 does not exist; Go normalizes it
	var got time.Time
	if _, err := us.Exec("INSERT INTO " + tbl + " VALUES (3, TIMESTAMP '2026-03-08 02:30:00')"); err != nil {
		t.Fatal(err)
	}
	if err := us.QueryRow("SELECT TS FROM " + tbl + " WHERE ID = 3").Scan(&got); err != nil {
		t.Fatal(err)
	}
	t.Logf("non-existent local time 02:30 read in New York as %v", got)
	if _, err := sql.Open("timesten", ttxtest.DSN(t)+";go_location=Mars/Base"); err == nil {
		// sql.Open is lazy; the error shows on first use
		db, _ := sql.Open("timesten", ttxtest.DSN(t)+";go_location=Mars/Base")
		if err := db.Ping(); err == nil {
			t.Error("unknown go_location accepted")
		}
		db.Close()
	}
}

// Inputs over 32 KB reach PL/SQL CLOB/BLOB variables and VARCHAR2/VARBINARY/LOB columns.
func TestLongInputs(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, V VARCHAR2(100000), VB VARBINARY(100000), C CLOB, B BLOB")
	text := strings.Repeat("é", 50000) // 100,000 bytes
	bin := bytes.Repeat([]byte{0xA5}, 100000)
	var n, m string
	if _, err := db.Exec("DECLARE c CLOB := ?; b BLOB := ?; BEGIN ? := LENGTH(c); ? := DBMS_LOB.GETLENGTH(b); END;",
		text, bin, sql.Out{Dest: &n}, sql.Out{Dest: &m}); err != nil || n != "50000" || m != "100000" {
		t.Errorf("PL/SQL LOB inputs: %q %q %v", n, m, err)
	}
	if _, err := db.Exec("INSERT INTO "+tbl+" VALUES (1, ?, ?, ?, ?)", text, bin, text, bin); err != nil {
		t.Fatal(err)
	}
	var v, c string
	var vb, b []byte
	if err := db.QueryRow("SELECT V, VB, C, B FROM "+tbl).Scan(&v, &vb, &c, &b); err != nil {
		t.Fatal(err)
	}
	if v != text || c != text || !bytes.Equal(vb, bin) || !bytes.Equal(b, bin) {
		t.Errorf("long column values: %d %d %d %d", len(v), len(vb), len(c), len(b))
	}
	// in a WHERE clause too
	var id int
	if err := db.QueryRow("SELECT ID FROM "+tbl+" WHERE V = ?", text).Scan(&id); err != nil || id != 1 {
		t.Errorf("long value in WHERE: %d %v", id, err)
	}
}

// Text longer than one SQLGetData piece (64 KB) whose characters straddle a piece boundary
// (regression: a split multibyte character used to leave stray bytes in the value).
func TestLongMultibyteText(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, V VARCHAR2(300000), C CLOB, N NCLOB")
	for i, ch := range []string{"a", "é", "ệ", "😀", "aé😀ệ"} {
		text := strings.Repeat(ch, 200000/len(ch)) // ~200 KB: three pieces
		if _, err := db.Exec("INSERT INTO "+tbl+" VALUES (?, ?, ?, ?)", i, text, text, text); err != nil {
			t.Fatal(err)
		}
		for _, col := range []string{"V", "C", "N"} {
			var got string
			err := db.QueryRow("SELECT "+col+" FROM "+tbl+" WHERE ID = ?", i).Scan(&got)
			if col == "C" && errors.Is(err, tt.ErrClientCLOB) && isClientServer(t, db) {
				continue // known TimesTen client defect, reported instead of returning garbled text
			}
			if err != nil {
				t.Errorf("%q %s: %v", ch, col, err)
			} else if got != text {
				t.Errorf("%q %s: %d bytes, want %d", ch, col, len(got), len(text))
			}
		}
	}
}

func isClientServer(t *testing.T, db *sql.DB) bool {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var cs bool
	conn.Raw(func(dc any) error { cs = dc.(*ttsql.Conn).IsClientServer(); return nil })
	return cs
}

// Over client/server, the TimesTen client driver garbles multibyte CLOB text over 4 KB. The driver
// reports it (tt.ErrClientCLOB); NCLOB and VARCHAR2 columns hold the same text correctly.
func TestClientCLOBWorkarounds(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, C CLOB, N NCLOB, V VARCHAR2(400000)")
	text := strings.Repeat("Tiếng Việt có dấu, 東京 😀. ", 3000) // ~100 KB of mixed 1–4 byte characters
	if _, err := db.Exec("INSERT INTO "+tbl+" VALUES (1, ?, ?, ?)", text, text, text); err != nil {
		t.Fatal(err)
	}
	var c string
	err := db.QueryRow("SELECT C FROM " + tbl).Scan(&c)
	cs := isClientServer(t, db)
	switch {
	case err == nil && c == text:
		t.Logf("CLOB read correctly (client/server=%v)", cs)
	case errors.Is(err, tt.ErrClientCLOB) && cs:
		t.Logf("client/server CLOB defect reported: %v", err)
	default:
		t.Fatalf("CLOB (client/server=%v): %d bytes, err %v", cs, len(c), err)
	}
	var n, v string
	if err := db.QueryRow("SELECT N, V FROM "+tbl).Scan(&n, &v); err != nil || n != text || v != text {
		t.Errorf("NCLOB/VARCHAR2: %d %d %v", len(n), len(v), err)
	}
}
