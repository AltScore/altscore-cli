package cmd

// `workflows-v2 diff` compares two EXISTING workflow versions.
//
// This is not the same job as `apply --diff`, which previews a spec against a
// live workflow and only needs the fields apply itself mutates. Here the
// interesting differences live INSIDE the task bodies: a condition's operator, a
// notice's message, an auto-generated inputSchema, a compute-variable
// expression. So both sides are flattened through bundleToApplySpec, which
// inlines each node's backing task body, and every body field is compared.
//
// It also reports which tasks are missing `specRef`. A task version bump drops
// the pair unless the writer supplies it, and the builder does not, so a task
// with no specRef is one a human edited in the Hub. On a CLI-authored workflow
// that makes the census a reliable list of builder-touched nodes -- which is the
// fastest way to split a diff into "the tool generated this" and "somebody
// decided this", two populations that need opposite review.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AltScore/altscore-cli/internal/output"
)

type sideSummary struct {
	ID              string   `json:"id"`
	Alias           string   `json:"alias"`
	Label           string   `json:"label"`
	Version         any      `json:"version"`
	Status          string   `json:"status"`
	Nodes           int      `json:"nodes"`
	Edges           int      `json:"edges"`
	CustomVariables int      `json:"customVariables"`
	InputVariables  int      `json:"inputVariables"`
	Tasks           int      `json:"tasks"`
	TasksMissingRef []string `json:"tasksMissingSpecRef"`
}

type fieldDelta struct {
	Field  string `json:"field"`
	BytesA int    `json:"bytesA"`
	BytesB int    `json:"bytesB"`
}

type nodeDelta struct {
	Ref    string       `json:"ref"`
	Type   string       `json:"type"`
	Label  string       `json:"label"`
	Fields []fieldDelta `json:"fields"`
}

type edgeTriple struct {
	Source string `json:"source"`
	Handle string `json:"handle"`
	Target string `json:"target"`
}

type varDelta struct {
	Name   string `json:"name"`
	BytesA int    `json:"bytesA"`
	BytesB int    `json:"bytesB"`
}

type diffReport struct {
	A sideSummary `json:"a"`
	B sideSummary `json:"b"`

	NodesOnlyInA []string    `json:"nodesOnlyInA"`
	NodesOnlyInB []string    `json:"nodesOnlyInB"`
	NodesRenamed [][2]string `json:"nodesRenamed"`
	NodesChanged []nodeDelta `json:"nodesChanged"`
	NodesShared  int         `json:"nodesShared"`

	EdgesOnlyInA []edgeTriple `json:"edgesOnlyInA"`
	EdgesOnlyInB []edgeTriple `json:"edgesOnlyInB"`

	CustomVarsOnlyInA []string   `json:"customVariablesOnlyInA"`
	CustomVarsOnlyInB []string   `json:"customVariablesOnlyInB"`
	CustomVarsChanged []varDelta `json:"customVariablesChanged"`

	InputVarsOnlyInA []string   `json:"inputVariablesOnlyInA"`
	InputVarsOnlyInB []string   `json:"inputVariablesOnlyInB"`
	InputVarsChanged []varDelta `json:"inputVariablesChanged"`
}

// nodeFieldDropList are per-node keys that differ between any two versions as
// bookkeeping, never as an authored change. Reporting them buries the real diff.
var nodeFieldDropList = map[string]bool{
	"ref":         true,
	"taskVersion": true,
	"taskId":      true,
	"version":     true,
	"createdAt":   true,
	"updatedAt":   true,
	"isLatest":    true,
}

