//go:build integration

package xla_test

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chuongnx/go-timesten/tt"
	"github.com/chuongnx/go-timesten/ttxtest"
	"github.com/chuongnx/go-timesten/xla"
	"github.com/chuongnx/go-timesten/xla/admin"
)

const owner = "GOTEST"

// setup creates a table, a bookmark subscribed to it, and returns an open reader.
func setup(t *testing.T, columns string) (*sql.DB, string, *xla.Reader, string) {
	t.Helper()
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, columns)
	bm := strings.ToLower(ttxtest.Name("bm"))
	ctx := context.Background()
	r, err := xla.Open(ctx, ttxtest.DSN(t), bm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.DeleteBookmark(context.Background(), db, bm) })
	t.Cleanup(func() { r.Close() })
	if err := r.Subscribe(ctx, owner, tbl); err != nil {
		t.Fatal(err)
	}
	drain(t, r)
	return db, tbl, r, bm
}

// drain reads and acknowledges everything currently in the log for the bookmark.
func drain(t *testing.T, r *xla.Reader) {
	t.Helper()
	for {
		recs, err := r.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 0 {
			break
		}
	}
	if err := r.Ack(); err != nil {
		t.Fatal(err)
	}
}

// collect reads until n records of the table (ignoring other tables) have arrived.
func collect(t *testing.T, r *xla.Reader, tbl string, n int) []xla.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out []xla.Record
	for len(out) < n {
		recs, err := r.Next(ctx, xla.Wait(2*time.Second))
		if err != nil {
			t.Fatalf("next after %d records: %v", len(out), err)
		}
		for _, rec := range recs {
			if rec.Name == tbl {
				out = append(out, rec)
			}
		}
	}
	return out
}

const allTypes = `ID TT_INTEGER NOT NULL PRIMARY KEY,
	C_TINY TT_TINYINT, C_SMALL TT_SMALLINT, C_BIG TT_BIGINT,
	C_N10 NUMBER(10), C_N3810 NUMBER(38,10), C_NUM NUMBER, C_FLT FLOAT, C_BF BINARY_FLOAT, C_BD BINARY_DOUBLE,
	C_DATE DATE, C_TS TIMESTAMP(9), C_TIME TIME, C_TTDATE TT_DATE, C_TTTS TT_TIMESTAMP,
	C_CHAR CHAR(10), C_VC VARCHAR2(200), C_LONGVC VARCHAR2(5000), C_NCHAR NCHAR(10), C_NVC NVARCHAR2(200),
	C_BIN BINARY(4), C_VBIN VARBINARY(100), C_CLOB CLOB, C_ROWID ROWID`

func TestDecodeAllTypes(t *testing.T) {
	db, tbl, r, _ := setup(t, allTypes)
	loc := time.Local
	ts := time.Date(2026, 11, 14, 9, 30, 15, 123456789, loc)
	date := time.Date(2026, 1, 31, 23, 59, 58, 0, loc)
	long := strings.Repeat("Ư", 1500) // 4500 bytes in AL32UTF8: stored out of line
	_, err := db.Exec("INSERT INTO "+tbl+" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)",
		1, 255, -32768, int64(-9223372036854775808),
		int64(9999999999), "-1234567890123456789012345678.0123456789", "3.14159265358979323846", "2.5", float32(1.5), 2.25,
		date, ts, "13:45:59", "2026-02-03", ts,
		"abc", "Tiếng Việt có dấu ✓", long, "Xin chào", "日本語テキスト",
		[]byte{1, 2, 3, 4}, []byte{0, 255, 7}, "clob text")
	if err != nil {
		t.Fatal(err)
	}
	recs := collect(t, r, tbl, 1)
	rec := recs[0]
	if rec.Op != xla.OpInsert || rec.Owner != owner || rec.New == nil || rec.Old != nil {
		t.Fatalf("record: op=%v owner=%q new=%v old=%v", rec.Op, rec.Owner, rec.New != nil, rec.Old != nil)
	}
	if !rec.FirstInTxn() || !rec.EndsTxn() {
		t.Errorf("single-row autocommit txn flags = %b", rec.Flags)
	}
	if fmt.Sprint(rec.Table.PrimaryKey) != "[0]" || !rec.Table.Columns[0].PrimaryKey {
		t.Errorf("primary key = %v", rec.Table.PrimaryKey)
	}
	want := map[string]string{
		"ID": "1", "C_TINY": "255", "C_SMALL": "-32768", "C_BIG": "-9223372036854775808",
		"C_N10": "9999999999", "C_N3810": "-1234567890123456789012345678.0123456789",
		"C_NUM": "3.14159265358979323846", "C_FLT": "2.5", "C_BF": "1.5", "C_BD": "2.25",
		"C_DATE": date.Format("2006-01-02 15:04:05.999999999"), "C_TS": ts.Format("2006-01-02 15:04:05.999999999"),
		"C_TIME": "13:45:59", "C_TTDATE": "2026-02-03 00:00:00", "C_TTTS": ts.Truncate(time.Microsecond).Format("2006-01-02 15:04:05.999999999"),
		"C_CHAR": "abc       ", "C_VC": "Tiếng Việt có dấu ✓", "C_NCHAR": "Xin chào  ", "C_NVC": "日本語テキスト",
		"C_BIN": "0x01020304", "C_VBIN": "0x00FF07", "C_CLOB": "<LOB>", "C_ROWID": "NULL",
	}
	for col, w := range want {
		v, ok := rec.NewValue(col)
		if !ok {
			t.Errorf("%s missing", col)
			continue
		}
		if got := v.String(); got != w {
			t.Errorf("%s (%v, kind %d) = %q, want %q", col, rec.Table.Columns[rec.Table.ColumnIndex(col)].Type, v.Kind, got, w)
		}
	}
	if v, _ := rec.NewValue("C_LONGVC"); v.Text != long {
		t.Errorf("long varchar: %d bytes, want %d", len(v.Text), len(long))
	}
	if v, _ := rec.NewValue("C_N3810"); v.Kind != xla.KindDecimal {
		t.Errorf("NUMBER kind = %d", v.Kind)
	}
}

