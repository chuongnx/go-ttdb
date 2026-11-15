//go:build integration && !tt_client && !darwin

package xla_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chuongnx/go-ttdb/tt"
	"github.com/chuongnx/go-ttdb/ttsql"
	"github.com/chuongnx/go-ttdb/ttxtest"
	"github.com/chuongnx/go-ttdb/xla"
	"github.com/chuongnx/go-ttdb/xla/admin"
)

// A table without a primary key: rows are identified by their full before image.
func TestNoPrimaryKey(t *testing.T) {
	db, tbl, r, _ := setup(t, "A TT_INTEGER, B VARCHAR2(10)")
	ttxtest.Exec(t, db,
		"INSERT INTO "+tbl+" VALUES (1, 'x')",
		"INSERT INTO "+tbl+" VALUES (1, 'x')", // duplicate row
		"UPDATE "+tbl+" SET B = 'y' WHERE ROWNUM = 1",
		"DELETE FROM "+tbl)
	recs := collect(t, r, tbl, 5)
	ops := fmt.Sprintf("%v %v %v %v %v", recs[0].Op, recs[1].Op, recs[2].Op, recs[3].Op, recs[4].Op)
	if ops != "INSERT INSERT UPDATE DELETE DELETE" {
		t.Fatalf("ops = %s", ops)
	}
	if len(recs[0].Table.PrimaryKey) != 0 || len(recs[0].Key()) != 0 {
		t.Errorf("primary key %v, Key() = %v", recs[0].Table.PrimaryKey, recs[0].Key())
	}
	up := recs[2]
	if b, _ := up.OldValue("B"); b.Text != "x" {
		t.Errorf("update before image B = %v", b)
	}
	if b, _ := up.NewValue("B"); b.Text != "y" {
		t.Errorf("update after image B = %v", b)
	}
	for _, d := range recs[3:] {
		if d.Old == nil || d.New != nil {
			t.Errorf("delete images: old=%v new=%v", d.Old, d.New)
		}
	}
}

// ALTER TABLE on a subscribed table while the reader keeps up: the DDL record arrives in order,
// names its table, and later rows are decoded with the new definition.
func TestAlterTableInSync(t *testing.T) {
	db, tbl, r, _ := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY, A VARCHAR2(10)")
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" VALUES (1, 'a')")
	if v, _ := collect(t, r, tbl, 1)[0].NewValue("A"); v.Text != "a" {
		t.Fatalf("row 1 A = %v", v)
	}
	ttxtest.Exec(t, db, "ALTER TABLE "+tbl+" ADD (B NUMBER(5,2), C VARCHAR2(10) DEFAULT 'dflt')",
		"INSERT INTO "+tbl+" (ID, A, B) VALUES (2, 'b', 1.5)")
	recs := collect(t, r, tbl, 2)
	if recs[0].Op != xla.OpAddColumns || recs[0].Owner != owner {
		t.Fatalf("first record %v %s.%s, want ADD COLUMNS on %s.%s", recs[0].Op, recs[0].Owner, recs[0].Name, owner, tbl)
	}
	if v, _ := recs[1].NewValue("B"); v.Text != "1.5" {
		t.Errorf("row 2 B = %v", v)
	}
	if v, _ := recs[1].NewValue("C"); v.Text != "dflt" {
		t.Errorf("row 2 C (default) = %v", v)
	}
	ttxtest.Exec(t, db, "ALTER TABLE "+tbl+" DROP (A)", "INSERT INTO "+tbl+" (ID, B, C) VALUES (3, 2.5, 'c')")
	recs = collect(t, r, tbl, 2)
	if recs[0].Op != xla.OpDropColumns {
		t.Fatalf("record %v, want DROP COLUMNS", recs[0].Op)
	}
	if _, ok := recs[1].NewValue("A"); ok {
		t.Error("row 3 still has the dropped column A")
	}
	if v, _ := recs[1].NewValue("C"); v.Text != "c" {
		t.Errorf("row 3 C = %v", v)
	}
}

// TimesTen cannot describe rows logged before an ALTER TABLE once the table has changed: a reader
// that has not yet read them gets TT8037 (tt.ErrXlaTableDiff) for every such row. Let readers catch
// up (Next returns nothing, then Ack) before altering a subscribed table.
func TestAlterTableWhileLagging(t *testing.T) {
	db, tbl, r, _ := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY, A VARCHAR2(10)")
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" VALUES (1, 'a')", "ALTER TABLE "+tbl+" ADD (B NUMBER)")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	for err == nil {
		var recs []xla.Record
		recs, err = r.Next(ctx, xla.Wait(time.Second))
		for _, rec := range recs {
			if rec.Name == tbl && rec.Op == xla.OpInsert {
				t.Skip("this TimesTen release decodes rows logged before ALTER TABLE")
			}
		}
	}
	if tt.Code(err) != tt.ErrXlaTableDiff {
		t.Fatalf("err = %v, want TT8037", err)
	}
}

