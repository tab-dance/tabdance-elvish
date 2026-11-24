# Build, Test, and Release Workflows

Source of truth: `Makefile`, `docs/workflows.md`, `docs/building.md`, `docs/testing.md`, `docs/packaging.md`, `.github/workflows/ci.yml`, `.goreleaser.yml`.

## Build / Install

```sh
make get                        # builds ELVISH_MAKE_BIN (default $GOBIN/elvish)
go install ./cmd/elvish         # from local tree
go build -o ./elvish ./cmd/elvish
```

`make get` accepts `ELVISH_MAKE_BIN` and `ELVISH_MAKE_PKG`. Alternate entrypoints: `./cmd/nodaemon/elvish`, `./cmd/withpprof/elvish`.

## Formatting and Generation

```sh
make fmt                        # goimports + gofmt -s + elvmdfmt -w -width 80
go run src.elv.sh/cmd/elvmdfmt -width 80 -w file.md
go generate ./...               # regenerate stringer outputs; needs stringer
```

Generated files to avoid hand-editing: `zstring.go` (stringer), `ztypes_windows.go` (cgo godefs).

## Tests

```sh
make test                       # go test $(tools/run-race.elv) ./... and website/
go test ./...                   # root module
cd website && go test ./...     # website module
make cover                      # cover pkg/... + prune + HTML report
```

Use `ELVISH_TEST_TIME_SCALE` (e.g. 10/20) on slow/VM hosts. On Windows CI, `core.autocrlf` must be off.

## Checks

```sh
make most-checks                # fmt-go, fmt-md, disallowed, vet, staticcheck, codespell
make all-checks                 # most-checks + check-gen (requires clean git tree)
```

Tooling (keep versions aligned with CI/docs):

```sh
go install golang.org/x/tools/cmd/goimports@latest
go install golang.org/x/tools/cmd/stringer@latest
go install honnef.co/go/tools/cmd/staticcheck@v0.6.0
pip install codespell==2.3.0
```

`tools/check-gen.sh` runs `go generate` and fails on diff; run on clean tree. `tools/check-disallowed.sh` enforces import/syscall/link restrictions.

## CI / Release

CI (`.github/workflows/ci.yml`): tests on ubuntu/macos/windows with Go 1.23.x and ubuntu 1.22.x; emulated riscv64/s390x tests; benchmark smoke; checks on all three OSes. Release job runs GoReleaser on tags.

GoReleaser (`.goreleaser.yml`): builds `./cmd/elvish` with `CGO_ENABLED=0`, ldflags set `buildinfo.VersionBase`, multi-OS/arch archives, SHA256SUMS, AUR package `tabdance-elvish-bin`.

## Local Gotchas

- `GOWORK=off` for website tests/checks when a `go.work` exists (pre-push does this).
- `tools/pre-push` hook: `ln -s ../../tools/pre-push .git/hooks/pre-push`; runs tests, checks, cross-compile smoke.
- Do not push/open PR/create release unless explicitly commanded.