func TestUpdateDeleteTruncate(t *testing.T) {
	db, tbl, r, _ := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR2(30), AMT NUMBER(12,2), NOTE VARCHAR2(30)")
	ttxtest.Exec(t, db,
		"INSERT INTO "+tbl+" VALUES (1, 'a', 10.5, 'n1')",
		"UPDATE "+tbl+" SET NAME = 'b', NOTE = NULL WHERE ID = 1",
		"DELETE FROM "+tbl+" WHERE ID = 1",
		"INSERT INTO "+tbl+" VALUES (2, 'c', 1, NULL)",
		"TRUNCATE TABLE "+tbl,
	)
	recs := collect(t, r, tbl, 5)
	ops := []xla.Op{recs[0].Op, recs[1].Op, recs[2].Op, recs[3].Op, recs[4].Op}
	if fmt.Sprint(ops) != "[INSERT UPDATE DELETE INSERT TRUNCATE]" {
		t.Fatalf("ops = %v", ops)
	}
	up := recs[1]
	oldName, _ := up.OldValue("NAME")
	newName, _ := up.NewValue("NAME")
	oldNote, _ := up.OldValue("NOTE")
	newNote, _ := up.NewValue("NOTE")
	amt, _ := up.NewValue("AMT")
	if oldName.Text != "a" || newName.Text != "b" || oldNote.Text != "n1" || !newNote.IsNull() || amt.Text != "10.5" {
		t.Errorf("update images: %v→%v, %v→%v, amt %v", oldName, newName, oldNote, newNote, amt)
	}
	var changed []string
	for _, i := range up.UpdatedColumns {
		changed = append(changed, up.Table.Columns[i].Name)
	}
	slices.Sort(changed)
	if fmt.Sprint(changed) != "[NAME NOTE]" {
		t.Errorf("updated columns = %v", changed)
	}
	del := recs[2]
	if k := del.Key(); len(k) != 1 || k[0].Int != 1 || del.New != nil {
		t.Errorf("delete key %v new %v", k, del.New)
	}
	tr := recs[4]
	if tr.Owner != owner || tr.Name != tbl || tr.Table != nil {
		t.Errorf("truncate record: %q.%q table=%v", tr.Owner, tr.Name, tr.Table)
	}
}

func TestTransactionBoundaries(t *testing.T) {
	db, tbl, r, _ := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY")
	tx, _ := db.Begin()
	for i := 1; i <= 3; i++ {
		if _, err := tx.Exec("INSERT INTO "+tbl+" VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}
	tx.Commit()
	recs := collect(t, r, tbl, 3)
	if !recs[0].FirstInTxn() || recs[0].EndsTxn() || recs[1].FirstInTxn() || recs[1].EndsTxn() || !recs[2].EndsTxn() {
		t.Errorf("flags %b %b %b", recs[0].Flags, recs[1].Flags, recs[2].Flags)
	}
	if recs[0].TxnID != recs[2].TxnID {
		t.Errorf("txn ids differ: %d %d", recs[0].TxnID, recs[2].TxnID)
	}
	// records of one transaction may share an LSN; it never decreases
	if recs[2].LSN.File < recs[0].LSN.File || (recs[2].LSN.File == recs[0].LSN.File && recs[2].LSN.Offset < recs[0].LSN.Offset) {
		t.Errorf("LSN decreased: %v %v", recs[0].LSN, recs[2].LSN)
	}
}

func TestAckAndRedelivery(t *testing.T) {
	db, tbl, r, bm := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY")
	ctx := context.Background()
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" VALUES (1)", "INSERT INTO "+tbl+" VALUES (2)")
	first := collect(t, r, tbl, 2)

	// same handle, not acknowledged: rewind delivers them again
	pos0 := first // keep for comparison
	r.Close()
	r2, err := xla.Open(ctx, ttxtest.DSN(t), bm, xla.WithMode(xla.ReuseOnly))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	again := collect(t, r2, tbl, 2)
	if again[0].TxnID != pos0[0].TxnID || again[1].LSN != pos0[1].LSN {
		t.Fatalf("redelivered records differ: %v vs %v", again[1].LSN, pos0[1].LSN)
	}
	if err := r2.Ack(); err != nil {
		t.Fatal(err)
	}
	// after Ack: rewinding to a position read later, and reopening, delivers nothing old
	p, _ := r2.Position()
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" VALUES (3)")
	got := collect(t, r2, tbl, 1)
	if v, _ := got[0].NewValue("ID"); v.Int != 3 {
		t.Fatalf("expected ID 3, got %v", v)
	}
	if err := r2.Rewind(p); err != nil {
		t.Fatal(err)
	}
	rewound := collect(t, r2, tbl, 1)
	if v, _ := rewound[0].NewValue("ID"); v.Int != 3 {
		t.Fatalf("rewind delivered %v", v)
	}
	r2.Ack()
	r2.Close()
	r3, err := xla.Open(ctx, ttxtest.DSN(t), bm, xla.WithMode(xla.ReuseOnly))
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Close()
	recs, err := r3.Next(ctx, xla.Wait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Name == tbl {
			t.Fatalf("acknowledged record delivered again: %v %v", rec.Op, rec.New)
		}
	}
}

