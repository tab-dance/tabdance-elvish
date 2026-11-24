package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/sourcegraph/jsonrpc2"
	lsp "pkg.nimblebun.works/go-lsp"
	"src.elv.sh/pkg/diag"
	"src.elv.sh/pkg/edit/complete"
	"src.elv.sh/pkg/eval"
	"src.elv.sh/pkg/format"
	"src.elv.sh/pkg/mods/doc"
	"src.elv.sh/pkg/parse"
	"src.elv.sh/pkg/parse/np"
	"src.elv.sh/pkg/ui"
)

var (
	errMethodNotFound = &jsonrpc2.Error{
		Code: jsonrpc2.CodeMethodNotFound, Message: "method not found"}
	errInvalidParams = &jsonrpc2.Error{
		Code: jsonrpc2.CodeInvalidParams, Message: "invalid params"}
	errInvalidRequest = &jsonrpc2.Error{
		Code: jsonrpc2.CodeInvalidRequest, Message: "invalid request"}
)

type server struct {
	mu        sync.Mutex
	evaler    *eval.Evaler
	documents map[lsp.DocumentURI]document
	diagSeq   map[lsp.DocumentURI]int
	shutdown  bool
}

type document struct {
	code      string
	version   int
	parseTree parse.Tree
}

func newServer() *server {
	return &server{
		evaler:    eval.NewEvaler(),
		documents: make(map[lsp.DocumentURI]document),
		diagSeq:   make(map[lsp.DocumentURI]int),
	}
}

type method func(context.Context, json.RawMessage) (any, error)

func convertMethod[T any](f func(context.Context, T) (any, error)) method {
	return func(ctx context.Context, rawParams json.RawMessage) (any, error) {
		var params T
		if json.Unmarshal(rawParams, &params) != nil {
			return nil, errInvalidParams
		}
		return f(ctx, params)
	}
}

func noop(_ context.Context, _ json.RawMessage) (any, error) { return nil, nil }

func (s *server) shutdownHandler(_ context.Context, _ json.RawMessage) (any, error) {
	s.mu.Lock()
	s.shutdown = true
	s.mu.Unlock()
	return nil, nil
}

func (s *server) exitHandler(ctx context.Context, _ json.RawMessage) (any, error) {
	s.mu.Lock()
	shutdown := s.shutdown
	s.mu.Unlock()
	// exit terminates the server with or without a preceding shutdown; the
	// error only signals an abnormal (no-shutdown) exit.
	go conn(ctx).Close()
	if !shutdown {
		return nil, &jsonrpc2.Error{Code: 1, Message: "exit before shutdown"}
	}
	return nil, nil
}

type connKey struct{}

