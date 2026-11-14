// xla-tail prints committed changes of TimesTen tables as JSON lines.
//
//	xla-tail -dsn "DSN=sampledb;UID=xlauser;PWD=secret" -bookmark tail1 APP.ORDERS APP.ITEMS
//
// Records are acknowledged after they are written to stdout. Use -delete to remove the bookmark on
// exit (otherwise it keeps holding transaction log files until the next run).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chuongnx/go-timesten/xla"
)

type jsonRecord struct {
	Op      string         `json:"op"`
	Table   string         `json:"table"`
	Txn     uint64         `json:"txn"`
	First   bool           `json:"first,omitempty"`
	Commit  bool           `json:"commit,omitempty"`
	Old     map[string]any `json:"old,omitempty"`
	New     map[string]any `json:"new,omitempty"`
	Changed []string       `json:"changed,omitempty"`
}

func row(r *xla.Record, vals []xla.Value) map[string]any {
	if vals == nil || r.Table == nil {
		return nil
	}
	m := make(map[string]any, len(vals))
	for i, v := range vals {
		m[r.Table.Columns[i].Name] = v.Any()
	}
	return m
}

func main() {
	dsn := flag.String("dsn", os.Getenv("TIMESTEN_DSN"), "direct-mode connection string")
	bookmark := flag.String("bookmark", "xla_tail", "XLA bookmark name (max 31 characters)")
	del := flag.Bool("delete", false, "delete the bookmark on exit")
	flag.Parse()
	if *dsn == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: xla-tail -dsn CONNSTR [-bookmark NAME] OWNER.TABLE...")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, err := xla.Open(ctx, *dsn, *bookmark)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if *del {
			if err := r.DeleteBookmark(); err != nil {
				log.Print(err)
			}
		}
		r.Close()
	}()
	want := map[string]bool{}
	for _, t := range flag.Args() {
		if err := r.Subscribe(ctx, "", t); err != nil {
			log.Fatalf("subscribe %s: %v", t, err)
		}
		want[strings.ToUpper(t)] = true
	}
	enc := json.NewEncoder(os.Stdout)
	for ctx.Err() == nil {
		recs, err := r.Next(ctx, xla.Wait(2*time.Second))
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Fatal(err)
		}
		for i := range recs {
			rec := &recs[i]
			if !want[rec.QualifiedName()] {
				continue // DDL of other tables reaches every bookmark
			}
			out := jsonRecord{Op: rec.Op.String(), Table: rec.QualifiedName(), Txn: rec.TxnID,
				First: rec.FirstInTxn(), Commit: rec.EndsTxn(), Old: row(rec, rec.Old), New: row(rec, rec.New)}
			for _, c := range rec.UpdatedColumns {
				out.Changed = append(out.Changed, rec.Table.Columns[c].Name)
			}
			if err := enc.Encode(out); err != nil {
				log.Fatal(err)
			}
		}
		if len(recs) > 0 {
			if err := r.Ack(); err != nil {
				log.Fatal(err)
			}
		}
	}
}
