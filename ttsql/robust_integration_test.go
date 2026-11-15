//go:build integration

package ttsql_test

// Behaviour under conditions production code meets: DDL while statements are cached, lock
// conflicts and deadlocks, cursors left open, cancellation at different points, bad credentials.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chuongnx/go-ttdb/tt"
	"github.com/chuongnx/go-ttdb/ttsql"
	"github.com/chuongnx/go-ttdb/ttxtest"
)

// Statements cached per connection (Exec) and prepared by the caller (db.Prepare) keep working
// after the table they use is altered, or dropped and created again.
func TestStmtCacheAfterDDL(t *testing.T) {
	db := ttxtest.DB(t)
	db.SetMaxOpenConns(1) // one connection: the cached statement is the one reused
	tbl := ttxtest.Name("T")
	t.Cleanup(func() { db.Exec("DROP TABLE " + tbl) })
	ttxtest.Exec(t, db, "CREATE TABLE "+tbl+" (ID TT_INTEGER NOT NULL PRIMARY KEY, V VARCHAR2(10))")
	ins := "INSERT INTO " + tbl + " (ID, V) VALUES (?, ?)"
	sel := "SELECT V FROM " + tbl + " WHERE ID = ?"
	prepared, err := db.Prepare(sel)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	step := func(name string, id int) {
		t.Helper()
		if _, err := db.Exec(ins, id, name); err != nil {
			t.Errorf("%s: cached INSERT: %v", name, err)
			return
		}
		var v string
		if err := prepared.QueryRow(id).Scan(&v); err != nil || v != name {
			t.Errorf("%s: prepared SELECT = %q %v", name, v, err)
		}
	}
	step("before", 1)
	ttxtest.Exec(t, db, "ALTER TABLE "+tbl+" ADD (EXTRA NUMBER)")
	step("added", 2)
	ttxtest.Exec(t, db, "CREATE INDEX "+tbl+"_IX ON "+tbl+" (V)")
	step("indexed", 3)
	ttxtest.Exec(t, db, "DROP TABLE "+tbl, "CREATE TABLE "+tbl+" (ID NUMBER(9) NOT NULL PRIMARY KEY, V VARCHAR2(20), W DATE)")
	step("recreated", 4)
	// ExecBatch reuses the same cached statement
	conn, _ := db.Conn(context.Background())
	if _, err := ttsql.ExecBatch(context.Background(), conn, ins, [][]any{{40, "b1"}, {41, "b2"}}); err != nil {
		t.Errorf("batch before DDL: %v", err)
	}
	conn.Close()
	ttxtest.Exec(t, db, "DROP TABLE "+tbl, "CREATE TABLE "+tbl+" (ID NUMBER(9) NOT NULL PRIMARY KEY, V VARCHAR2(20))")
	conn, _ = db.Conn(context.Background())
	if n, err := ttsql.ExecBatch(context.Background(), conn, ins, [][]any{{42, "b3"}, {43, "b4"}}); err != nil || n != 2 {
		t.Errorf("batch after DROP/CREATE: %d %v", n, err)
	}
	conn.Close()
	// a column type change the cached statement cannot absorb must fail cleanly, not corrupt data
	ttxtest.Exec(t, db, "DROP TABLE "+tbl, "CREATE TABLE "+tbl+" (ID NUMBER(9) NOT NULL PRIMARY KEY, V TIMESTAMP)")
	if _, err := db.Exec(ins, 5, "not a time"); err == nil {
		t.Error("string into TIMESTAMP accepted")
	}
	if _, err := db.Exec(ins, 6, time.Now()); err != nil {
		t.Errorf("cached INSERT with the new type: %v", err)
	}
}

