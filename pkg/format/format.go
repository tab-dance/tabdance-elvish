package format

import (
	"sort"
	"strings"

	"src.elv.sh/pkg/parse"
)

// Options controls formatting behavior. Options mirror LSP FormattingOptions.
type Options struct {
	TabSize                int
	InsertSpaces           bool
	TrimTrailingWhitespace bool
	InsertFinalNewline     bool
	TrimFinalNewlines      bool
}

// Source formats Elvish source code. It returns an error if the source cannot
// be parsed. On parse errors the caller should avoid mutating the original
// source.
func Source(src string, opts Options) (string, error) {
	if src == "" {
		if opts.InsertFinalNewline {
			return "\n", nil
		}
		return "", nil
	}
	opts = normalizeOptions(opts)
	tree, err := parse.Parse(parse.Source{Name: "[format]", Code: src}, parse.Config{})
	if err != nil {
		return "", err
	}

	formatted := formatWithSeps(src, tree.Root, opts)
	formatted = applyFinalNewline(formatted, opts)

	if _, err := parse.Parse(parse.Source{Name: "[format]", Code: formatted}, parse.Config{}); err != nil {
		return src, nil
	}
	return formatted, nil
}

func normalizeOptions(opts Options) Options {
	// Conservative defaults for non-empty files when client sends all-false options.
	if !opts.TrimTrailingWhitespace && !opts.InsertFinalNewline && !opts.TrimFinalNewlines {
		opts.TrimTrailingWhitespace = true
		opts.InsertFinalNewline = true
	}
	return opts
}

type sepInfo struct {
	sep    *parse.Sep
	parent parse.Node
}

func formatWithSeps(src string, root parse.Node, opts Options) string {
	seps := collectSeps(root)
	sort.Slice(seps, func(i, j int) bool { return seps[i].sep.Range().From < seps[j].sep.Range().From })

	var out strings.Builder
	out.Grow(len(src))
	last := 0
	for _, si := range seps {
		sep := si.sep
		r := sep.Range()
		if r.From < last || r.To > len(src) {
			continue
		}
		if last < r.From {
			out.WriteString(src[last:r.From])
		}
		out.WriteString(formatSep(src, r.From, r.To, parse.SourceText(sep), opts, si.parent))
		last = r.To
	}
	if last < len(src) {
		out.WriteString(src[last:])
	}
	return out.String()
}

func collectSeps(n parse.Node) []sepInfo {
	var seps []sepInfo
	var walk func(parse.Node)
	walk = func(n parse.Node) {
		for _, ch := range parse.Children(n) {
			if s, ok := ch.(*parse.Sep); ok {
				seps = append(seps, sepInfo{sep: s, parent: n})
			}
			walk(ch)
		}
	}
	walk(n)
	return seps
}

