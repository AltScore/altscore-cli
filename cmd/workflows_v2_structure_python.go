package cmd

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A deliberately tiny reading of a custom variable's Python. It recognises only the shapes
// a deep path or a literal can replace (re-expose one dependency, walk it with .get/[...],
// cast it, or return a literal). Anything else -- an operator, a branch, a call, a second
// dependency -- is "transform" and never flagged: a miss is cheaper than calling a real
// indicator replaceable.

type pyShapeKind int

const (
	shapeTransform pyShapeKind = iota
	shapeEmpty
	shapeConstant
	shapePassthrough
	shapeExtract
)

type pyShape struct {
	kind pyShapeKind
	lit  string
	root string
	segs []string
	cast string
}

func (s pyShape) deepPath() string {
	return s.root + strings.Join(s.segs, "")
}

type pyTokKind int

const (
	pyName pyTokKind = iota
	pyNumber
	pyString
	pyOp
	pyEnd
)

type pyTok struct {
	kind pyTokKind
	text string
	// f-strings and strings with escapes: their value is not their text.
	opaque bool
}

var pyNumberRe = regexp.MustCompile(`^(?:0[xXoObB][0-9a-fA-F_]+|[0-9][0-9_]*\.?[0-9_]*(?:[eE][+-]?[0-9]+)?[jJ]?|\.[0-9][0-9_]*(?:[eE][+-]?[0-9]+)?[jJ]?)`)

var pyOps3 = []string{"**=", "//=", ">>=", "<<=", "..."}
var pyOps2 = []string{"==", "!=", "<=", ">=", "->", "**", "//", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", ":=", "<<", ">>"}

func tokenizePython(src string) ([]pyTok, bool) {
	rs := []rune(src)
	var out []pyTok
	depth := 0
	end := func() {
		if len(out) > 0 && out[len(out)-1].kind != pyEnd {
			out = append(out, pyTok{kind: pyEnd})
		}
	}
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case c == '\n' || c == '\r':
			if depth == 0 {
				end()
			}
			i++
		case c == ' ' || c == '\t' || c == '\f':
			i++
		case c == '\\' && i+1 < len(rs) && (rs[i+1] == '\n' || rs[i+1] == '\r'):
			i += 2
		case c == '#':
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case pyStringStart(rs, i):
			tok, n, ok := scanPyString(rs, i)
			if !ok {
				return nil, false
			}
			out = append(out, tok)
			i += n
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j]) || rs[j] == '_') {
				j++
			}
			out = append(out, pyTok{kind: pyName, text: string(rs[i:j])})
			i = j
		case unicode.IsDigit(c) || (c == '.' && i+1 < len(rs) && unicode.IsDigit(rs[i+1])):
			window := rs[i:]
			if len(window) > 64 {
				window = window[:64]
			}
			m := pyNumberRe.FindString(string(window))
			if m == "" {
				return nil, false
			}
			out = append(out, pyTok{kind: pyNumber, text: m})
			i += utf8.RuneCountInString(m)
		case c == ';' && depth == 0:
			end()
			i++
		default:
			op := string(c)
			for _, cands := range [][]string{pyOps3, pyOps2} {
				matched := false
				for _, cand := range cands {
					if strings.HasPrefix(string(rs[i:min(len(rs), i+len(cand))]), cand) {
						op, matched = cand, true
						break
					}
				}
				if matched {
					break
				}
			}
			switch op {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
				if depth < 0 {
					return nil, false
				}
			}
			out = append(out, pyTok{kind: pyOp, text: op})
			i += utf8.RuneCountInString(op)
		}
	}
	end()
	return out, depth == 0
}

func pyStringStart(rs []rune, i int) bool {
	j := i
	for j < len(rs) && j-i < 2 && strings.ContainsRune("rRbBuUfF", rs[j]) {
		j++
	}
	return j < len(rs) && (rs[j] == '\'' || rs[j] == '"')
}