// The log hold of a bookmark stays where it was acknowledged until the next Ack, whatever was read.
func TestLogHoldFollowsAck(t *testing.T) {
	db, tbl, r, bm := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY, V VARCHAR2(1000)")
	ctx := context.Background()
	hold := func() admin.Hold {
		t.Helper()
		holds, err := admin.LogHolds(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range holds {
			if h.Type == "XLA" && admin.BookmarkFromDescription(h.Description) == bm {
				return h
			}
		}
		t.Fatalf("no log hold for bookmark %s in %v", bm, holds)
		return admin.Hold{}
	}
	before := hold()
	conn, _ := db.Conn(ctx)
	rows := make([][]any, 2000)
	for i := range rows {
		rows[i] = []any{i, strings.Repeat("v", 1000)}
	}
	if _, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?, ?)", rows); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	collect(t, r, tbl, 2000)
	ttxtest.Exec(t, db, "CALL ttCkpt")
	read := hold()
	if read.LogFile != before.LogFile || read.LogOffset != before.LogOffset {
		t.Errorf("hold moved without Ack: %d/%d → %d/%d", before.LogFile, before.LogOffset, read.LogFile, read.LogOffset)
	}
	if err := r.Ack(); err != nil {
		t.Fatal(err)
	}
	// the hold moves at the next checkpoint; under load that can take a few attempts
	var acked admin.Hold
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		db.Exec("CALL ttCkpt")
		acked = hold()
		if acked.LogFile > before.LogFile || (acked.LogFile == before.LogFile && acked.LogOffset > before.LogOffset) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hold did not advance after Ack: %d/%d → %d/%d", before.LogFile, before.LogOffset, acked.LogFile, acked.LogOffset)
		}
	}
	t.Logf("hold %d/%d → (read) %d/%d → (ack) %d/%d", before.LogFile, before.LogOffset, read.LogFile, read.LogOffset, acked.LogFile, acked.LogOffset)
}

// Many transactions of many rows: every record arrives once, in commit order, with intact
// transaction boundaries.
func TestHighVolume(t *testing.T) {
	db, tbl, r, _ := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY, TXN TT_INTEGER, V VARCHAR2(50)")
	ctx := context.Background()
	const txns, perTxn = 50, 1000
	conn, _ := db.Conn(ctx)
	for x := 0; x < txns; x++ {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		rows := make([][]any, perTxn)
		for i := range rows {
			rows[i] = []any{x*perTxn + i, x, fmt.Sprint("row ", x*perTxn+i)}
		}
		if _, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?, ?, ?)", rows); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	var got []xla.Record
	for len(got) < txns*perTxn {
		recs, err := r.Next(cctx, xla.Batch(5000), xla.Wait(2*time.Second))
		if err != nil {
			t.Fatalf("after %d records: %v", len(got), err)
		}
		for _, rec := range recs {
			if rec.Name == tbl {
				got = append(got, rec)
			}
		}
		if err := r.Ack(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d records in %v (%.0f/s)", len(got), time.Since(start).Round(time.Millisecond), float64(len(got))/time.Since(start).Seconds())
	ends := 0
	for i, rec := range got {
		id, _ := rec.NewValue("ID")
		txn, _ := rec.NewValue("TXN")
		if int(id.Int) != i || int(txn.Int) != i/perTxn {
			t.Fatalf("record %d: id %d txn %d", i, id.Int, txn.Int)
		}
		if rec.FirstInTxn() != (i%perTxn == 0) {
			t.Fatalf("record %d: FirstInTxn = %v", i, rec.FirstInTxn())
		}
		if rec.EndsTxn() {
			ends++
			if i%perTxn != perTxn-1 {
				t.Fatalf("record %d ends a transaction", i)
			}
		}
	}
	if ends != txns {
		t.Errorf("transaction ends = %d, want %d", ends, txns)
	}
}

// Every LOB type is reported as KindLOB (XLA does not carry LOB contents); NULL LOBs as NULL.
func TestLOBColumnsReported(t *testing.T) {
	db, tbl, r, _ := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY, C CLOB, N NCLOB, B BLOB, V VARCHAR2(10)")
	if _, err := db.Exec("INSERT INTO "+tbl+" VALUES (1, ?, ?, ?, 'v')", "clob", "nclob", []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" (ID, V) VALUES (2, 'null lobs')", "UPDATE "+tbl+" SET C = 'changed' WHERE ID = 1")
	recs := collect(t, r, tbl, 3)
	for _, col := range []string{"C", "N", "B"} {
		if v, _ := recs[0].NewValue(col); v.Kind != xla.KindLOB {
			t.Errorf("%s kind = %d, want KindLOB", col, v.Kind)
		}
		if v, _ := recs[1].NewValue(col); !v.IsNull() {
			t.Errorf("NULL %s = %v", col, v)
		}
	}
	if v, _ := recs[0].NewValue("V"); v.Text != "v" {
		t.Errorf("V = %v", v)
	}
	if recs[2].Op != xla.OpUpdate {
		t.Errorf("LOB update op = %v", recs[2].Op)
	}
}
