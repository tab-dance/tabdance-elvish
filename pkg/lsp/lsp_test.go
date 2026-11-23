package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/sourcegraph/jsonrpc2"
	lsp "pkg.nimblebun.works/go-lsp"
	"src.elv.sh/pkg/mods/doc"
	"src.elv.sh/pkg/must"
	"src.elv.sh/pkg/prog"
	"src.elv.sh/pkg/testutil"
)

var bgCtx = context.Background()

var diagTests = []struct {
	name      string
	text      string
	wantDiags []lsp.Diagnostic
}{
	{"empty", "", []lsp.Diagnostic{}},
	{"no error", "echo", []lsp.Diagnostic{}},
	{"single error", "$!", []lsp.Diagnostic{
		{
			Range: lsp.Range{
				Start: lsp.Position{Line: 0, Character: 1},
				End:   lsp.Position{Line: 0, Character: 2}},
			Severity: lsp.DSError, Source: "parse", Message: "should be variable name",
		},
	}},
	{"multi line with NL", "\n$!", []lsp.Diagnostic{
		{
			Range: lsp.Range{
				Start: lsp.Position{Line: 1, Character: 1},
				End:   lsp.Position{Line: 1, Character: 2}},
			Severity: lsp.DSError, Source: "parse", Message: "should be variable name",
		},
	}},
	{"multi line with CR", "\r$!", []lsp.Diagnostic{
		{
			Range: lsp.Range{
				Start: lsp.Position{Line: 1, Character: 1},
				End:   lsp.Position{Line: 1, Character: 2}},
			Severity: lsp.DSError, Source: "parse", Message: "should be variable name",
		},
	}},
	{"multi line with CRNL", "\r\n$!", []lsp.Diagnostic{
		{
			Range: lsp.Range{
				Start: lsp.Position{Line: 1, Character: 1},
				End:   lsp.Position{Line: 1, Character: 2}},
			Severity: lsp.DSError, Source: "parse", Message: "should be variable name",
		},
	}},
	{"text with code point beyond FFFF", "\U00010000 $!", []lsp.Diagnostic{
		{
			Range: lsp.Range{
				Start: lsp.Position{Line: 0, Character: 4},
				End:   lsp.Position{Line: 0, Character: 5}},
			Severity: lsp.DSError, Source: "parse", Message: "should be variable name",
		},
	}},
}

func TestDidOpenDiagnostics(t *testing.T) {
	f := setup(t)
	for _, test := range diagTests {
		t.Run(test.name, func(t *testing.T) {
			f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(test.text))
			checkDiag(t, f, diagParam(test.wantDiags))
		})
	}
}

func TestDidChangeDiagnostics(t *testing.T) {
	f := setup(t)
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(""))
	checkDiag(t, f, diagParam([]lsp.Diagnostic{}))

	for _, test := range diagTests {
		t.Run(test.name, func(t *testing.T) {
			f.conn.Notify(bgCtx, "textDocument/didChange", didChangeParams(test.text))
			checkDiag(t, f, diagParam(test.wantDiags))
		})
	}
}

var hoverTests = []struct {
	name string
	text string
	pos  lsp.Position

	wantHover lsp.Hover
}{
	{
		name: "command doc",
		text: "echo foo",
		pos:  lsp.Position{Line: 0, Character: 0},

		wantHover: hoverWith(must.OK1(doc.Source("echo")), lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 4}}),
	},
	{
		name: "variable doc",
		//     012345
		text: "echo $paths",
		pos:  lsp.Position{Line: 0, Character: 5},

		wantHover: hoverWith(must.OK1(doc.Source("$paths")), lsp.Range{Start: lsp.Position{Line: 0, Character: 5}, End: lsp.Position{Line: 0, Character: 11}}),
	},
	{
		name: "unknown command",
		text: "some-external",
		pos:  lsp.Position{Line: 0, Character: 0},

		wantHover: lsp.Hover{},
	},
	{
		name: "command at non-command position",
		//     012345678
		text: "echo echo",
		pos:  lsp.Position{Line: 0, Character: 5},

		wantHover: lsp.Hover{},
	},
}

func hoverWith(markdown string, r ...lsp.Range) lsp.Hover {
	h := lsp.Hover{Contents: lsp.MarkupContent{Kind: lsp.MKMarkdown, Value: markdown}}
	if len(r) > 0 {
		h.Range = &r[0]
	}
	return h
}

