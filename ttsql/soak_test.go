//go:build soak

// Soak test: a mixed workload for a long time while process memory (RSS, which includes C
// allocations the Go heap does not show), goroutines and pool size are sampled. Leaks of
// statement handles, bind buffers or LOB buffers show up as RSS growth.
//
//	TIMESTEN_DSN=... TIMESTEN_SOAK=30m go test -tags soak -run Soak -timeout 0 -v ./ttsql/
package ttsql_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chuongnx/go-timesten/ttsql"
	"github.com/chuongnx/go-timesten/ttxtest"
)

// rssBytes returns the private resident memory of the process. On Linux that is RssAnon: plain RSS
// also counts the TimesTen shared memory segment a direct connection maps, which grows with the
// data, not with leaks.
func rssBytes() int64 {
	if b, err := os.ReadFile("/proc/self/status"); err == nil { // Linux
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "RssAnon:"); ok {
				kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
				return kb * 1024
			}
		}
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output() // macOS, AIX
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return kb * 1024
}

func TestSoak(t *testing.T) {
	dur := 2 * time.Minute
	if v := os.Getenv("TIMESTEN_SOAK"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		dur = d
	}
	db := ttxtest.DB(t)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(dur / 4) // connections are also recycled during the run
	tbl := ttxtest.Table(t, db, "ID TT_BIGINT NOT NULL PRIMARY KEY, NAME VARCHAR2(40), AMT NUMBER(12,2), TS TIMESTAMP, C CLOB, B BLOB")
	pkg := ttxtest.Name("PKG")
	ttxtest.Exec(t, db, `CREATE OR REPLACE PACKAGE `+pkg+` AS
		PROCEDURE stats(p_min IN NUMBER, o_n OUT NUMBER, o_sum OUT NUMBER, o_cur OUT SYS_REFCURSOR);
	END;`, `CREATE OR REPLACE PACKAGE BODY `+pkg+` AS
		PROCEDURE stats(p_min IN NUMBER, o_n OUT NUMBER, o_sum OUT NUMBER, o_cur OUT SYS_REFCURSOR) IS
		BEGIN
			SELECT COUNT(*), NVL(SUM(AMT), 0) INTO o_n, o_sum FROM `+tbl+` WHERE ID >= p_min;
			OPEN o_cur FOR SELECT ID, NAME, AMT FROM `+tbl+` WHERE ID >= p_min AND ROWNUM <= 50;
		END;
	END;`)
	t.Cleanup(func() { db.Exec("DROP PACKAGE " + pkg) })

	var nextID, ops, errs atomic.Int64
	// fixed data set read by the query workloads (IDs 1..1000); the others insert above it
	seed := make([][]any, 1000)
	for i := range seed {
		seed[i] = []any{int64(i + 1), fmt.Sprint("seed", i), float64(i) / 4, time.Now()}
	}
	{
		conn, _ := db.Conn(context.Background())
		if _, err := ttsql.ExecBatch(context.Background(), conn, "INSERT INTO "+tbl+" (ID, NAME, AMT, TS) VALUES (?, ?, ?, ?)", seed); err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	nextID.Store(1_000_000)
	clob := strings.Repeat("Dữ liệu ", 10000) // 90 KB: SQLGetData path
	blob := bytes.Repeat([]byte{0xAB}, 50000)
	ctx, stop := context.WithTimeout(context.Background(), dur)
	defer stop()

	work := []func(c context.Context) error{
		func(c context.Context) error { // single-row DML; the table size stays constant
			id := nextID.Add(1)
			if _, err := db.ExecContext(c, "INSERT INTO "+tbl+" (ID, NAME, AMT, TS) VALUES (?, ?, ?, ?)", id, fmt.Sprint("n", id), float64(id%1000)/4, time.Now()); err != nil {
				return err
			}
			if _, err := db.ExecContext(c, "UPDATE "+tbl+" SET AMT = AMT + 1 WHERE ID = ?", id); err != nil {
				return err
			}
			_, err := db.ExecContext(c, "DELETE FROM "+tbl+" WHERE ID = ?", id)
			return err
		},
		func(c context.Context) error { // batch + delete
			conn, err := db.Conn(c)
			if err != nil {
				return err
			}
			defer conn.Close()
			base := nextID.Add(200)
			rows := make([][]any, 200)
			for i := range rows {
				rows[i] = []any{base - int64(i), "batch", 1.25, time.Now()}
			}
			if _, err := ttsql.ExecBatch(c, conn, "INSERT INTO "+tbl+" (ID, NAME, AMT, TS) VALUES (?, ?, ?, ?)", rows); err != nil {
				return err
			}
			_, err = conn.ExecContext(c, "DELETE FROM "+tbl+" WHERE ID > ? AND ID <= ?", base-200, base)
			return err
		},
		func(c context.Context) error { // LOB write + read
			id := nextID.Add(1)
			if _, err := db.ExecContext(c, "INSERT INTO "+tbl+" (ID, NAME, C, B) VALUES (?, 'lob', ?, ?)", id, clob, blob); err != nil {
				return err
			}
			var cs string
			var bs []byte
			if err := db.QueryRowContext(c, "SELECT C, B FROM "+tbl+" WHERE ID = ?", id).Scan(&cs, &bs); err != nil {
				return err
			}
			if len(cs) != len(clob) || len(bs) != len(blob) {
				return fmt.Errorf("LOB sizes %d %d", len(cs), len(bs))
			}
			_, err := db.ExecContext(c, "DELETE FROM "+tbl+" WHERE ID = ?", id)
			return err
		},
		func(c context.Context) error { // query many rows
			rows, err := db.QueryContext(c, "SELECT ID, NAME, AMT, TS FROM "+tbl+" WHERE ID <= 1000")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				var name sql.NullString
				var amt sql.NullString
				var ts sql.NullTime
				if err := rows.Scan(&id, &name, &amt, &ts); err != nil {
					return err
				}
			}
			return rows.Err()
		},
		func(c context.Context) error { // package call with OUT values and a cursor
			conn, err := db.Conn(c)
			if err != nil {
				return err
			}
			defer conn.Close()
			var n int64
			var sum float64
			var cur ttsql.Cursor
			if _, err := conn.ExecContext(c, "BEGIN "+pkg+".stats(?, ?, ?, ?); END;", 900, sql.Out{Dest: &n}, sql.Out{Dest: &sum}, sql.Out{Dest: &cur}); err != nil {
				return err
			}
			defer cur.Close()
			for cur.Next() {
			}
			return cur.Err()
		},
		func(c context.Context) error { // transaction rolled back
			tx, err := db.BeginTx(c, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			_, err = tx.ExecContext(c, "UPDATE "+tbl+" SET AMT = 0 WHERE ID <= 100")
			return err
		},
		func(c context.Context) error { // a query cancelled half-way
			cc, cancel := context.WithTimeout(c, time.Millisecond)
			defer cancel()
			db.ExecContext(cc, "SELECT COUNT(*) FROM SYS.COLUMNS a, SYS.COLUMNS b")
			return nil
		},
	}

	// TIMESTEN_SOAK_OPS=0,2 runs only those workloads (to find which one a memory trend comes from)
	if v := os.Getenv("TIMESTEN_SOAK_OPS"); v != "" {
		var sel []func(context.Context) error
		for _, f := range strings.Split(v, ",") {
			i, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || i < 0 || i >= len(work) {
				t.Fatalf("TIMESTEN_SOAK_OPS: bad index %q", f)
			}
			sel = append(sel, work[i])
		}
		work = sel
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; ctx.Err() == nil; i++ {
				if err := work[i%len(work)](ctx); err != nil && ctx.Err() == nil {
					if errs.Add(1) <= 5 {
						t.Errorf("worker %d op %d: %v", w, i%len(work), err)
					}
				}
				ops.Add(1)
			}
		}(w)
	}

	type sample struct {
		at                time.Duration
		rss, heap         int64
		goroutines, conns int
	}
	var samples []sample
	start := time.Now()
	tick := time.NewTicker(max(dur/20, time.Second))
	defer tick.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			s := sample{time.Since(start).Round(time.Second), rssBytes(), int64(ms.HeapAlloc), runtime.NumGoroutine(), db.Stats().OpenConnections}
			samples = append(samples, s)
			t.Logf("%6v ops=%-8d private=%5.1fMB heap=%5.1fMB goroutines=%d conns=%d", s.at, ops.Load(),
				float64(s.rss)/1e6, float64(s.heap)/1e6, s.goroutines, s.conns)
		}
	}
	wg.Wait()
	if errs.Load() > 0 {
		t.Errorf("%d operations failed", errs.Load())
	}
	if len(samples) < 8 {
		t.Fatalf("only %d samples", len(samples))
	}
	// compare after warm-up (first quarter) with the end: RSS must level off
	base := samples[len(samples)/4]
	last := samples[len(samples)-1]
	growth := last.rss - base.rss
	t.Logf("%d ops in %v; RSS %0.1fMB → %0.1fMB (%+0.1fMB) after warm-up", ops.Load(), dur,
		float64(base.rss)/1e6, float64(last.rss)/1e6, float64(growth)/1e6)
	if base.rss > 0 && growth > max(base.rss/4, 32<<20) {
		t.Errorf("RSS grew by %0.1fMB after warm-up: possible leak", float64(growth)/1e6)
	}
	if last.goroutines > base.goroutines+10 {
		t.Errorf("goroutines %d → %d", base.goroutines, last.goroutines)
	}
}
