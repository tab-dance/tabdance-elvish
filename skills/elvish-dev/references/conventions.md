# Conventions and Repo Rules

Source of truth: `AGENTS.md`, `docs/documenting.md`, `docs/workflows.md`, `docs/packaging.md`, tools/check scripts.

## Code and Docs Style

- Module imports use `src.elv.sh/...`, not GitHub module path.
- File names: `snake_case`. Generated: `zstring.go`, `ztypes_windows.go`; regenerate, don't hand-edit.
- Go formatting: `goimports` then `gofmt -s`. Markdown formatted by `cmd/elvmdfmt` to width 80.
- Doc comments: exported symbols start with symbol name; unexported comments do not repeat name.
- Elvdocs (`.d.elv`) use comment blocks before `fn`/`var`; function first sentence is 3rd-person singular verb; include `elvish-transcript` examples with default prompt `~>` and `▶`.

## Checks / Disallowed Patterns

`tools/check-disallowed.sh` enforces:
- no `"net/rpc"` import (use `pkg/rpc`);
- no `unix.Umask/Getrlimit/Setrlimit` outside allowed packages;
- no `./` prefix in links to `language.html` in `.elv` files.

`make all-checks` requires clean tree because `check-gen.sh` runs `go generate` and diffs.

## Testing Conventions

- Prefer real behavior; mock only untestable deps via package-level vars + `testutil.Set`.
- External test packages use `testexport_test.go` pointer exports.
- Transcript tests are standard for eval/module behavior; use `//go:embed` and evaltest directives.
- Set `ELVISH_TEST_TIME_SCALE` on constrained environments.

## Fork/Process Rules

- Read relevant files before editing; preserve exact style.
- Run targeted tests after changes, then broader checks as needed.
- **Never commit, push, open PR, or create release unless explicitly commanded.** Any remote modification needs explicit approval.
- For packaging/versioning, set build metadata through `pkg/buildinfo` ldflags; don't invent old ldflags. Keep user docs/release notes in mind for user-visible behavior changes.

## Useful Locations

- Architecture: `docs/architecture/doc.go`
- Build: `docs/building.md`, `Makefile`
- Testing: `docs/testing.md`
- Workflows/checks: `docs/workflows.md`, `tools/`
- CI/release: `.github/workflows/ci.yml`, `.goreleaser.yml`