func scanPyString(rs []rune, i int) (pyTok, int, bool) {
	j := i
	opaque := false
	for rs[j] != '\'' && rs[j] != '"' {
		if rs[j] == 'f' || rs[j] == 'F' {
			opaque = true
		}
		j++
	}
	q := rs[j]
	triple := j+2 < len(rs) && rs[j+1] == q && rs[j+2] == q
	start := j + 1
	if triple {
		start = j + 3
	}
	for k := start; k < len(rs); k++ {
		switch {
		case rs[k] == '\\':
			opaque = true
			k++
		case !triple && rs[k] == '\n':
			return pyTok{}, 0, false
		case rs[k] == q && (!triple || (k+2 < len(rs) && rs[k+1] == q && rs[k+2] == q)):
			stop := k + 1
			if triple {
				stop = k + 3
			}
			return pyTok{kind: pyString, text: string(rs[start:k]), opaque: opaque}, stop - i, true
		}
	}
	return pyTok{}, 0, false
}

type pyValKind int

const (
	pvConst pyValKind = iota
	pvPath
	pvInputs
)

type pyVal struct {
	kind pyValKind
	lit  string
	root string
	segs []string
	cast string
}

type pyEval struct {
	toks  []pyTok
	pos   int
	env   map[string]pyVal
	reads map[string]bool
}

var pyKeywords = map[string]bool{
	"and": true, "or": true, "not": true, "if": true, "else": true, "elif": true, "for": true,
	"while": true, "in": true, "is": true, "lambda": true, "def": true, "class": true, "try": true,
	"except": true, "finally": true, "with": true, "as": true, "return": true, "yield": true,
	"import": true, "from": true, "pass": true, "global": true, "nonlocal": true, "del": true,
	"assert": true, "raise": true, "break": true, "continue": true, "await": true, "async": true,
}

var pyCasts = map[string]bool{"str": true, "int": true, "float": true, "bool": true}

// A segment a deep path can carry: SOURCE_IDs keep their hyphens, a space or a dot cannot.
var pyPathKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (e *pyEval) peek(off int) (pyTok, bool) {
	if e.pos+off < len(e.toks) {
		return e.toks[e.pos+off], true
	}
	return pyTok{}, false
}

func (e *pyEval) peekOp(off int, op string) bool {
	t, ok := e.peek(off)
	return ok && t.kind == pyOp && t.text == op
}

func (e *pyEval) peekName(off int, name string) bool {
	t, ok := e.peek(off)
	return ok && t.kind == pyName && t.text == name
}

func (e *pyEval) parseAll(toks []pyTok) (pyVal, bool) {
	e.toks, e.pos = toks, 0
	v, ok := e.expr()
	return v, ok && e.pos == len(e.toks)
}

func (e *pyEval) expr() (pyVal, bool) {
	left, ok := e.primary()
	if !ok {
		return left, false
	}
	for e.peekName(0, "or") {
		e.pos++
		right, ok := e.primary()
		if !ok {
			return right, false
		}
		if left, ok = combinePyOr(left, right); !ok {
			return left, false
		}
	}
	return left, true
}

// `x or <default>` keeps x: the fallback is a default, not a second value.
func combinePyOr(a, b pyVal) (pyVal, bool) {
	switch {
	case a.kind == pvInputs || b.kind == pvInputs:
		return a, false
	case b.kind == pvConst:
		return a, true
	case a.kind == pvConst:
		return b, true
	case a.root == b.root && a.cast == b.cast && strings.Join(a.segs, "") == strings.Join(b.segs, ""):
		return a, true
	}
	return a, false
}

func (e *pyEval) primary() (pyVal, bool) {
	v, ok := e.atom()
	if !ok {
		return v, false
	}
	for {
		switch {
		case e.peekOp(0, "."):
			if !e.peekName(1, "get") || !e.peekOp(2, "(") {
				return v, false
			}
			e.pos += 3
			key, ok := e.next()
			if !ok || key.kind != pyString || key.opaque {
				return v, false
			}
			if e.peekOp(0, ",") {
				e.pos++
				if !e.defaultArg() {
					return v, false
				}
			}
			if !e.peekOp(0, ")") {
				return v, false
			}
			e.pos++
			if v, ok = e.index(v, key); !ok {
				return v, false
			}
		case e.peekOp(0, "["):
			e.pos++
			key, ok := e.next()
			if !ok || (key.kind != pyString && key.kind != pyNumber) || key.opaque || !e.peekOp(0, "]") {
				return v, false
			}
			e.pos++
			if v, ok = e.index(v, key); !ok {
				return v, false
			}
		case e.peekOp(0, "("):
			return v, false
		default:
			return v, true
		}
	}
}

func (e *pyEval) next() (pyTok, bool) {
	t, ok := e.peek(0)
	if ok {
		e.pos++
	}
	return t, ok
}

