# Troubleshooting

- [Build errors](#build-errors)
- [Runtime: loading libraries](#runtime-loading-libraries)
- [Connection errors](#connection-errors)
- [TT0946: SQL command is no longer valid](#tt0946-sql-command-is-no-longer-valid)
- [TT8521: Cannot attach PL/SQL shared memory](#tt8521-cannot-attach-plsql-shared-memory)
- [TT8037 from XLA Next](#tt8037-from-xla-next)
- [CLOB error over client/server (ErrClientCLOB)](#clob-error-over-clientserver-errclientclob)
- [ORA-06502 / OUT parameter truncated](#ora-06502--out-parameter-truncated)
- [TT8503 / ORA-06550: wrong number or types of arguments](#tt8503--ora-06550-wrong-number-or-types-of-arguments)
- [Unknown time zone](#unknown-time-zone)

## Build errors

**`fatal error: 'sql.h' file not found`, or `'tt_xla.h' file not found`**

`CGO_CFLAGS` does not point at the TimesTen headers:

```bash
export CGO_CFLAGS="-I$TIMESTEN_HOME/install/include"
```

`tt_xla.h` exists only in full installations. On a client-only installation, build with
`-tags tt_client`. macOS does this automatically.

**`ld: library not found for -lttdrvmgr`** (or `-ltten`, `-lttclient`)

`CGO_LDFLAGS` does not point at the libraries (`-L$TIMESTEN_HOME/install/lib`), or that library does
not exist in your installation. Pick the [build tag](installation.md#which-library-is-linked-build-tags)
that matches what is in `install/lib`. On AIX, also try `-Wl,-brtl`.

**`cgo: C compiler "gcc" not found`**

Install a C compiler, and check that `CGO_ENABLED=1`. On AIX, set `CC=gcc`.

## Runtime: loading libraries

**`error while loading shared libraries: libttdrvmgr.so`** (Linux), **`Library not loaded: @rpath/libttclient.dylib`** (macOS), **`Could not load module`** (AIX)

The libraries are not on the run-time search path. Fix it the platform's way:

- Linux: `LD_LIBRARY_PATH`.
- macOS: `-Wl,-rpath,...` in `CGO_LDFLAGS`.
- AIX: `LIBPATH`, or `-Wl,-blibpath:...`.
- On a TimesTen host, `source $TIMESTEN_HOME/bin/ttenv.sh`.

## Connection errors

| Error | Meaning |
|---|---|
| TT7001 "User authentication failed" | wrong `UID` / `PWD` |
| "Data source name not found" | `DSN` is not in `sys.odbc.ini` of the instance the program uses (check `TIMESTEN_HOME` / `ODBCINI`) |
| SQLSTATE 08001 over client/server, failing at once | server not reachable: host, port (default 6625), firewall, or the TimesTen server is not running |
| hangs for 60 s, then fails | the server accepts TCP but does not answer; set `TTC_Timeout` |
| TT8521 right after a database restart | see [below](#tt8521-cannot-attach-plsql-shared-memory) |

The integration test `TestConnectErrors` shows the exact errors for bad credentials.

## TT0946: SQL command is no longer valid

A statement was prepared before DDL on its table. The driver re-prepares and retries once, so you
should not see this error. If you do, the DDL happened while the statement was executing, or between
the two attempts. Retry the operation.

## TT8521: Cannot attach PL/SQL shared memory

This happens with direct connections, after the database or daemon restarts. Your process still has
the old instance's shared memory attached, through some connection that is still open. Often that is
an idle connection in a `*sql.DB` pool, or an XLA reader.

1. Close the XLA readers.
2. Flush the pool: `db.SetMaxIdleConns(0)`, then restore the value.
3. Reconnect in a retry loop.

Restarting the process always works. See [xla.md](xla.md#restarts-and-failures).

## TT8037 from XLA Next

A subscribed table was altered while the reader had unread rows from before the ALTER. The reader
cannot decode those rows. Recover in one of two ways:

- Restore the old table definition.
- Delete the bookmark and create it again. This loses the unread changes, so resynchronize the
  table, for example with a full copy.

To avoid this error, follow the procedure in [xla.md](xla.md#ddl-and-alter-table).

## CLOB error over client/server (ErrClientCLOB)

`errors.Is(err, tt.ErrClientCLOB)` is true when the TimesTen client driver delivered a CLOB value
corrupted. That happens with multibyte text over 4 KB. Change the column to NCLOB or VARCHAR2, or
read it over a direct connection. See [production.md](production.md#clob-over-clientserver).

## ORA-06502 / OUT parameter truncated

- **"OUT parameter truncated".** A string or RAW OUT value is longer than 32767 bytes. Return it
  through a REF CURSOR, or as a CLOB/BLOB OUT parameter.
- **ORA-06502 on input.** Make sure you are on a version that binds IN values over 32 KB as LOBs
  (0.1.0 or later), and that the PL/SQL parameter is CLOB/BLOB, not VARCHAR2.

## TT8503 / ORA-06550: wrong number or types of arguments

This error has two common causes:

- The number of `?` does not match the arguments.
- A parameter has a PL/SQL-only type (BOOLEAN, RECORD, collection), which cannot be bound. See
  [plsql.md](plsql.md#boolean-record-and-collections).

## Unknown time zone

**`ttsql: go_location: unknown time zone Asia/Ho_Chi_Minh`**

There is no time zone database on the host. Add `import _ "time/tzdata"` to the program, or set
`ZONEINFO`. See [installation.md](installation.md#time-zone-data).
