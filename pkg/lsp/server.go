package lsp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sourcegraph/jsonrpc2"
	lsp "pkg.nimblebun.works/go-lsp"
	"src.elv.sh/pkg/diag"
	"src.elv.sh/pkg/edit/complete"
	"src.elv.sh/pkg/eval"
	"src.elv.sh/pkg/mods/doc"
	"src.elv.sh/pkg/parse"
	"src.elv.sh/pkg/parse/np"
)

var (
	errMethodNotFound = &jsonrpc2.Error{
		Code: jsonrpc2.CodeMethodNotFound, Message: "method not found"}
	errInvalidParams = &jsonrpc2.Error{
		Code: jsonrpc2.CodeInvalidParams, Message: "invalid params"}
)

type server struct {
	evaler    *eval.Evaler
	documents map[lsp.DocumentURI]document
}

type document struct {
	code      string
	parseTree parse.Tree
	parseErr  error
}

func newServer() *server {
	return &server{eval.NewEvaler(), make(map[lsp.DocumentURI]document)}
}

func handler(s *server) jsonrpc2.Handler {
	return routingHandler(map[string]method{
		"initialize":                  s.initialize,
		"initialized":                 noop,
		"shutdown":                    noop,
		"exit":                        noop,
		"textDocument/didOpen":        convertMethod(s.didOpen),
		"textDocument/didChange":      convertMethod(s.didChange),
		"textDocument/didClose":       convertMethod(s.didClose),
		"textDocument/hover":          convertMethod(s.hover),
		"textDocument/completion":     convertMethod(s.completion),
		"textDocument/definition":     convertMethod(s.definition),
		"textDocument/references":     convertMethod(s.references),
		"textDocument/documentSymbol": convertMethod(s.documentSymbol),

		// Called by clients even when server doesn't advertise support:
		// https://microsoft.github.io/language-server-protocol/specification#workspace_didChangeWatchedFiles
		"workspace/didChangeWatchedFiles": noop,
	})
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

type connKey struct{}

func routingHandler(methods map[string]method) jsonrpc2.Handler {
	return jsonrpc2.HandlerWithError(func(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (any, error) {
		fn, ok := methods[req.Method]
		if !ok {
			return nil, errMethodNotFound
		}
		return fn(context.WithValue(ctx, connKey{}, conn), *req.Params)
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
			CompletionProvider:     &lsp.CompletionOptions{},
			HoverProvider:          &lsp.HoverOptions{},
			DefinitionProvider:     &lsp.DefinitionRegistrationOptions{},
			ReferencesProvider:     &lsp.ReferenceRegistrationOptions{},
			DocumentSymbolProvider: &lsp.DocumentSymbolRegistrationOptions{},
		},
		ServerInfo: lsp.ServerInfo{Name: "elvish"},
	}, nil
}

func (s *server) didOpen(ctx context.Context, params lsp.DidOpenTextDocumentParams) (any, error) {
	uri, content := params.TextDocument.URI, params.TextDocument.Text
	s.updateDocument(conn(ctx), uri, content)
	return nil, nil
}

func (s *server) didChange(ctx context.Context, params lsp.DidChangeTextDocumentParams) (any, error) {
	// ContentChanges includes full text since the server is only advertised to
	// support that; see the initialize method.
	uri, content := params.TextDocument.URI, params.ContentChanges[0].Text
	s.updateDocument(conn(ctx), uri, content)
	return nil, nil
}

func (s *server) didClose(_ context.Context, params lsp.DidCloseTextDocumentParams) (any, error) {
	delete(s.documents, params.TextDocument.URI)
	return nil, nil
}

func (s *server) hover(_ context.Context, params lsp.TextDocumentPositionParams) (any, error) {
	document, ok := s.documents[params.TextDocument.URI]
	if !ok {
		return nil, unknownDocument(params.TextDocument.URI)
	}
	pos := lspPositionToIdx(document.code, params.Position)

	p := np.Find(document.parseTree.Root, pos)
	// Try variable doc
	var primary *parse.Primary
	if p.Match(np.Store(&primary)) && primary.Type == parse.Variable {
		// TODO: Take shadowing into consideration.
		markdown, err := doc.Source("$" + primary.Value)
		if err == nil {
			return lsp.Hover{Contents: lsp.MarkupContent{Kind: lsp.MKMarkdown, Value: markdown}}, nil
		}
	}
	// Try command doc
	var expr np.SimpleExprData
	var form *parse.Form
	if p.Match(np.SimpleExpr(&expr, nil), np.Store(&form)) && form.Head == expr.Compound {
		// TODO: Take shadowing into consideration.
		markdown, err := doc.Source(expr.Value)
		if err == nil {
			return lsp.Hover{Contents: lsp.MarkupContent{Kind: lsp.MKMarkdown, Value: markdown}}, nil
		}
	}
	return nil, nil
}

func (s *server) completion(_ context.Context, params lsp.CompletionParams) (any, error) {
	document, ok := s.documents[params.TextDocument.URI]
	if !ok {
		return nil, unknownDocument(params.TextDocument.URI)
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
		lspItems[i] = lsp.CompletionItem{
			Label: item.ToInsert,
			Kind:  kind,
			TextEdit: &lsp.TextEdit{
				Range:   lspRange,
				NewText: item.ToInsert,
			},
		}
	}
	return lspItems, nil
}

func (s *server) definition(_ context.Context, params lsp.DefinitionParams) (any, error) {
	document, ok := s.documents[params.TextDocument.URI]
	if !ok {
		return nil, unknownDocument(params.TextDocument.URI)
	}
	pos := lspPositionToIdx(document.code, params.Position)
	sym, ok := symbolAt(document.parseTree.Root, pos)
	if !ok {
		return nil, nil
	}
	def, ok := findDefinition(document.parseTree.Root, sym)
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
	document, ok := s.documents[uri]
	if !ok {
		return nil, unknownDocument(uri)
	}
	pos := lspPositionToIdx(document.code, params.Position)
	sym, ok := symbolAt(document.parseTree.Root, pos)
	if !ok {
		return []lsp.Location{}, nil
	}
	ranges := findReferences(document.parseTree.Root, sym, params.Context.IncludeDeclaration)
	locs := make([]lsp.Location, len(ranges))
	for i, r := range ranges {
		locs[i] = lsp.Location{URI: uri, Range: lspRangeFromRange(document.code, r)}
	}
	return locs, nil
}

func (s *server) documentSymbol(_ context.Context, params lsp.DocumentSymbolParams) (any, error) {
	document, ok := s.documents[params.TextDocument.URI]
	if !ok {
		return nil, unknownDocument(params.TextDocument.URI)
	}
	return collectDocumentSymbols(document.code, document.parseTree.Root), nil
}

func (s *server) updateDocument(conn *jsonrpc2.Conn, uri lsp.DocumentURI, code string) {
	tree, err := parse.Parse(parse.Source{Name: string(uri), Code: code}, parse.Config{})
	s.documents[uri] = document{code, tree, err}
	go func() {
		// Convert the parse error to lsp.Diagnostic objects and publish them.
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
		conn.Notify(context.Background(), "textDocument/publishDiagnostics",
			lsp.PublishDiagnosticsParams{URI: uri, Diagnostics: diags})
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

type symbolKind int

const (
	symbolNone symbolKind = iota
	symbolVariable
	symbolFunction
	symbolCommand
)

type symbolRef struct {
	kind symbolKind
	name string
}

func symbolAt(root parse.Node, pos int) (symbolRef, bool) {
	p := np.Find(root, pos)
	var primary *parse.Primary
	if p.Match(np.Store(&primary)) {
		if primary.Type == parse.Variable && primary.Value != "" {
			return symbolRef{kind: symbolVariable, name: normalizeVarName(primary.Value)}, true
		}
	}
	var expr np.SimpleExprData
	var form *parse.Form
	if p.Match(np.SimpleExpr(&expr, nil), np.Store(&form)) && form.Head == expr.Compound {
		if expr.Value != "" {
			return symbolRef{kind: symbolCommand, name: expr.Value}, true
		}
	}
	// Also check if cursor is on a function/variable definition name.
	if len(p) > 0 {
		if c, ok := p[0].(*parse.Compound); ok {
			if s, ok := parseStringLiteral(c); ok {
				if fn := enclosingFnName(c); fn != "" && fn == s {
					return symbolRef{kind: symbolFunction, name: fn}, true
				}
			}
		}
	}
	return symbolRef{}, false
}

func normalizeVarName(name string) string {
	// Strip leading sigil like @ for rest vars.
	for len(name) > 0 && (name[0] == '@') {
		name = name[1:]
	}
	return name
}

func parseStringLiteral(n *parse.Compound) (string, bool) {
	if n == nil || len(n.Indexings) != 1 || len(n.Indexings[0].Indices) != 0 {
		return "", false
	}
	p := n.Indexings[0].Head
	switch p.Type {
	case parse.Bareword, parse.SingleQuoted, parse.DoubleQuoted:
		return p.Value, true
	default:
		return "", false
	}
}

func enclosingFnName(n parse.Node) string {
	for cur := parse.Parent(n); cur != nil; cur = parse.Parent(cur) {
		form, ok := cur.(*parse.Form)
		if !ok {
			continue
		}
		head, ok := parseStringLiteral(form.Head)
		if !ok || head != "fn" || len(form.Args) < 1 {
			continue
		}
		name, ok := parseStringLiteral(form.Args[0])
		if !ok {
			continue
		}
		return name
	}
	return ""
}

func findDefinition(root parse.Node, sym symbolRef) (diag.Ranging, bool) {
	var found *parse.Primary
	walkParse(root, func(n parse.Node) bool {
		primary, ok := n.(*parse.Primary)
		if !ok {
			return true
		}
		switch sym.kind {
		case symbolFunction, symbolCommand:
			if isFnDefinition(primary, sym.name) {
				found = primary
				return false
			}
			if sym.kind == symbolCommand && isVarDefinition(primary, sym.name) {
				found = primary
				return false
			}
		case symbolVariable:
			if isVarDefinition(primary, sym.name) {
				found = primary
				return false
			}
		}
		return true
	})
	if found != nil {
		return found.Range(), true
	}
	return diag.Ranging{}, false
}

func isFnDefinition(primary *parse.Primary, name string) bool {
	if !isStringPrimary(primary) || primary.Value != name {
		return false
	}
	compound, ok := parentCompound(primary)
	if !ok {
		return false
	}
	form, ok := parse.Parent(compound).(*parse.Form)
	if !ok || len(form.Args) < 1 || form.Args[0] != compound {
		return false
	}
	head, ok := parseStringLiteral(form.Head)
	return ok && head == "fn"
}

func isVarDefinition(primary *parse.Primary, name string) bool {
	if !isStringPrimary(primary) || normalizeVarName(primary.Value) != normalizeVarName(name) {
		return false
	}
	compound, ok := parentCompound(primary)
	if !ok {
		return false
	}
	form, ok := parse.Parent(compound).(*parse.Form)
	if !ok {
		return false
	}
	head, ok := parseStringLiteral(form.Head)
	if !ok {
		return false
	}
	switch head {
	case "var", "set", "tmp":
		for i, a := range form.Args {
			if a == compound && i < len(form.Args) {
				// Args before '=' are LHS in typical forms.
				if assignmentLHS(form.Args, i) {
					return true
				}
			}
		}
	case "for":
		return len(form.Args) > 0 && form.Args[0] == compound
	}
	return false
}

func assignmentLHS(args []*parse.Compound, idx int) bool {
	for i, a := range args {
		if s, ok := parseStringLiteral(a); ok && s == "=" {
			return idx < i
		}
	}
	return true
}

func isStringPrimary(p *parse.Primary) bool {
	return p.Type == parse.Bareword || p.Type == parse.SingleQuoted || p.Type == parse.DoubleQuoted
}

func parentCompound(primary *parse.Primary) (*parse.Compound, bool) {
	indexing, ok := parse.Parent(primary).(*parse.Indexing)
	if !ok {
		return nil, false
	}
	compound, ok := parse.Parent(indexing).(*parse.Compound)
	return compound, ok
}

func findReferences(root parse.Node, sym symbolRef, includeDeclaration bool) []diag.Ranging {
	var ranges []diag.Ranging
	walkParse(root, func(n parse.Node) bool {
		primary, ok := n.(*parse.Primary)
		if !ok {
			return true
		}
		switch sym.kind {
		case symbolVariable:
			if primary.Type == parse.Variable && normalizeVarName(primary.Value) == normalizeVarName(sym.name) {
				ranges = append(ranges, primary.Range())
				return true
			}
			if includeDeclaration && isVarDefinition(primary, sym.name) {
				ranges = append(ranges, primary.Range())
			}
		case symbolFunction, symbolCommand:
			if primary.Type == parse.Variable {
				return true
			}
			if isStringPrimary(primary) && primary.Value == sym.name {
				// command usage in head position or fn definition name
				if includeDeclaration && isFnDefinition(primary, sym.name) {
					ranges = append(ranges, primary.Range())
					return true
				}
				if compound, ok := parentCompound(primary); ok {
					if form, ok := parse.Parent(compound).(*parse.Form); ok && form.Head == compound {
						ranges = append(ranges, primary.Range())
					}
				}
			}
		}
		return true
	})
	return ranges
}

func collectDocumentSymbols(code string, root parse.Node) []lsp.DocumentSymbol {
	var symbols []lsp.DocumentSymbol
	walkParse(root, func(n parse.Node) bool {
		form, ok := n.(*parse.Form)
		if !ok {
			return true
		}
		head, ok := parseStringLiteral(form.Head)
		if !ok {
			return true
		}
		switch head {
		case "fn":
			if len(form.Args) < 1 {
				return true
			}
			name, ok := parseStringLiteral(form.Args[0])
			if !ok {
				return true
			}
			r := lspRangeFromRange(code, form)
			sel := lspRangeFromRange(code, form.Args[0])
			symbols = append(symbols, lsp.DocumentSymbol{
				Name:           name,
				Kind:           lsp.SKFunction,
				Range:          &r,
				SelectionRange: &sel,
			})
		case "var":
			for _, a := range varDefinitionArgs(form.Args) {
				name, ok := parseStringLiteral(a)
				if !ok {
					continue
				}
				r := lspRangeFromRange(code, a)
				symbols = append(symbols, lsp.DocumentSymbol{
					Name:           name,
					Kind:           lsp.SKVariable,
					Range:          &r,
					SelectionRange: &r,
				})
			}
		}
		return true
	})
	return symbols
}

func varDefinitionArgs(args []*parse.Compound) []*parse.Compound {
	var lhs []*parse.Compound
	for _, a := range args {
		if s, ok := parseStringLiteral(a); ok && s == "=" {
			break
		}
		lhs = append(lhs, a)
	}
	return lhs
}

func walkParse(n parse.Node, f func(parse.Node) bool) {
	if n == nil || !f(n) {
		return
	}
	for _, ch := range parse.Children(n) {
		walkParse(ch, f)
	}
}