// A row lock held by one transaction makes another fail with a transient error; retrying after the
// holder ends succeeds.
func TestLockTimeoutIsTransient(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, V TT_INTEGER")
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" VALUES (1, 0)")
	ctx := context.Background()
	holder, _ := db.BeginTx(ctx, nil)
	if _, err := holder.Exec("UPDATE "+tbl+" SET V = 1 WHERE ID = 1", nil...); err != nil {
		t.Fatal(err)
	}
	conn, _ := db.Conn(ctx)
	defer conn.Close()
	// lock wait is a connection setting: set it on a dedicated connection, restore it afterwards
	conn.ExecContext(ctx, "CALL ttLockWait(1)")
	defer conn.ExecContext(ctx, "CALL ttLockWait(10)")
	start := time.Now()
	_, err := conn.ExecContext(ctx, "UPDATE "+tbl+" SET V = 2 WHERE ID = 1")
	code := tt.Code(err)
	if err == nil || !tt.IsTransient(err) || (code != tt.ErrLockConflict && code != tt.ErrLockTimeout) {
		t.Fatalf("conflicting update after %v: err = %v (code %d)", time.Since(start), err, code)
	}
	t.Logf("lock conflict after %v: code %d", time.Since(start).Round(time.Millisecond), code)
	holder.Rollback()
	if _, err := conn.ExecContext(ctx, "UPDATE "+tbl+" SET V = 2 WHERE ID = 1"); err != nil {
		t.Fatalf("retry after the holder ended: %v", err)
	}
}

// Two transactions locking rows in opposite order: TimesTen picks a victim (6002) and the other
// completes.
func TestDeadlockIsTransient(t *testing.T) {
	db := ttxtest.DB(t)
	tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, V TT_INTEGER")
	ttxtest.Exec(t, db, "INSERT INTO "+tbl+" VALUES (1, 0)", "INSERT INTO "+tbl+" VALUES (2, 0)")
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	ready := make(chan struct{}, 2)
	proceed := make(chan struct{})
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			first, second := 1+w, 2-w
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				errs[w] = err
				ready <- struct{}{}
				return
			}
			defer tx.Rollback()
			if _, err := tx.Exec(fmt.Sprintf("UPDATE %s SET V = V + 1 WHERE ID = %d", tbl, first)); err != nil {
				errs[w] = err
				ready <- struct{}{}
				return
			}
			ready <- struct{}{}
			<-proceed
			if _, err := tx.Exec(fmt.Sprintf("UPDATE %s SET V = V + 1 WHERE ID = %d", tbl, second)); err != nil {
				errs[w] = err
				return
			}
			errs[w] = tx.Commit()
		}(w)
	}
	<-ready
	<-ready
	close(proceed)
	wg.Wait()
	var victims, ok int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case tt.IsTransient(err):
			victims++
			t.Logf("victim: code %d", tt.Code(err))
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if victims < 1 {
		t.Errorf("no transaction failed: ok=%d victims=%d", ok, victims)
	}
}

// Cursors that are never closed, or outlive their connection, do not crash, leak into the pool or
// exhaust statement handles.
func TestCursorsLeftOpen(t *testing.T) {
	db := ttxtest.DB(t)
	db.SetMaxOpenConns(2)
	ctx := context.Background()
	open := func(conn *sql.Conn) *ttsql.Cursor {
		t.Helper()
		var cur ttsql.Cursor
		if _, err := conn.ExecContext(ctx, "BEGIN OPEN ? FOR SELECT 1 FROM DUAL UNION ALL SELECT 2 FROM DUAL; END;", sql.Out{Dest: &cur}); err != nil {
			t.Fatal(err)
		}
		return &cur
	}
	// the connection is returned to the pool while its cursor is still open
	for i := 0; i < 200; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cur := open(conn)
		cur.Next()
		conn.Close() // back to the pool; the cursor stays open on that connection
	}
	// many cursors opened on one connection and never closed
	conn, _ := db.Conn(ctx)
	var last *ttsql.Cursor
	for i := 0; i < 2000; i++ {
		last = open(conn)
	}
	if !last.Next() {
		t.Fatalf("cursor 2000: %v", last.Err())
	}
	conn.Raw(func(dc any) error { return dc.(*ttsql.Conn).Close() }) // physically close: frees its cursors
	conn.Close()
	if last.Next() {
		t.Error("cursor still readable after its connection closed")
	}
	if err := last.Scan(new(int)); err == nil {
		t.Log("Scan after close returns the last row (no error)")
	}
	last.Close() // closing twice is harmless
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM DUAL").Scan(&n); err != nil {
		t.Fatal(err)
	}
}