func makeWfv2DiffCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "diff <id-a> <id-b>",
		Short: "Compare two existing v2 workflow versions, including task bodies",
		Long: `Compares two v2 workflow versions field by field and reports what actually
differs: nodes added or removed, per-node task-body fields that changed, edges,
custom variables and input variables.

Both sides are flattened the way 'export --format apply-spec' flattens one, so a
node carries its backing task body inline. That is where version-to-version
differences live -- a condition operator, a notice message, an inputSchema, a
compute-variable expression -- none of which 'apply --diff' inspects.

Also reports which tasks are missing 'specRef'. A task version bump drops the
stable-apply pair unless the writer sends it, and the Hub builder does not, so on
a CLI-authored workflow a task with no specRef is one somebody edited in the
builder. That splits a diff into machine-generated hunks and deliberate ones,
which need opposite review: revert the first by default, read the second closely.

Nodes are matched by 'ref' (the server task alias, stable across versions of the
same workflow). A ref present on one side only is reported as added or removed,
except when a node with the same type and label exists on the other side under a
different ref -- that is an alias rotation, reported as a rename, and it is the
symptom of the specRef loss above.`,
		Args: cobra.ExactArgs(2),
		Example: `  altscore workflows-v2 diff <active-id> <draft-id>
  altscore workflows-v2 diff <id-a> <id-b> --json | jq '.nodesChanged'
  altscore workflows-v2 diff <id-a> <id-b> --profile bolivariano`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			specA, sideA, err := loadSideForDiff(c, args[0])
			if err != nil {
				return err
			}
			specB, sideB, err := loadSideForDiff(c, args[1])
			if err != nil {
				return err
			}

			report := buildDiffReport(specA, sideA, specB, sideB)
			if asJSON {
				return output.JSON(report)
			}
			fmt.Fprint(cmd.OutOrStdout(), renderDiffReport(report))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the full report as JSON instead of text")
	return cmd
}

// loadSideForDiff fetches one version's export bundle once and derives both the
// flattened spec and the summary (including the specRef census) from it.
func loadSideForDiff(c clientDoer, id string) (map[string]any, sideSummary, error) {
	raw, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+id+"/export", nil)
	if err != nil {
		return nil, sideSummary{}, fmt.Errorf("diff: export %s: %w", id, err)
	}

	spec, err := bundleToApplySpec(raw)
	if err != nil {
		return nil, sideSummary{}, fmt.Errorf("diff: flatten %s: %w", id, err)
	}

	// The version lives at the bundle ROOT as sourceVersion; bundle.workflow
	// carries only the authoring body (no version, no status).
	var bundle struct {
		SourceAlias   string           `json:"sourceAlias"`
		SourceVersion any              `json:"sourceVersion"`
		Workflow      map[string]any   `json:"workflow"`
		Tasks         []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return nil, sideSummary{}, fmt.Errorf("diff: parse bundle %s: %w", id, err)
	}

	side := sideSummary{
		ID:              id,
		Nodes:           len(toMapSlice(spec["nodes"])),
		Edges:           len(toMapSlice(spec["edges"])),
		CustomVariables: len(toMap(spec["customVariables"])),
		InputVariables:  len(toMap(spec["inputVariables"])),
		Tasks:           len(bundle.Tasks),
		TasksMissingRef: []string{},
	}
	side.Alias, _ = spec["alias"].(string)
	if side.Alias == "" {
		side.Alias = bundle.SourceAlias
	}
	side.Label, _ = spec["label"].(string)
	side.Version = bundle.SourceVersion
	side.Status = fetchWorkflowStatus(c, id)
	for _, t := range bundle.Tasks {
		if ref, _ := t["specRef"].(string); ref == "" {
			alias, _ := t["alias"].(string)
			if alias == "" {
				continue
			}
			side.TasksMissingRef = append(side.TasksMissingRef, alias)
		}
	}
	sort.Strings(side.TasksMissingRef)
	return spec, side, nil
}

// clientDoer is the one client method this command needs, so the diff can be
// exercised without a live backend.
type clientDoer interface {
	Do(method, module, path string, body any) (json.RawMessage, int, error)
}

// fetchWorkflowStatus is best-effort: ACTIVE-vs-DRAFT is the context that makes
// a version comparison readable ("am I diffing a live workflow against a draft
// somebody is still editing?"), but the export bundle does not carry status, and
// failing the whole diff over a decoration would be the wrong trade.
func fetchWorkflowStatus(c clientDoer, id string) string {
	raw, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+id, nil)
	if err != nil {
		return ""
	}
	var wf struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &wf); err != nil {
		return ""
	}
	return wf.Status
}