func routingHandler(s *server, methods map[string]method) jsonrpc2.Handler {
	return jsonrpc2.HandlerWithError(func(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (any, error) {
		if req.Method != "initialize" && req.Method != "initialized" && req.Method != "shutdown" && req.Method != "exit" {
			s.mu.Lock()
			shutdown := s.shutdown
			s.mu.Unlock()
			if shutdown {
				return nil, errInvalidRequest
			}
		}
		fn, ok := methods[req.Method]
		if !ok {
			return nil, errMethodNotFound
		}
		var params json.RawMessage
		if req.Params != nil {
			params = *req.Params
		}
		return fn(context.WithValue(ctx, connKey{}, conn), params)
	})
}

func handler(s *server) jsonrpc2.Handler {
	return routingHandler(s, map[string]method{
		"initialize":                     s.initialize,
		"initialized":                    noop,
		"shutdown":                       s.shutdownHandler,
		"exit":                           s.exitHandler,
		"textDocument/didOpen":           convertMethod(s.didOpen),
		"textDocument/didChange":         convertMethod(s.didChange),
		"textDocument/didClose":          convertMethod(s.didClose),
		"textDocument/hover":             convertMethod(s.hover),
		"textDocument/completion":        convertMethod(s.completion),
		"textDocument/definition":        convertMethod(s.definition),
		"textDocument/references":        convertMethod(s.references),
		"textDocument/documentSymbol":    convertMethod(s.documentSymbol),
		"workspace/symbol":               convertMethod(s.workspaceSymbol),
		"textDocument/prepareRename":     convertMethod(s.prepareRename),
		"textDocument/rename":            convertMethod(s.rename),
		"textDocument/documentHighlight": convertMethod(s.documentHighlight),

		// Called by clients even when server doesn't advertise support:
		// https://microsoft.github.io/language-server-protocol/specification#workspace_didChangeWatchedFiles
		"textDocument/formatting":         convertMethod(s.formatting),
		"textDocument/rangeFormatting":    convertMethod(s.rangeFormatting),
		"workspace/didChangeWatchedFiles": noop,
	})
}

// Can be used within handler implementations to recover the connection stored
// in the Context.
func conn(ctx context.Context) *jsonrpc2.Conn { return ctx.Value(connKey{}).(*jsonrpc2.Conn) }

// Handler implementations. These are all called synchronously.

func (s *server) initialize(_ context.Context, _ json.RawMessage) (any, error) {
	return &lsp.InitializeResult{
		Capabilities: lsp.ServerCapabilities{
			TextDocumentSync: &lsp.TextDocumentSyncOptions{
				OpenClose: true,
				Change:    lsp.TDSyncKindFull,
			},
			CompletionProvider:              &lsp.CompletionOptions{},
			HoverProvider:                   &lsp.HoverOptions{},
			DefinitionProvider:              &lsp.DefinitionRegistrationOptions{},
			ReferencesProvider:              &lsp.ReferenceRegistrationOptions{},
			DocumentSymbolProvider:          &lsp.DocumentSymbolRegistrationOptions{},
			WorkspaceSymbolProvider:         &lsp.WorkspaceSymbolRegistrationOptions{},
			RenameProvider:                  &lsp.RenameRegistrationOptions{RenameOptions: lsp.RenameOptions{PrepareProvider: true}},
			DocumentHighlightProvider:       &lsp.DocumentHighlightRegistrationOptions{},
			DocumentFormattingProvider:      &lsp.DocumentFormattingRegistrationOptions{},
			DocumentRangeFormattingProvider: &lsp.DocumentRangeFormattingRegistrationOptions{},
		},
		ServerInfo: lsp.ServerInfo{Name: "elvish"},
	}, nil
}

func (s *server) didOpen(ctx context.Context, params lsp.DidOpenTextDocumentParams) (any, error) {
	uri, content := params.TextDocument.URI, params.TextDocument.Text
	s.updateDocument(conn(ctx), uri, params.TextDocument.Version, content)
	return nil, nil
}

func (s *server) didChange(ctx context.Context, params lsp.DidChangeTextDocumentParams) (any, error) {
	if len(params.ContentChanges) == 0 {
		return nil, nil
	}
	// ContentChanges includes full text since the server is only advertised to
	// support that; see the initialize method.
	if params.ContentChanges[0].Range != nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "incremental change not supported"}
	}
	uri, content := params.TextDocument.URI, params.ContentChanges[0].Text
	s.updateDocument(conn(ctx), uri, params.TextDocument.Version, content)
	return nil, nil
}

func (s *server) formatting(_ context.Context, params lsp.DocumentFormattingParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	doc, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}

	formatted, err := formatSource(doc.code, params.Options)
	if err != nil || formatted == doc.code {
		return []lsp.TextEdit{}, nil
	}
	return []lsp.TextEdit{{
		Range:   lspRangeFromRange(doc.code, diag.Ranging{From: 0, To: len(doc.code)}),
		NewText: formatted,
	}}, nil
}

func (s *server) rangeFormatting(_ context.Context, params lsp.DocumentRangeFormattingParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	doc, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}

	formatted, err := formatSource(doc.code, params.Options)
	if err != nil || formatted == doc.code {
		return []lsp.TextEdit{}, nil
	}

	// Whole-document formatter MVP: return a full-document edit only when the
	// requested range intersects changed text. This keeps range formatting safe
	// and simple while whole-file normalization is the canonical behavior.
	start := lspPositionToIdx(doc.code, params.Range.Start)
	end := lspPositionToIdx(doc.code, params.Range.End)
	if end < start {
		start, end = end, start
	}
	if !intersectsChange(doc.code, formatted, start, end) {
		return []lsp.TextEdit{}, nil
	}
	return []lsp.TextEdit{{
		Range:   lspRangeFromRange(doc.code, diag.Ranging{From: 0, To: len(doc.code)}),
		NewText: formatted,
	}}, nil
}

