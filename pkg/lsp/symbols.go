package lsp

import (
	"slices"
	"strings"
	"unicode"

	lsp "pkg.nimblebun.works/go-lsp"
	"src.elv.sh/pkg/diag"
	"src.elv.sh/pkg/parse"
	"src.elv.sh/pkg/parse/np"
)

type symbolKind int

const (
	symbolNone symbolKind = iota
	symbolVariable
	symbolFunction
	symbolCommand
)

type symbolRef struct {
	kind  symbolKind
	name  string
	scope *parse.Primary
}

func symbolAt(root parse.Node, pos int) (symbolRef, bool) {
	p := np.Find(root, pos)
	var primary *parse.Primary
	if p.Match(np.Store(&primary)) {
		if primary.Type == parse.Variable && primary.Value != "" {
			name := normalizeVarName(primary.Value)
			if base, ok := functionVarBase(name); ok {
				return symbolRef{kind: symbolFunction, name: base, scope: enclosingLambda(primary)}, true
			}
			return symbolRef{kind: symbolVariable, name: name, scope: enclosingLambda(primary)}, true
		}
		if isStringPrimary(primary) {
			if _, ok := lambdaParamOwner(primary); ok {
				return symbolRef{kind: symbolVariable, name: normalizeVarName(primary.Value), scope: enclosingLambda(primary)}, true
			}
			if compound, ok := parentCompound(primary); ok {
				if form, ok := parse.Parent(compound).(*parse.Form); ok {
					if head, ok := parseStringLiteral(form.Head); ok && head == "fn" &&
						len(form.Args) > 0 && form.Args[0] == compound && primary.Value != "" {
						return symbolRef{kind: symbolFunction, name: primary.Value, scope: enclosingLambda(primary)}, true
					}
					if isVarDefinition(primary, normalizeVarName(primary.Value)) {
						name := normalizeVarName(primary.Value)
						if base, ok := functionVarBase(name); ok {
							return symbolRef{kind: symbolFunction, name: base, scope: declarationScope(primary)}, true
						}
						return symbolRef{kind: symbolVariable, name: name, scope: declarationScope(primary)}, true
					}
				}
			}
		}
	}
	var expr np.SimpleExprData
	var form *parse.Form
	if p.Match(np.SimpleExpr(&expr, nil), np.Store(&form)) && form.Head == expr.Compound {
		if expr.Value != "" {
			return symbolRef{kind: symbolCommand, name: expr.Value, scope: enclosingLambdaNode(expr.Compound)}, true
		}
	}
	return symbolRef{}, false
}

