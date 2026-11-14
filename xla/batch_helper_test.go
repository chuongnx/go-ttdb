//go:build integration

package xla_test

import (
	"context"
	"database/sql"

	"github.com/chuongnx/go-timesten/ttsql"
)

func ttsqlBatch(ctx context.Context, conn *sql.Conn, q string, rows [][]any) error {
	_, err := ttsql.ExecBatch(ctx, conn, q, rows)
	return err
}
