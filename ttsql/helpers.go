package ttsql

import (
	"context"
	"database/sql"
	"fmt"
)

// ExecBatch runs query once per row with ODBC parameter arrays on conn (in its current transaction
// state) and returns the affected row count. See Conn.ExecBatch.
func ExecBatch(ctx context.Context, conn *sql.Conn, query string, rows [][]any) (int64, error) {
	var n int64
	err := conn.Raw(func(dc any) error {
		c, ok := dc.(*Conn)
		if !ok {
			return fmt.Errorf("ttsql: not a TimesTen connection (%T)", dc)
		}
		var err error
		n, err = c.ExecBatch(ctx, query, rows)
		return err
	})
	return n, err
}

// DurableCommit marks the transaction to be committed durably (CALL ttDurableCommit). Call it before
// tx.Commit on databases running with DurableCommits=0.
func DurableCommit(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, "CALL ttDurableCommit")
	return err
}