func formatSource(code string, options lsp.FormattingOptions) (string, error) {
	opts := format.Options{
		TabSize:                options.TabSize,
		InsertSpaces:           options.InsertSpaces,
		TrimTrailingWhitespace: options.TrimTrailingWhitespace,
		InsertFinalNewline:     options.InsertFinalNewline,
		TrimFinalNewlines:      options.TrimFinalNewlines,
	}
	return format.Source(code, opts)
}

func intersectsChange(old, new string, start, end int) bool {
	if old == new {
		return false
	}
	prefix := commonPrefixLen(old, new)
	oldSuffixStart := len(old) - commonSuffixLen(old[prefix:], new[prefix:])
	if oldSuffixStart < prefix {
		oldSuffixStart = prefix
	}
	return start < oldSuffixStart && end > prefix
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func commonSuffixLen(a, b string) int {
	i := 0
	na, nb := len(a), len(b)
	for i < na && i < nb && a[na-1-i] == b[nb-1-i] {
		i++
	}
	return i
}

func (s *server) didClose(ctx context.Context, params lsp.DidCloseTextDocumentParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	delete(s.documents, uri)
	delete(s.diagSeq, uri)
	s.mu.Unlock()
	_ = conn(ctx).Notify(context.Background(), "textDocument/publishDiagnostics",
		lsp.PublishDiagnosticsParams{URI: uri, Diagnostics: []lsp.Diagnostic{}})
	return nil, nil
}

func (s *server) hover(_ context.Context, params lsp.TextDocumentPositionParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	document, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}
	pos := lspPositionToIdx(document.code, params.Position)

	p := np.Find(document.parseTree.Root, pos)
	var primary *parse.Primary
	if p.Match(np.Store(&primary)) && primary.Type == parse.Variable {
		sym, ok := symbolAt(document.parseTree.Root, pos)
		if !ok {
			return nil, nil
		}
		name := "$" + normalizeVarName(primary.Value)
		if sym.kind == symbolFunction {
			name = sym.name
		} else if def, ok := resolveDefinition(document.parseTree.Root, sym, pos); ok {
			if def.Type == parse.Variable {
				if base, ok := functionVarBase(normalizeVarName(def.Value)); ok {
					name = base
				} else {
					name = "$" + normalizeVarName(def.Value)
				}
			} else {
				name = "$" + normalizeVarName(def.Value)
			}
			if _, isParam := lambdaParamOwner(def); isParam || isVarDefinition(def, sym.name) {
				return nil, nil
			}
		}
		if markdown, err := doc.Source(name); err == nil {
			r := lspRangeFromRange(document.code, primary)
			return lsp.Hover{Contents: lsp.MarkupContent{Kind: lsp.MKMarkdown, Value: markdown}, Range: &r}, nil
		}
	}
	var expr np.SimpleExprData
	var form *parse.Form
	if p.Match(np.SimpleExpr(&expr, nil), np.Store(&form)) && form.Head == expr.Compound {
		scope := enclosingLambdaNode(expr.Compound)
		sym := symbolRef{kind: symbolCommand, name: expr.Value, scope: scope}
		if _, ok := resolveDefinition(document.parseTree.Root, symbolRef{kind: symbolFunction, name: expr.Value, scope: scope}, pos); ok {
			sym.kind = symbolFunction
		}
		if _, ok := resolveDefinition(document.parseTree.Root, sym, pos); ok {
			return nil, nil
		}
		if markdown, err := doc.Source(expr.Value); err == nil {
			r := lspRangeFromRange(document.code, expr.Compound)
			return lsp.Hover{Contents: lsp.MarkupContent{Kind: lsp.MKMarkdown, Value: markdown}, Range: &r}, nil
		}
	}
	return nil, nil
}

func (s *server) completion(_ context.Context, params lsp.CompletionParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	document, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}
	code := document.code
	result, err := complete.Complete(
		complete.CodeBuffer{
			Content: code,
			Dot:     lspPositionToIdx(code, params.Position)},
		s.evaler,
		complete.Config{},
	)

	if err != nil {
		return []lsp.CompletionItem{}, nil
	}

	lspItems := make([]lsp.CompletionItem, len(result.Items))
	lspRange := lspRangeFromRange(code, result.Replace)
	kind := completionKind(result.Name)
	for i, item := range result.Items {
		label := plainText(item.ToShow)
		if label == "" {
			label = item.ToInsert
		}
		lspItems[i] = lsp.CompletionItem{
			Label:            label,
			Kind:             kind,
			FilterText:       item.ToInsert,
			InsertTextFormat: lsp.ITFPlainText,
			TextEdit: &lsp.TextEdit{
				Range:   lspRange,
				NewText: item.ToInsert,
			},
		}
	}
	return lspItems, nil
}