func functionVarBase(name string) (string, bool) {
	if strings.HasSuffix(name, "~") && len(name) > 1 {
		return strings.TrimSuffix(name, "~"), true
	}
	return "", false
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

func renameTarget(root parse.Node, pos int) (symbolRef, bool) {
	sym, ok := symbolAt(root, pos)
	if !ok {
		return symbolRef{}, false
	}
	switch sym.kind {
	case symbolVariable:
		return canonicalSymbol(root, sym, pos), true
	case symbolFunction:
		return canonicalSymbol(root, sym, pos), true
	case symbolCommand:
		if _, ok := resolveDefinition(root, symbolRef{kind: symbolFunction, name: sym.name, scope: sym.scope}, pos); ok {
			sym.kind = symbolFunction
			return canonicalSymbol(root, sym, pos), true
		}
		return symbolRef{}, false
	default:
		return symbolRef{}, false
	}
}
func symbolRangeAt(root parse.Node, pos int, sym symbolRef) (diag.Ranging, bool) {
	p := np.Find(root, pos)
	var primary *parse.Primary
	if !p.Match(np.Store(&primary)) {
		return diag.Ranging{}, false
	}
	switch sym.kind {
	case symbolVariable:
		if primary.Type == parse.Variable && normalizeVarName(primary.Value) == normalizeVarName(sym.name) {
			return primary.Range(), true
		}
		if isVarDefinition(primary, sym.name) || isLambdaParam(primary, sym.name) {
			return primary.Range(), true
		}
	case symbolFunction, symbolCommand:
		if primary.Type == parse.Variable {
			name := normalizeVarName(primary.Value)
			if base, ok := functionVarBase(name); ok && base == sym.name {
				return primary.Range(), true
			}
		}
		if isFnDefinition(primary, sym.name) || (isStringPrimary(primary) && primary.Value == sym.name) {
			return primary.Range(), true
		}
	}
	return diag.Ranging{}, false
}

func findDefinition(root parse.Node, sym symbolRef, pos int) (diag.Ranging, bool) {
	def, ok := resolveDefinition(root, sym, pos)
	if !ok {
		return diag.Ranging{}, false
	}
	return def.Range(), true
}

func resolveDefinition(root parse.Node, sym symbolRef, pos int) (*parse.Primary, bool) {
	switch sym.kind {
	case symbolFunction, symbolCommand:
		var latest *parse.Primary
		walkParse(root, func(n parse.Node) bool {
			primary, ok := n.(*parse.Primary)
			if !ok {
				return true
			}
			if !isFunctionDeclaration(primary, sym.name) {
				return true
			}
			// A function definition is only visible from scopes that enclose it,
			// mirroring the compiler's scope chain.
			if !inScopeChain(sym.scope, declarationScope(primary)) {
				return true
			}
			if from := primary.Range().From; from <= pos && (latest == nil || from > latest.Range().From) {
				latest = primary
			}
			return true
		})
		return latest, latest != nil
	case symbolVariable:
		return resolveVarDefinition(root, sym, pos)
	default:
		return nil, false
	}
}

func inScopeChain(from, target *parse.Primary) bool {
	for scope := from; ; scope = enclosingLambda(scope) {
		if scope == target {
			return true
		}
		if scope == nil {
			break
		}
	}
	return false
}

func resolveVarDefinition(root parse.Node, sym symbolRef, pos int) (*parse.Primary, bool) {
	var candidates []*parse.Primary
	walkParse(root, func(n parse.Node) bool {
		primary, ok := n.(*parse.Primary)
		if !ok {
			return true
		}
		if isVarDeclaration(primary, sym.name) {
			candidates = append(candidates, primary)
		}
		return true
	})
	if len(candidates) == 0 {
		return nil, false
	}
	// Elvish resolves variables sequentially as forms are compiled, so only
	// declarations at or before the reference position take effect.
	for scope := sym.scope; ; scope = enclosingLambda(scope) {
		var latest *parse.Primary
		for _, c := range candidates {
			if from := c.Range().From; declarationScope(c) == scope && from <= pos &&
				(latest == nil || from > latest.Range().From) {
				latest = c
			}
		}
		if latest != nil {
			return latest, true
		}
		if scope == nil {
			break
		}
	}
	// No declaration precedes the reference anywhere in the scope chain; fall
	// back to the earliest declaration visible from the chain.
	for scope := sym.scope; ; scope = enclosingLambda(scope) {
		var earliest *parse.Primary
		for _, c := range candidates {
			if from := c.Range().From; declarationScope(c) == scope &&
				(earliest == nil || from < earliest.Range().From) {
				earliest = c
			}
		}
		if earliest != nil {
			return earliest, true
		}
		if scope == nil {
			break
		}
	}
	return nil, false
}

// bindingEquals reports whether the variable-name occurrence primary refers
// to the same binding as target, resolving the occurrence at its own position.
func bindingEquals(root parse.Node, name string, primary *parse.Primary, target *parse.Primary) bool {
	occSym := symbolRef{kind: symbolVariable, name: name, scope: declarationScope(primary)}
	def, ok := resolveVarDefinition(root, occSym, primary.Range().From)
	return ok && def == target
}

func sameFunctionBinding(root parse.Node, sym symbolRef, primary *parse.Primary, target *parse.Primary) bool {
	pos := primary.Range().From
	def, ok := resolveDefinition(root, sym, pos)
	if !ok {
		return false
	}
	return def == target
}

func isFnDefinition(primary *parse.Primary, name string) bool {
	return isFnFormDefinition(primary, name) || isFunctionVarDefinition(primary, name)
}

func isFunctionDeclaration(primary *parse.Primary, name string) bool {
	return isFnFormDefinition(primary, name) || isFunctionVarDeclaration(primary, name)
}

func isFnFormDefinition(primary *parse.Primary, name string) bool {
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

func isFunctionVarDefinition(primary *parse.Primary, name string) bool {
	return functionVarOccurrence(primary, name, false)
}

func isFunctionVarDeclaration(primary *parse.Primary, name string) bool {
	return functionVarOccurrence(primary, name, true)
}

func functionVarOccurrence(primary *parse.Primary, name string, declarationOnly bool) bool {
	if !isStringPrimary(primary) {
		return false
	}
	base, ok := functionVarBase(primary.Value)
	if !ok || base != name {
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
	if !ok {
		return false
	}
	if declarationOnly {
		return head == "var"
	}
	return head == "var" || head == "set" || head == "tmp"
}

func lambdaParamOwner(primary *parse.Primary) (*parse.Primary, bool) {
	compound, ok := parentCompound(primary)
	if !ok {
		return nil, false
	}
	parent := parse.Parent(compound)
	if lambda, ok := parent.(*parse.Primary); ok && lambda.Type == parse.Lambda {
		if slices.Contains(lambda.Elements, compound) {
			return lambda, true
		}
		return nil, false
	}
	if mp, ok := parent.(*parse.MapPair); ok && mp.Key == compound {
		if lambda, ok := parse.Parent(mp).(*parse.Primary); ok && lambda.Type == parse.Lambda {
			return lambda, true
		}
	}
	return nil, false
}

func isLambdaParam(primary *parse.Primary, name string) bool {
	if !isStringPrimary(primary) || normalizeVarName(primary.Value) != normalizeVarName(name) {
		return false
	}
	_, ok := lambdaParamOwner(primary)
	return ok
}

func enclosingLambda(primary *parse.Primary) *parse.Primary {
	if primary == nil {
		return nil
	}
	return enclosingLambdaNode(primary)
}

func enclosingLambdaNode(n parse.Node) *parse.Primary {
	for cur := parse.Parent(n); cur != nil; cur = parse.Parent(cur) {
		if p, ok := cur.(*parse.Primary); ok && p.Type == parse.Lambda {
			return p
		}
	}
	return nil
}
func isVarDefinition(primary *parse.Primary, name string) bool {
	_, _, ok := varOccurrence(primary, name)
	return ok
}

// isVarDeclaration reports whether the primary creates a new binding for
// name: a lambda parameter, a var LHS, or a for loop variable. set and tmp
// only assign existing variables and are not declarations.
func isVarDeclaration(primary *parse.Primary, name string) bool {
	declares, _, ok := varOccurrence(primary, name)
	return ok && declares
}

// declarationScope returns the lambda (or nil for the top level) that a
// variable occurrence's binding lives in. For a simple for loop variable that
// is the loop body lambda, mirroring the scope the compiler creates for it.
func declarationScope(primary *parse.Primary) *parse.Primary {
	if _, scope, ok := varOccurrenceInfo(primary); ok {
		return scope
	}
	return enclosingLambda(primary)
}

func matchesVarName(primary *parse.Primary, name string) bool {
	if !isStringPrimary(primary) {
		return false
	}
	normPrimary := normalizeVarName(primary.Value)
	normName := normalizeVarName(name)
	if normPrimary == normName {
		return true
	}
	base, ok := functionVarBase(normPrimary)
	return ok && base == normName
}

func varOccurrence(primary *parse.Primary, name string) (bool, *parse.Primary, bool) {
	if !matchesVarName(primary, name) {
		return false, nil, false
	}
	return varOccurrenceInfo(primary)
}

func varOccurrenceInfo(primary *parse.Primary) (bool, *parse.Primary, bool) {
	if lambda, ok := lambdaParamOwner(primary); ok {
		return true, lambda, true
	}
	compound, ok := parentCompound(primary)
	if !ok {
		return false, nil, false
	}
	cur := parse.Node(compound)
	for parent := parse.Parent(cur); parent != nil; cur, parent = parent, parse.Parent(parent) {
		form, isForm := parent.(*parse.Form)
		if !isForm {
			switch parent.(type) {
			case *parse.Indexing, *parse.Compound, *parse.Primary, *parse.Chunk, *parse.Pipeline, *parse.Sep:
				continue
			default:
				return false, nil, false
			}
		}
		head, ok := parseStringLiteral(form.Head)
		if !ok {
			continue
		}
		switch head {
		case "var":
			for i, a := range form.Args {
				if a == cur || nodeContains(a, compound) {
					return assignmentLHS(form.Args, i), enclosingLambda(primary), true
				}
			}
			return false, nil, false
		case "set", "tmp":
			// Assignments to existing variables, not declarations.
			for _, a := range form.Args {
				if a == cur || nodeContains(a, compound) {
					return false, enclosingLambda(primary), true
				}
			}
			return false, nil, false
		case "for":
			if len(form.Args) > 0 && (form.Args[0] == cur || nodeContains(form.Args[0], compound)) {
				if form.Args[0] == compound {
					if p, ok := compoundPrimary(compound); ok && p.Type == parse.Bareword {
						if body := forBodyScope(form); body != nil {
							return true, body, true
						}
					}
				}
				// A destructured loop variable is compiled into the enclosing
				// scope, not the loop body scope.
				return true, enclosingLambda(primary), true
			}
			return false, nil, false
		default:
			// Unrelated form (e.g. a nested command inside a destructuring
			// LHS); keep walking up to the enclosing form.
			continue
		}
	}
	return false, nil, false
}

func forBodyScope(form *parse.Form) *parse.Primary {
	if len(form.Args) > 2 {
		if body, ok := compoundLambda(form.Args[2]); ok {
			return body
		}
	}
	return nil
}

func compoundLambda(n *parse.Compound) (*parse.Primary, bool) {
	p, ok := compoundPrimary(n)
	if !ok || p.Type != parse.Lambda {
		return nil, false
	}
	return p, true
}

func nodeContains(parent, child parse.Node) bool {
	found := false
	walkParse(parent, func(n parse.Node) bool {
		if n == child {
			found = true
			return false
		}
		return true
	})
	return found
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

func findReferencePrimaries(root parse.Node, sym symbolRef, includeDeclaration bool, pos int) []*parse.Primary {
	sym = canonicalSymbol(root, sym, pos)
	var targetFn *parse.Primary
	var targetVar *parse.Primary
	if sym.kind == symbolFunction || sym.kind == symbolCommand {
		targetFn, _ = resolveDefinition(root, sym, pos)
	} else if sym.kind == symbolVariable {
		targetVar, _ = resolveVarDefinition(root, sym, pos)
	}
	var primaries []*parse.Primary
	walkParse(root, func(n parse.Node) bool {
		primary, ok := n.(*parse.Primary)
		if !ok {
			return true
		}
		switch sym.kind {
		case symbolVariable:
			if targetVar == nil {
				return true
			}
			if isLambdaParam(primary, sym.name) {
				if includeDeclaration && bindingEquals(root, sym.name, primary, targetVar) {
					primaries = append(primaries, primary)
				}
				return true
			}
			if primary.Type == parse.Variable && normalizeVarName(primary.Value) == normalizeVarName(sym.name) {
				if bindingEquals(root, sym.name, primary, targetVar) {
					primaries = append(primaries, primary)
				}
				return true
			}
			if includeDeclaration && isVarDefinition(primary, sym.name) && bindingEquals(root, sym.name, primary, targetVar) {
				primaries = append(primaries, primary)
			}
		case symbolFunction, symbolCommand:
			if targetFn == nil {
				return true
			}
			if primary.Type == parse.Variable {
				if base, ok := functionVarBase(normalizeVarName(primary.Value)); ok && base == sym.name {
					if sameFunctionBinding(root, sym, primary, targetFn) {
						primaries = append(primaries, primary)
					}
				}
				return true
			}
			if isStringPrimary(primary) {
				val := primary.Value
				base, isFnVar := functionVarBase(val)
				if val == sym.name || (isFnVar && base == sym.name) {
					if isFnDefinition(primary, sym.name) {
						if includeDeclaration && sameFunctionBinding(root, sym, primary, targetFn) {
							primaries = append(primaries, primary)
						}
						return true
					}
					if compound, ok := parentCompound(primary); ok {
						if form, ok := parse.Parent(compound).(*parse.Form); ok && form.Head == compound {
							if sameFunctionBinding(root, sym, primary, targetFn) {
								primaries = append(primaries, primary)
							}
						}
					}
				}
			}
		}
		return true
	})
	return primaries
}

func canonicalSymbol(root parse.Node, sym symbolRef, pos int) symbolRef {
	switch sym.kind {
	case symbolVariable:
		if def, ok := resolveVarDefinition(root, sym, pos); ok {
			sym.scope = declarationScope(def)
		}
	case symbolCommand:
		if _, ok := resolveDefinition(root, symbolRef{kind: symbolFunction, name: sym.name, scope: sym.scope}, pos); ok {
			sym.kind = symbolFunction
		}
	case symbolFunction:
		if def, ok := resolveDefinition(root, sym, pos); ok {
			sym.scope = declarationScope(def)
		}
	}
	return sym
}

func normalizeRenameName(sym symbolRef, newName string) string {
	if sym.kind == symbolVariable {
		for strings.HasPrefix(newName, "$") || strings.HasPrefix(newName, "@") {
			newName = newName[1:]
		}
		if base, ok := functionVarBase(newName); ok {
			newName = base
		}
		return newName
	}
	if sym.kind == symbolFunction {
		newName = strings.TrimPrefix(newName, "$")
		if base, ok := functionVarBase(newName); ok {
			return base
		}
	}
	return newName
}

// isValidVariableName reports whether name is a valid unquoted variable name,
// following the same character rules as the parser.
func isValidVariableName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 0x80 && unicode.IsPrint(r)) && !('0' <= r && r <= '9') &&
			!('a' <= r && r <= 'z') && !('A' <= r && r <= 'Z') &&
			r != '-' && r != '_' && r != ':' && r != '~' {
			return false
		}
	}
	return true
}
func renameNewText(p *parse.Primary, newName string) string {
	if p.Type == parse.Variable {
		name := p.Value
		i := 0
		for i < len(name) && name[i] == '@' {
			i++
		}
		suffix := ""
		if strings.HasSuffix(name, "~") {
			suffix = "~"
		}
		return "$" + name[:i] + newName + suffix
	}
	if isStringPrimary(p) && strings.HasSuffix(p.Value, "~") {
		return newName + "~"
	}
	return newName
}
func findReferences(root parse.Node, sym symbolRef, includeDeclaration bool, pos int) []diag.Ranging {
	primaries := findReferencePrimaries(root, sym, includeDeclaration, pos)
	ranges := make([]diag.Ranging, len(primaries))
	for i, p := range primaries {
		ranges[i] = p.Range()
	}
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
			sym := lsp.DocumentSymbol{
				Name:           name,
				Kind:           lsp.SKFunction,
				Range:          &r,
				SelectionRange: &sel,
			}
			if len(form.Args) > 1 {
				sym.Children = lambdaParamSymbols(code, form.Args[1])
			}
			symbols = append(symbols, sym)
		case "var", "set", "tmp":
			for _, a := range varDefinitionArgs(form.Args) {
				symbols = append(symbols, varDefinitionSymbols(code, a)...)
			}
		case "for":
			if len(form.Args) > 0 {
				symbols = append(symbols, varDefinitionSymbols(code, form.Args[0])...)
			}
		}
		return true
	})
	return symbols
}