func indexSpecNodesByRef(spec map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, n := range toMapSlice(spec["nodes"]) {
		if ref, _ := n["ref"].(string); ref != "" {
			out[ref] = n
		}
	}
	return out
}

func nodeIdentity(n map[string]any) string {
	t, _ := n["type"].(string)
	l, _ := n["label"].(string)
	return t + "\x00" + l
}

func buildDiffReport(specA map[string]any, sideA sideSummary, specB map[string]any, sideB sideSummary) diffReport {
	r := diffReport{
		A: sideA, B: sideB,
		NodesOnlyInA: []string{}, NodesOnlyInB: []string{},
		NodesRenamed: [][2]string{}, NodesChanged: []nodeDelta{},
		EdgesOnlyInA: []edgeTriple{}, EdgesOnlyInB: []edgeTriple{},
		CustomVarsOnlyInA: []string{}, CustomVarsOnlyInB: []string{}, CustomVarsChanged: []varDelta{},
		InputVarsOnlyInA: []string{}, InputVarsOnlyInB: []string{}, InputVarsChanged: []varDelta{},
	}

	a := indexSpecNodesByRef(specA)
	b := indexSpecNodesByRef(specB)

	var onlyA, onlyB []string
	for ref := range a {
		if _, has := b[ref]; !has {
			onlyA = append(onlyA, ref)
		}
	}
	for ref := range b {
		if _, has := a[ref]; !has {
			onlyB = append(onlyB, ref)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)

	// An alias rotation shows up as one ref missing on each side for the same
	// (type, label). Pair those off as renames so they do not read as a node
	// having been deleted and a different one added.
	claimed := map[string]bool{}
	for _, refA := range onlyA {
		identity := nodeIdentity(a[refA])
		for _, refB := range onlyB {
			if claimed[refB] || nodeIdentity(b[refB]) != identity {
				continue
			}
			r.NodesRenamed = append(r.NodesRenamed, [2]string{refA, refB})
			claimed[refB] = true
			break
		}
	}
	renamedA := map[string]bool{}
	for _, pair := range r.NodesRenamed {
		renamedA[pair[0]] = true
	}
	for _, ref := range onlyA {
		if !renamedA[ref] {
			r.NodesOnlyInA = append(r.NodesOnlyInA, ref)
		}
	}
	for _, ref := range onlyB {
		if !claimed[ref] {
			r.NodesOnlyInB = append(r.NodesOnlyInB, ref)
		}
	}

	// Shared refs plus renamed pairs both get a body comparison.
	pairs := make([][2]string, 0, len(a))
	for ref := range a {
		if _, has := b[ref]; has {
			pairs = append(pairs, [2]string{ref, ref})
			r.NodesShared++
		}
	}
	pairs = append(pairs, r.NodesRenamed...)
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })

	for _, pair := range pairs {
		na, nb := a[pair[0]], b[pair[1]]
		keys := map[string]bool{}
		for k := range na {
			keys[k] = true
		}
		for k := range nb {
			keys[k] = true
		}
		var fields []fieldDelta
		for k := range keys {
			if nodeFieldDropList[k] {
				continue
			}
			if reflect.DeepEqual(na[k], nb[k]) {
				continue
			}
			fields = append(fields, fieldDelta{
				Field:  k,
				BytesA: jsonLen(na[k]),
				BytesB: jsonLen(nb[k]),
			})
		}
		if len(fields) == 0 {
			continue
		}
		sort.Slice(fields, func(i, j int) bool { return fields[i].Field < fields[j].Field })
		nodeType, _ := na["type"].(string)
		label, _ := na["label"].(string)
		r.NodesChanged = append(r.NodesChanged, nodeDelta{
			Ref: pair[0], Type: nodeType, Label: label, Fields: fields,
		})
	}

	r.EdgesOnlyInA, r.EdgesOnlyInB = diffSpecEdges(specA, specB)
	r.CustomVarsOnlyInA, r.CustomVarsOnlyInB, r.CustomVarsChanged = diffVarMaps(
		toMap(specA["customVariables"]), toMap(specB["customVariables"]))
	r.InputVarsOnlyInA, r.InputVarsOnlyInB, r.InputVarsChanged = diffVarMaps(
		toMap(specA["inputVariables"]), toMap(specB["inputVariables"]))
	return r
}