func formatSep(src string, sepStart, sepEnd int, text string, opts Options, parent parse.Node) string {
	// Context-aware normalization for a few safe separator tokens.
	if isPipeSep(text) {
		return formatPipeSep(src, sepStart, sepEnd, parent)
	}
	if isRedirSignSep(text) {
		return formatRedirSignSep(src, sepStart, sepEnd, text)
	}
	if isEqualsSep(text, parent) {
		return formatEqualsSep(src, sepStart, sepEnd)
	}
	if out, ok := formatContainerPadding(src, sepStart, sepEnd, text, opts, parent); ok {
		return out
	}

	var out strings.Builder
	out.Grow(len(text))

	for i := 0; i < len(text); {
		c := text[i]
		if c == '#' {
			for i < len(text) && text[i] != '\n' && text[i] != '\r' {
				out.WriteByte(text[i])
				i++
			}
			continue
		}
		if c == '^' && i+1 < len(text) && (text[i+1] == '\n' || text[i+1] == '\r') {
			out.WriteByte('^')
			i++
			if text[i] == '\r' {
				out.WriteByte('\r')
				i++
				if i < len(text) && text[i] == '\n' {
					out.WriteByte('\n')
					i++
				}
			} else {
				out.WriteByte('\n')
				i++
			}
			continue
		}
		if c == '\r' || c == '\n' {
			if opts.TrimTrailingWhitespace {
				trimTrailingHorizontal(&out)
			}
			if c == '\r' {
				out.WriteByte('\r')
				i++
				if i < len(text) && text[i] == '\n' {
					out.WriteByte('\n')
					i++
				}
			} else {
				out.WriteByte('\n')
				i++
			}
			continue
		}
		if c == ' ' || c == '\t' {
			j := i
			for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
				j++
			}
			if atLineIndent(src, sepStart, text, i) {
				out.WriteString(text[i:j])
			} else if shouldKeepSpace(src, sepStart, sepEnd, text, j, parent) && !endsWithSpace(&out) {
				out.WriteByte(' ')
			} else if !opts.TrimTrailingWhitespace {
				out.WriteString(text[i:j])
			}
			i = j
			continue
		}
		out.WriteByte(c)
		i++
	}

	if opts.TrimTrailingWhitespace && sepEndsAtLineEnd(src, sepEnd) {
		trimTrailingHorizontal(&out)
	}
	return out.String()
}

func isPipeSep(text string) bool { return text == "|" }

func isRedirSignSep(text string) bool {
	return text == ">" || text == "<" || text == ">>" || text == "<>"
}

func formatPipeSep(src string, sepStart, sepEnd int, parent parse.Node) string {
	if isLambdaParamBar(src, sepStart, parent) {
		if isLambdaParamCloseBar(src, sepStart) && hasImmediateNonNewlineContent(src, sepEnd) {
			return "| "
		}
		return "|"
	}
	prefix := ""
	suffix := ""
	if hasNonNewlineContentBefore(src, sepStart) {
		prefix = " "
	}
	if hasNonNewlineContentAfter(src, sepEnd) {
		suffix = " "
	}
	return prefix + "|" + suffix
}

func isLambdaParamBar(src string, sepStart int, parent parse.Node) bool {
	p, ok := parent.(*parse.Primary)
	if !ok || p.Type != parse.Lambda {
		return false
	}
	open, close, ok := lambdaParamBarRange(src, p)
	return ok && (sepStart == open || sepStart == close)
}

func isLambdaParamCloseBar(src string, barPos int) bool {
	if barPos < 0 || barPos >= len(src) || src[barPos] != '|' {
		return false
	}
	// Scan left for matching opening bar after '{' on same line.
	for i := barPos - 1; i >= 0; i-- {
		c := src[i]
		if c == '|' {
			// Check this is lambda param opening bar: preceded by '{' or whitespace after '{'
			for j := i - 1; j >= 0; j-- {
				d := src[j]
				if d == ' ' || d == '\t' {
					continue
				}
				return d == '{'
			}
			return false
		}
		if c == '\n' || c == '\r' {
			return false
		}
	}
	return false
}

func formatRedirSignSep(src string, sepStart, sepEnd int, text string) string {
	prefix := ""
	suffix := ""
	if hasNonNewlineContentBefore(src, sepStart) && !isLeftFdBefore(src, sepStart) {
		prefix = " "
	}
	if hasNonNewlineContentAfter(src, sepEnd) && !nextIsAmpFd(src, sepEnd) {
		suffix = " "
	}
	return prefix + text + suffix
}

func isEqualsSep(text string, parent parse.Node) bool {
	if text != "=" {
		return false
	}
	_, ok := parent.(*parse.MapPair)
	return ok
}

func formatEqualsSep(src string, sepStart, sepEnd int) string {
	_ = src
	_ = sepStart
	_ = sepEnd
	return "="
}