// Cancellation while rows are being read, during a batch and inside PL/SQL.
func TestCancelVariants(t *testing.T) {
	db := ttxtest.DB(t)
	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Logf("%s: finished before the deadline", name)
			return
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v, want a context error", name, err)
		}
		var one int
		if err := db.QueryRow("SELECT 1 FROM DUAL").Scan(&one); err != nil {
			t.Fatalf("%s: pool unusable afterwards: %v", name, err)
		}
	}

	t.Run("while reading rows", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rows, err := db.QueryContext(ctx, "SELECT a.COLNAME FROM SYS.COLUMNS a, SYS.COLUMNS b")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			if n++; n == 1000 {
				cancel()
			}
		}
		err = rows.Err()
		rows.Close()
		t.Logf("stopped after %d rows: %v", n, err)
		check("rows", err)
	})

	t.Run("PL/SQL loop", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := db.ExecContext(ctx, "DECLARE x NUMBER := 0; BEGIN WHILE x < 1E10 LOOP x := x + 1; END LOOP; END;")
		t.Logf("after %v: %v", time.Since(start).Round(time.Millisecond), err)
		check("plsql", err)
		if time.Since(start) > 10*time.Second {
			t.Errorf("cancel took %v", time.Since(start))
		}
	})

	t.Run("batch", func(t *testing.T) {
		tbl := ttxtest.Table(t, db, "ID TT_INTEGER NOT NULL PRIMARY KEY, V VARCHAR2(100)")
		rows := make([][]any, 200_000)
		for i := range rows {
			rows[i] = []any{i, strings.Repeat("x", 100)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		conn, _ := db.Conn(context.Background())
		defer conn.Close()
		n, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO "+tbl+" VALUES (?, ?)", rows)
		t.Logf("batch stopped after %d rows: %v", n, err)
		check("batch", err)
		// autocommit: rows applied before the cancel (also part of a chunk) stay committed, n counts them
		var c int64
		db.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&c)
		if c != n {
			t.Errorf("rows in table %d, reported %d", c, n)
		}
	})

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := db.ExecContext(ctx, "SELECT 1 FROM DUAL")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestConnectErrors(t *testing.T) {
	dsn := ttxtest.DSN(t)
	ttxtest.DB(t) // database reachable
	replace := func(key, val string) string {
		parts := strings.Split(dsn, ";")
		for i, p := range parts {
			if k, _, _ := strings.Cut(p, "="); strings.EqualFold(strings.TrimSpace(k), key) {
				parts[i] = key + "=" + val
			}
		}
		return strings.Join(parts, ";")
	}
	for _, c := range []struct{ name, dsn string }{
		{"wrong password", replace("PWD", "definitely-wrong")},
		{"unknown user", replace("UID", "NO_SUCH_USER_X")},
		{"unknown DSN", replace("DSN", "no_such_dsn_x") + ";DSN=no_such_dsn_x"},
	} {
		db, err := sql.Open("timesten", c.dsn)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		err = db.PingContext(ctx)
		cancel()
		db.Close()
		var te *tt.Error
		if err == nil || !errors.As(err, &te) {
			t.Errorf("%s: err = %v, want *tt.Error", c.name, err)
			continue
		}
		if strings.Contains(err.Error(), "definitely-wrong") {
			t.Errorf("%s: the password appears in the error: %v", c.name, err)
		}
		t.Logf("%s: code %d after %v", c.name, te.Code, time.Since(start).Round(time.Millisecond))
	}
}