// specEdgeSet keys edges by (from, sourceHandle, to). apply-spec edges use
// from/to holding node refs, so the triple is comparable across versions.
func specEdgeSet(spec map[string]any) map[edgeTriple]bool {
	out := map[edgeTriple]bool{}
	for _, e := range toMapSlice(spec["edges"]) {
		from, _ := e["from"].(string)
		to, _ := e["to"].(string)
		handle, _ := e["sourceHandle"].(string)
		out[edgeTriple{Source: from, Handle: handle, Target: to}] = true
	}
	return out
}

func diffSpecEdges(specA, specB map[string]any) (onlyA, onlyB []edgeTriple) {
	a, b := specEdgeSet(specA), specEdgeSet(specB)
	onlyA, onlyB = []edgeTriple{}, []edgeTriple{}
	for e := range a {
		if !b[e] {
			onlyA = append(onlyA, e)
		}
	}
	for e := range b {
		if !a[e] {
			onlyB = append(onlyB, e)
		}
	}
	sortEdges(onlyA)
	sortEdges(onlyB)
	return
}

func sortEdges(es []edgeTriple) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].Source != es[j].Source {
			return es[i].Source < es[j].Source
		}
		if es[i].Handle != es[j].Handle {
			return es[i].Handle < es[j].Handle
		}
		return es[i].Target < es[j].Target
	})
}