func formatContainerPadding(src string, sepStart, sepEnd int, text string, opts Options, parent parse.Node) (string, bool) {
	p, ok := parent.(*parse.Primary)
	if !ok {
		return "", false
	}
	switch p.Type {
	case parse.List, parse.Map, parse.Braced, parse.Lambda:
	default:
		return "", false
	}
	if !isPureHorizontalWhitespace(text) {
		return "", false
	}
	r := p.Range()
	if p.Type == parse.Lambda {
		if out, ok := formatLambdaParamPadding(src, sepStart, sepEnd, text, p); ok {
			return out, true
		}
		// Whitespace sep between '{' and lambda '|' should be removed.
		if sepStart == r.From+1 {
			open, _, ok := lambdaParamBarRange(src, p)
			if ok && sepEnd <= open {
				return "", true
			}
		}
	}
	if sepStart == r.From+1 {
		if hasImmediateNonNewlineContent(src, sepEnd) {
			if p.Type == parse.Lambda && src[sepEnd] == '|' {
				return "", true
			}
			return " ", true
		}
		return "", true
	}
	if sepEnd == r.To-1 {
		if hasImmediateNonNewlineContentBefore(src, sepStart) {
			if p.Type == parse.Lambda && !opts.TrimTrailingWhitespace {
				return text, true
			}
			return " ", true
		}
		return "", true
	}
	return "", false
}

func formatLambdaParamPadding(src string, sepStart, sepEnd int, text string, p *parse.Primary) (string, bool) {
	open, close, ok := lambdaParamBarRange(src, p)
	if !ok {
		return "", false
	}
	// No space between '{' and opening '|' in lambda params.
	if sepEnd == open && sepStart == p.Range().From+1 {
		return "", true
	}
	if sepStart == p.Range().From+1 && sepEnd > open {
		trimmed := strings.TrimLeft(text, " \t")
		if strings.HasPrefix(trimmed, "|") {
			return trimmed, true
		}
	}
	// No space inside parameter bars: {|x|} not { | x | }.
	if sepStart == open+1 && sepEnd <= close && hasImmediateNonNewlineContent(src, sepEnd) {
		return "", true
	}
	if sepEnd == close && sepStart >= open+1 && hasImmediateNonNewlineContentBefore(src, sepStart) {
		return "", true
	}
	return "", false
}

func lambdaParamBarRange(src string, p *parse.Primary) (open, close int, ok bool) {
	r := p.Range()
	open = r.From + 1
	for open < r.To && open < len(src) && (src[open] == ' ' || src[open] == '\t') {
		open++
	}
	if open >= r.To || open >= len(src) || src[open] != '|' {
		return 0, 0, false
	}
	for i := open + 1; i < r.To && i < len(src); i++ {
		if src[i] == '|' {
			return open, i, true
		}
		if src[i] == '\n' || src[i] == '\r' {
			return 0, 0, false
		}
	}
	return 0, 0, false
}

func isPureHorizontalWhitespace(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] != ' ' && text[i] != '\t' {
			return false
		}
	}
	return true
}

func hasImmediateNonNewlineContent(src string, pos int) bool {
	if pos >= len(src) {
		return false
	}
	c := src[pos]
	return c != '\n' && c != '\r' && c != ' ' && c != '\t'
}

func hasImmediateNonNewlineContentBefore(src string, pos int) bool {
	if pos <= 0 {
		return false
	}
	c := src[pos-1]
	return c != '\n' && c != '\r' && c != ' ' && c != '\t'
}

func hasNonNewlineContentBefore(src string, pos int) bool {
	for i := pos - 1; i >= 0; i-- {
		c := src[i]
		if c == '\n' || c == '\r' {
			return false
		}
		if c == ' ' || c == '\t' {
			continue
		}
		return true
	}
	return false
}

func hasNonNewlineContentAfter(src string, pos int) bool {
	for i := pos; i < len(src); i++ {
		c := src[i]
		if c == '\n' || c == '\r' {
			return false
		}
		if c == ' ' || c == '\t' {
			continue
		}
		return true
	}
	return false
}