func TestHover(t *testing.T) {
	f := setup(t)

	for _, test := range hoverTests {
		t.Run(test.name, func(t *testing.T) {
			f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(test.text))
			request := lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
				Position:     test.pos,
			}
			var response lsp.Hover
			err := f.conn.Call(bgCtx, "textDocument/hover", request, &response)
			if err != nil {
				t.Errorf("got error %v", err)
			}
			if diff := cmp.Diff(test.wantHover, response); diff != "" {
				t.Errorf("response (-want +got):\n%s", diff)
			}
		})
	}
}

var completionTests = []struct {
	name     string
	text     string
	params   lsp.CompletionParams
	wantKind lsp.CompletionItemKind
}{
	{"command", "", completionParams(0, 0), lsp.CIKFunction},
	{"variable", "put $", completionParams(0, 5), lsp.CIKVariable},
	{"bad", "put [", completionParams(0, 5), 0},
}

func TestCompletion(t *testing.T) {
	f := setup(t)
	testutil.Setenv(t, "PATH", "")

	for _, test := range completionTests {
		t.Run(test.name, func(t *testing.T) {
			var items []lsp.CompletionItem
			f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(test.text))
			err := f.conn.Call(bgCtx, "textDocument/completion", test.params, &items)
			if err != nil {
				t.Errorf("got error %v", err)
			}
			if test.wantKind == 0 {
				if len(items) > 0 {
					t.Errorf("got %v items, want 0", len(items))
				}
			} else {
				if len(items) == 0 {
					t.Fatalf("got 0 items, want non-zero")
				}
				if items[0].Kind != test.wantKind {
					t.Errorf("got kind %v, want %v", items[0].Kind, test.wantKind)
				}
			}
		})
	}
}

func TestDefinition(t *testing.T) {
	f := setup(t)
	text := "fn foo { }\nfoo\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var got lsp.Location
	err := f.conn.Call(bgCtx, "textDocument/definition", lsp.DefinitionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 0},
		},
	}, &got)
	if err != nil {
		t.Fatalf("got error %v", err)
	}
	want := lsp.Location{URI: testURI, Range: lsp.Range{
		Start: lsp.Position{Line: 0, Character: 3},
		End:   lsp.Position{Line: 0, Character: 6},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestReferences(t *testing.T) {
	f := setup(t)
	text := "fn foo { }\nfoo\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var got []lsp.Location
	err := f.conn.Call(bgCtx, "textDocument/references", referenceParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 0},
		},
		Context: lsp.ReferenceContext{IncludeDeclaration: true},
	}, &got)
	if err != nil {
		t.Fatalf("got error %v", err)
	}
	want := []lsp.Location{
		{URI: testURI, Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 3}, End: lsp.Position{Line: 0, Character: 6}}},
		{URI: testURI, Range: lsp.Range{Start: lsp.Position{Line: 1, Character: 0}, End: lsp.Position{Line: 1, Character: 3}}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestDocumentSymbol(t *testing.T) {
	f := setup(t)
	text := "fn foo { }\nvar bar = 1\nset bar = 2\nfor x [a] { }\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var got []lsp.DocumentSymbol
	err := f.conn.Call(bgCtx, "textDocument/documentSymbol", lsp.DocumentSymbolParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
	}, &got)
	if err != nil {
		t.Fatalf("got error %v", err)
	}
	wantNames := []string{"foo", "bar", "bar", "x"}
	if len(got) != len(wantNames) {
		t.Fatalf("got %d symbols, want %d: %+v", len(got), len(wantNames), got)
	}
	for i, name := range wantNames {
		if got[i].Name != name {
			t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, name)
		}
	}
}

func TestReferencesExcludeDeclaration(t *testing.T) {
	f := setup(t)
	text := "fn foo { }\nfoo\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var got []lsp.Location
	err := f.conn.Call(bgCtx, "textDocument/references", referenceParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 0},
		},
		Context: lsp.ReferenceContext{IncludeDeclaration: false},
	}, &got)
	if err != nil {
		t.Fatalf("got error %v", err)
	}
	want := []lsp.Location{
		{URI: testURI, Range: lsp.Range{Start: lsp.Position{Line: 1, Character: 0}, End: lsp.Position{Line: 1, Character: 3}}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestVariableDefinitionAndReferences(t *testing.T) {
	f := setup(t)
	text := "var foo = 1\necho $foo\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var def lsp.Location
	err := f.conn.Call(bgCtx, "textDocument/definition", lsp.DefinitionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 6},
		},
	}, &def)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	wantDef := lsp.Location{URI: testURI, Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 4}, End: lsp.Position{Line: 0, Character: 7}}}
	if diff := cmp.Diff(wantDef, def); diff != "" {
		t.Errorf("definition (-want +got):\n%s", diff)
	}
	var refs []lsp.Location
	err = f.conn.Call(bgCtx, "textDocument/references", referenceParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 6},
		},
		Context: lsp.ReferenceContext{IncludeDeclaration: true},
	}, &refs)
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(refs), refs)
	}
}

