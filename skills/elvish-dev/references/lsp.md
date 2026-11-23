# LSP (`pkg/lsp`)

Source of truth: `pkg/lsp/lsp.go`, `pkg/lsp/server.go`, `pkg/lsp/symbols.go`, `pkg/lsp/*_test*`.

## Entry and Transport

`pkg/lsp.Program` registers `-lsp` flag. When selected, it creates a server, wires `jsonrpc2` over stdio buffered stream with VSCode object codec, and blocks until disconnect. Registered in default cmd composite (`cmd/elvish/main.go`).

## Server State

```go
type server struct {
    mu        sync.Mutex
    evaler    *eval.Evaler
    documents map[lsp.DocumentURI]document
    diagSeq   map[lsp.DocumentURI]int
    shutdown  bool
}
```

`document` holds code, version, and parsed tree. `diagSeq` guards async diagnostic publishing so stale parse diagnostics are dropped.

## Capabilities / Methods

`initialize` advertises: full text sync, completion, hover, definition, references, document/workspace symbols, rename with prepare, document highlight. Handler routes JSON-RPC methods; unknown -> method not found; requests after shutdown (except lifecycle) -> invalid request.

Implemented handlers:
- lifecycle: `initialize`, `initialized` noop, `shutdown`, `exit`
- sync: `didOpen`, `didChange` (full only), `didClose`, `workspace/didChangeWatchedFiles` noop
- language: `hover`, `completion`, `definition`, `references`, `documentSymbol`, `workspaceSymbol`, `prepareRename`, `rename`, `documentHighlight`

## Diagnostics and Parsing

On open/change, parse with `parse.Parse(..., parse.Config{})`; unpack errors via `parse.UnpackErrors` and publish as `source: "parse"` diagnostics. Position/range conversion uses UTF-aware helpers in server/symbol code.

## Completion / Symbols / Rename

Completion uses `complete.Complete` with `s.evaler` and default config, maps items to LSP `CompletionItem` with plain text insert and replace range. Symbol resolution lives in `symbols.go` (symbol kinds: variable/function/command; scope-aware definition/reference lookup; rename target normalization/validation). Hover can return builtin/module docs via `pkg/mods/doc` when not a local user definition.

## Testing

LSP tests include transcript-style and Go tests in `pkg/lsp` (`*_test.elvts`, `*_test.go`). When changing parse node semantics or symbol scoping, update both `pkg/lsp/symbols.go` and tests. Keep method behavior JSON-RPC/LSP-spec compatible with advertised capabilities.