func varDefinitionSymbols(code string, n *parse.Compound) []lsp.DocumentSymbol {
	if n == nil {
		return nil
	}
	var syms []lsp.DocumentSymbol
	walkParse(n, func(node parse.Node) bool {
		primary, ok := node.(*parse.Primary)
		if !ok || !isStringPrimary(primary) {
			return true
		}
		if !isVarDefinition(primary, normalizeVarName(primary.Value)) {
			return true
		}
		r := lspRangeFromRange(code, primary.Range())
		syms = append(syms, lsp.DocumentSymbol{
			Name:           normalizeVarName(primary.Value),
			Kind:           lsp.SKVariable,
			Range:          &r,
			SelectionRange: &r,
		})
		return true
	})
	return syms
}

func lambdaParamSymbols(code string, n *parse.Compound) []lsp.DocumentSymbol {
	primary, ok := compoundPrimary(n)
	if !ok || primary.Type != parse.Lambda {
		return nil
	}
	var syms []lsp.DocumentSymbol
	for _, e := range primary.Elements {
		syms = append(syms, varDefinitionSymbols(code, e)...)
	}
	for _, mp := range primary.MapPairs {
		syms = append(syms, varDefinitionSymbols(code, mp.Key)...)
	}
	return syms
}

func compoundPrimary(n *parse.Compound) (*parse.Primary, bool) {
	if n == nil || len(n.Indexings) != 1 || len(n.Indexings[0].Indices) != 0 {
		return nil, false
	}
	return n.Indexings[0].Head, true
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
