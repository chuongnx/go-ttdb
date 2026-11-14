// Package admin manages XLA bookmarks and subscriptions through SQL (TimesTen built-in procedures),
// without opening an XLA handle. Every function takes a *sql.DB opened with the ttsql driver
// (direct or client/server); the user needs the XLA privilege.
package admin

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Hold is one transaction log hold (CALL ttLogHolds).
type Hold struct {
	LogFile     int64
	LogOffset   int64
	Type        string // "XLA", "Checkpoint", "Replication", "Backup", ...
	Description string // for XLA holds: the bookmark name (and owner details)
}

// LogHolds returns every transaction log hold of the database.
func LogHolds(ctx context.Context, db *sql.DB) ([]Hold, error) {
	rows, err := db.QueryContext(ctx, "CALL ttLogHolds")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hold
	for rows.Next() {
		var h Hold
		if err := rows.Scan(&h.LogFile, &h.LogOffset, &h.Type, &h.Description); err != nil {
			return nil, err
		}
		h.Type, h.Description = strings.TrimSpace(h.Type), strings.TrimSpace(h.Description)
		out = append(out, h)
	}
	return out, rows.Err()
}

// Subscription is a table tracked by a bookmark (SYS.XLASUBSCRIPTIONS).
type Subscription struct {
	Bookmark string
	Owner    string
	Table    string
}

// Subscriptions lists all table subscriptions, optionally only for one bookmark.
func Subscriptions(ctx context.Context, db *sql.DB, bookmark string) ([]Subscription, error) {
	q := "SELECT BOOKMARK, TBLOWNER, TBLNAME FROM SYS.XLASUBSCRIPTIONS"
	var args []any
	if bookmark != "" {
		q += " WHERE RTRIM(BOOKMARK) = ?"
		args = append(args, bookmark)
	}
	rows, err := db.QueryContext(ctx, q+" ORDER BY 1, 2, 3", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var s Subscription
		if err := rows.Scan(&s.Bookmark, &s.Owner, &s.Table); err != nil {
			return nil, err
		}
		s.Bookmark, s.Owner, s.Table = strings.TrimSpace(s.Bookmark), strings.TrimSpace(s.Owner), strings.TrimSpace(s.Table)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Bookmarks returns the names of all XLA bookmarks (from the XLA log holds).
func Bookmarks(ctx context.Context, db *sql.DB) ([]string, error) {
	holds, err := LogHolds(ctx, db)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, h := range holds {
		if strings.HasPrefix(strings.ToUpper(h.Type), "XLA") {
			out = append(out, BookmarkFromDescription(h.Description))
		}
	}
	return out, nil
}

// BookmarkFromDescription extracts the bookmark name from a ttLogHolds description.
func BookmarkFromDescription(d string) string {
	d = strings.TrimSpace(d)
	if i := strings.IndexAny(d, " \t"); i > 0 {
		return d[:i]
	}
	return d
}

// CreateBookmark creates a bookmark (CALL ttXlaBookmarkCreate).
func CreateBookmark(ctx context.Context, db *sql.DB, bookmark string, replicated bool) error {
	var err error
	if replicated {
		_, err = db.ExecContext(ctx, "CALL ttXlaBookmarkCreate(?, 0x01)", bookmark)
	} else {
		_, err = db.ExecContext(ctx, "CALL ttXlaBookmarkCreate(?)", bookmark)
	}
	return err
}

// DeleteBookmark deletes a bookmark that no connection is using (CALL ttXlaBookmarkDelete),
// releasing its log hold. Its subscriptions are removed with it.
func DeleteBookmark(ctx context.Context, db *sql.DB, bookmark string) error {
	_, err := db.ExecContext(ctx, "CALL ttXlaBookmarkDelete(?)", bookmark)
	return err
}

// Subscribe tracks OWNER.TABLE for the bookmark (CALL ttXlaSubscribe).
func Subscribe(ctx context.Context, db *sql.DB, ownerTable, bookmark string) error {
	_, err := db.ExecContext(ctx, "CALL ttXlaSubscribe(?, ?)", strings.ToUpper(ownerTable), bookmark)
	return err
}

// Unsubscribe stops tracking OWNER.TABLE for the bookmark (CALL ttXlaUnsubscribe).
func Unsubscribe(ctx context.Context, db *sql.DB, ownerTable, bookmark string) error {
	_, err := db.ExecContext(ctx, "CALL ttXlaUnsubscribe(?, ?)", strings.ToUpper(ownerTable), bookmark)
	return err
}

// UnsubscribeAll removes every subscription of a table, e.g. before DROP TABLE.
func UnsubscribeAll(ctx context.Context, db *sql.DB, owner, table string) error {
	subs, err := Subscriptions(ctx, db, "")
	if err != nil {
		return err
	}
	for _, s := range subs {
		if strings.EqualFold(s.Owner, owner) && strings.EqualFold(s.Table, table) {
			if err := Unsubscribe(ctx, db, s.Owner+"."+s.Table, s.Bookmark); err != nil {
				return fmt.Errorf("unsubscribe %s from %s: %w", s.Table, s.Bookmark, err)
			}
		}
	}
	return nil
}
