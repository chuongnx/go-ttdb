# Changelog

All notable changes to this project are documented in this file. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows
[Semantic Versioning](https://semver.org/) (pre-1.0: minor versions may change the API).

## [Unreleased]

## [0.1.0] - unreleased

First public release.

- Project renamed from go-timesten to **go-ttdb**; module path `github.com/chuongnx/go-ttdb`.
  Package names (`ttsql`, `xla`, `xla/admin`, `tt`, `ttxtest`) and the driver name `"timesten"` are
  unchanged.
- Apache-2.0 license; documentation in `docs/`, Vietnamese README, contributing guide, security
  policy, runnable examples on pkg.go.dev.
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
- NUMBER / FLOAT values read as text (columns, REF CURSOR rows, XLA `KindDecimal`) carry a leading
  zero: `0.25`, `-0.5` instead of TimesTen's `.25`, `-.5`. String OUT parameters are not changed
  (TimesTen describes PL/SQL parameters as VARCHAR).
- Fix: text longer than 64 KB (CLOB, NCLOB, long VARCHAR2) with multibyte UTF-8 characters could
  come back with stray bytes where a character straddled a `SQLGetData` piece.
- Fix: over client/server, long text (VARCHAR2, NCLOB) with multibyte characters was cut off after
  the first piece (the client driver returns `SQL_SUCCESS` early); pieces are read until `SQL_NO_DATA`.
- CLOB values garbled by the TimesTen client driver (client/server, multibyte text over 4 KB) are
  reported with an error wrapping `tt.ErrClientCLOB` instead of being returned corrupted.
  `ttsql.Conn.IsClientServer`.
- Fix: NUMBER values in rows that also contain LOB columns were not normalized (leading zero).
- Statements invalidated by DDL (TT0946) are prepared again and retried once: cached `Exec`
  statements, `db.Prepare` statements and `ExecBatch` survive ALTER / DROP + CREATE.
- After a connection-level failure, pooled connections opened before it are checked before reuse
  and replaced silently (one application error per failure instead of one per pooled connection).
- String / `[]byte` parameters over 32 KB are bound as LOB types, so they reach PL/SQL CLOB/BLOB
  variables (previously ORA-06502).
- A cancelled `ExecBatch` returns a `BatchError` that also wraps `ctx.Err()`.
- XLA: ADD COLUMNS / DROP COLUMNS records carry the table owner and name.
- `tt.ErrCommandInvalid` (946).
- Tests: data-type edge cases, long multibyte text, fetch boundaries, statement cache after DDL,
  lock timeouts and deadlocks, cancellation, client/server network faults (in-test TCP proxy),
  soak test (`-tags soak`), XLA edge cases and daemon restart; make targets `itest-cs`, `race-cs`,
  `netfaults`, `bench-cs`, `soak`.
- Dev container, integration tests (race + cgocheck), fault-injection test, benchmarks, examples.
