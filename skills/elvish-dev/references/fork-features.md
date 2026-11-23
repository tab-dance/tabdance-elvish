# Fork-Specific Features (tab-dance/elvish)

Source of truth: `README.md`, `AGENTS.md`, `pkg/edit/completion.go`, `pkg/edit/complete/complete.go`, `pkg/eval/context.go`, `pkg/mods/runtime/runtime.go`, related tests/docs.

## External Command Completer

- **Var**: `$edit:completion:command-completer` (fn or nil). Declared in `pkg/edit/completion.d.elv`.
- **Wiring**: `pkg/edit/completion.go` stores `newFnVar(nil)`, passes through `cfg.CommandGenerator = adaptCommandGenerator(...)`.
- **Engine**: `complete.Config.CommandGenerator` (`func(seed string) ([]RawItem, error)`). `generateCommandCandidates` calls it when non-nil; otherwise built-in `generateCommands`.
- **Use case**: delegate command-name completion to external tools (e.g. carapace bridge) while preserving Elvish completion UI.

## External Variable Completer

- **Var**: `$edit:completion:variable-completer` (fn or nil).
- **Wiring**: `adaptVariableGenerator` in `pkg/edit/completion.go`.
- **Engine**: `complete.Config.VariableGenerator` (`func(seed, ns string) ([]RawItem, error)`). `completeVariable` uses it when set; otherwise enumerates namespace vars and adds `e:`/`E:` for root ns.
- **Args**: seed is partial var name without leading `$`; ns is namespace prefix context.

## Runtime Context Introspection

`pkg/eval/context.go` defines:

```go
type ShellContext struct {
    Shell     string   // "elvish"
    Aliases   []string // edit:abbr keys
    Builtins  []string // specials + builtin functions, ~ stripped
    Functions []string // global fn~ names, ~ stripped
    Jobs      []string // always empty in Elvish
    Variables []string // global + builtin names, ~/: stripped
}
```

`Evaler.Context()` snapshots live state; slices sorted and deduped. Aliases read from `edit:abbr` if editor namespace exists. Builtins include `IsBuiltinSpecial` and builtin namespace function names. Functions are global `fn~` vars. Variables merge global and builtin names with suffix stripping.

`pkg/mods/runtime/runtime.go` exposes `runtime:context` as an Elvish map:

```elvish
use runtime
var c = (runtime:context)
put $c[shell $aliases $builtins $functions $jobs $variables]
```

Designed for external completion tools to inspect shell state without shell-specific scraping.

## Smart Completion Prefix/Quoting

`completionStart` smart-start handles single-candidate direct insertion and common-prefix insertion when prefix extends seed. `extendsSeed`/`unquoteValue` compare logical unquoted values, so quote position changes are treated as extension. Tests in `pkg/edit/completion_test.go` cover quote-at-start, quote-within-word, bareword, single/double quotes, and multiple-candidate common prefix.

## LSP in Fork

`pkg/lsp` provides builtin language server (`-lsp` flag program). Not upstream-minimal; keep behavior aligned with fork tests when editing parser/eval symbols.

## Compatibility Notes

- Keep default behavior unchanged when completer vars are nil/unset.
- Preserve deterministic ordering and no-duplicate guarantees for `ShellContext` slices.
- When modifying completion adapters, preserve string and `edit:complex-candidate` output handling, byte-stream line handling, and error propagation semantics.