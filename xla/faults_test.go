//go:build faults && !tt_client && !darwin

// Fault injection: restarts the TimesTen daemon of the local instance, so it runs only with
// -tags faults inside the dev container (never against a shared database).
package xla_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/chuongnx/go-ttdb/tt"
	"github.com/chuongnx/go-ttdb/ttxtest"
	"github.com/chuongnx/go-ttdb/xla"
)

// A reader whose database restarts: Next fails with a connection error; reopening the bookmark
// resumes after the last Ack, so acknowledged records are not repeated and unacknowledged ones are
// not lost.
func TestReaderSurvivesDaemonRestart(t *testing.T) {
	db, tbl, r, bm := setup(t, "ID TT_INTEGER NOT NULL PRIMARY KEY")
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" VALUES (1)", "INSERT INTO "+tbl+" VALUES (2)")
	collect(t, r, tbl, 2)
	if err := r.Ack(); err != nil {
		t.Fatal(err)
	}
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" VALUES (3)", "INSERT INTO "+tbl+" VALUES (4)")
	collect(t, r, tbl, 2) // read, not acknowledged
	ttxtest.Exec(t, db, "CALL ttCkpt")

	out, err := exec.Command("bash", "-lc", "ttDaemonAdmin -stop -force; sleep 2; ttDaemonAdmin -start -force; ttAdmin -ramLoad gotest").CombinedOutput()
	t.Logf("restart: %s %v", out, err)

	ctx := context.Background()
	if _, err := r.Next(ctx, xla.Wait(time.Second)); err == nil || !tt.IsConnectionLost(err) {
		t.Logf("Next after restart: %v (connection-lost: %v)", err, tt.IsConnectionLost(err))
	}
	r.Close()
	// Direct mode: a process keeps the old instance's shared memory attached while any of its
	// connections to it stay open, and new connections fail with TT8521 until then. Close the idle
	// connections of the pool too (they have not noticed the restart yet).
	db.SetMaxIdleConns(0)
	db.SetMaxIdleConns(2)

	var r2 *xla.Reader
	deadline := time.Now().Add(90 * time.Second)
	for {
		r2, err = xla.Open(ctx, ttxtest.DSN(t), bm, xla.WithMode(xla.ReuseOnly))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reopen: %v", err)
		}
		time.Sleep(time.Second)
	}
	defer r2.Close()
	recs := collect(t, r2, tbl, 2)
	var ids []int64
	for _, rec := range recs {
		v, _ := rec.NewValue("ID")
		ids = append(ids, v.Int)
	}
	if len(ids) != 2 || ids[0] != 3 || ids[1] != 4 {
		t.Fatalf("after restart got %v, want [3 4] (unacknowledged records redelivered, acknowledged ones not)", ids)
	}
	if err := r2.Ack(); err != nil {
		t.Fatal(err)
	}
}