func TestDidCloseRemovesDocument(t *testing.T) {
	f := setup(t)
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams("echo"))
	f.conn.Notify(bgCtx, "textDocument/didClose", lsp.DidCloseTextDocumentParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
	})
	checkDiag(t, f, lsp.PublishDiagnosticsParams{URI: testURI, Diagnostics: []lsp.Diagnostic{}})
	var resp lsp.Hover
	err := f.conn.Call(bgCtx, "textDocument/hover", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
		Position:     lsp.Position{Line: 0, Character: 0},
	}, &resp)
	if err == nil || err.Error() != unknownDocument(testURI).Error() {
		t.Errorf("got %v, want %v", err, unknownDocument(testURI))
	}
}

func TestWorkspaceSymbol(t *testing.T) {
	f := setup(t)
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams("fn foo { }\nvar bar = 1\n"))
	var got []lsp.SymbolInformation
	err := f.conn.Call(bgCtx, "workspace/symbol", lsp.WorkspaceSymbolParams{Query: "ba"}, &got)
	if err != nil {
		t.Fatalf("workspace/symbol: %v", err)
	}
	if len(got) != 1 || got[0].Name != "bar" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Location.URI != testURI {
		t.Errorf("got URI %q", got[0].Location.URI)
	}
}

func TestRename(t *testing.T) {
	f := setup(t)
	text := "fn foo { }\nfoo\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var prep lsp.Range
	err := f.conn.Call(bgCtx, "textDocument/prepareRename", lsp.PrepareRenameParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 0},
		},
	}, &prep)
	if err != nil {
		t.Fatalf("prepareRename: %v", err)
	}
	if prep.Start.Line != 1 || prep.Start.Character != 0 {
		t.Fatalf("got prep %+v", prep)
	}
	var edit lsp.WorkspaceEdit
	err = f.conn.Call(bgCtx, "textDocument/rename", lsp.RenameParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 0},
		},
		NewName: "bar",
	}, &edit)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	edits := edit.Changes[testURI]
	if len(edits) != 2 {
		t.Fatalf("got %d edits: %+v", len(edits), edits)
	}
	for _, e := range edits {
		if e.NewText != "bar" {
			t.Errorf("got edit %+v", e)
		}
	}
}

func TestRenameFunctionVariable(t *testing.T) {
	f := setup(t)
	text := "var foo~ = { }\nset foo~ = { }\nput $foo~\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var edit lsp.WorkspaceEdit
	err := f.conn.Call(bgCtx, "textDocument/rename", lsp.RenameParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 0, Character: 4},
		},
		NewName: "bar",
	}, &edit)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	edits := edit.Changes[testURI]
	if len(edits) != 3 {
		t.Fatalf("got %d edits: %+v", len(edits), edits)
	}
	for i, want := range []string{"bar~", "bar~", "$bar~"} {
		if edits[i].NewText != want {
			t.Errorf("edit[%d] = %+v, want NewText %q", i, edits[i], want)
		}
	}
}

func TestRenameLambdaParameter(t *testing.T) {
	f := setup(t)
	text := "fn external {|program| echo $program }\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var edit lsp.WorkspaceEdit
	err := f.conn.Call(bgCtx, "textDocument/rename", lsp.RenameParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 0, Character: 16},
		},
		NewName: "prog",
	}, &edit)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	edits := edit.Changes[testURI]
	if len(edits) != 2 {
		t.Fatalf("got %d edits: %+v", len(edits), edits)
	}
	for i, want := range []string{"prog", "$prog"} {
		if edits[i].NewText != want {
			t.Errorf("edit[%d] = %+v, want NewText %q", i, edits[i], want)
		}
	}
}