func prevTokenByte(src string, sepStart int, text string, i int) (bool, byte) {
	_ = i
	for p := sepStart - 1; p >= 0; p-- {
		c := src[p]
		if c == ' ' || c == '\t' {
			continue
		}
		return true, c
	}
	return false, 0
}

func nextTokenByte(src string, sepEnd int, text string, j int) (bool, byte) {
	for k := j; k < len(text); k++ {
		c := text[k]
		if c == ' ' || c == '\t' {
			continue
		}
		return true, c
	}
	for p := sepEnd; p < len(src); p++ {
		c := src[p]
		if c == ' ' || c == '\t' {
			continue
		}
		return true, c
	}
	return false, 0
}

func isNewlineByte(b byte) bool { return b == '\n' || b == '\r' }

func atLineIndent(src string, sepStart int, text string, i int) bool {
	if i > 0 {
		prev := text[i-1]
		return prev == '\n' || prev == '\r'
	}
	if sepStart == 0 {
		return true
	}
	prev := src[sepStart-1]
	return prev == '\n' || prev == '\r'
}

func sepEndsAtLineEnd(src string, sepEnd int) bool {
	return sepEnd >= len(src) || src[sepEnd] == '\n' || src[sepEnd] == '\r'
}

func shouldKeepSpace(src string, sepStart, sepEnd int, text string, j int, parent parse.Node) bool {
	hasPrev, prev := prevTokenByte(src, sepStart, text, 0)
	hasNext, next := nextTokenByte(src, sepEnd, text, j)
	if !hasPrev || !hasNext || isNewlineByte(prev) || isNewlineByte(next) {
		return false
	}
	if prev == '|' {
		// Keep space after lambda parameter closing bar: {|x| echo}
		if isLambdaParamCloseBar(src, sepStart-1) {
			return true
		}
		return false
	}
	if next == '|' || next == '>' || next == '<' {
		return false
	}
	if prev == '>' || prev == '<' {
		return false
	}
	if _, ok := parent.(*parse.MapPair); ok {
		if prev == '=' || next == '=' {
			return false
		}
	}
	return true
}

func nextIsAmpFd(src string, sepEnd int) bool {
	for i := sepEnd; i < len(src); i++ {
		c := src[i]
		if c == ' ' || c == '\t' {
			continue
		}
		return c == '&'
	}
	return false
}

func isLeftFdBefore(src string, sepStart int) bool {
	i := sepStart - 1
	if i < 0 {
		return false
	}
	if src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r' {
		return false
	}
	for i >= 0 && src[i] >= '0' && src[i] <= '9' {
		i--
	}
	if i == sepStart-1 {
		return false
	}
	if i < 0 {
		return true
	}
	prev := src[i]
	return prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r'
}

func trimTrailingHorizontal(b *strings.Builder) {
	s := b.String()
	n := len(s)
	for n > 0 && (s[n-1] == ' ' || s[n-1] == '\t') {
		n--
	}
	if n == len(s) {
		return
	}
	b.Reset()
	b.WriteString(s[:n])
}

func endsWithSpace(b *strings.Builder) bool {
	s := b.String()
	return len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t')
}

func applyFinalNewline(src string, opts Options) string {
	if src == "" {
		if opts.InsertFinalNewline {
			return "\n"
		}
		return ""
	}

	eol := "\n"
	if strings.Contains(src, "\r\n") {
		eol = "\r\n"
	} else if strings.Contains(src, "\r") {
		eol = "\r"
	}

	if opts.TrimFinalNewlines {
		src = strings.TrimRight(src, "\r\n")
		if opts.InsertFinalNewline && src != "" {
			src += eol
		}
		return src
	}
	if opts.InsertFinalNewline && !strings.HasSuffix(src, "\n") && !strings.HasSuffix(src, "\r") {
		src += eol
	}
	return src
}