func (e *pyEval) index(v pyVal, key pyTok) (pyVal, bool) {
	switch v.kind {
	case pvInputs:
		// A dependency key with no dot is a flat name whose namespace cannot be told apart.
		if key.kind != pyString || !strings.Contains(key.text, ".") {
			return v, false
		}
		e.reads[key.text] = true
		return pyVal{kind: pvPath, root: key.text}, true
	case pvPath:
		if v.cast != "" {
			return v, false
		}
		seg := ""
		switch {
		case key.kind == pyString && pyPathKeyRe.MatchString(key.text):
			seg = "." + key.text
		case key.kind == pyNumber:
			n, err := strconv.Atoi(key.text)
			if err != nil || n < 0 {
				return v, false
			}
			seg = "[" + key.text + "]"
		default:
			return v, false
		}
		return pyVal{kind: pvPath, root: v.root, segs: append(append([]string{}, v.segs...), seg)}, true
	}
	return v, false
}

func (e *pyEval) defaultArg() bool {
	t, ok := e.next()
	if !ok {
		return false
	}
	switch {
	case t.kind == pyString || t.kind == pyNumber:
		return true
	case t.kind == pyName:
		return t.text == "None" || t.text == "True" || t.text == "False"
	case t.kind == pyOp && t.text == "-":
		n, ok := e.next()
		return ok && n.kind == pyNumber
	case t.kind == pyOp && t.text == "{":
		return e.peekOp(0, "}") && e.skip()
	case t.kind == pyOp && t.text == "[":
		return e.peekOp(0, "]") && e.skip()
	}
	return false
}

func (e *pyEval) skip() bool {
	e.pos++
	return true
}

func (e *pyEval) atom() (pyVal, bool) {
	t, ok := e.next()
	if !ok {
		return pyVal{}, false
	}
	switch t.kind {
	case pyName:
		switch {
		case t.text == "None" || t.text == "True" || t.text == "False":
			return pyVal{kind: pvConst, lit: t.text}, true
		case t.text == "inputs":
			return pyVal{kind: pvInputs}, true
		case pyCasts[t.text] && e.peekOp(0, "("):
			e.pos++
			inner, ok := e.expr()
			if !ok || !e.peekOp(0, ")") {
				return inner, false
			}
			e.pos++
			switch inner.kind {
			case pvPath:
				inner.cast = t.text
				return inner, true
			case pvConst:
				return pyVal{kind: pvConst, lit: t.text + "(" + inner.lit + ")"}, true
			}
			return inner, false
		case pyKeywords[t.text]:
			return pyVal{}, false
		}
		v, ok := e.env[t.text]
		return v, ok
	case pyNumber:
		return pyVal{kind: pvConst, lit: t.text}, true
	case pyString:
		if t.opaque {
			return pyVal{}, false
		}
		text := t.text
		for {
			n, ok := e.peek(0)
			if !ok || n.kind != pyString {
				break
			}
			if n.opaque {
				return pyVal{}, false
			}
			e.pos++
			text += n.text
		}
		return pyVal{kind: pvConst, lit: strconv.Quote(text)}, true
	case pyOp:
		switch t.text {
		case "-":
			n, ok := e.next()
			if ok && n.kind == pyNumber {
				return pyVal{kind: pvConst, lit: "-" + n.text}, true
			}
		case "(":
			v, ok := e.expr()
			if ok && e.peekOp(0, ")") {
				e.pos++
				return v, true
			}
		case "{":
			if e.peekOp(0, "}") {
				e.pos++
				return pyVal{kind: pvConst, lit: "{}"}, true
			}
		case "[":
			if e.peekOp(0, "]") {
				e.pos++
				return pyVal{kind: pvConst, lit: "[]"}, true
			}
		}
	}
	return pyVal{}, false
}