func TestRenameVariableFromUse(t *testing.T) {
	f := setup(t)
	text := "fn external {|one two| echo $one $two }\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var edit lsp.WorkspaceEdit
	err := f.conn.Call(bgCtx, "textDocument/rename", lsp.RenameParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 0, Character: 36},
		},
		NewName: "three",
	}, &edit)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	edits := edit.Changes[testURI]
	if len(edits) != 2 {
		t.Fatalf("got %d edits: %+v", len(edits), edits)
	}
	for i, want := range []string{"three", "$three"} {
		if edits[i].NewText != want {
			t.Errorf("edit[%d] = %+v, want NewText %q", i, edits[i], want)
		}
	}
}

func TestRenameVariableWithDollarNewName(t *testing.T) {
	f := setup(t)
	text := "fn external {|one two| echo $one $two }\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var edit lsp.WorkspaceEdit
	err := f.conn.Call(bgCtx, "textDocument/rename", lsp.RenameParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 0, Character: 36},
		},
		NewName: "$twofew",
	}, &edit)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	edits := edit.Changes[testURI]
	if len(edits) != 2 {
		t.Fatalf("got %d edits: %+v", len(edits), edits)
	}
	for i, want := range []string{"twofew", "$twofew"} {
		if edits[i].NewText != want {
			t.Errorf("edit[%d] = %+v, want NewText %q", i, edits[i], want)
		}
	}
}

func TestRenameFunctionVariableFromUse(t *testing.T) {
	f := setup(t)
	text := "var foo~ = { }\nput $foo~\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var prep lsp.Range
	err := f.conn.Call(bgCtx, "textDocument/prepareRename", lsp.PrepareRenameParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 5},
		},
	}, &prep)
	if err != nil {
		t.Fatalf("prepareRename: %v", err)
	}
	if prep.Start.Line != 1 || prep.Start.Character != 4 {
		t.Fatalf("got prep %+v", prep)
	}
	var edit lsp.WorkspaceEdit
	err = f.conn.Call(bgCtx, "textDocument/rename", lsp.RenameParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 5},
		},
		NewName: "bar",
	}, &edit)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	edits := edit.Changes[testURI]
	if len(edits) != 2 {
		t.Fatalf("got %d edits: %+v", len(edits), edits)
	}
}

func TestRenameLambdaOptionParameter(t *testing.T) {
	f := setup(t)
	text := "fn f {|&opt=x| echo $opt }\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var edit lsp.WorkspaceEdit
	err := f.conn.Call(bgCtx, "textDocument/rename", lsp.RenameParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 0, Character: 21},
		},
		NewName: "flag",
	}, &edit)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	edits := edit.Changes[testURI]
	if len(edits) != 2 {
		t.Fatalf("got %d edits: %+v", len(edits), edits)
	}
}

func TestDidChangeEmptyContentChanges(t *testing.T) {
	f := setup(t)
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams("echo"))
	f.conn.Notify(bgCtx, "textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: testURI}, Version: 2},
	})
	// Server should remain responsive.
	var got lsp.Hover
	err := f.conn.Call(bgCtx, "textDocument/hover", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
		Position:     lsp.Position{Line: 0, Character: 0},
	}, &got)
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
}

func TestDocumentHighlight(t *testing.T) {
	f := setup(t)
	text := "var foo = 1\necho $foo\n"
	f.conn.Notify(bgCtx, "textDocument/didOpen", didOpenParams(text))
	var got []lsp.DocumentHighlight
	err := f.conn.Call(bgCtx, "textDocument/documentHighlight", lsp.DocumentHighlightParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: 1, Character: 6},
		},
	}, &got)
	if err != nil {
		t.Fatalf("documentHighlight: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d highlights: %+v", len(got), got)
	}
}

func TestShutdownRejectsRequests(t *testing.T) {
	f := setup(t)
	var shutdownResp any
	err := f.conn.Call(bgCtx, "shutdown", nil, &shutdownResp)
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	err = f.conn.Call(bgCtx, "textDocument/hover", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
		Position:     lsp.Position{Line: 0, Character: 0},
	}, &lsp.Hover{})
	if err == nil || err.Error() != errInvalidRequest.Error() {
		t.Errorf("got %v, want %v", err, errInvalidRequest)
	}
	err = f.conn.Notify(bgCtx, "exit", nil)
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	select {
	case <-f.conn.DisconnectNotify():
	case <-time.After(testutil.Scaled(time.Second)):
		t.Fatalf("timed out waiting for disconnect after exit")
	}
}

