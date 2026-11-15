package ttsql_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/chuongnx/go-ttdb/tt"
	"github.com/chuongnx/go-ttdb/ttsql"
)

// Open a pool and run a query. The connection string is a TimesTen ODBC connection string; go_*
// keys configure the driver.
func Example() {
	ctx := context.Background()
	db, err := sql.Open(ttsql.DriverName, "DSN=sampledb;UID=app;PWD=secret;go_location=UTC")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(16)
	db.SetConnMaxIdleTime(5 * time.Minute)

	rows, err := db.QueryContext(ctx, "SELECT id, customer, amount FROM orders WHERE amount > ?", 100)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var customer, amount string // NUMBER(p,s) as exact decimal text
		if err := rows.Scan(&id, &customer, &amount); err != nil {
			log.Fatal(err)
		}
		fmt.Println(id, customer, amount)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
}

// Connect to a TimesTen server over client/server and set options from code.
func ExampleNewConnector() {
	cfg := ttsql.DefaultConfig("TTC_SERVER=dbhost/6625;TTC_SERVER_DSN=sampledb;UID=app;PWD=secret;TTC_Timeout=10")
	cfg.Location = time.UTC
	cfg.InitSQL = []string{"CALL ttLockWait(5)"} // run on every new connection
	conr, err := ttsql.NewConnector(cfg)
	if err != nil {
		log.Fatal(err)
	}
	db := sql.OpenDB(conr)
	defer db.Close()
}

// Insert many rows with one round trip per go_batch_rows rows, all or nothing.
func ExampleExecBatch() {
	ctx := context.Background()
	db, _ := sql.Open(ttsql.DriverName, "DSN=sampledb;UID=app;PWD=secret")
	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	rows := [][]any{
		{1, "alice", "10.50"},
		{2, "bob", "20.00"},
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback()
	n, err := ttsql.ExecBatch(ctx, conn, "INSERT INTO orders (id, customer, amount) VALUES (?, ?, ?)", rows)
	var be *ttsql.BatchError
	if errors.As(err, &be) {
		log.Fatalf("row %d: %v (TimesTen error %d)", be.Row, be.Err, tt.Code(err))
	}
	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}
	fmt.Println(n, "rows inserted")
}

// Call a PL/SQL procedure with OUT and IN OUT parameters and a function returning a value.
func Example_outParameters() {
	ctx := context.Background()
	db, _ := sql.Open(ttsql.DriverName, "DSN=sampledb;UID=app;PWD=secret")

	var customer string
	var amount float64
	var created time.Time
	_, err := db.ExecContext(ctx, "BEGIN order_pkg.get_order(?, ?, ?, ?); END;", 42,
		sql.Out{Dest: &customer}, sql.Out{Dest: &amount}, sql.Out{Dest: &created})
	if err != nil {
		log.Fatal(err)
	}

	counter := int64(41)
	_, err = db.ExecContext(ctx, "BEGIN order_pkg.bump(?); END;", sql.Out{Dest: &counter, In: true})
	if err != nil {
		log.Fatal(err)
	}

	var count int64
	_, err = db.ExecContext(ctx, "BEGIN ? := order_pkg.order_count; END;", sql.Out{Dest: &count})
	if err != nil {
		log.Fatal(err)
	}
}

// Read a REF CURSOR returned by a PL/SQL function. The cursor is read on the connection that
// opened it, so use a *sql.Conn (or *sql.Tx).
func ExampleCursor() {
	ctx := context.Background()
	db, _ := sql.Open(ttsql.DriverName, "DSN=sampledb;UID=app;PWD=secret")
	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	var cur ttsql.Cursor
	if _, err := conn.ExecContext(ctx, "BEGIN ? := order_pkg.orders_of(?); END;", sql.Out{Dest: &cur}, "alice"); err != nil {
		log.Fatal(err)
	}
	defer cur.Close()
	for cur.Next() {
		var id int64
		var amount float64
		if err := cur.Scan(&id, &amount); err != nil {
			log.Fatal(err)
		}
		fmt.Println(id, amount)
	}
	if err := cur.Err(); err != nil {
		log.Fatal(err)
	}
}

// Retry a transaction that failed with a deadlock, a lock timeout or a lost connection.
func Example_retryTransient() {
	ctx := context.Background()
	db, _ := sql.Open(ttsql.DriverName, "DSN=sampledb;UID=app;PWD=secret")

	transfer := func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance - 100 WHERE id = 1"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance + 100 WHERE id = 2"); err != nil {
			return err
		}
		return tx.Commit()
	}
	for attempt := 0; ; attempt++ {
		err := transfer()
		if err == nil || !tt.IsTransient(err) || attempt == 3 {
			if err != nil {
				log.Fatal(err)
			}
			break
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
}