func splitPyStatements(toks []pyTok) [][]pyTok {
	var out [][]pyTok
	var cur []pyTok
	for _, t := range toks {
		if t.kind == pyEnd {
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func pyStatementIsInert(st []pyTok) bool {
	first := st[0]
	switch {
	case first.kind == pyName && (first.text == "import" || first.text == "from"):
		return true
	case len(st) == 1 && first.kind == pyName && first.text == "pass":
		return true
	case len(st) == 1 && first.kind == pyString:
		return true
	}
	return false
}

const pyMaxReplaceableStatements = 4

// The runtime runs `expression` and then `return <returnValue>`, so returnValue is itself
// an expression (usually the name `result`).
func analyzePythonVariable(expression, returnValue string) pyShape {
	if strings.TrimSpace(expression) == "" {
		return pyShape{kind: shapeEmpty}
	}
	toks, ok := tokenizePython(expression)
	if !ok {
		return pyShape{}
	}
	e := &pyEval{env: map[string]pyVal{}, reads: map[string]bool{}}
	statements := 0
	var final *pyVal
	for _, st := range splitPyStatements(toks) {
		if pyStatementIsInert(st) {
			continue
		}
		statements++
		if statements > pyMaxReplaceableStatements {
			return pyShape{}
		}
		if st[0].kind == pyName && st[0].text == "return" {
			v, ok := e.parseAll(st[1:])
			if !ok {
				return pyShape{}
			}
			final = &v
			break
		}
		if len(st) < 3 || st[0].kind != pyName || pyKeywords[st[0].text] || st[1].kind != pyOp || st[1].text != "=" {
			return pyShape{}
		}
		target := st[0].text
		v, ok := e.parseAll(st[2:])
		if !ok {
			return pyShape{}
		}
		e.env[target] = v
	}
	if statements == 0 {
		return pyShape{kind: shapeEmpty}
	}
	if final == nil {
		rv := strings.TrimSpace(returnValue)
		if rv == "" {
			rv = "result"
		}
		rvToks, ok := tokenizePython(rv)
		if !ok {
			return pyShape{}
		}
		var clean []pyTok
		for _, t := range rvToks {
			if t.kind != pyEnd {
				clean = append(clean, t)
			}
		}
		v, ok := e.parseAll(clean)
		if !ok {
			return pyShape{}
		}
		final = &v
	}
	if len(e.reads) > 1 {
		return pyShape{}
	}
	switch final.kind {
	case pvConst:
		return pyShape{kind: shapeConstant, lit: final.lit}
	case pvPath:
		if len(final.segs) == 0 {
			return pyShape{kind: shapePassthrough, root: final.root, cast: final.cast}
		}
		return pyShape{kind: shapeExtract, root: final.root, segs: final.segs, cast: final.cast}
	}
	return pyShape{}
}

// strip_code: the non-blank, non-comment lines. Size thresholds are measured on this.
func pyCodeChars(expression string) int {
	n, lines := 0, 0
	for _, ln := range strings.Split(expression, "\n") {
		ln = strings.TrimRight(ln, "\r")
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if lines > 0 {
			n++
		}
		n += utf8.RuneCountInString(ln)
		lines++
	}
	return n
}

var (
	pyHTMLTags       = []string{"<div", "<table", "<tr", "<td", "<span", "<p>", "<h1", "<h2", "<h3", "<ul", "<li"}
	pyImportRe       = regexp.MustCompile(`(?m)^\s*(?:import\s+([\w., \t]+)|from\s+([\w.]+)\s+import\b)`)
	pyLoopRe         = regexp.MustCompile(`\b(?:for|while)\b`)
	pyParseModules   = map[string]bool{"json": true, "re": true, "xml": true, "xmltodict": true, "base64": true, "fitz": true}
	pyNetworkModules = map[string]bool{"httpx": true, "requests": true, "urllib": true, "altscore": true, "aiohttp": true}
	// Parsing calls the runtime makes available without an import, text normalisation (name
	// splitting, accent folding), and the SDK client names.
	pyParseMarkers = []string{"json.loads", "json.load(", "re.search(", "re.match(", "re.findall(", "re.sub(", "re.split(", "re.compile(", "re.fullmatch(", "xmltodict.", "ElementTree", "fromstring(", ".split(", ".rsplit(", "unicodedata", "altscore", "alts_cli"}
)

// The three kinds every large legitimate variable falls into: HTML for a PDF block, parsing
// a payload (or calling out), and iteration over a variable-length list.
func pyExemptKind(expression string) string {
	low := strings.ToLower(expression)
	for _, tag := range pyHTMLTags {
		if strings.Contains(low, tag) {
			return "html"
		}
	}
	for _, m := range pyImportRe.FindAllStringSubmatch(expression, -1) {
		names := m[2]
		if m[1] != "" {
			names = m[1]
		}
		for _, part := range strings.Split(names, ",") {
			fields := strings.Fields(part)
			if len(fields) == 0 {
				continue
			}
			mod := strings.SplitN(fields[0], ".", 2)[0]
			if pyParseModules[mod] || pyNetworkModules[mod] {
				return "parsing"
			}
		}
	}
	for _, marker := range pyParseMarkers {
		if strings.Contains(expression, marker) {
			return "parsing"
		}
	}
	var code []string
	for _, ln := range strings.Split(expression, "\n") {
		if t := strings.TrimSpace(ln); t != "" && !strings.HasPrefix(t, "#") {
			code = append(code, ln)
		}
	}
	if pyLoopRe.MatchString(strings.Join(code, "\n")) {
		return "iteration"
	}
	return ""
}

var pyAssignHeadRe = regexp.MustCompile(`^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*=([^=]|$)`)

// One scalar per variable. Returns the literal's top-level keys when it can read them, so the
// fix can name the variables to split into.
func pyReturnsObject(def map[string]any) (bool, []string) {
	declared, _ := def["type"].(string)
	if declared == "" {
		declared, _ = def["dataType"].(string)
	}
	declared = strings.ToLower(strings.TrimSpace(declared))
	expression, _ := def["expression"].(string)
	rv, _ := def["returnValue"].(string)
	rv = strings.TrimSpace(rv)
	if rv == "" {
		rv = "result"
	}

	literal := ""
	switch {
	case strings.HasPrefix(rv, "{") || strings.HasPrefix(rv, "dict("):
		literal = rv
	case strings.HasPrefix(rv, "[") && rv != "[]" && declared != "array" && !pyLoopRe.MatchString(rv) && !pyListOfDicts(rv):
		return true, nil
	case pyIdentRe.MatchString(rv):
		// The LAST assignment to the returned name decides; the literal may span lines.
		offset := 0
		for _, ln := range strings.SplitAfter(expression, "\n") {
			lineStart := offset
			offset += len(ln)
			m := pyAssignHeadRe.FindStringSubmatchIndex(ln)
			if m == nil || ln[m[2]:m[3]] != rv {
				continue
			}
			literal = ""
			rhs := strings.TrimSpace(ln[m[4]:])
			if strings.HasPrefix(rhs, "{") || strings.HasPrefix(rhs, "dict(") {
				literal = strings.TrimSpace(expression[lineStart+m[4]:])
			}
		}
	}
	if literal != "" {
		// `{...}[key]` is a lookup that returns one value, not an object.
		if keys, whole := pyDictLiteral(literal); whole {
			return true, keys
		}
	}
	if declared == "object" || declared == "dict" {
		return true, nil
	}
	return false, nil
}

var pyIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// `[{...}, {...}]` is a per-party list for a fan-out, not a bundle of unrelated values.
func pyListOfDicts(src string) bool {
	toks, ok := tokenizePython(src)
	if !ok {
		return false
	}
	depth, elems, dicts := 0, 0, 0
	expectElem := false
	for _, t := range toks {
		isOp := t.kind == pyOp
		if expectElem && depth == 1 && !(isOp && t.text == "]") {
			elems++
			if isOp && t.text == "{" {
				dicts++
			}
		}
		expectElem = false
		if !isOp {
			continue
		}
		switch t.text {
		case "[", "(", "{":
			depth++
			if depth == 1 {
				expectElem = true
			}
		case "]", ")", "}":
			depth--
		case ",":
			expectElem = depth == 1
		}
	}
	return elems > 0 && dicts == elems
}

// The string keys at the top level of a dict literal (`{"a": ..., "b": ...}` or `dict(a=...)`),
// and whether the literal is the whole statement rather than the head of `{...}[key]`.
func pyDictLiteral(literal string) ([]string, bool) {
	toks, _ := tokenizePython(literal)
	var keys []string
	depth := 0
	isCall := strings.HasPrefix(literal, "dict(")
	for i, t := range toks {
		if t.kind == pyOp {
			switch t.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
				if depth == 0 {
					return keys, i+1 == len(toks) || toks[i+1].kind == pyEnd
				}
			}
			continue
		}
		if depth != 1 || i+1 >= len(toks) || toks[i+1].kind != pyOp {
			continue
		}
		if !isCall && t.kind == pyString && toks[i+1].text == ":" {
			keys = append(keys, t.text)
		}
		if isCall && t.kind == pyName && toks[i+1].text == "=" {
			keys = append(keys, t.text)
		}
	}
	return keys, false
}
