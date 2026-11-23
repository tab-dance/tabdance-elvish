package lsp

import (
	"strings"

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
				return symbolRef{kind: symbolFunction, name: base}, true
			}
			return symbolRef{kind: symbolVariable, name: name, scope: enclosingLambda(primary)}, true
		}
		if isStringPrimary(primary) {
			if _, ok := lambdaParamOwner(primary); ok {
				return symbolRef{kind: symbolVariable, name: normalizeVarName(primary.Value), scope: enclosingLambda(primary)}, true
			}
			if compound, ok := parentCompound(primary); ok {
				if form, ok := parse.Parent(compound).(*parse.Form); ok {
					head, _ := parseStringLiteral(form.Head)
					if head == "fn" && len(form.Args) > 0 && form.Args[0] == compound && primary.Value != "" {
						return symbolRef{kind: symbolFunction, name: primary.Value}, true
					}
					if isVarDefinition(primary, normalizeVarName(primary.Value)) {
						name := normalizeVarName(primary.Value)
						if base, ok := functionVarBase(name); ok {
							return symbolRef{kind: symbolFunction, name: base}, true
						}
						return symbolRef{kind: symbolVariable, name: name, scope: enclosingLambda(primary)}, true
					}
				}
			}
		}
	}
	var expr np.SimpleExprData
	var form *parse.Form
	if p.Match(np.SimpleExpr(&expr, nil), np.Store(&form)) && form.Head == expr.Compound {
		if expr.Value != "" {
			return symbolRef{kind: symbolCommand, name: expr.Value}, true
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

func renameTarget(root parse.Node, pos int) (symbolRef, bool) {
	sym, ok := symbolAt(root, pos)
	if !ok {
		return symbolRef{}, false
	}
	switch sym.kind {
	case symbolVariable:
		return canonicalSymbol(root, sym), true
	case symbolFunction:
		return canonicalSymbol(root, sym), true
	case symbolCommand:
		if _, ok := resolveDefinition(root, symbolRef{kind: symbolFunction, name: sym.name}, pos); ok {
			sym.kind = symbolFunction
			return canonicalSymbol(root, sym), true
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
		var found *parse.Primary
		walkParse(root, func(n parse.Node) bool {
			primary, ok := n.(*parse.Primary)
			if !ok {
				return true
			}
			if isFnDefinition(primary, sym.name) {
				if pos < 0 || primary.Range().From <= pos {
					found = primary
				}
			}
			return true
		})
		if found != nil {
			return found, true
		}
		return nil, false
	case symbolVariable:
		return resolveVarDefinition(root, sym)
	default:
		return nil, false
	}
}

func resolveVarDefinition(root parse.Node, sym symbolRef) (*parse.Primary, bool) {
	var candidates []*parse.Primary
	walkParse(root, func(n parse.Node) bool {
		primary, ok := n.(*parse.Primary)
		if !ok {
			return true
		}
		if isVarDefinition(primary, sym.name) || isLambdaParam(primary, sym.name) {
			candidates = append(candidates, primary)
		}
		return true
	})
	if len(candidates) == 0 {
		return nil, false
	}
	for scope := sym.scope; ; scope = enclosingLambda(scope) {
		for _, c := range candidates {
			if enclosingLambda(c) == scope {
				return c, true
			}
		}
		if scope == nil {
			break
		}
	}
	return candidates[0], true
}

func sameVarBinding(root parse.Node, sym symbolRef, primary *parse.Primary) bool {
	pos := primary.Range().From
	def, ok := resolveDefinition(root, sym, pos)
	if !ok {
		return sym.scope == nil || sym.scope == enclosingLambda(primary)
	}
	refSym := symbolRef{kind: sym.kind, name: sym.name, scope: enclosingLambda(primary)}
	refDef, ok := resolveDefinition(root, refSym, pos)
	if !ok {
		return false
	}
	return refDef == def
}

func isFnDefinition(primary *parse.Primary, name string) bool {
	if !isStringPrimary(primary) {
		return false
	}
	if primary.Value != name {
		if base, ok := functionVarBase(primary.Value); !ok || base != name {
			return false
		}
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
	if head == "fn" {
		return primary.Value == name
	}
	if head == "var" || head == "set" || head == "tmp" {
		base, ok := functionVarBase(primary.Value)
		return ok && base == name
	}
	return false
}

func lambdaParamOwner(primary *parse.Primary) (*parse.Primary, bool) {
	compound, ok := parentCompound(primary)
	if !ok {
		return nil, false
	}
	parent := parse.Parent(compound)
	if lambda, ok := parent.(*parse.Primary); ok && lambda.Type == parse.Lambda {
		for _, e := range lambda.Elements {
			if e == compound {
				return lambda, true
			}
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
	for cur := parse.Parent(primary); cur != nil; cur = parse.Parent(cur) {
		if p, ok := cur.(*parse.Primary); ok && p.Type == parse.Lambda {
			return p
		}
	}
	return nil
}
func isVarDefinition(primary *parse.Primary, name string) bool {
	if !isStringPrimary(primary) {
		return false
	}
	normName := normalizeVarName(name)
	normPrimary := normalizeVarName(primary.Value)
	if normPrimary != normName {
		base, ok := functionVarBase(normPrimary)
		if !ok || base != normName {
			return false
		}
	}
	if _, ok := lambdaParamOwner(primary); ok {
		return true
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
			if a == compound {
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

func findReferencePrimaries(root parse.Node, sym symbolRef, includeDeclaration bool) []*parse.Primary {
	sym = canonicalSymbol(root, sym)
	var primaries []*parse.Primary
	walkParse(root, func(n parse.Node) bool {
		primary, ok := n.(*parse.Primary)
		if !ok {
			return true
		}
		switch sym.kind {
		case symbolVariable:
			if isLambdaParam(primary, sym.name) {
				if includeDeclaration && sameVarBinding(root, sym, primary) {
					primaries = append(primaries, primary)
				}
				return true
			}
			if primary.Type == parse.Variable && normalizeVarName(primary.Value) == normalizeVarName(sym.name) {
				if sameVarBinding(root, sym, primary) {
					primaries = append(primaries, primary)
				}
				return true
			}
			if includeDeclaration && isVarDefinition(primary, sym.name) && sameVarBinding(root, sym, primary) {
				primaries = append(primaries, primary)
			}
		case symbolFunction, symbolCommand:
			if primary.Type == parse.Variable {
				if base, ok := functionVarBase(normalizeVarName(primary.Value)); ok && base == sym.name {
					primaries = append(primaries, primary)
				}
				return true
			}
			if isStringPrimary(primary) {
				val := primary.Value
				base, isFnVar := functionVarBase(val)
				if val == sym.name || (isFnVar && base == sym.name) {
					if isFnDefinition(primary, sym.name) {
						if includeDeclaration {
							primaries = append(primaries, primary)
						}
						return true
					}
					if compound, ok := parentCompound(primary); ok {
						if form, ok := parse.Parent(compound).(*parse.Form); ok && form.Head == compound {
							primaries = append(primaries, primary)
						}
					}
				}
			}
		}
		return true
	})
	return primaries
}

func canonicalSymbol(root parse.Node, sym symbolRef) symbolRef {
	if sym.kind == symbolVariable {
		def, ok := resolveVarDefinition(root, sym)
		if ok {
			sym.scope = enclosingLambda(def)
		}
	}
	if sym.kind == symbolCommand {
		if _, ok := resolveDefinition(root, symbolRef{kind: symbolFunction, name: sym.name}, -1); ok {
			sym.kind = symbolFunction
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
func findReferences(root parse.Node, sym symbolRef, includeDeclaration bool) []diag.Ranging {
	primaries := findReferencePrimaries(root, sym, includeDeclaration)
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
				name, ok := parseStringLiteral(a)
				if !ok {
					continue
				}
				r := lspRangeFromRange(code, a)
				symbols = append(symbols, lsp.DocumentSymbol{
					Name:           normalizeVarName(name),
					Kind:           lsp.SKVariable,
					Range:          &r,
					SelectionRange: &r,
				})
			}
		case "for":
			if len(form.Args) > 0 {
				if name, ok := parseStringLiteral(form.Args[0]); ok {
					r := lspRangeFromRange(code, form.Args[0])
					symbols = append(symbols, lsp.DocumentSymbol{
						Name:           normalizeVarName(name),
						Kind:           lsp.SKVariable,
						Range:          &r,
						SelectionRange: &r,
					})
				}
			}
		}
		return true
	})
	return symbols
}

func lambdaParamSymbols(code string, n *parse.Compound) []lsp.DocumentSymbol {
	primary, ok := compoundPrimary(n)
	if !ok || primary.Type != parse.Lambda {
		return nil
	}
	var syms []lsp.DocumentSymbol
	for _, e := range primary.Elements {
		name, ok := parseStringLiteral(e)
		if !ok {
			continue
		}
		r := lspRangeFromRange(code, e)
		syms = append(syms, lsp.DocumentSymbol{Name: normalizeVarName(name), Kind: lsp.SKVariable, Range: &r, SelectionRange: &r})
	}
	for _, mp := range primary.MapPairs {
		name, ok := parseStringLiteral(mp.Key)
		if !ok {
			continue
		}
		r := lspRangeFromRange(code, mp.Key)
		syms = append(syms, lsp.DocumentSymbol{Name: normalizeVarName(name), Kind: lsp.SKVariable, Range: &r, SelectionRange: &r})
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
