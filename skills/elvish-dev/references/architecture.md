# Architecture and Package Layout

Source of truth: `docs/architecture/doc.go`, `AGENTS.md`, and package code under `pkg/` and `cmd/`.

## Module and Entrypoints

- Module: `src.elv.sh` (imports as `src.elv.sh/pkg/...`).
- Main entrypoint: `cmd/elvish/main.go`: builds a `prog.Composite` of subprograms (`buildinfo`, `daemon`, `shell`, `lsp`) and calls `prog.Run`.
- Alternative entrypoints:
  - `cmd/nodaemon/elvish`: no storage daemon.
  - `cmd/withpprof/elvish`: profiling support.
  - `cmd/elvmdfmt`: Markdown formatter used by `make fmt`.

## Core Flow

```text
prog.Run
  ├─ parse global flags (-log, -help, -deprecation-level)
  ├─ Program.RegisterFlags / Run dispatch via Composite
  └─ shell.Program.Run
       ├─ incSHLVL, initSignal, NO_COLOR, makeEvaler
       ├─ script mode: script()/evalInTTY
       └─ interactive mode: daemon activate + interact() REPL
            ├─ edit.Editor reads input
            └─ eval.Evaler evaluates parsed code
```

`pkg/prog` provides testable/composable `Program` abstraction: `RegisterFlags`, `Run(fds, args)`, `Composite`, `NextProgram`, `Exit/BadUsage`.

## Package Map

| Package | Role |
|---------|------|
| `pkg/prog` | Program interface, flag composition, global flags, exit handling. |
| `pkg/shell` | Shell subprogram: RC/lib path resolution, module install, script/interactive modes, daemon spawn wiring. |
| `pkg/eval` | Interpreter core: Evaler, compile/eval, frames/ports, builtin specials/functions, namespaces/vars, exceptions. |
| `pkg/parse` | Handwritten recursive-descent parser; `Parse`/`ParseAs`, Node tree with `Children`, no separate tokenizer. |
| `pkg/edit` | Interactive editor: completion UI integration, keybindings, prompts/hooks, abbreviation, modes. |
| `pkg/edit/complete` | Completion algorithm: context detection, generators, filterers, raw item cooking, dedup. |
| `pkg/cli`, `pkg/ui` | Low-level TUI/app primitives and styled text/rendering. |
| `pkg/daemon` | Storage daemon subprogram and client activation; used by interactive shell. |
| `pkg/store` | Persistent history/dir/cmd storage abstractions used by daemon/editor. |
| `pkg/mods/*` | Builtin modules (`runtime`, `str`, `path`, `unix`, `re`, `math`, `file`, `doc`, etc.). |
| `pkg/lsp` | Builtin LSP server (`-lsp` flag): parse/diagnostics, completion, hover, definition, references, symbols, rename, highlight. |
| `pkg/eval/evaltest` | Transcript test harness for `.elv`/`.elvts`. |
| `pkg/transcript` | Transcript parser/discovery used by evaltest. |
| `pkg/buildinfo` | Version/build metadata; linker-overridable `VersionBase`, `BuildVariant`, `VCSOverride`. |
| `pkg/persistent` | Persistent vector/map/hash structures used by values. |
| `pkg/sys`, `pkg/fsutil`, `pkg/env`, `pkg/testutil`, `pkg/strutil` | OS/files/env/test/string helpers. |

## Interpreter Shape (`pkg/eval`)

`Evaler.Eval` pipeline: parse source → compile AST to operation tree → execute operations with ports/frames/interrupts. Semantics are mostly in `compile_*.go` and builtin implementations in `builtin_*.go` and `builtin_fn_*.go`. Values live in `pkg/eval/vals`; variables/namespaces in `pkg/eval/vars`.

## Editor/Completion Shape

`pkg/edit` owns editor state and binds `$edit:completion:*` vars/functions. `completionStart` applies autofix/smart behavior and calls `complete.Complete`. `pkg/edit/complete` finds parse-tree context and dispatches completers in order: command, index, redir, variable, arg. It then filters, sorts, cooks `RawItem` into `modes.CompletionItem`, and dedups by `ToInsert`.

## Fork-Specific Additions

This fork adds external-completion hooks and runtime introspection:
- `$edit:completion:command-completer` and `$edit:completion:variable-completer`.
- `eval.Evaler.Context()` / `eval.ShellContext` and `runtime:context`.
- `pkg/lsp` language server and GoReleaser-based release packaging.

See [fork-features.md](fork-features.md) and [completion.md](completion.md).