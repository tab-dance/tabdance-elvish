---
name: elvish-dev
description: >
  Use when developing or debugging this Elvish fork repo (tab-dance/elvish): Go package
  layout, eval/parser/edit internals, completion pipeline, fork completers, runtime:context,
  LSP, transcript tests, build/check/release workflows. Triggers on: "elvish-dev", "pkg/eval",
  "pkg/edit", "pkg/parse", "pkg/shell", "pkg/lsp", "evaltest", ".elvts", "command-completer",
  "variable-completer", "runtime:context", "ShellContext", "complete.Complete", "RawItem",
  "make test", "most-checks", "elvmdfmt".
user-invocable: true
---

# Elvish Development Reference

Reference for developing [tab-dance/elvish](https://github.com/tab-dance/elvish), a fork of Elvish with completion/runtime enhancements for external tools. Module path: `src.elv.sh`. `go.mod` declares Go 1.21; CI tests Go 1.22.x and 1.23.x.

## Data Flow

```
cmd/elvish/main → prog.Run(Composite(..., shell.Program))
  → shell.Program.Run: make Evaler, add mods, init signal/SHLVL
    → script mode: eval source/file
    → interact mode: daemon/storage + edit.Editor REPL
      → edit reads line, evals code
      → completion: edit.CompletionStart → complete.Complete
        → completers (command/index/redir/variable/arg)
        → Filterer + RawItem.Cook → CompletionItem
```

## Sub-Resources

Load the reference that matches your task. When in doubt, load multiple references.

| Keywords | Reference |
|----------|----------|
| repo layout, cmd entrypoints, pkg map, architecture, prog.Run, shell.Program, eval.Evaler, parse.Parse, edit.Editor, daemon, mods, persistent, buildinfo, module path src.elv.sh | [references/architecture.md](references/architecture.md) |
| build, test, fmt, checks, make get, make test, make cover, most-checks, all-checks, check-gen, go generate, stringer, goimports, staticcheck, codespell, elvmdfmt, pre-push, CI, goreleaser, GOWORK, ELVISH_TEST_TIME_SCALE | [references/workflows.md](references/workflows.md) |
| testing, evaltest, TestTranscriptsInFS, .elvts, transcript directives, in-temp-dir, eval, only-on, ELVISH_TRANSCRIPT_RUN, mocking, testutil.Set, testexport_test.go, race, embed transcripts | [references/testing.md](references/testing.md) |
| completion, complete.Complete, completers, RawItem, PlainItem, ComplexItem, noQuoteItem, Filterer, ArgGenerator, CommandGenerator, VariableGenerator, command-completer, variable-completer, arg-completer, matcher, complex-candidate, smart-start, extendsSeed, Cook, quoting pass | [references/completion.md](references/completion.md) |
| fork features, tab-dance, runtime:context, ShellContext, Evaler.Context, aliases, builtins, functions, variables, jobs, command-completer, variable-completer, smart completion prefix, carapace integration | [references/fork-features.md](references/fork-features.md) |
| LSP, pkg/lsp, language server, jsonrpc2, go-lsp, definition, references, rename, hover, completion, documentSymbol, workspaceSymbol, documentHighlight, diagnostics, didOpen/didChange | [references/lsp.md](references/lsp.md) |
| conventions, style, doc comments, snake_case, generated files, zstring.go, disallowed imports, dependency injection, commit/PR policy, packaging, version buildinfo, AGENTS.md | [references/conventions.md](references/conventions.md) |

## Quick Guide

- **How is the repo structured?** → [references/architecture.md](references/architecture.md)
- **How do I build, test, or run checks?** → [references/workflows.md](references/workflows.md)
- **How do transcript tests work?** → [references/testing.md](references/testing.md)
- **How does completion work, including fork hooks?** → [references/completion.md](references/completion.md)
- **How does `runtime:context` or command/variable completers work?** → [references/fork-features.md](references/fork-features.md)
- **Where is the LSP implemented?** → [references/lsp.md](references/lsp.md)
- **What conventions/checks must I follow?** → [references/conventions.md](references/conventions.md)
- **Where should a new builtin/module/command go?** → [references/architecture.md](references/architecture.md)
- **How do I update generated code or docs formatting?** → [references/workflows.md](references/workflows.md)

## Cross-Project References

- For general Elvish shell/language/completion user-facing behavior, use the **elvish** skill.
- For carapace integration details when changing completer behavior, see **carapace-dev** skill (especially `references/shell-elvish.md`) and **carapace** skill as appropriate.
- For git workflows and GitButler, use **git** / **but** skills; do not push or open PRs without explicit user command.