// Package ttxtest helps writing integration tests against a real TimesTen database.
//
// Tests are skipped unless TIMESTEN_DSN is set, e.g.
//
//	TIMESTEN_DSN="DSN=gotest;UID=gotest;PWD=gotest" go test -tags integration ./...
package ttxtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/chuongnx/go-ttdb/ttsql"
)

// DSN returns the test connection string or skips the test.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("TIMESTEN_DSN")
	if dsn == "" {
		t.Skip("TIMESTEN_DSN not set")
	}
	return dsn
}

// DB opens a pool on the test database, closed when the test ends.
func DB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("timesten", DSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return db
}

var seq atomic.Int64

// Name returns a unique upper-case object name with the prefix.
func Name(prefix string) string {
	return strings.ToUpper(fmt.Sprintf("%s_%d_%d", prefix, os.Getpid()%100000, seq.Add(1)))
}

// Exec runs statements and fails the test on error.
func Exec(t testing.TB, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// Table creates a table from a column list and drops it when the test ends. It returns the name.
func Table(t testing.TB, db *sql.DB, columns string) string {
	t.Helper()
	name := Name("T")
	Exec(t, db, fmt.Sprintf("CREATE TABLE %s (%s)", name, columns))
	t.Cleanup(func() { db.Exec("DROP TABLE " + name) })
	return name
}
