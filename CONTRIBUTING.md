# Contributing to go-ttdb

Thank you for helping. Bug reports, documentation fixes, test results from new platforms (AIX, other
TimesTen releases) and pull requests are all welcome.

## Reporting a bug

Open an issue using the bug report template, and include:

- go-ttdb version or commit, `go version`, OS and architecture.
- TimesTen release (`ttVersion`), and whether the connection is direct or client/server.
- The connection attributes that matter, such as the character sets. **Remove passwords.**
- A minimal program or SQL script that reproduces the problem, the full error text, and the
  `tt.Error` code.

## Development setup

The library is cgo code over TimesTen's C APIs, so building and testing need TimesTen. The repository
provides a dev container that runs its own TimesTen instance and test database, so nothing has to be
installed on your machine except Docker.

### The TimesTen image

TimesTen cannot be redistributed, so the dev image is built **from a TimesTen image you provide**.
Pass it in `TT_IMAGE`. The image must meet these requirements:

- Oracle Linux based, with `yum` available.
- TimesTen 22.1 **Classic** installed as the instance `/usr/tt/instance1`.
- Owned by user `oracle`, group `oinstall`.

Examples:

- An image you built from Oracle's TimesTen container files, or from the TimesTen installer.
- An image from Oracle Container Registry, after you accept Oracle's license there.

### Start the container and run the tests

```bash
export TT_IMAGE=my-timesten:22.1
make dev-up        # builds the image (adds gcc and Go), starts it, creates database "gotest"
make test          # unit tests
make race          # integration tests with -race and cgocheck
make ci            # everything a pull request must pass: lint, race, race-cs, netfaults
make dev-down
```

The other targets are listed in [docs/testing.md](docs/testing.md#make-targets). On a machine that
has TimesTen installed, you can run `go test` directly, as shown in
[docs/testing.md](docs/testing.md#running-on-a-host-with-timesten).

## Pull requests

1. Open an issue first for anything larger than a small fix, so the approach can be agreed on.
2. Keep each pull request to one change. Add or update tests:
   - unit tests where no database is needed;
   - integration tests (`-tags integration`) for driver behaviour.
3. Run `make ci`, and say in the pull request which TimesTen release and platform you tested on.
4. Update the documentation in `docs/`, and add a line to `CHANGELOG.md` under *Unreleased*.

### Code guidelines

- `gofmt` and `go vet` clean. `make lint` checks every build tag.
- Memory that ODBC keeps a pointer to after a call returns must be allocated with `C.malloc` and freed
  by the `internal/cli` package. Never pass Go memory to it. See the package comment in
  `internal/cli/cgo.go`.
- ODBC handles are not safe for concurrent use. Serialize access per connection. The only concurrent
  call by design is `SQLCancel`.
- Errors from TimesTen are returned as `*tt.Error`, with every diagnostic record.
- New behaviour that depends on TimesTen quirks needs an integration test that shows the quirk.
- Public API changes need documentation comments that read well on pkg.go.dev.

### Commit messages

Use the imperative mood ("Add NCLOB support to ExecBatch"). Keep the first line under 72 characters,
and explain *why* in the body when it is not obvious.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