func (s *server) definition(_ context.Context, params lsp.DefinitionParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	document, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}
	pos := lspPositionToIdx(document.code, params.Position)
	sym, ok := symbolAt(document.parseTree.Root, pos)
	if !ok {
		return nil, nil
	}
	def, ok := findDefinition(document.parseTree.Root, sym, pos)
	if !ok {
		return nil, nil
	}
	return lsp.Location{
		URI:   params.TextDocument.URI,
		Range: lspRangeFromRange(document.code, def),
	}, nil
}

type referenceParams struct {
	lsp.TextDocumentPositionParams
	Context lsp.ReferenceContext `json:"context"`
}

func (s *server) references(_ context.Context, params referenceParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	document, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}
	pos := lspPositionToIdx(document.code, params.Position)
	sym, ok := symbolAt(document.parseTree.Root, pos)
	if !ok {
		return []lsp.Location{}, nil
	}
	if sym.kind == symbolCommand {
		if _, ok := resolveDefinition(document.parseTree.Root, symbolRef{kind: symbolFunction, name: sym.name, scope: sym.scope}, pos); !ok {
			return []lsp.Location{}, nil
		}
	}
	ranges := findReferences(document.parseTree.Root, sym, params.Context.IncludeDeclaration, pos)
	locs := make([]lsp.Location, len(ranges))
	for i, r := range ranges {
		locs[i] = lsp.Location{URI: uri, Range: lspRangeFromRange(document.code, r)}
	}
	return locs, nil
}

func (s *server) documentSymbol(_ context.Context, params lsp.DocumentSymbolParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	document, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}
	return collectDocumentSymbols(document.code, document.parseTree.Root), nil
}

func (s *server) workspaceSymbol(_ context.Context, params lsp.WorkspaceSymbolParams) (any, error) {
	s.mu.Lock()
	docs := make([]lsp.DocumentURI, 0, len(s.documents))
	for uri := range s.documents {
		docs = append(docs, uri)
	}
	s.mu.Unlock()
	query := strings.ToLower(params.Query)
	var out []lsp.SymbolInformation
	for _, uri := range docs {
		s.mu.Lock()
		doc, ok := s.documents[uri]
		s.mu.Unlock()
		if !ok {
			continue
		}
		var walk func([]lsp.DocumentSymbol)
		walk = func(syms []lsp.DocumentSymbol) {
			for _, sym := range syms {
				if query == "" || strings.Contains(strings.ToLower(sym.Name), query) {
					out = append(out, lsp.SymbolInformation{
						Name:     sym.Name,
						Kind:     sym.Kind,
						Location: lsp.Location{URI: uri, Range: *sym.Range},
					})
				}
				walk(sym.Children)
			}
		}
		walk(collectDocumentSymbols(doc.code, doc.parseTree.Root))
	}
	return out, nil
}

func (s *server) prepareRename(_ context.Context, params lsp.PrepareRenameParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	document, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}
	pos := lspPositionToIdx(document.code, params.Position)
	sym, ok := renameTarget(document.parseTree.Root, pos)
	if !ok {
		return nil, nil
	}
	rg, ok := symbolRangeAt(document.parseTree.Root, pos, sym)
	if !ok {
		return nil, nil
	}
	return lspRangeFromRange(document.code, rg), nil
}

func (s *server) rename(_ context.Context, params lsp.RenameParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	document, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}
	pos := lspPositionToIdx(document.code, params.Position)
	sym, ok := renameTarget(document.parseTree.Root, pos)
	if !ok {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "cannot rename symbol"}
	}
	newName := normalizeRenameName(sym, params.NewName)
	if !isValidVariableName(newName) {
		return nil, &jsonrpc2.Error{
			Code: jsonrpc2.CodeInvalidParams, Message: "invalid variable name: " + params.NewName}
	}
	refs := findReferencePrimaries(document.parseTree.Root, sym, true, pos)
	edits := make([]lsp.TextEdit, 0, len(refs))
	for _, p := range refs {
		edits = append(edits, lsp.TextEdit{
			Range:   lspRangeFromRange(document.code, p.Range()),
			NewText: renameNewText(p, newName),
		})
	}
	return lsp.WorkspaceEdit{
		Changes: map[lsp.DocumentURI][]lsp.TextEdit{uri: edits},
	}, nil
}

