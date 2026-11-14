// Package ttsql is a database/sql driver for Oracle TimesTen (ODBC/CLI over cgo).
//
//	import _ "github.com/chuongnx/go-timesten/ttsql"
//	db, err := sql.Open("timesten", "DSN=sampledb;UID=app;PWD=secret")
//
// The data source name is a TimesTen connection string. Keys starting with "go_" configure the
// driver and are removed before the string is passed to TimesTen:
//
//	go_location=Asia/Ho_Chi_Minh  time zone for DATE/TIMESTAMP values (default: Local)
//	go_stmt_cache=32              prepared statements cached per connection for Exec (0 = off)
//	go_fetch_rows=256             rows fetched per driver round trip
//	go_batch_rows=1024            rows per array execution in ExecBatch
//
// Pooling is provided by database/sql (SetMaxOpenConns, SetMaxIdleConns, SetConnMaxLifetime,
// SetConnMaxIdleTime); a connection whose TimesTen error means "connection lost" is discarded.
//
// Placeholders are positional ("?" or ":name" — bound by position). OUT and IN OUT parameters of
// PL/SQL blocks and procedures use sql.Out; a REF CURSOR OUT parameter is received in a *Cursor.
// TimesTen-specific operations (ExecBatch, DurableCommit) are reached through sql.Conn.Raw or the
// helper functions of this package.
package ttsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chuongnx/go-timesten/internal/cli"
)

// DriverName is the name registered with database/sql.
const DriverName = "timesten"

func init() { sql.Register(DriverName, &Driver{}) }

// Driver implements driver.Driver and driver.DriverContext.
type Driver struct{}

// Open opens a new connection (database/sql prefers OpenConnector).
func (d *Driver) Open(dsn string) (driver.Conn, error) {
	c, err := d.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

// OpenConnector parses the DSN once for all connections of a pool.
func (d *Driver) OpenConnector(dsn string) (driver.Connector, error) {
	cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return NewConnector(*cfg)
}

// Config configures connections.
type Config struct {
	ConnString    string         // TimesTen connection string passed to SQLDriverConnect
	Location      *time.Location // zone of DATE/TIMESTAMP values; default time.Local
	StmtCacheSize int            // prepared statements cached per connection for Exec; 0 disables
	FetchRows     int            // rows per fetch round trip
	BatchRows     int            // rows per array execution in ExecBatch
	InitSQL       []string       // statements run on every new connection (e.g. ALTER SESSION ...)
}

// DefaultConfig returns the defaults for a connection string.
func DefaultConfig(connString string) Config {
	return Config{ConnString: connString, Location: time.Local, StmtCacheSize: 32, FetchRows: 256, BatchRows: 1024}
}

// ParseDSN splits driver options (go_*) from the TimesTen connection string.
func ParseDSN(dsn string) (*Config, error) {
	cfg := DefaultConfig("")
	var keep []string
	for _, part := range splitConnString(dsn) {
		k, v, _ := strings.Cut(part, "=")
		key := strings.ToLower(strings.TrimSpace(k))
		if !strings.HasPrefix(key, "go_") {
			if strings.TrimSpace(part) != "" {
				keep = append(keep, part)
			}
			continue
		}
		v = strings.TrimSpace(v)
		switch key {
		case "go_location":
			loc, err := time.LoadLocation(v)
			if err != nil {
				return nil, fmt.Errorf("ttsql: go_location: %w", err)
			}
			cfg.Location = loc
		case "go_stmt_cache", "go_fetch_rows", "go_batch_rows":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("ttsql: %s must be a non-negative integer, got %q", key, v)
			}
			switch key {
			case "go_stmt_cache":
				cfg.StmtCacheSize = n
			case "go_fetch_rows":
				cfg.FetchRows = max(n, 1)
			default:
				cfg.BatchRows = max(n, 1)
			}
		default:
			return nil, fmt.Errorf("ttsql: unknown driver option %q", k)
		}
	}
	cfg.ConnString = strings.Join(keep, ";")
	if cfg.ConnString == "" {
		return nil, fmt.Errorf("ttsql: empty connection string")
	}
	return &cfg, nil
}

// splitConnString splits on ';' outside {braces} (ODBC quoting for values such as passwords).
func splitConnString(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// Connector creates connections with a fixed Config; use with sql.OpenDB.
type Connector struct {
	cfg Config
	// lostAt is when a connection of this connector last saw a connection-level failure (unix ns).
	// Connections created before it are checked before reuse (see Conn.ResetSession), so a network
	// failure costs the application one error instead of one per pooled connection.
	lostAt atomic.Int64
}

// NewConnector validates cfg and fills defaults.
func NewConnector(cfg Config) (*Connector, error) {
	if strings.TrimSpace(cfg.ConnString) == "" {
		return nil, fmt.Errorf("ttsql: empty connection string")
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.FetchRows <= 0 {
		cfg.FetchRows = 256
	}
	if cfg.BatchRows <= 0 {
		cfg.BatchRows = 1024
	}
	return &Connector{cfg: cfg}, nil
}

// Driver returns the driver.
func (c *Connector) Driver() driver.Driver { return &Driver{} }

// Connect opens one connection. SQLDriverConnect cannot be interrupted; if ctx ends first the
// connection is closed as soon as it is established.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	env, err := cli.SharedEnv()
	if err != nil {
		return nil, err
	}
	type result struct {
		cc  *cli.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		cc, err := env.Connect(c.cfg.ConnString)
		ch <- result{cc, err}
	}()
	var r result
	select {
	case r = <-ch:
	case <-ctx.Done():
		go func() {
			if late := <-ch; late.cc != nil {
				late.cc.Close()
			}
		}()
		return nil, ctx.Err()
	}
	if r.err != nil {
		return nil, r.err
	}
	conn := newConn(r.cc, &c.cfg)
	conn.conr, conn.created = c, time.Now().UnixNano()
	for _, q := range c.cfg.InitSQL {
		if _, err := conn.ExecContext(ctx, q, nil); err != nil {
			conn.Close()
			return nil, fmt.Errorf("ttsql: init statement %q: %w", q, err)
		}
	}
	return conn, nil
}
