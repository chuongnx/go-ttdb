//go:build faults

// Fault-injection tests: they restart the TimesTen daemon of the local instance, so they run only
// with -tags faults inside the dev container (never against a shared database).
package ttsql_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/chuongnx/go-timesten/tt"
	"github.com/chuongnx/go-timesten/ttxtest"
)

func sh(t *testing.T, cmd string) {
	t.Helper()
	out, err := exec.Command("bash", "-lc", cmd).CombinedOutput()
	t.Logf("$ %s\n%s", cmd, out)
	if err != nil {
		t.Logf("(exit: %v)", err)
	}
}

func TestPoolSurvivesDaemonRestart(t *testing.T) {
	db := ttxtest.DB(t)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY")
	for i := 0; i < 4; i++ { // fill the pool
		if _, err := db.Exec("INSERT INTO "+tbl+" VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}
	sh(t, "ttDaemonAdmin -stop -force; sleep 2; ttDaemonAdmin -start -force; ttAdmin -ramLoad gotest")

	var lost int
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var n int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n)
		cancel()
		if err == nil {
			t.Logf("recovered after %d failed attempts; rows=%d; pool=%+v", lost, n, db.Stats())
			break
		}
		if tt.IsConnectionLost(err) {
			lost++
		}
		t.Logf("attempt failed: %v", err)
		if time.Now().After(deadline) {
			t.Fatalf("pool did not recover: %v", err)
		}
		time.Sleep(time.Second)
	}
	// every pooled connection is usable again
	for i := 0; i < 8; i++ {
		if _, err := db.Exec("SELECT 1 FROM DUAL"); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	}
}