func (s *server) documentHighlight(_ context.Context, params lsp.DocumentHighlightParams) (any, error) {
	uri := params.TextDocument.URI
	s.mu.Lock()
	document, ok := s.documents[uri]
	s.mu.Unlock()
	if !ok {
		return nil, unknownDocument(uri)
	}
	pos := lspPositionToIdx(document.code, params.Position)
	sym, ok := symbolAt(document.parseTree.Root, pos)
	if !ok {
		return []lsp.DocumentHighlight{}, nil
	}
	ranges := findReferences(document.parseTree.Root, sym, true, pos)
	highlights := make([]lsp.DocumentHighlight, len(ranges))
	for i, r := range ranges {
		highlights[i] = lsp.DocumentHighlight{Range: lspRangeFromRange(document.code, r)}
	}
	return highlights, nil
}

func (s *server) updateDocument(conn *jsonrpc2.Conn, uri lsp.DocumentURI, version int, code string) {
	tree, err := parse.Parse(parse.Source{Name: string(uri), Code: code}, parse.Config{})
	s.mu.Lock()
	s.documents[uri] = document{code: code, version: version, parseTree: tree}
	s.diagSeq[uri]++
	seq := s.diagSeq[uri]
	s.mu.Unlock()
	go func() {
		entries := parse.UnpackErrors(err)
		diags := make([]lsp.Diagnostic, len(entries))
		for i, err := range entries {
			diags[i] = lsp.Diagnostic{
				Range:    lspRangeFromRange(code, err),
				Severity: lsp.DSError,
				Source:   "parse",
				Message:  err.Message,
			}
		}
		s.mu.Lock()
		current := s.diagSeq[uri]
		s.mu.Unlock()
		if current != seq {
			return
		}
		_ = conn.Notify(context.Background(), "textDocument/publishDiagnostics",
			lsp.PublishDiagnosticsParams{URI: uri, Version: version, Diagnostics: diags})
	}()
}

func unknownDocument(uri lsp.DocumentURI) error {
	return &jsonrpc2.Error{
		Code:    jsonrpc2.CodeInvalidParams,
		Message: fmt.Sprintf("unknown document: %v", uri),
	}
}

func lspRangeFromRange(s string, r diag.Ranger) lsp.Range {
	rg := r.Range()
	return lsp.Range{
		Start: lspPositionFromIdx(s, rg.From),
		End:   lspPositionFromIdx(s, rg.To),
	}
}

func lspPositionToIdx(s string, pos lsp.Position) int {
	var idx int
	walkString(s, func(i int, p lsp.Position) bool {
		idx = i
		return p.Line < pos.Line || (p.Line == pos.Line && p.Character < pos.Character)
	})
	return idx
}

func lspPositionFromIdx(s string, idx int) lsp.Position {
	var pos lsp.Position
	walkString(s, func(i int, p lsp.Position) bool {
		pos = p
		return i < idx
	})
	return pos
}

// Generates (index, lspPosition) pairs in s, stopping if f returns false.
func walkString(s string, f func(i int, p lsp.Position) bool) {
	var p lsp.Position
	lastCR := false

	for i, r := range s {
		if !f(i, p) {
			return
		}
		switch {
		case r == '\r':
			p.Line++
			p.Character = 0
		case r == '\n':
			if lastCR {
				// Ignore \n if it's part of a \r\n sequence
			} else {
				p.Line++
				p.Character = 0
			}
		case r <= 0xFFFF:
			// Encoded in UTF-16 with one unit
			p.Character++
		default:
			// Encoded in UTF-16 with two units
			p.Character += 2
		}
		lastCR = r == '\r'
	}
	f(len(s), p)
}

func plainText(t ui.Text) string {
	var sb strings.Builder
	for _, seg := range t {
		sb.WriteString(seg.Text)
	}
	return sb.String()
}

func completionKind(name string) lsp.CompletionItemKind {
	switch name {
	case "command":
		return lsp.CIKFunction
	case "variable":
		return lsp.CIKVariable
	case "index":
		return lsp.CIKField
	case "redir":
		return lsp.CIKFile
	default:
		return lsp.CIKText
	}
}