var jsonrpcErrorTests = []struct {
	name    string
	method  string
	params  any
	wantErr error
}{
	{"unknown method", "unknown/method", struct{}{}, errMethodNotFound},
	{"invalid request type", "textDocument/didOpen", []int{}, errInvalidParams},
	{"unknown document to hover", "textDocument/hover",
		lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: "file://unknown"}},
		unknownDocument("file://unknown")},
	{"unknown document to completion", "textDocument/completion",
		lsp.CompletionParams{
			TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: "file://unknown"}}},
		unknownDocument("file://unknown")},
	{"unknown document to definition", "textDocument/definition",
		lsp.DefinitionParams{
			TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: "file://unknown"}}},
		unknownDocument("file://unknown")},
	{"unknown document to references", "textDocument/references",
		referenceParams{
			TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: "file://unknown"}}},
		unknownDocument("file://unknown")},
	{"unknown document to documentSymbol", "textDocument/documentSymbol",
		lsp.DocumentSymbolParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: "file://unknown"}},
		unknownDocument("file://unknown")},
}

func TestJSONRPCErrors(t *testing.T) {
	f := setup(t)
	for _, test := range jsonrpcErrorTests {
		t.Run(test.name, func(t *testing.T) {
			err := f.conn.Call(context.Background(), test.method, test.params, &struct{}{})
			if err.Error() != test.wantErr.Error() {
				t.Errorf("got error %v, want %v", err, test.wantErr)
			}
		})
	}
}

const testURI = "file:///foo"

func didOpenParams(text string) lsp.DidOpenTextDocumentParams {
	return lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{URI: testURI, Text: text}}
}

func didChangeParams(text string) lsp.DidChangeTextDocumentParams {
	return lsp.DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: testURI},
		},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{
			{Text: text},
		}}
}

func diagParam(diags []lsp.Diagnostic) lsp.PublishDiagnosticsParams {
	return lsp.PublishDiagnosticsParams{URI: testURI, Diagnostics: diags}
}

func checkDiag(t *testing.T, f *clientFixture, want lsp.PublishDiagnosticsParams) {
	t.Helper()
	select {
	case got := <-f.diags:
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	case <-time.After(testutil.Scaled(time.Second)):
		t.Errorf("time out")
	}
}

func completionParams(line, char int) lsp.CompletionParams {
	return lsp.CompletionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testURI},
			Position:     lsp.Position{Line: line, Character: char},
		},
	}
}

type clientFixture struct {
	conn  *jsonrpc2.Conn
	diags <-chan lsp.PublishDiagnosticsParams
}

func setup(t *testing.T) *clientFixture {
	r0, w0 := must.Pipe()
	r1, w1 := must.Pipe()

	// Run server
	done := make(chan struct{})
	go func() {
		prog.Run([3]*os.File{r0, w1, nil}, []string{"elvish", "-lsp"}, &Program{})
		close(done)
	}()
	t.Cleanup(func() { <-done })

	// Run client
	diags := make(chan lsp.PublishDiagnosticsParams, 100)
	client := client{diags}
	conn := jsonrpc2.NewConn(context.Background(),
		jsonrpc2.NewBufferedStream(transport{r1, w0}, jsonrpc2.VSCodeObjectCodec{}),
		client.handler())
	t.Cleanup(func() { conn.Close() })

	// LSP handshake
	err := conn.Call(context.Background(),
		"initialize", lsp.InitializeParams{}, &lsp.InitializeResult{})
	if err != nil {
		t.Errorf("got error %v, want nil", err)
	}
	err = conn.Notify(context.Background(), "initialized", struct{}{})
	if err != nil {
		t.Errorf("got error %v, want nil", err)
	}

	return &clientFixture{conn, diags}
}

type client struct {
	diags chan<- lsp.PublishDiagnosticsParams
}

func (c *client) handler() jsonrpc2.Handler {
	return routingHandler(&server{documents: map[lsp.DocumentURI]document{}, diagSeq: map[lsp.DocumentURI]int{}}, map[string]method{
		"textDocument/publishDiagnostics": c.publishDiagnostics,
	})
}

func (c *client) publishDiagnostics(_ context.Context, rawParams json.RawMessage) (any, error) {
	var params lsp.PublishDiagnosticsParams
	err := json.Unmarshal(rawParams, &params)
	if err != nil {
		panic(fmt.Sprintf("parse PublishDiagnosticsParams: %v", err))
	}
	c.diags <- params
	return nil, nil
}
