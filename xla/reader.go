//go:build !tt_client && !darwin

// Package xla reads committed changes from the TimesTen transaction log (XLA).
//
//	r, err := xla.Open(ctx, "DSN=sampledb;UID=xlauser;PWD=secret", "my_bookmark")
//	defer r.Close()
//	err = r.Subscribe(ctx, "APP", "ORDERS")
//	for {
//		recs, err := r.Next(ctx, xla.Batch(1000), xla.Wait(2*time.Second))
//		... process / forward recs ...
//		err = r.Ack() // only once everything returned so far is safely handled
//	}
//
// Requirements (Oracle TimesTen): a direct connection on the database host, a user with the XLA
// privilege, at most 64 bookmarks per database, one reader per bookmark at a time. A bookmark holds
// transaction log files until it is acknowledged; delete bookmarks that are no longer used.
//
// Records are returned in commit order. The read position advances with every Next; Ack persists
// it, so after a crash reading resumes after the last acknowledged record (records read but not
// acknowledged are delivered again). A Reader is safe for concurrent use; calls are serialized.
package xla

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chuongnx/go-timesten/internal/cli"
	"github.com/chuongnx/go-timesten/tt"
)

// OpenMode selects how the bookmark is opened.
type OpenMode int

const (
	CreateOrReuse OpenMode = iota // reuse the bookmark if it exists, otherwise create it
	CreateOnly                    // fail if the bookmark exists
	ReuseOnly                     // fail if the bookmark does not exist
)

type options struct {
	mode       OpenMode
	replicated bool
	loc        *time.Location
}

// Option configures Open.
type Option func(*options)

// WithMode sets how the bookmark is opened (default CreateOrReuse).
func WithMode(m OpenMode) Option { return func(o *options) { o.mode = m } }

// Replicated creates the bookmark as a replicated bookmark (active standby pairs).
func Replicated() Option { return func(o *options) { o.replicated = true } }

// WithLocation sets the zone given to DATE/TIMESTAMP values (default time.Local).
func WithLocation(loc *time.Location) Option { return func(o *options) { o.loc = loc } }

// Reader reads one bookmark.
type Reader struct {
	mu       sync.Mutex
	conn     *cli.Conn
	x        *cli.Xla
	bookmark string
	closed   bool
	tables   map[*cli.XTable]*Table
}

// ErrClosed is returned by methods of a closed Reader.
var ErrClosed = errors.New("xla: reader closed")

// MaxBookmarkLen is the longest bookmark name TimesTen accepts.
const MaxBookmarkLen = 31

