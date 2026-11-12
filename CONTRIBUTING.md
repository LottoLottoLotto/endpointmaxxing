# Contributing

Bug reports and pull requests are welcome. For a suspected vulnerability, do
not open an issue: follow [SECURITY.md](SECURITY.md).

## Build

substrate needs Go 1.26 or later and has no cgo dependencies.

```sh
git clone https://github.com/LottoLottoLotto/substrate.git
cd substrate
go build -o bin/substrate ./cmd/substrate
bin/substrate doctor
```

Linux, macOS and Windows are supported; CI runs the tests and
`scripts/smoke.sh` (start, serve and stop an engine with the built binary) on
all three. Code that depends on the operating system lives in per-platform
files (`internal/proc`, `internal/lock`); keep it there.

## Test

```sh
go test -race ./...
```

Set `SUBSTRATE_HOME` to a scratch directory when you try a build by hand, so
it does not touch your own configuration and state.

## Style

- Format with `gofmt`; `gofmt -l .` must print nothing.
- `go vet ./...` must pass.
- Use the standard library and the existing dependencies; discuss a new
  dependency in an issue first.
- Follow the conventions of the package you change: its error helpers
  (`internal/errs`) and its test helpers.
- A bug fix comes with a regression test next to the existing tests.

CI runs these checks, cross-builds for linux/amd64, linux/arm64 and
darwin/arm64, and runs `govulncheck`.

## Adding an engine

substrate contains no engine-specific code: an engine is a YAML manifest. The
format is specified in [docs/SPEC.md](docs/SPEC.md).

1. Write the manifest and try it from `~/.config/substrate/engines/`, where
   user manifests live.
2. Check it with `substrate engines` and `substrate run <model> --dry-run`,
   which prints the exact command without starting it.
3. To propose it as a builtin, add it to `internal/spec/engines/` and add a
   row to the table in [docs/engines.md](docs/engines.md).

## Commits and pull requests

- One logical change per pull request, with a description of what changes
  for users and why.
- Commit messages: a short summary line in the imperative, then details where
  they help.
- User-visible changes update the documentation (`README.md`, `docs/`) and
  add a line under `Unreleased` in [CHANGELOG.md](CHANGELOG.md).
- Contributions are accepted under the project's license, the
  [Apache License 2.0](LICENSE).
