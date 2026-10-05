package cmd

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Where a workflow's logic lives: values wired directly in each node, decisions made by
// entities, Python kept for real computation. Advisory by construction, like the readability
// group: it never moves the exit code, and a clean workflow prints nothing.

type structureFinding struct {
	Practice string
	Subject  string
	Message  string
}

const (
	structureMaxSelectedVariables = 6
	structureMaxSelfChained       = 2
	structureMaxPolicyCodeChars   = 1000
	structurePracticeCap          = 5
)

var structurePractices = []string{
	"decision-in-python",
	"passthrough",
	"extract-only",
	"constant",
	"wide-compute-node",
	"self-chain",
	"large-policy-variable",
	"object-return",
}

type structureNode struct {
	Ref  string
	Name string
	Type string
	Body map[string]any
}

type structureGraph struct {
	Nodes      []structureNode
	Edges      []map[string]any
	Vars       map[string]map[string]any
	index      map[string]int
	selectedBy map[string]int
}

func structureGraphFromSpec(spec *composeSpec) *structureGraph {
	if spec == nil {
		return nil
	}
	nodes := spec.Nodes
	if len(nodes) == 0 {
		nodes = append(append([]map[string]any{}, spec.ExtraNodes...), spec.Tasks...)
	}
	g := &structureGraph{Edges: spec.Edges, Vars: map[string]map[string]any{}}
	g.addVars(spec.CustomVariables)
	for i, n := range nodes {
		ref := localRef(n, fmt.Sprintf("nodes[%d]", i))
		t, _ := n["type"].(string)
		g.Nodes = append(g.Nodes, structureNode{Ref: ref, Name: strconv.Quote(ref), Type: strings.ToLower(t), Body: n})
		g.addVars(asMap(n["customVariables"]))
	}
	return g.finish()
}

// GET /v2/workflows/{id} carries the graph only: a node's config lives on its task, with
// node.data.inputMappings as the canvas mirror. The task body wins, as in export.
func structureGraphFromWorkflow(wf map[string]any, taskBodies []map[string]any) *structureGraph {
	if wf == nil {
		return nil
	}
	byAlias := map[string]map[string]any{}
	for _, t := range taskBodies {
		if a, _ := t["alias"].(string); a != "" {
			byAlias[a] = t
		}
	}
	g := &structureGraph{Vars: map[string]map[string]any{}}
	g.addVars(asMap(wf["customVariables"]))
	for i, raw := range asSlice(wf["nodes"]) {
		nm := asMap(raw)
		alias, _ := nm["taskAlias"].(string)
		nodeID, _ := nm["nodeId"].(string)
		label, _ := nm["label"].(string)
		body := map[string]any{}
		for k, v := range asMap(nm["data"]) {
			body[k] = v
		}
		for k, v := range byAlias[alias] {
			body[k] = v
		}
		body["alias"], body["nodeId"] = alias, nodeID
		ref := alias
		if ref == "" {
			ref = nodeID
		}
		if ref == "" {
			ref = fmt.Sprintf("nodes[%d]", i)
		}
		name := strconv.Quote(ref)
		if label != "" && label != ref {
			name = fmt.Sprintf("%q (%s)", label, ref)
		}
		g.Nodes = append(g.Nodes, structureNode{
			Ref: ref, Name: name, Type: strings.ToLower(fmt.Sprint(nm["type"])), Body: body,
		})
		g.addVars(asMap(body["customVariables"]))
	}
	for _, raw := range asSlice(wf["edges"]) {
		g.Edges = append(g.Edges, asMap(raw))
	}
	return g.finish()
}

func (g *structureGraph) addVars(vars map[string]any) {
	for name, raw := range vars {
		def, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, seen := g.Vars[name]; !seen {
			g.Vars[name] = def
		}
	}
}

