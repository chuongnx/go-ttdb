// sql shows the database/sql driver: pool settings, queries, a transaction, a batch insert, a
// procedure with OUT parameters and a REF CURSOR.
//
//	go run ./examples/sql -dsn "DSN=sampledb;UID=app;PWD=secret"
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/chuongnx/go-ttdb/ttsql"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("TIMESTEN_DSN"), "connection string")
	flag.Parse()
	ctx := context.Background()

	db, err := sql.Open(ttsql.DriverName, *dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	must := func(_ any, err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	db.ExecContext(ctx, "DROP TABLE demo_orders")
	must(db.ExecContext(ctx, `CREATE TABLE demo_orders (id NUMBER(10) NOT NULL PRIMARY KEY,
		customer VARCHAR2(40), amount NUMBER(12,2), created TIMESTAMP)`))
	defer db.ExecContext(ctx, "DROP TABLE demo_orders")

	// batch insert on one connection
	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	rows := make([][]any, 1000)
	for i := range rows {
		rows[i] = []any{i + 1, fmt.Sprintf("customer-%d", i%10), float64(i) * 1.5, time.Now()}
	}
	n, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO demo_orders VALUES (?, ?, ?, ?)", rows)
	fmt.Println("inserted", n, err)

	// transaction with a durable commit
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	must(tx.ExecContext(ctx, "UPDATE demo_orders SET amount = amount * 2 WHERE customer = ?", "customer-3"))
	must(nil, ttsql.DurableCommit(ctx, tx))
	must(nil, tx.Commit())

	// query
	var total string
	db.QueryRowContext(ctx, "SELECT SUM(amount) FROM demo_orders").Scan(&total)
	fmt.Println("total amount", total)

	// PL/SQL block with OUT parameters and a REF CURSOR (same connection while reading the cursor)
	var count int64
	var cur ttsql.Cursor
	_, err = conn.ExecContext(ctx, `BEGIN
		SELECT COUNT(*) INTO :1 FROM demo_orders WHERE customer = :2;
		OPEN :3 FOR SELECT id, amount FROM demo_orders WHERE customer = :2 AND ROWNUM <= 3 ORDER BY id;
	END;`, sql.Out{Dest: &count}, "customer-3", sql.Out{Dest: &cur})
	if err != nil {
		log.Fatal(err)
	}
	defer cur.Close()
	fmt.Println("customer-3 orders:", count)
	for cur.Next() {
		var id int
		var amount float64
		cur.Scan(&id, &amount)
		fmt.Println("  ", id, amount)
	}
	if cur.Err() != nil {
		log.Fatal(cur.Err())
	}
}
