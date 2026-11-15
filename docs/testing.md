# Testing

The unit tests need no database. The integration tests run against a real TimesTen database:

- Integration tests: `-tags integration`.
- Soak test: `-tags soak`.
- Fault-injection tests, which restart the daemon: `-tags faults`.

Contents:

- [Dev container](#dev-container)
- [Make targets](#make-targets)
- [Running on a host with TimesTen](#running-on-a-host-with-timesten)
- [From a client-only host (macOS)](#from-a-client-only-host-macos)
- [Testing on AIX](#testing-on-aix)
- [Environment variables](#environment-variables)
- [Integration tests by area](#integration-tests-by-area)
- [TimesTen behaviour found by the tests](#timesten-behaviour-found-by-the-tests)

## Dev container

`dev/` builds a container with TimesTen, Go and gcc. It runs its own instance, with database `gotest`
and user `gotest` / `gotest`. TimesTen cannot be redistributed, so you provide the base image
(`TT_IMAGE`); see [CONTRIBUTING.md](../CONTRIBUTING.md#the-timesten-image).

```bash
export TT_IMAGE=my-timesten:22.1
make dev-up      # build and start; creates the gotest database on first start
make test        # unit tests
make race        # integration tests with -race, cgocheck and glibc heap checking
make dev-down    # stop (the database volume is kept)
```

The source tree is mounted at `/src`, so edits on the host are seen at once. `make dev-shell` opens a
shell in the container.

## Make targets

| Target | What it runs |
|---|---|
| `make test` | unit tests |
| `make itest` | integration tests, direct connection |
| `make race` | integration tests with `-race`, `GODEBUG=cgocheck=1` and glibc heap checking (`MALLOC_CHECK_=3 MALLOC_PERTURB_=165`: a bad free or a use-after-free in the C layer or the TimesTen driver fails every run) |
| `make itest-cs` / `make race-cs` | `ttsql` integration tests over client/server (the container's own server, port 6625) |
| `make netfaults` | client/server network failures through an in-test TCP proxy |
| `make bench` / `make bench-cs` | `ExecBatch` and XLA throughput |
| `make faults` | restarts the container's TimesTen daemon: pool and XLA reader recovery |
| `make soak` | mixed workload for `SOAK=30m`, watching private memory, goroutines and the pool |
| `make lint` | `gofmt` and `go vet` with every build tag |
| `make ci` | `lint`, `race`, `race-cs` and `netfaults` |

`make faults` and `make soak` must not run at the same time, because `faults` restarts the database.

## Running on a host with TimesTen

```bash
export TIMESTEN_HOME=/path/to/instance
make test-host                                   # unit tests
TIMESTEN_DSN="DSN=gotest;UID=gotest;PWD=gotest" make itest-host
```

Or call `go test` directly with the cgo variables from [installation.md](installation.md):

```bash
export TIMESTEN_DSN="DSN=gotest;UID=gotest;PWD=gotest"
go test -tags integration -count=1 ./...
```

Use a **dedicated** test database. Its connection attributes must include
`DatabaseCharacterSet=AL32UTF8` and `ConnectionCharacterSet=AL32UTF8`. Create the user with these
privileges:

```sql
CREATE USER gotest IDENTIFIED BY gotest;
GRANT CREATE SESSION, CREATE TABLE, CREATE PROCEDURE, CREATE SEQUENCE, CREATE VIEW, XLA TO gotest;
GRANT ADMIN TO gotest;   -- for the log-hold and checkpoint tests
```

The tests create tables with unique names and drop them afterwards.

## From a client-only host (macOS)

```bash
export TIMESTEN_DSN="TTC_SERVER=dbhost/6625;TTC_SERVER_DSN=gotest;UID=gotest;PWD=gotest"
go test -tags integration -count=1 ./ttsql/
```

The dev container's server listens on 6625 inside the Docker network. To reach it from the host,
forward the port:

```bash
docker run -d --rm --name tt-cs-forward --network go-ttdb-dev_default -p 127.0.0.1:16625:16625 \
  alpine/socat tcp-listen:16625,fork,reuseaddr tcp-connect:go-ttdb-dev:6625
export TIMESTEN_DSN="TTC_SERVER=localhost/16625;TTC_SERVER_DSN=gotest;UID=gotest;PWD=gotest"
```

## Testing on AIX

Docker is not available on AIX, so run `go test` on an AIX machine with TimesTen installed. AIX on
POWER is **big-endian**, and every test so far has run on little-endian machines, so the XLA decoder
needs checking there in particular. It reads integers and NCHAR UTF-16 in native byte order.

1. **Prepare the machine.** Set up Go, gcc and the cgo variables as described in
   [installation.md](installation.md#aix). Check which TimesTen installation the machine has:

   ```sh
   ls $TIMESTEN_HOME/install/include/tt_xla.h        # present: XLA can be built
   ls $TIMESTEN_HOME/install/lib | grep -E 'libtten|libttdrvmgr|libttclient'
   ```

   With only `libttclient`, add `-tags tt_client` to every command below and test only `ttsql` over
   client/server.

2. **Build and run the unit tests.**

   ```sh
   go build ./... && go vet ./...
   go test -count=1 ./...
   ```

3. **Run the integration tests, direct.** Use a test database as described in
   [Running on a host with TimesTen](#running-on-a-host-with-timesten).

   ```sh
   export TIMESTEN_DSN="DSN=gotest;UID=gotest;PWD=gotest"
   go test -tags integration -count=1 ./...
   GODEBUG=cgocheck=1 go test -race -tags integration -count=1 ./...   # skip if -race is unsupported
   ```

4. **Run the client/server and network-fault tests.**

   ```sh
   export TIMESTEN_DSN="TTC_SERVER=dbhost/6625;TTC_SERVER_DSN=gotest;UID=gotest;PWD=gotest"
   go test -tags integration -count=1 ./ttsql/
   export TIMESTEN_CS_SERVER=dbhost:6625
   export TIMESTEN_CS_CONN="TTC_SERVER_DSN=gotest;UID=gotest;PWD=gotest"
   go test -tags integration -run NetFault -count=1 -v ./ttsql/
   ```

5. **Run the soak and fault tests, on a dedicated instance only.**

   ```sh
   TIMESTEN_SOAK=30m go test -tags soak -run Soak -timeout 0 -v ./ttsql/
   go test -tags "integration faults" -run "Survives|Restart" -count=1 -v ./ttsql/ ./xla/
   ```

   The soak test measures memory with `ps -o rss=` on AIX. That number includes TimesTen shared
   memory on direct connections. The fault tests restart the daemon with `ttDaemonAdmin`, and expect
   the database to be called `gotest`.

Tests to look at closely on AIX:

| Test | Why |
|---|---|
| xla `TestDecodeAllTypes`, `TestHighVolume`, `TestDecodeDecimalLeadingZero` | byte order of BIGINT, BINARY_DOUBLE, NCHAR UTF-16 |
| `TestTypesRoundTrip`, `TestTypeEdgeCases`, `TestExecBatchAllKinds` | ODBC type sizes (`SQLLEN`, TIMESTAMP struct) |
| `TestLongMultibyteText`, `TestClientCLOBWorkarounds` | how the AIX driver returns long data in pieces |
| `TestLocationOption` | time zone data (`ZONEINFO`) |
| `TestContextCancel`, `TestCancelVariants` | `SQLCancel` from another thread |

Please report AIX results, passing or failing, in a GitHub issue. Include `go version`, `go env`,
`oslevel -s` and the TimesTen version.

## Environment variables

| Variable | Used by |
|---|---|
| `TIMESTEN_DSN` | Every integration test: the connection string of the test database. Tests skip when it is unset. |
| `TIMESTEN_CS_SERVER`, `TIMESTEN_CS_CONN` | Network-fault tests: `host:port` of the TimesTen server, and the rest of the client/server connection string |
| `TIMESTEN_SOAK`, `TIMESTEN_SOAK_OPS` | Soak test: duration (e.g. `30m`) and an optional operation limit |

## Integration tests by area

Run one area with `go test -tags integration -run <name> ./ttsql/`:

| Test | Covers |
|---|---|
| `TestTypesRoundTrip`, `TestColumnTypes` | every column type in and out, NULLs, column metadata |
| `TestCRUDPlainSQL` | INSERT / SELECT / UPDATE / DELETE, `RowsAffected`, `sql.ErrNoRows` |
| `TestPackageCRUD` | DML through package procedures, function results, rollback of procedure calls |
| `TestPackageOutputTypes` | an OUT parameter of each type, NULL OUT values, `RAISE_APPLICATION_ERROR` |
| `TestPackageInOut` | IN OUT parameters |
| `TestPackageCursors` | REF CURSOR from a function, several cursors from one call, empty cursor |
| `TestDecimalLeadingZero` (ttsql), `TestDecodeDecimalLeadingZero` (xla) | NUMBER text form in columns, cursors, OUT parameters and XLA |
| `TestPLSQLOnlyTypes` | BOOLEAN / RECORD / collection: direct binding fails, conversion in a block works |
| `TestProceduresAndRefCursor` | standalone procedure and function, built-in `CALL` returning rows |
| `TestTransactions`, `TestBatchErrorRow`, `TestContextCancel`, `TestPoolConcurrency` | transactions, batches, cancellation, pool |
| `TestTypeEdgeCases`, `TestNCharKeys` | extreme NUMBERs, NaN/Inf, `""` = NULL, 4-byte UTF-8, CHAR semantics, years 1–9999, ROWID |
| `TestLongMultibyteText`, `TestLongInputs`, `TestLOBMultiRowFetch` | text and LOBs over 64 KB with multibyte characters, inputs over 32 KB, LOB rows mixed with plain columns |
| `TestClientCLOBWorkarounds` | the client/server CLOB defect is reported; NCLOB / VARCHAR2 hold the same text correctly |
| `TestFetchBoundaries`, `TestExecBatchAllKinds` | result sizes around the fetch size, batches of every kind (including the LOB limitation) |
| `TestOutLOBAndTruncation`, `TestLocationOption` | CLOB/BLOB OUT, OUT size limit, `go_location` |
| `TestStmtCacheAfterDDL` | cached / prepared statements and batches across ALTER, CREATE INDEX, DROP + CREATE |
| `TestLockTimeoutIsTransient`, `TestDeadlockIsTransient` | real lock conflicts and deadlocks are transient |
| `TestCursorsLeftOpen`, `TestCancelVariants`, `TestConnectErrors` | abandoned cursors, cancellation while fetching / in PL/SQL / in a batch, bad credentials |
| `TestNetFault*` (client/server) | dead idle pool, failure during query / transaction / fetch, server down, server hanging (`TTC_Timeout`) |
| `TestSoak` (`-tags soak`) | long mixed workload; private memory, goroutines and pool stay flat |
| xla: `TestDecodeAllTypes`, `TestNoPrimaryKey`, `TestAlterTableInSync`, `TestAlterTableWhileLagging`, `TestLogHoldFollowsAck`, `TestHighVolume`, `TestLOBColumnsReported`, `TestReaderSurvivesDaemonRestart` (`-tags faults`) | XLA decoding, edge cases and recovery |

`TestLogHoldFollowsAck` depends on checkpoint timing and can be flaky on a heavily loaded machine.

## TimesTen behaviour found by the tests

These were verified against TimesTen 22.1.1.36, and differ from what plain ODBC would lead you to
expect:

- NUMBER without precision and FLOAT(n) are described as `SQL_DOUBLE`, so they are fetched as text
  to keep every digit.
- With SERIALIZABLE isolation, an array fetch can return `SQL_NO_DATA` together with the last rows.
- REF CURSOR binding takes the second statement handle itself as the data pointer.
- `SQLGetData` on long text returns pieces that end on a whole character, so a piece can be shorter
  than the buffer. LOBs report `SQL_NO_TOTAL` instead of a length. Piece lengths are therefore taken
  from the terminator position.
- Every parameter of a PL/SQL block is described as `VARCHAR(4000)`, whatever its PL/SQL type.
- The client driver returns `SQL_SUCCESS` for a long-text piece cut short at a character boundary,
  although more data follows. Pieces are therefore read until `SQL_NO_DATA`.
- `DBMS_LOB` is available in PL/SQL only, not in TimesTen SQL.
- XLA:
  - `ttXlaConvertCharType` rejects NCHAR/NVARCHAR2, so they are decoded from UTF-16 here.
  - NCHAR sizes are in bytes.
  - `ttXlaTimeStampToODBCCType` reports TT_TIMESTAMP fractions in microseconds.
