# Completion Internals

Source of truth: `pkg/edit/complete/*`, `pkg/edit/completion.go`, `pkg/edit/completion.d.elv`, `pkg/edit/completion_test.go`, `pkg/edit/completion_test.elvts`.

## Pipeline

```text
Editor key (Tab/smart-start)
  → edit.completionStart
    → optional autofix
    → complete.Complete(CodeBuffer, ev, cfg)
      → parse + np.FindLeft path
      → completers: command | index | redir | variable | arg
      → cfg.Filterer(ctx.name, ctx.seed, rawItems)
      → sort by String(), cook RawItem via Cook(ctx.quote)
      → dedup by ToInsert
    → smart-start: single item insert, or extend common prefix if extendsSeed
    → else open Completion addon
```

## Core Types (`pkg/edit/complete`)

| Type | Role |
|------|------|
| `Config` | `Filterer`, `ArgGenerator`, `CommandGenerator`, `VariableGenerator`. |
| `Filterer` | `func(ctxName, seed string, rawItems []RawItem) []RawItem`; default `FilterPrefix`. |
| `ArgGenerator` | `func(args []string) ([]RawItem, error)`; default `GenerateFileNames`. |
| `CommandGenerator` | `func(seed string) ([]RawItem, error)`; nil => built-in `generateCommands`. |
| `VariableGenerator` | `func(seed, ns string) ([]RawItem, error)`; nil => built-in variable enumeration. |
| `RawItem` | `String() string`, `Cook(parse.PrimaryType) modes.CompletionItem`. |
| `PlainItem` | Quotes via `parse.QuoteAs` on cook. |
| `noQuoteItem` | Unquoted cook (internal). |
| `ComplexItem` | `Stem`, `CodeSuffix`, `Display`; cooked as quoted stem + code suffix. |
| `Result` | `Name`, `Seed` (unquoted logical seed), `Replace`, `Items`. |

## Context Detection (`completers.go`)

Completers run in order and return `errNoCompletion` to fall through:

- **command**: empty chunk, after sep/pipe, start of output/exception capture/lambda, or simple expr at form head.
- **index**: after `[` or in incomplete single-level index; uses `PurelyEvalPrimary` for indexee.
- **redir**: after redir op or inside redir target; generates filenames.
- **variable**: primary type `Variable`; splits sigil/qname/ns; uses `VariableGenerator` or enumerates ns vars + `e:`/`E:` in root.
- **arg**: new arg or incomplete arg in form; special handling for `set`/`tmp` (var assignment before `=`) and `del`.

## Generators (`generators.go`)

- `GenerateFileNames`: file candidates with lscolors display, dirs get separator suffix behavior via `ComplexItem`.
- `GenerateDirNames`: directories and symlinks-to-dirs.
- `GenerateForSudo`: command completion for sudo arg1, else delegate arg generator.
- `generateCommands`: local external if `DontSearch`; else specials, externals, and for bare/sigil forms functions/namespaces in relevant ns. `e:` prefix returns external commands with `e:` prefix.
- `generateArgs`: dispatches `set/tmp/del`; otherwise `cfg.ArgGenerator`.

## Edit Integration (`pkg/edit/completion.go`)

`initCompletion` registers `edit:completion:` ns vars:
- `arg-completer` (map), `matcher` (map), `binding`, `command-completer` (fn), `variable-completer` (fn).
- functions: `start`, `smart-start`, `accept`, `up/down/left/right`, etc.

`edit:complex-candidate` creates a `complexItem` (Elvish map-like value) with `stem`, `code-suffix`, `display` (string or styled). `wrapArgGenerator` converts generator output to value/byte stream items.

## Adapter Behavior

`adaptArgGeneratorMap`, `adaptCommandGenerator`, `adaptVariableGenerator` call Elvish callables through `eval.PipePort`, collecting value output (string/complexItem/other stringified) and byte output (lines). Errors from callable are returned after collecting output.

`adaptMatcherMap` converts `$edit:completion:matcher` map into `Filterer`; fallback to `FilterPrefix` on missing/non-function; notifies on bad output count/type and keeps valid booleans.

## Smart-Start Details

`smart-start` may auto-insert when a single candidate exists, or when all candidates share common prefix extending seed. `extendsSeed` compares logical unquoted values: it unquotes candidate with `parse.Parse` + `PurelyEvalCompound`, and compares against result seed or unquoted replacement. This handles quote-position changes (`name-"` -> `"name-with-space"`).

## Testing Hooks

- Unit tests: `pkg/edit/complete/complete_test.go`, `pkg/edit/completion_test.go`.
- Transcript tests: `pkg/edit/completion_test.elvts`, runtime/elvish behavior in evaltest transcripts.
- When changing quoting/prefix behavior, cover bareword, single/double quote, mid-word quote, and common-prefix cases.