func (g *structureGraph) finish() *structureGraph {
	g.index = map[string]int{}
	for i, n := range g.Nodes {
		g.index[n.Ref] = i
	}
	for i, n := range g.Nodes {
		for _, k := range []string{"alias", "taskAlias", "nodeId", "specRef"} {
			if v, _ := n.Body[k].(string); v != "" {
				if _, seen := g.index[v]; !seen {
					g.index[v] = i
				}
			}
		}
	}
	// An exported spec keeps server aliases in its references and names edges "<a>-><b>".
	for _, e := range g.Edges {
		id, _ := e["id"].(string)
		a, b, ok := strings.Cut(id, "->")
		if !ok {
			continue
		}
		from, to := edgeEndpoints(e)
		for _, pair := range [][2]string{{a, from}, {b, to}} {
			if i, known := g.index[pair[1]]; known {
				if _, seen := g.index[pair[0]]; !seen {
					g.index[pair[0]] = i
				}
			}
		}
	}
	g.selectedBy = map[string]int{}
	for i, n := range g.Nodes {
		if n.Type != "compute-variables" {
			continue
		}
		for _, name := range structureSelected(n) {
			if _, seen := g.selectedBy[name]; !seen {
				g.selectedBy[name] = i
			}
		}
	}
	return g
}

