//go:build integration

package ttsql_test

// Client/server network faults. The client connects through a TCP proxy inside the test, which
// cuts connections on demand, refuses new ones, or accepts and never answers.
//
// Needs a TimesTen server reachable from the test:
//
//	TIMESTEN_CS_SERVER=localhost:6625                         # server host:port
//	TIMESTEN_CS_CONN="TTC_SERVER_DSN=gotest;UID=gotest;PWD=gotest"
//	go test -tags integration -run NetFault ./ttsql/

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/chuongnx/go-timesten/tt"
	"github.com/chuongnx/go-timesten/ttxtest"
)

type tcpProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
	mode   string // "forward", "blackhole"
}

func newProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tcpProxy{ln: ln, target: target, mode: "forward"}
	go p.serve()
	t.Cleanup(func() { p.ln.Close(); p.cut() })
	return p
}

func (p *tcpProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *tcpProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		mode := p.mode
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		if mode == "blackhole" {
			continue // accept, never answer
		}
		s, err := net.Dial("tcp", p.target)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, s)
		p.mu.Unlock()
		go func() { io.Copy(s, c); s.Close() }()
		go func() { io.Copy(c, s); c.Close() }()
	}
}

// cut closes every proxied connection (both sides), like a network failure or a server crash.
func (p *tcpProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func (p *tcpProxy) setMode(m string) { p.mu.Lock(); p.mode = m; p.mu.Unlock() }

func netFaultDB(t *testing.T, extra string) (*sql.DB, *tcpProxy) {
	t.Helper()
	server, conn := os.Getenv("TIMESTEN_CS_SERVER"), os.Getenv("TIMESTEN_CS_CONN")
	if server == "" || conn == "" {
		t.Skip("TIMESTEN_CS_SERVER / TIMESTEN_CS_CONN not set")
	}
	p := newProxy(t, server)
	db, err := sql.Open("timesten", fmt.Sprintf("TTC_SERVER=127.0.0.1/%d;%s%s", p.port(), conn, extra))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping through proxy: %v", err)
	}
	return db, p
}

// recovers runs a query until it succeeds, returning the errors seen on the way.
func recovers(t *testing.T, db *sql.DB, within time.Duration) []error {
	t.Helper()
	var seen []error
	deadline := time.Now().Add(within)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var one int
		err := db.QueryRowContext(ctx, "SELECT 1 FROM DUAL").Scan(&one)
		cancel()
		if err == nil {
			return seen
		}
		seen = append(seen, err)
		if time.Now().After(deadline) {
			t.Fatalf("pool did not recover within %v: %v", within, seen)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func lostOrNil(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil && !tt.IsConnectionLost(err) {
		t.Errorf("%s: error not classified as connection lost: %v (code %d)", what, err, tt.Code(err))
	}
}

func TestNetFaultIdlePool(t *testing.T) {
	db, p := netFaultDB(t, "")
	db.SetMaxIdleConns(4)
	var held []*sql.Conn
	for i := 0; i < 4; i++ { // open four connections, then return them to the pool idle
		c, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Close()
	}
	if s := db.Stats(); s.Idle < 4 {
		t.Fatalf("idle connections = %d", s.Idle)
	}
	p.cut()
	seen := recovers(t, db, 60*time.Second)
	for _, err := range seen {
		lostOrNil(t, "first use after cut", err)
	}
	// the first query on a dead connection fails; the pool then checks the other idle connections
	// before reuse and replaces them silently
	if len(seen) > 1 {
		t.Errorf("application saw %d errors for one network failure, want at most 1", len(seen))
	}
	t.Logf("recovered after %d failed attempts", len(seen))
	// all pooled connections are replaced
	for i := 0; i < 8; i++ {
		if _, err := db.Exec("SELECT 1 FROM DUAL"); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	}
}

func TestNetFaultDuringQuery(t *testing.T) {
	db, p := netFaultDB(t, "")
	errc := make(chan error, 1)
	go func() {
		_, err := db.Exec("SELECT COUNT(*) FROM SYS.COLUMNS a, SYS.COLUMNS b, SYS.COLUMNS c")
		errc <- err
	}()
	time.Sleep(300 * time.Millisecond)
	p.cut()
	select {
	case err := <-errc:
		if err == nil {
			t.Skip("query finished before the cut")
		}
		lostOrNil(t, "running query", err)
		t.Logf("running query: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("query did not return after the connection was cut")
	}
	recovers(t, db, 60*time.Second)
}

func TestNetFaultInTransaction(t *testing.T) {
	db, p := netFaultDB(t, "")
	direct := ttxtest.DB(t) // control connection, not through the proxy
	tbl := ttxtest.Table(t, direct, "ID TT_INTEGER NOT NULL PRIMARY KEY")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO "+tbl+" VALUES (1)", nil...); err != nil {
		t.Fatal(err)
	}
	p.cut()
	err = tx.Commit()
	if err == nil {
		t.Fatal("commit succeeded on a cut connection")
	}
	lostOrNil(t, "commit", err)
	recovers(t, db, 60*time.Second)
	// the server rolled the transaction back
	var n int
	if err := direct.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows after failed commit = %d %v", n, err)
	}
}

func TestNetFaultWhileFetching(t *testing.T) {
	db, p := netFaultDB(t, ";go_fetch_rows=16")
	rows, err := db.Query("SELECT a.COLNAME FROM SYS.COLUMNS a, SYS.COLUMNS b")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		if n++; n == 100 {
			p.cut()
		}
	}
	err = rows.Err()
	rows.Close()
	if err == nil {
		t.Fatalf("read %d rows after the cut without error", n)
	}
	lostOrNil(t, "fetch", err)
	t.Logf("fetch stopped after %d rows: %v", n, err)
	recovers(t, db, 60*time.Second)
}

func TestNetFaultServerDown(t *testing.T) {
	db, p := netFaultDB(t, "")
	db.SetMaxIdleConns(0) // every query needs a new connection
	p.ln.Close()          // nothing listens: connection refused
	p.cut()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := db.PingContext(ctx)
	if err == nil {
		t.Fatal("ping succeeded with the server down")
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("connection refused took %v", d)
	}
	t.Logf("server down: %v after %v", err, time.Since(start).Round(time.Millisecond))
}

// A server that accepts the TCP connection but never answers: TTC_Timeout bounds the wait.
func TestNetFaultServerHangs(t *testing.T) {
	db, p := netFaultDB(t, ";TTC_Timeout=5")
	db.SetMaxIdleConns(0)
	p.setMode("blackhole")
	start := time.Now()
	errc := make(chan error, 1)
	go func() { errc <- db.Ping() }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("ping succeeded against a hanging server")
		}
		d := time.Since(start)
		t.Logf("hanging server: %v after %v", err, d.Round(time.Millisecond))
		if d > 30*time.Second {
			t.Errorf("TTC_Timeout=5 not honoured: %v", d)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("connect did not time out within 90s")
	}
	p.setMode("forward")
	recovers(t, db, 60*time.Second)
}
