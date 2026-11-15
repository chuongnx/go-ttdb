//go:build !tt_client && !darwin

package xla_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/chuongnx/go-ttdb/xla"
)

// Follow the changes of one table and acknowledge them batch by batch.
func Example() {
	ctx := context.Background()
	r, err := xla.Open(ctx, "DSN=sampledb;UID=xlauser;PWD=secret", "orders_cdc")
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	if err := r.Subscribe(ctx, "APP", "ORDERS"); err != nil {
		log.Fatal(err)
	}
	for ctx.Err() == nil {
		recs, err := r.Next(ctx, xla.Batch(1000), xla.Wait(2*time.Second))
		if err != nil {
			log.Fatal(err)
		}
		for _, rec := range recs {
			switch {
			case rec.Op == xla.OpUpdate:
				for _, i := range rec.UpdatedColumns {
					col := rec.Table.Columns[i].Name
					fmt.Printf("%s %v: %s %v -> %v\n", rec.QualifiedName(), rec.Key(), col, rec.Old[i], rec.New[i])
				}
			case rec.Op.IsDML():
				fmt.Println(rec.Op, rec.QualifiedName(), rec.Key())
			case rec.Op == xla.OpTruncate:
				fmt.Println("TRUNCATE", rec.QualifiedName())
			}
		}
		// make recs durable downstream first, then move the bookmark past them
		if err := r.Ack(); err != nil {
			log.Fatal(err)
		}
	}
}

// Re-read records after a downstream failure without reconnecting.
func ExampleReader_Rewind() {
	ctx := context.Background()
	r, err := xla.Open(ctx, "DSN=sampledb;UID=xlauser;PWD=secret", "orders_cdc", xla.WithMode(xla.ReuseOnly))
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()

	p, err := r.Position()
	if err != nil {
		log.Fatal(err)
	}
	recs, err := r.Next(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if err := send(recs); err != nil {
		_ = r.Rewind(p) // the next Next returns the same records
		return
	}
	_ = r.Ack()
}

func send([]xla.Record) error { return nil }
