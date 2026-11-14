# Changelog

## Unreleased (0.1.0)

- `ttsql`: database/sql driver over the TimesTen ODBC API (cgo) — connector with DSN options, pool
  validation and session reset, transactions (READ COMMITTED, SERIALIZABLE), statement cache,
  array fetch, all TimesTen data types incl. LOBs, OUT / IN OUT parameters (`sql.Out`), REF CURSOR
  (`Cursor`), `ExecBatch` with parameter arrays and per-row error (`BatchError`), `DurableCommit`,
  context cancellation through `SQLCancel`.
- `xla`: Transaction Log API reader — create/reuse/replicated bookmarks, subscribe/unsubscribe,
  `Next` with batch size and wait, decoded row images for every XLA data type, changed-column list,
  TRUNCATE / DDL records, `Ack`, `Position` / `Rewind`, `DeleteBookmark`.
- `xla/admin`: log holds, bookmarks, subscriptions via SQL built-ins.
- `tt`: error type and classification.
- Dev container, integration tests (race + cgocheck), fault-injection test, benchmarks, examples.