func structureSelected(n structureNode) []string {
	var out []string
	for _, sv := range asSlice(n.Body["selectedVariables"]) {
		if s, ok := sv.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

var structureAliasSuffixRe = regexp.MustCompile(`^(.*)-([0-9a-f]{6,8})$`)
var structureSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// A server alias is "<label slug or ref>-<hex>", possibly suffixed more than once.
func (g *structureGraph) resolve(alias string) *structureNode {
	if i, ok := g.index[alias]; ok {
		return &g.Nodes[i]
	}
	stem := alias
	for {
		m := structureAliasSuffixRe.FindStringSubmatch(stem)
		if m == nil {
			return nil
		}
		stem = m[1]
		if i, ok := g.index[stem]; ok {
			g.index[alias] = i
			return &g.Nodes[i]
		}
		var hits []int
		for i, n := range g.Nodes {
			label, _ := n.Body["label"].(string)
			slug := strings.Trim(structureSlugRe.ReplaceAllString(strings.ToLower(label), "-"), "-")
			if len(slug) > 30 {
				slug = strings.Trim(slug[:30], "-")
			}
			if slug != "" && slug == stem {
				hits = append(hits, i)
			}
		}
		if len(hits) != 1 {
			hits = hits[:0]
			for i, n := range g.Nodes {
				if strings.HasSuffix(stem+"-", "-"+n.Ref+"-") {
					hits = append(hits, i)
				}
			}
		}
		if len(hits) == 1 {
			g.index[alias] = hits[0]
			return &g.Nodes[hits[0]]
		}
	}
}

type structureRef struct {
	Class string
	Node  *structureNode
	Field string
}

func (g *structureGraph) classifyRef(value string) structureRef {
	v := strings.TrimSpace(value)
	if strings.HasPrefix(v, "{{") && strings.HasSuffix(v, "}}") {
		v = strings.TrimSpace(v[2 : len(v)-2])
	}
	if !strings.Contains(v, ".") {
		return structureRef{Class: "literal"}
	}
	parts := strings.Split(v, ".")
	field := func(i int) string {
		if i < len(parts) {
			return strings.SplitN(parts[i], "[", 2)[0]
		}
		return ""
	}
	nodeRef := func(n *structureNode, f string) structureRef {
		if n.Type == "compute-variables" {
			return structureRef{Class: "compute", Node: n, Field: f}
		}
		return structureRef{Class: "task", Node: n}
	}
	switch parts[0] {
	case "inputs", "entity", "system":
		return structureRef{Class: parts[0]}
	case "self", "custom":
		return structureRef{Class: parts[0], Field: field(1)}
	case "task_outputs_by_type":
		if field(1) == "compute-variables" {
			return structureRef{Class: "compute", Field: field(2)}
		}
		return structureRef{Class: "task"}
	case "task_outputs":
		alias := field(1)
		if n := g.resolve(alias); n != nil {
			return nodeRef(n, field(2))
		}
		if strings.Contains(alias, "compute-variables") {
			return structureRef{Class: "compute", Field: field(2)}
		}
		return structureRef{Class: "task"}
	}
	if n := g.resolve(field(0)); n != nil {
		return nodeRef(n, field(1))
	}
	return structureRef{Class: "other"}
}

func (g *structureGraph) selecting(name string) *structureNode {
	if i, ok := g.selectedBy[name]; ok {
		return &g.Nodes[i]
	}
	return nil
}

func (g *structureGraph) varSubject(name string) string {
	if n := g.selecting(name); n != nil {
		return fmt.Sprintf("variable %q (node %s)", name, n.Name)
	}
	return fmt.Sprintf("variable %q", name)
}

func (g *structureGraph) varShape(name string) pyShape {
	def := g.Vars[name]
	expr, isString := def["expression"].(string)
	if !isString && def["expression"] != nil {
		return pyShape{}
	}
	rv, _ := def["returnValue"].(string)
	return analyzePythonVariable(expr, rv)
}

func (g *structureGraph) varExemptKind(name string) string {
	expr, _ := g.Vars[name]["expression"].(string)
	return pyExemptKind(expr)
}

func (g *structureGraph) sortedVarNames() []string {
	names := make([]string, 0, len(g.Vars))
	for name := range g.Vars {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (g *structureGraph) findings() []structureFinding {
	var out []structureFinding
	out = append(out, g.adviseDecisionInPython()...)
	out = append(out, g.adviseReplaceableVariables()...)
	out = append(out, g.adviseComputeNodes()...)
	out = append(out, g.adviseLargePolicyVariables()...)
	out = append(out, g.adviseObjectReturns()...)
	return out
}

var structurePlaceholderRe = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// The decision source, in the order the runtime records it: the End node's decision_key
// mapping, then its standardOutput.decision, and only when the End records neither, a
// decision_key another node writes.
func (g *structureGraph) adviseDecisionInPython() []structureFinding {
	var out []structureFinding
	endHasSource := false
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.Type != "end" {
			continue
		}
		where, src := "", ""
		if dk, _ := asMap(n.Body["inputMappings"])["decision_key"].(string); strings.TrimSpace(dk) != "" {
			where, src = fmt.Sprintf("end %s takes decision_key", n.Name), dk
		} else if so, _ := asMap(asMap(n.Body["endConfig"])["standardOutput"])["decision"].(string); so != "" {
			if m := structurePlaceholderRe.FindStringSubmatch(so); m != nil {
				where, src = fmt.Sprintf("end %s takes standardOutput.decision", n.Name), m[1]
			}
		}
		if src == "" {
			continue
		}
		endHasSource = true
		if f, ok := g.decisionFinding(where, src); ok {
			out = append(out, f)
		}
	}
	if endHasSource {
		return out
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.Type == "end" || n.Type == "compute-variables" || n.Type == "start" {
			continue
		}
		for _, d := range structureMappingDicts(n.Body) {
			src, _ := d["decision_key"].(string)
			if strings.TrimSpace(src) == "" {
				continue
			}
			if f, ok := g.decisionFinding(fmt.Sprintf("node %s writes decision_key", n.Name), src); ok {
				return []structureFinding{f}
			}
			return nil
		}
	}
	return out
}

func (g *structureGraph) decisionFinding(where, src string) (structureFinding, bool) {
	name, node, ok := g.pythonDecisionSource(src)
	if !ok {
		return structureFinding{}, false
	}
	target := "task_outputs.<ruleTreeRef>.decision_key"
	for _, n := range g.Nodes {
		if n.Type == "rule-tree" {
			target = "task_outputs." + n.Ref + ".decision_key"
			break
		}
	}
	loc := ""
	if node != nil {
		loc = fmt.Sprintf(" (node %s)", node.Name)
	}
	return structureFinding{
		Practice: "decision-in-python",
		Subject:  where,
		Message: fmt.Sprintf("%s from Python variable %q%s. Fix: make the decision in a rule-tree "+
			"and map decision_key to %s; keep the thresholds and bands in mapping tables or evaluation rules",
			where, name, loc, target),
	}, true
}

// Follows a variable that only re-exposes another variable, so the finding names the one
// that computes. A variable that only re-exposes an entity's or an input's value is not a
// decision made in Python; the passthrough finding covers it.
func (g *structureGraph) pythonDecisionSource(src string) (string, *structureNode, bool) {
	r := g.classifyRef(src)
	if (r.Class != "compute" && r.Class != "custom") || r.Field == "" {
		return "", nil, false
	}
	name, node := r.Field, r.Node
	seen := map[string]bool{}
	for !seen[name] {
		seen[name] = true
		if sel := g.selecting(name); sel != nil {
			node = sel
		}
		if g.Vars[name] == nil {
			break
		}
		shape := g.varShape(name)
		if shape.kind != shapePassthrough && shape.kind != shapeExtract {
			break
		}
		up := g.classifyRef(shape.root)
		if up.Class != "self" && up.Class != "custom" && up.Class != "compute" {
			return "", nil, false
		}
		if up.Field == "" {
			break
		}
		name = up.Field
		if up.Node != nil {
			node = up.Node
		}
	}
	return name, node, true
}

var structureSkipKeys = map[string]bool{
	"inputSchema": true, "outputSchema": true, "oneOf": true, "inputMappingConfig": true,
	"position": true, "retryPolicy": true,
}

func structureMappingDicts(body map[string]any) []map[string]any {
	var out []map[string]any
	if im, ok := body["inputMappings"].(map[string]any); ok {
		out = append(out, im)
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if structureSkipKeys[k] {
					continue
				}
				if cm, ok := child.(map[string]any); ok && (k == "inputMappings" || k == "variableMappings") {
					out = append(out, cm)
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	for k, v := range body {
		if k != "inputMappings" && !structureSkipKeys[k] {
			walk(v)
		}
	}
	return out
}

func (g *structureGraph) adviseReplaceableVariables() []structureFinding {
	var out []structureFinding
	for _, name := range g.sortedVarNames() {
		// The extraction-probe advisory already reports the bare `result = inputs.get("<path>")`.
		if _, probe := isPureExtractionProbe(g.Vars[name]); probe {
			continue
		}
		shape := g.varShape(name)
		subject := g.varSubject(name)
		f := structureFinding{Subject: name}
		switch shape.kind {
		case shapeEmpty:
			f.Practice = "constant"
			f.Message = fmt.Sprintf("%s has an empty expression, so it is always null. "+
				"Fix: delete it, or put the literal it stands for in outputJson or the consuming mapping", subject)
		case shapeConstant:
			f.Practice = "constant"
			f.Message = fmt.Sprintf("%s only holds the constant %s. Fix: write the literal into outputJson "+
				"or the consuming mapping (a threshold or label belongs in an entity: mapping table or "+
				"evaluation rule) and delete the variable", subject, structureClip(shape.lit, 40))
		case shapePassthrough:
			f.Practice = "passthrough"
			via := ""
			if shape.cast != "" {
				via = " through " + shape.cast + "()"
			}
			if sibling, ok := structureSiblingVar(shape.root); ok {
				f.Message = fmt.Sprintf("%s only re-exposes variable %q%s. Fix: read %q directly where "+
					"%q is used and delete %q", subject, sibling, via, sibling, name, name)
			} else {
				f.Message = fmt.Sprintf("%s only re-exposes %s%s. Fix: wire %s directly where it is used "+
					"and delete the variable", subject, shape.root, via, shape.root)
			}
		case shapeExtract:
			f.Practice = "extract-only"
			fieldPath := strings.TrimPrefix(strings.Join(shape.segs, ""), ".")
			if sibling, ok := structureSiblingVar(shape.root); ok {
				deep := ""
				if n := g.selecting(sibling); n != nil {
					deep = fmt.Sprintf("wire task_outputs.%s.%s%s directly, or ", n.Ref, sibling, strings.Join(shape.segs, ""))
				}
				f.Message = fmt.Sprintf("%s only extracts %s from variable %q. Fix: %shave %q return "+
					"that scalar (one scalar per variable), and delete %q", subject, fieldPath, sibling, deep, sibling, name)
			} else {
				f.Message = fmt.Sprintf("%s only extracts %s from %s. Fix: wire the deep path %s directly "+
					"where it is used and delete the variable", subject, fieldPath, shape.root, shape.deepPath())
			}
		default:
			continue
		}
		out = append(out, f)
	}
	return out
}

func structureSiblingVar(root string) (string, bool) {
	for _, p := range []string{"self.", "custom."} {
		if rest, ok := strings.CutPrefix(root, p); ok && !strings.Contains(rest, ".") {
			return rest, true
		}
	}
	return "", false
}

// HTML builders are left out of both counts: a PDF node with ten blocks is not a rules engine.
func (g *structureGraph) adviseComputeNodes() []structureFinding {
	var out []structureFinding
	for _, n := range g.Nodes {
		if n.Type != "compute-variables" {
			continue
		}
		var computed, chained []string
		html := 0
		for _, name := range structureSelected(n) {
			if g.varExemptKind(name) == "html" {
				html++
				continue
			}
			computed = append(computed, name)
			for _, d := range asSlice(g.Vars[name]["dependencies"]) {
				if s, ok := d.(string); ok && strings.HasPrefix(s, "self.") {
					chained = append(chained, name)
					break
				}
			}
		}
		if len(computed) > structureMaxSelectedVariables {
			extra := ""
			if html > 0 {
				extra = fmt.Sprintf(" (plus %d HTML)", html)
			}
			out = append(out, structureFinding{
				Practice: "wide-compute-node",
				Subject:  n.Ref,
				Message: fmt.Sprintf("compute node %s computes %d variables%s. Fix: split it into small "+
					"single-purpose nodes, and move thresholds, bands and gates into mapping tables, "+
					"evaluation rules or a rule-tree", n.Name, len(computed), extra),
			})
		}
		if len(chained) > structureMaxSelfChained {
			out = append(out, structureFinding{
				Practice: "self-chain",
				Subject:  n.Ref,
				Message: fmt.Sprintf("compute node %s chains %d variables through self. (%s), which is a "+
					"rules engine in Python. Fix: move the gates and their order into a rule-tree or "+
					"evaluation rules and keep only the indicators here", n.Name, len(chained), structureSample(chained, 4)),
			})
		}
	}
	return out
}

func (g *structureGraph) adviseLargePolicyVariables() []structureFinding {
	var out []structureFinding
	for _, name := range g.sortedVarNames() {
		expr, _ := g.Vars[name]["expression"].(string)
		chars := pyCodeChars(expr)
		if chars <= structureMaxPolicyCodeChars || pyExemptKind(expr) != "" {
			continue
		}
		out = append(out, structureFinding{
			Practice: "large-policy-variable",
			Subject:  name,
			Message: fmt.Sprintf("%s is %d code chars with no loop, parsing or HTML, which reads as "+
				"branching policy. Fix: move its thresholds and bands into mapping tables or evaluation "+
				"rules, make the decision in a rule-tree, and keep only the indicators it derives in Python",
				g.varSubject(name), chars),
		})
	}
	return out
}

func (g *structureGraph) adviseObjectReturns() []structureFinding {
	var out []structureFinding
	for _, name := range g.sortedVarNames() {
		def := g.Vars[name]
		isObject, keys := pyReturnsObject(def)
		// A parse node hands downstream indicators one parsed payload; that is its job.
		if !isObject || g.varExemptKind(name) == "parsing" {
			continue
		}
		split := "one variable per field"
		if len(keys) > 0 {
			split = fmt.Sprintf("one variable per field (%s)", structureSample(keys, 5))
		}
		tail := ""
		for _, k := range keys {
			if strings.Contains(strings.ToLower(k), "decision") {
				tail = "; the decision itself belongs in a rule-tree"
				break
			}
		}
		out = append(out, structureFinding{
			Practice: "object-return",
			Subject:  name,
			Message: fmt.Sprintf("%s returns an object. Fix: return one scalar per variable: split it into "+
				"%s and wire each where it is read%s", g.varSubject(name), split, tail),
		})
	}
	return out
}

func structureSample(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, +%d more", strings.Join(items[:limit], ", "), len(items)-limit)
}

func structureClip(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "..."
}

func adviseWorkflowStructure(g *structureGraph, w io.Writer) {
	if g == nil || w == nil {
		return
	}
	printStructureFindings(w, g.findings())
}

func printStructureFindings(w io.Writer, findings []structureFinding) {
	if w == nil || len(findings) == 0 {
		return
	}
	byPractice := map[string][]structureFinding{}
	for _, f := range findings {
		byPractice[f.Practice] = append(byPractice[f.Practice], f)
	}
	fmt.Fprintf(w, "# structure advisory: %d finding(s). Wire values directly in each node, make decisions "+
		"in entities, keep Python for real computation. Advisory only -- never fails lint or blocks apply.\n",
		len(findings))
	for _, p := range structurePractices {
		fs := byPractice[p]
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].Subject < fs[j].Subject })
		for i, f := range fs {
			if i == structurePracticeCap {
				rest := make([]string, 0, len(fs)-i)
				for _, r := range fs[i:] {
					rest = append(rest, r.Subject)
				}
				fmt.Fprintf(w, "#   [structure] %s: %d more with the same fix: %s\n", p, len(rest), structureSample(rest, 10))
				break
			}
			fmt.Fprintf(w, "#   [structure] %s: %s\n", p, f.Message)
		}
	}
	fmt.Fprintf(w, "#   see `altscore workflows-v2 schema-guide variables` and "+
		"`altscore workflows-v2 schema-guide creditDecisioningEntities`\n")
}