func diffVarMaps(a, b map[string]any) (onlyA, onlyB []string, changed []varDelta) {
	onlyA, onlyB, changed = []string{}, []string{}, []varDelta{}
	for k := range a {
		if _, has := b[k]; !has {
			onlyA = append(onlyA, k)
		}
	}
	for k := range b {
		if _, has := a[k]; !has {
			onlyB = append(onlyB, k)
		}
	}
	for k, va := range a {
		vb, has := b[k]
		if !has || reflect.DeepEqual(va, vb) {
			continue
		}
		changed = append(changed, varDelta{Name: k, BytesA: jsonLen(va), BytesB: jsonLen(vb)})
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	sort.Slice(changed, func(i, j int) bool { return changed[i].Name < changed[j].Name })
	return
}

func jsonLen(v any) int {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

func renderDiffReport(r diffReport) string {
	var buf strings.Builder

	side := func(tag string, s sideSummary) {
		fmt.Fprintf(&buf, "%s  %s v%v %s  (%s)\n", tag, orDash(s.Alias), orZero(s.Version), orDash(s.Status), s.ID)
		fmt.Fprintf(&buf, "    nodes %d  edges %d  customVariables %d  inputVariables %d\n",
			s.Nodes, s.Edges, s.CustomVariables, s.InputVariables)
	}
	side("A:", r.A)
	side("B:", r.B)
	buf.WriteString("\n")

	if len(r.NodesOnlyInA) == 0 && len(r.NodesOnlyInB) == 0 && len(r.NodesRenamed) == 0 &&
		len(r.NodesChanged) == 0 && len(r.EdgesOnlyInA) == 0 && len(r.EdgesOnlyInB) == 0 &&
		len(r.CustomVarsChanged) == 0 && len(r.CustomVarsOnlyInA) == 0 && len(r.CustomVarsOnlyInB) == 0 &&
		len(r.InputVarsChanged) == 0 && len(r.InputVarsOnlyInA) == 0 && len(r.InputVarsOnlyInB) == 0 {
		buf.WriteString("No differences in nodes, edges or variables.\n")
	}

	if len(r.NodesRenamed) > 0 {
		fmt.Fprintf(&buf, "ALIAS ROTATION (%s) -- same type and label under a different ref.\n",
			plural(len(r.NodesRenamed), "node", "nodes"))
		buf.WriteString("  Every task_outputs.<alias>.* reference to the old ref is now dangling.\n")
		for _, pair := range r.NodesRenamed {
			fmt.Fprintf(&buf, "  ~ %s -> %s\n", pair[0], pair[1])
		}
		buf.WriteString("\n")
	}

	for _, block := range []struct {
		label string
		refs  []string
		sign  string
	}{
		{"only in A", r.NodesOnlyInA, "-"},
		{"only in B", r.NodesOnlyInB, "+"},
	} {
		if len(block.refs) == 0 {
			continue
		}
		fmt.Fprintf(&buf, "Nodes %s (%d):\n", block.label, len(block.refs))
		for _, ref := range block.refs {
			fmt.Fprintf(&buf, "  %s %s\n", block.sign, ref)
		}
		buf.WriteString("\n")
	}

	if len(r.NodesChanged) > 0 {
		fmt.Fprintf(&buf, "Nodes changed (%d of %d shared):\n", len(r.NodesChanged), r.NodesShared)
		for _, n := range r.NodesChanged {
			fmt.Fprintf(&buf, "  ~ %s [%s] %s\n", n.Ref, orDash(n.Type), orDash(n.Label))
			for _, f := range n.Fields {
				fmt.Fprintf(&buf, "      %-20s %d -> %d bytes\n", f.Field, f.BytesA, f.BytesB)
			}
		}
		buf.WriteString("\n")
	}

	if len(r.EdgesOnlyInA) > 0 || len(r.EdgesOnlyInB) > 0 {
		buf.WriteString("Edges:\n")
		for _, e := range r.EdgesOnlyInA {
			fmt.Fprintf(&buf, "  - %s\n", formatTriple(e))
		}
		for _, e := range r.EdgesOnlyInB {
			fmt.Fprintf(&buf, "  + %s\n", formatTriple(e))
		}
		buf.WriteString("\n")
	}

	renderVars(&buf, "customVariables", r.CustomVarsOnlyInA, r.CustomVarsOnlyInB, r.CustomVarsChanged)
	renderVars(&buf, "inputVariables", r.InputVarsOnlyInA, r.InputVarsOnlyInB, r.InputVarsChanged)

	renderSpecRefCensus(&buf, r)
	return buf.String()
}

func renderVars(buf *strings.Builder, section string, onlyA, onlyB []string, changed []varDelta) {
	if len(onlyA) == 0 && len(onlyB) == 0 && len(changed) == 0 {
		return
	}
	fmt.Fprintf(buf, "%s:\n", section)
	for _, n := range onlyA {
		fmt.Fprintf(buf, "  - %s\n", n)
	}
	for _, n := range onlyB {
		fmt.Fprintf(buf, "  + %s\n", n)
	}
	for _, v := range changed {
		fmt.Fprintf(buf, "  ~ %-28s %d -> %d bytes\n", v.Name, v.BytesA, v.BytesB)
	}
	buf.WriteString("\n")
}

func renderSpecRefCensus(buf *strings.Builder, r diffReport) {
	if len(r.A.TasksMissingRef) == 0 && len(r.B.TasksMissingRef) == 0 {
		fmt.Fprintf(buf, "specRef: present on all %d tasks in A and all %d in B.\n", r.A.Tasks, r.B.Tasks)
		return
	}
	buf.WriteString("specRef census -- a task with no specRef was written by a client that does not\n")
	buf.WriteString("send it, which in practice means it was edited in the Hub builder. On a\n")
	buf.WriteString("CLI-authored workflow, read those nodes first: the diff hunks on them are as\n")
	buf.WriteString("likely to be editor-generated as authored.\n")
	for _, s := range []struct {
		tag  string
		side sideSummary
	}{{"A", r.A}, {"B", r.B}} {
		fmt.Fprintf(buf, "  %s: %d of %d tasks missing specRef\n", s.tag, len(s.side.TasksMissingRef), s.side.Tasks)
		for _, alias := range s.side.TasksMissingRef {
			fmt.Fprintf(buf, "      %s\n", alias)
		}
	}
}

func formatTriple(e edgeTriple) string {
	handle := e.Handle
	if handle == "" {
		handle = "-"
	}
	return fmt.Sprintf("%s [%s] -> %s", e.Source, handle, e.Target)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func orZero(v any) any {
	if v == nil {
		return 0
	}
	return v
}