// Open connects with connStr (a direct-mode connection string) and opens the bookmark.
func Open(ctx context.Context, connStr, bookmark string, opts ...Option) (*Reader, error) {
	o := options{loc: time.Local}
	for _, f := range opts {
		f(&o)
	}
	if bookmark == "" || len(bookmark) > MaxBookmarkLen {
		return nil, fmt.Errorf("xla: bookmark name must be 1..%d characters, got %q", MaxBookmarkLen, bookmark)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env, err := cli.SharedEnv()
	if err != nil {
		return nil, err
	}
	conn, err := env.Connect(connStr)
	if err != nil {
		return nil, err
	}
	create := cli.XlaCreate
	if o.replicated {
		create = cli.XlaRepl
	}
	var x *cli.Xla
	switch o.mode {
	case ReuseOnly:
		x, err = conn.XlaOpen(bookmark, cli.XlaReuse, o.loc)
	case CreateOnly:
		x, err = conn.XlaOpen(bookmark, create, o.loc)
	default:
		x, err = conn.XlaOpen(bookmark, create, o.loc)
		if err != nil && tt.Code(err) == tt.ErrKeyExists {
			x, err = conn.XlaOpen(bookmark, cli.XlaReuse, o.loc)
		}
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &Reader{conn: conn, x: x, bookmark: bookmark, tables: map[*cli.XTable]*Table{}}, nil
}

// Bookmark returns the bookmark name.
func (r *Reader) Bookmark() string { return r.bookmark }

func splitName(owner, table string) (string, string) {
	if owner == "" {
		if o, t, ok := strings.Cut(table, "."); ok {
			return strings.ToUpper(o), strings.ToUpper(t)
		}
	}
	return strings.ToUpper(owner), strings.ToUpper(table)
}

func (r *Reader) setStatus(owner, table string, status int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, ErrClosed
	}
	owner, table = splitName(owner, table)
	sys, user, err := r.x.TableByName(owner, table)
	if err != nil {
		return 0, err
	}
	return r.x.TableStatus(sys, user, status)
}

// Subscribe starts tracking changes of OWNER.TABLE for this bookmark (persistent until Unsubscribe
// or the bookmark is deleted). owner may be empty when table is "OWNER.TABLE".
func (r *Reader) Subscribe(ctx context.Context, owner, table string) error {
	_, err := r.setStatus(owner, table, 1)
	return err
}

// Unsubscribe stops tracking the table.
func (r *Reader) Unsubscribe(ctx context.Context, owner, table string) error {
	_, err := r.setStatus(owner, table, 0)
	return err
}

// Subscribed reports whether the table is tracked by this bookmark.
func (r *Reader) Subscribed(ctx context.Context, owner, table string) (bool, error) {
	st, err := r.setStatus(owner, table, -1)
	return st == 1, err
}

type nextOptions struct {
	max  int
	wait time.Duration
}

// NextOption configures Next.
type NextOption func(*nextOptions)

// Batch sets the maximum number of records returned by one Next (1..1000, default 1000).
func Batch(n int) NextOption { return func(o *nextOptions) { o.max = n } }

// Wait sets how long Next waits for new records when the log has none (default 0: return at once).
// The wait is checked in steps of one second, so ctx cancellation is noticed within about a second.
func Wait(d time.Duration) NextOption { return func(o *nextOptions) { o.wait = d } }

// Next returns the next committed records (possibly none).
func (r *Reader) Next(ctx context.Context, opts ...NextOption) ([]Record, error) {
	o := nextOptions{max: 1000}
	for _, f := range opts {
		f(&o)
	}
	o.max = min(max(o.max, 1), 1000)
	deadline := time.Now().Add(o.wait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := time.Until(deadline)
		waitSec := 0
		if remaining > 0 {
			waitSec = 1
		}
		recs, err := r.fetch(o.max, waitSec)
		if err != nil || len(recs) > 0 || remaining <= 0 {
			return recs, err
		}
	}
}

func (r *Reader) fetch(maxRecs, waitSec int) ([]Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	raw, err := r.x.Next(maxRecs, waitSec)
	out := make([]Record, len(raw))
	for i := range raw {
		out[i] = r.convert(&raw[i])
	}
	return out, err
}

func (r *Reader) convert(x *cli.XRecord) Record {
	rec := Record{
		Op: Op(x.Type), Flags: Flags(x.Flags), TxnID: x.TranID, ConnID: x.ConnID,
		LSN: LSN{File: x.LSNFile, Offset: x.LSNOffset}, SysTableID: x.SysTableID,
		Owner: x.Owner, Name: x.Name, UpdatedColumns: x.UpdatedCols,
	}
	if x.Table != nil {
		rec.Table = r.table(x.Table)
	}
	rec.Old, rec.New = values(x.Old), values(x.New)
	return rec
}

func (r *Reader) table(xt *cli.XTable) *Table {
	if t, ok := r.tables[xt]; ok {
		return t
	}
	t := &Table{Owner: xt.Owner, Name: xt.Name, SysTableID: xt.SysTableID, Version: xt.Version,
		PrimaryKey: append([]int(nil), xt.PrimaryKey...), index: map[string]int{}}
	for i, c := range xt.Columns {
		t.Columns = append(t.Columns, Column{Name: c.Name, Type: DataType(c.DataType), Size: c.Size,
			Precision: c.Precision, Scale: c.Scale, Nullable: c.Nullable, PrimaryKey: c.PrimaryKey, SysColNum: c.SysColNum})
		t.index[c.Name] = i
	}
	if len(r.tables) > 4096 { // definitions of old table versions are no longer referenced
		clear(r.tables)
	}
	r.tables[xt] = t
	return t
}

func values(xs []cli.XValue) []Value {
	if xs == nil {
		return nil
	}
	out := make([]Value, len(xs))
	for i, x := range xs {
		v := Value{Int: x.Int, Float: x.Float, Text: x.Str, Bytes: x.Bytes, Time: x.Time}
		switch x.Kind {
		case cli.KindNull:
			v.Kind = KindNull
		case cli.KindInt:
			v.Kind = KindInt
		case cli.KindFloat:
			v.Kind = KindFloat
		case cli.KindDecimal:
			v.Kind = KindDecimal
		case cli.KindString:
			v.Kind = KindString
		case cli.KindBytes:
			v.Kind = KindBytes
		case cli.KindTime:
			v.Kind = KindTime
		case cli.KindTimeOfDay:
			v.Kind = KindTimeOfDay
		case cli.KindLOB:
			v.Kind = KindLOB
		}
		out[i] = v
	}
	return out
}

// Ack acknowledges every record returned by Next so far: the bookmark moves past them and the
// transaction log they occupy can be purged. Acknowledging is comparatively expensive — do it per
// batch, not per record.
func (r *Reader) Ack() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	return r.x.Ack()
}

// Position is an opaque read position for Rewind.
type Position struct{ lsn cli.XlaLSN }

// Position returns the current read position.
func (r *Reader) Position() (Position, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Position{}, ErrClosed
	}
	l, err := r.x.GetLSN()
	return Position{lsn: l}, err
}

// Rewind moves the read position back to p (not before the last Ack) so records are read again,
// e.g. to retry after a downstream failure without reconnecting.
func (r *Reader) Rewind(p Position) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	return r.x.SetLSN(p.lsn)
}

// DeleteBookmark deletes the bookmark (and its subscriptions) and closes the reader.
func (r *Reader) DeleteBookmark() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	err := r.x.DeleteBookmark()
	r.mu.Unlock()
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	return err
}

// Close closes the handle and its connection. The bookmark and its position (as of the last Ack)
// persist in the database.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.x.Close()
	if cerr := r.conn.Close(); err == nil {
		err = cerr
	}
	return err
}
