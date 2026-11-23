# Testing Patterns

Source of truth: `docs/testing.md`, `pkg/eval/evaltest/test_transcript.go`, `pkg/transcript`, `tools/run-race.elv`.

## Test Layers

- **Go unit tests**: `go test ./...` / `make test`. Race detector used where supported (`tools/run-race.elv`).
- **Transcript tests**: primary way to test Elvish language/module behavior; `.elv` and `.elvts` files parsed by `pkg/transcript` and executed by `pkg/eval/evaltest`.
- **E2E tests**: `e2e/` builds/runs binary at runtime (see `make cover` behavior).
- **Website tests**: separate module under `website/`; `cd website && go test ./...`.

## Transcript Harness (`pkg/eval/evaltest`)

Typical pattern:

```go
//go:embed *.elv *.elvts
var transcripts embed.FS

func TestTranscripts(t *testing.T) {
    evaltest.TestTranscriptsInFS(t, transcripts)
}
```

`TestTranscriptsInFS` discovers transcript sessions from embedded FS and runs them. It accepts optional `(name, f)` setup pairs. Setup `f` may take `(*testing.T, *eval.Evaler, string)` in that order, all optional.

Built-in directives:

| Directive | Effect |
|-----------|--------|
| `skip-test` | Skip session. |
| `in-temp-dir` | Run in temp directory. |
| `set-env $name $value` | Set env var for session. |
| `unset-env $name` | Unset env var. |
| `eval $code` | Evaluate Elvish code before session/interactions. |
| `only-on $cond` | Run only when constraint matches (`GOOS/GOARCH`, `unix`, `32bit`, `64bit`). |
| `deprecation-level $x` | Set deprecation level. |

Prefix with `each:` to apply to descendant sessions.

## ELVISH_TRANSCRIPT_RUN

Set `ELVISH_TRANSCRIPT_RUN=file.elvts:lineno` to run only the containing session up to that interaction. If output mismatches, evaltest reports a machine-readable `UPDATE {...}` instruction with `fromLine`, `toLine`, `content`. VS Code extension uses this for Alt+Enter transcript updates.

## Mocking / Dependency Injection

Prefer testing real behavior. For syscalls/timing/OS behavior, inject via package-level var and override in tests:

```go
var osSleep = os.Sleep
// test:
testutil.Set(&osSleep, func(d time.Duration) { /* fake */ })
```

For external test packages (`pkg_test`), expose pointer in internal `testexport_test.go`, e.g. `var OSSleep = &osSleep` / `&os.Sleep` as used by package convention.

## Practical Notes

- Read existing tests in the same package before adding new patterns.
- Keep transcript expectations deterministic; evaltest normalizes value/byte/stdout/stderr ordering for testability.
- Add tests for fork features in both unit/transcript form where present (`pkg/edit/completion*.go`, `pkg/mods/runtime/runtime_test.elvts`, `pkg/lsp/*_test*`).