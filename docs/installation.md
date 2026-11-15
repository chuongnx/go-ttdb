# Installation

go-ttdb is a cgo library. Two things must be on the machine where you build:

- The **TimesTen 22.1** headers (`$TIMESTEN_HOME/install/include`).
- The TimesTen 22.1 libraries (`$TIMESTEN_HOME/install/lib`).

The machine where the program runs needs the same libraries. TimesTen is not redistributed with this
module. Install it from Oracle: a full installation on the database host, or the client installation
anywhere else.

```bash
go get github.com/chuongnx/go-ttdb@latest
```

- [Common settings](#common-settings)
- [Which library is linked (build tags)](#which-library-is-linked-build-tags)
- [Linux](#linux)
- [macOS](#macos)
- [AIX](#aix)
- [Time zone data](#time-zone-data)
- [Deploying the binary](#deploying-the-binary)

## Common settings

```bash
export TIMESTEN_HOME=/path/to/timesten/instance   # the instance directory (contains install/)
export CGO_ENABLED=1
export CGO_CFLAGS="-I$TIMESTEN_HOME/install/include"
export CGO_LDFLAGS="-L$TIMESTEN_HOME/install/lib"
```

On a TimesTen host you usually source the TimesTen environment first:
`source $TIMESTEN_HOME/bin/ttenv.sh`. It sets `TIMESTEN_HOME`, `PATH` and the library path.

Requirements:

- Go 1.22 or later.
- A C compiler: gcc, or clang on macOS.
- Character set: use `DatabaseCharacterSet=AL32UTF8` and `ConnectionCharacterSet=AL32UTF8` in the DSN
  (or the connection string). The driver exchanges text as UTF-8. With another connection character
  set, non-ASCII text is converted by TimesTen and may be lost.

## Which library is linked (build tags)

| Build tag | Library | Direct | Client/server | XLA |
|---|---|---|---|---|
| *(none, default)* | `libttdrvmgr` (TimesTen Driver Manager) | ✓ | ✓ | ✓ on direct connections |
| `tt_direct` | `libtten` | ✓ | – | ✓ |
| `tt_client` | `libttclient` | – | ✓ | – (the `xla` package is not built) |

- The default works with full TimesTen installations and covers both connection types.
- Use `-tags tt_client` on a machine that has only the **client** installation. It has no
  `libttdrvmgr`, `libtten` or `tt_xla.h`.
- Use `-tags tt_direct` when you want to link the direct driver without the driver manager.

```bash
go build -tags tt_client ./...
```

## Linux

Linux x86-64 and arm64 are tested.

```bash
export LD_LIBRARY_PATH="$TIMESTEN_HOME/install/lib:$LD_LIBRARY_PATH"
go build ./...
go test ./...     # unit tests; no database needed
```

To avoid needing `LD_LIBRARY_PATH` at run time, add an rpath:

```bash
export CGO_LDFLAGS="-L$TIMESTEN_HOME/install/lib -Wl,-rpath,$TIMESTEN_HOME/install/lib"
```

## macOS

On macOS, Oracle ships only the TimesTen **client**:

- There is no `tt_xla.h`, no `libttdrvmgr` and no `libtten`.
- Darwin builds therefore link `libttclient` automatically, so you do not need `-tags tt_client`.
- They also leave out the `xla` package and `examples/xla-tail`.

`ttsql` works over client/server connections.

`LD_LIBRARY_PATH` is not used on macOS. Embed an rpath instead:

```bash
export TIMESTEN_HOME=$HOME/timesten/tt221          # the client instance
export CGO_CFLAGS="-I$TIMESTEN_HOME/install/include"
export CGO_LDFLAGS="-L$TIMESTEN_HOME/install/lib -Wl,-rpath,$TIMESTEN_HOME/install/lib"
go build ./...
```

Connect with a client/server connection string, as shown in [connecting.md](connecting.md#client-server).

## AIX

AIX 7.2 or later on POWER8 or later, which is the minimum for Go's `aix/ppc64` port.

1. Install Go for `aix/ppc64` from [go.dev/dl](https://go.dev/dl/).
2. Install gcc ≥ 10 from the IBM AIX Toolbox: `dnf install gcc`. cgo on AIX uses gcc.
3. Set the variables:

   ```sh
   export CGO_ENABLED=1 CC=gcc
   export CGO_CFLAGS="-I$TIMESTEN_HOME/install/include"
   export CGO_LDFLAGS="-L$TIMESTEN_HOME/install/lib"
   export LIBPATH="$TIMESTEN_HOME/install/lib:$LIBPATH"   # AIX uses LIBPATH, not LD_LIBRARY_PATH
   go build ./...
   ```

Link problems:

- If `ld` cannot find `-lttdrvmgr` although the `.so` is in the directory, add `-Wl,-brtl` to
  `CGO_LDFLAGS`.
- To embed the library path in the binary, add
  `-Wl,-blibpath:$TIMESTEN_HOME/install/lib:/usr/lib:/lib`.

AIX is big-endian. The driver is not yet verified on AIX, so run the test suite there before relying
on it; see [testing.md](testing.md#testing-on-aix).

## Time zone data

DATE and TIMESTAMP values are converted using the `go_location` option. To use a named zone such as
`Asia/Ho_Chi_Minh`, Go needs the IANA time zone database. Minimal containers and AIX often do not have
`/usr/share/zoneinfo`. Either embed the database in the binary:

```go
import _ "time/tzdata"
```

or set `ZONEINFO=$(go env GOROOT)/lib/time/zoneinfo.zip` at run time.

## Deploying the binary

The binary links TimesTen dynamically. On the target host:

- Install the same TimesTen release, or a newer patch of 22.1. A client installation is enough for
  client/server connections.
- Make the libraries findable: through `LD_LIBRARY_PATH` / `LIBPATH`, an rpath, or `ttenv.sh`.
- For direct connections and XLA, run as an OS user in the TimesTen instance administrators group,
  because the process attaches the database's shared memory.
- Cross-compiling needs the target platform's TimesTen headers and libraries, plus a cross C
  compiler. It is usually simpler to build on, or in a container of, the target platform.