func TestBookmarkInUseAndAdmin(t *testing.T) {
	db, tbl, r, bm := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY")
	ctx := context.Background()
	_, err := xla.Open(ctx, ttxtest.DSN(t), bm)
	if tt.Code(err) != tt.ErrXlaBookmarkInUse {
		t.Errorf("second open: %v", err)
	}
	if _, err := xla.Open(ctx, ttxtest.DSN(t), strings.Repeat("x", 40)); err == nil {
		t.Error("long bookmark name accepted")
	}
	if ok, err := r.Subscribed(ctx, owner, tbl); err != nil || !ok {
		t.Errorf("subscribed = %v %v", ok, err)
	}
	subs, err := admin.Subscriptions(ctx, db, bm)
	if err != nil || len(subs) != 1 || subs[0].Table != tbl {
		t.Errorf("subscriptions = %v %v", subs, err)
	}
	names, err := admin.Bookmarks(ctx, db)
	if err != nil || !slices.Contains(names, bm) {
		t.Errorf("bookmarks = %v %v (want %s)", names, err, bm)
	}
	if err := r.Unsubscribe(ctx, owner, tbl); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.Subscribed(ctx, owner, tbl); ok {
		t.Error("still subscribed")
	}
	// a second bookmark created through SQL, then deleted
	bm2 := bm + "b"
	if err := admin.CreateBookmark(ctx, db, bm2, false); err != nil {
		t.Fatal(err)
	}
	if err := admin.Subscribe(ctx, db, owner+"."+tbl, bm2); err != nil {
		t.Fatal(err)
	}
	if err := admin.UnsubscribeAll(ctx, db, owner, tbl); err != nil {
		t.Fatal(err)
	}
	if err := admin.DeleteBookmark(ctx, db, bm2); err != nil {
		t.Fatal(err)
	}
	names, _ = admin.Bookmarks(ctx, db)
	if slices.Contains(names, bm2) {
		t.Errorf("bookmark %s not deleted: %v", bm2, names)
	}
}

func TestNextHonoursContext(t *testing.T) {
	_, _, r, _ := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY")
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	var err error
	for err == nil { // DDL records of other tables reach every bookmark; keep reading
		_, err = r.Next(ctx, xla.Wait(time.Minute))
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Next ignored the context for %v", el)
	}
}

func BenchmarkReadInserts(b *testing.B) {
	db := ttxtest.DB(b)
	tbl := ttxtest.Table(b, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR2(40), AMT NUMBER(12,2), TS TIMESTAMP")
	bm := strings.ToLower(ttxtest.Name("bmb"))
	ctx := context.Background()
	r, err := xla.Open(ctx, ttxtest.DSN(b), bm)
	if err != nil {
		b.Fatal(err)
	}
	defer admin.DeleteBookmark(ctx, db, bm)
	defer r.Close()
	r.Subscribe(ctx, owner, tbl)
	conn, _ := db.Conn(ctx)
	defer conn.Close()
	b.ResetTimer()
	total := 0
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		rows := make([][]any, 10_000)
		for i := range rows {
			rows[i] = []any{n*10_000 + i, "name", 1.25, time.Now()}
		}
		if err := ttsqlBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?, ?, ?, ?)", rows); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		got := 0
		for got < 10_000 {
			recs, err := r.Next(ctx, xla.Wait(time.Second))
			if err != nil {
				b.Fatal(err)
			}
			got += len(recs)
		}
		r.Ack()
		total += got
	}
	b.ReportMetric(float64(total)/b.Elapsed().Seconds(), "records/s")
}
