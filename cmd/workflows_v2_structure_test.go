package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Direct assignment done right: one real indicator, everything else wired directly, the
// decision taken from a rule-tree.
const structureCleanSpecJSON = `{
  "label": "Structure Clean",
  "alias": "structure-clean",
  "category": "EVALUATION",
  "customVariables": {
    "debt_to_income": {
      "type": "number",
      "dependencies": ["inputs.monthly_debt", "inputs.monthly_income"],
      "expression": "income = inputs.get('inputs.monthly_income') or 0\ndebt = inputs.get('inputs.monthly_debt') or 0\nresult = round(debt / income, 4) if income > 0 else None",
      "returnValue": "result",
      "onError": "null"
    }
  },
  "nodes": [
    {"ref": "start", "type": "start", "label": "Start"},
    {"ref": "applicant", "type": "customer", "label": "Applicant", "operation": "write",
     "lookupBy": "identity", "key": "tax_id", "updateIfExists": true,
     "inputMappings": {"tax_id": "inputs.tax_id", "legal_name": "inputs.legal_name"},
     "sourcesConfig": [{"type": "identity", "key": "tax_id"}, {"type": "borrower_field", "key": "legal_name"}]},
    {"ref": "bureau", "type": "altdata-enrichment", "label": "Bureau", "mode": "single",
     "continueOnFailure": true, "borrowerIdField": "borrower_id",
     "sourcesConfig": [{"sourceId": "SRC-0001", "version": "v1"}],
     "inputKeys": {"taxId": "{{tax_id}}"},
     "inputMappings": {"borrower_id": "task_outputs.applicant.borrower_id", "tax_id": "inputs.tax_id"}},
    {"ref": "indicators", "type": "compute-variables", "label": "Indicators",
     "selectedVariables": ["debt_to_income"]},
    {"ref": "score-band", "type": "mapping-table", "label": "Score band",
     "inputMappings": {"bureau_score": "task_outputs.bureau.SRC-0001.data.score"},
     "mappingTableConfig": {"entries": [
       {"inputVariable": "bureau_score", "mappingTableCode": "score-band-table", "outputVariable": "score_band"}]}},
    {"ref": "policy", "type": "rule-tree", "label": "Credit policy",
     "ruleTreeConfig": {"ruleTreeCode": "policy-tree", "inputMappings": {
       "bureau_ok":        "task_outputs.bureau.SRC-0001.isSuccess",
       "sanctions_hits":   "task_outputs.bureau.SRC-0001.data.sanctions_count",
       "score_band":       "task_outputs.score-band.score_band",
       "debt_to_income":   "task_outputs.indicators.debt_to_income",
       "requested_amount": "inputs.requested_amount"}}},
    {"ref": "end", "type": "end", "label": "End",
     "inputMappings": {
       "borrower_id":  "task_outputs.applicant.borrower_id",
       "decision_key": "task_outputs.policy.decision_key"},
     "endConfig": {
       "decisionConfig": {"enabled": true, "decisionType": "final"},
       "pdfConfig": {"enabled": true, "title": "Credit decision"},
       "outputJson": "{\"decision\": \"{{task_outputs.policy.decision_key}}\", \"debt_to_income\": \"{{task_outputs.indicators.debt_to_income}}\"}"}}
  ],
  "edges": [
    {"from": "start", "to": "applicant"}, {"from": "applicant", "to": "bureau"},
    {"from": "bureau", "to": "indicators"}, {"from": "indicators", "to": "score-band"},
    {"from": "score-band", "to": "policy"}, {"from": "policy", "to": "end"}
  ]
}`

func structureSpecMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(structureCleanSpecJSON), &m); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return m
}

func structureRun(t *testing.T, spec map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var cs composeSpec
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	adviseWorkflowStructure(structureGraphFromSpec(&cs), &buf)
	return buf.String()
}

func structureLines(out, practice string) []string {
	var lines []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "[structure] "+practice+":") {
			lines = append(lines, ln)
		}
	}
	return lines
}

func setStructureVar(spec map[string]any, name string, def map[string]any) {
	spec["customVariables"].(map[string]any)[name] = def
}

func addStructureCompute(spec map[string]any, ref string, vars ...string) {
	sel := make([]any, len(vars))
	for i, v := range vars {
		sel[i] = v
	}
	spec["nodes"] = append(spec["nodes"].([]any), map[string]any{
		"ref": ref, "type": "compute-variables", "label": ref, "selectedVariables": sel,
	})
}

func structureNodeByRef(spec map[string]any, ref string) map[string]any {
	for _, n := range spec["nodes"].([]any) {
		if nm := n.(map[string]any); nm["ref"] == ref {
			return nm
		}
	}
	return nil
}

func setStructureEndDecision(spec map[string]any, src string) {
	structureNodeByRef(spec, "end")["inputMappings"].(map[string]any)["decision_key"] = src
}

func pyVar(expression string) map[string]any {
	return map[string]any{"type": "string", "expression": expression, "returnValue": "result"}
}

func requireOneLine(t *testing.T, out, practice string, wants ...string) {
	t.Helper()
	lines := structureLines(out, practice)
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 %s line, got %d:\n%s", practice, len(lines), out)
	}
	for _, w := range wants {
		if !strings.Contains(lines[0], w) {
			t.Errorf("%s line is missing %q:\n%s", practice, w, lines[0])
		}
	}
}

func requireNoLine(t *testing.T, out, practice string) {
	t.Helper()
	if lines := structureLines(out, practice); len(lines) != 0 {
		t.Fatalf("want no %s line, got:\n%s", practice, strings.Join(lines, "\n"))
	}
}

func TestStructureCleanSpecPrintsNothing(t *testing.T) {
	if out := structureRun(t, structureSpecMap(t)); out != "" {
		t.Fatalf("clean spec must print nothing, got:\n%s", out)
	}
}

const structurePolicyExpression = "dti = inputs.get('task_outputs.indicators.debt_to_income')\n" +
	"ok = inputs.get('task_outputs.bureau.SRC-0001.isSuccess')\n" +
	"if not ok:\n    result = 'refer'\n" +
	"elif dti is not None and dti > 0.45:\n    result = 'decline'\n" +
	"else:\n    result = 'approve'"

func TestStructureDecisionInPython(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "decision", pyVar(structurePolicyExpression))
	addStructureCompute(spec, "decide", "decision")
	setStructureEndDecision(spec, "task_outputs.decide.decision")
	requireOneLine(t, structureRun(t, spec), "decision-in-python",
		`end "end" takes decision_key from Python variable "decision" (node "decide")`,
		"task_outputs.policy.decision_key",
		"mapping tables or evaluation rules")

	// A lazy custom.<name> read and an exported server alias both name the same Python variable.
	for _, src := range []string{"custom.decision", "{{task_outputs.decide-9f8e7d.decision}}"} {
		setStructureEndDecision(spec, src)
		requireOneLine(t, structureRun(t, spec), "decision-in-python", `Python variable "decision"`)
	}

	// The finding names the variable that decides, not the one that unpacks it.
	setStructureVar(spec, "decision_key", pyVar("d = inputs.get('self.decision') or {}\nresult = d.get('key')"))
	addStructureCompute(spec, "unpack", "decision_key")
	setStructureEndDecision(spec, "task_outputs.unpack.decision_key")
	requireOneLine(t, structureRun(t, spec), "decision-in-python", `Python variable "decision" (node "decide")`)
}

func TestStructureDecisionInPythonNegative(t *testing.T) {
	requireNoLine(t, structureRun(t, structureSpecMap(t)), "decision-in-python")

	// Re-exposing the rule-tree's decision is a passthrough, not a decision made in Python.
	spec := structureSpecMap(t)
	setStructureVar(spec, "final", pyVar("result = inputs.get('task_outputs.policy.decision_key') or 'refer'"))
	addStructureCompute(spec, "relay", "final")
	setStructureEndDecision(spec, "task_outputs.relay.final")
	out := structureRun(t, spec)
	requireNoLine(t, out, "decision-in-python")
	requireOneLine(t, out, "passthrough", "task_outputs.policy.decision_key")
}

func TestStructureDecisionFeedFromStandardOutput(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "decision", pyVar(structurePolicyExpression))
	addStructureCompute(spec, "decide", "decision")
	end := structureNodeByRef(spec, "end")
	delete(end["inputMappings"].(map[string]any), "decision_key")
	end["endConfig"].(map[string]any)["standardOutput"] = map[string]any{
		"enabled": true, "decision": "{{task_outputs.decide.decision}}",
	}
	requireOneLine(t, structureRun(t, spec), "decision-in-python", `standardOutput.decision from Python variable "decision"`)
}

func TestStructurePassthrough(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "bureau_ok", pyVar("ok = inputs.get('task_outputs.bureau.SRC-0001.isSuccess')\nresult = ok"))
	addStructureCompute(spec, "gates", "bureau_ok")
	requireOneLine(t, structureRun(t, spec), "passthrough",
		`variable "bureau_ok" (node "gates") only re-exposes task_outputs.bureau.SRC-0001.isSuccess`,
		"wire task_outputs.bureau.SRC-0001.isSuccess directly")

	cast := structureSpecMap(t)
	setStructureVar(cast, "amount", pyVar("result = float(inputs.get('inputs.requested_amount') or 0)"))
	requireOneLine(t, structureRun(t, cast), "passthrough", "inputs.requested_amount through float()")

	sibling := structureSpecMap(t)
	setStructureVar(sibling, "dti_copy", pyVar("x = inputs['self.debt_to_income']\nresult = x"))
	requireOneLine(t, structureRun(t, sibling), "passthrough", `re-exposes variable "debt_to_income"`)
}

func TestStructurePassthroughNegative(t *testing.T) {
	requireNoLine(t, structureRun(t, structureSpecMap(t)), "passthrough")

	// The bare probe shape belongs to the extraction-probe advisory; never report it twice.
	spec := structureSpecMap(t)
	probe := pyVar(`result = inputs.get("task_outputs.bureau.SRC-0001.isSuccess")`)
	if _, ok := isPureExtractionProbe(probe); !ok {
		t.Fatal("fixture should be an extraction probe")
	}
	setStructureVar(spec, "bureau_ok", probe)
	if out := structureRun(t, spec); out != "" {
		t.Fatalf("probe must stay with its own advisory, got:\n%s", out)
	}
}

func TestStructureExtractOnly(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "income", pyVar("app = inputs.get('inputs.application') or {}\nresult = app.get('monthly_income')"))
	setStructureVar(spec, "score", pyVar("data = inputs.get('task_outputs.bureau.SRC-0001.data') or {}\nresult = data.get('score', 0)"))
	out := structureRun(t, spec)
	lines := structureLines(out, "extract-only")
	if len(lines) != 2 {
		t.Fatalf("want 2 extract-only lines, got:\n%s", out)
	}
	if !strings.Contains(lines[0], "wire the deep path inputs.application.monthly_income directly") {
		t.Errorf("income: %s", lines[0])
	}
	if !strings.Contains(lines[1], "wire the deep path task_outputs.bureau.SRC-0001.data.score directly") {
		t.Errorf("score: %s", lines[1])
	}

	sibling := structureSpecMap(t)
	setStructureVar(sibling, "decision", pyVar(structurePolicyExpression))
	setStructureVar(sibling, "decision_label", pyVar("d = inputs.get('self.decision') or {}\nresult = d.get('label')"))
	addStructureCompute(sibling, "decide", "decision", "decision_label")
	requireOneLine(t, structureRun(t, sibling), "extract-only",
		`only extracts label from variable "decision"`, "task_outputs.decide.decision.label")
}

func TestStructureExtractOnlyNegative(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "annual_income", pyVar("app = inputs.get('inputs.application') or {}\nresult = app.get('monthly_income', 0) * 12"))
	setStructureVar(spec, "picked", pyVar("app = inputs.get('inputs.application') or {}\nresult = app.get(app.get('field_name'))"))
	setStructureVar(spec, "spaced", pyVar("app = inputs.get('inputs.application') or {}\nresult = app.get('Fecha Nacimiento')"))
	if out := structureRun(t, spec); out != "" {
		t.Fatalf("derived or non-path extractions must not be flagged, got:\n%s", out)
	}
}

func TestStructureConstant(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "min_score", pyVar("result = 613"))
	setStructureVar(spec, "reason", pyVar(""))
	out := structureRun(t, spec)
	lines := structureLines(out, "constant")
	if len(lines) != 2 {
		t.Fatalf("want 2 constant lines, got:\n%s", out)
	}
	if !strings.Contains(lines[0], `variable "min_score" only holds the constant 613`) {
		t.Errorf("min_score: %s", lines[0])
	}
	if !strings.Contains(lines[1], `variable "reason" has an empty expression`) {
		t.Errorf("reason: %s", lines[1])
	}
}

func TestStructureConstantNegative(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "applicant_age", pyVar("from datetime import date\n"+
		"born = inputs.get('inputs.birth_date')\n"+
		"result = (date.today() - date.fromisoformat(born)).days // 365 if born else None"))
	if out := structureRun(t, spec); out != "" {
		t.Fatalf("a derived age is not a constant, got:\n%s", out)
	}
}

func ratioVar(i int) map[string]any {
	return map[string]any{"type": "number", "returnValue": "result", "expression": fmt.Sprintf(
		"a = inputs.get('inputs.a%d') or 0\nb = inputs.get('inputs.b%d') or 1\nresult = a / b", i, i)}
}

func htmlVar(i int) map[string]any {
	return map[string]any{"type": "string", "returnValue": "result", "expression": fmt.Sprintf(
		"v = inputs.get('task_outputs.policy.decision_key')\nresult = '<div>%d ' + str(v) + '</div>'", i)}
}

func TestStructureWideComputeNode(t *testing.T) {
	spec := structureSpecMap(t)
	var names []string
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("ratio_%d", i)
		setStructureVar(spec, name, ratioVar(i))
		names = append(names, name)
	}
	addStructureCompute(spec, "ratios", names...)
	requireOneLine(t, structureRun(t, spec), "wide-compute-node",
		`compute node "ratios" computes 7 variables`, "split it into small single-purpose nodes")
}

func TestStructureWideComputeNodeNegative(t *testing.T) {
	six := structureSpecMap(t)
	var names []string
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("ratio_%d", i)
		setStructureVar(six, name, ratioVar(i))
		names = append(names, name)
	}
	addStructureCompute(six, "ratios", names...)
	requireNoLine(t, structureRun(t, six), "wide-compute-node")

	// A PDF node of HTML blocks is not a rules engine.
	pdf := structureSpecMap(t)
	names = nil
	for i := 0; i < 9; i++ {
		name := fmt.Sprintf("html_block_%d", i)
		setStructureVar(pdf, name, htmlVar(i))
		names = append(names, name)
	}
	addStructureCompute(pdf, "report", names...)
	if out := structureRun(t, pdf); out != "" {
		t.Fatalf("HTML builders must not be flagged, got:\n%s", out)
	}
}

func chainedVar(dep string) map[string]any {
	return map[string]any{"type": "boolean", "returnValue": "result",
		"dependencies": []any{"self." + dep},
		"expression":   fmt.Sprintf("x = inputs.get('self.%s') or 0\nresult = x > 0.4", dep)}
}

func TestStructureSelfChain(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "step_1", chainedVar("debt_to_income"))
	setStructureVar(spec, "step_2", chainedVar("step_1"))
	setStructureVar(spec, "step_3", chainedVar("step_2"))
	addStructureCompute(spec, "rules", "debt_to_income", "step_1", "step_2", "step_3")
	requireOneLine(t, structureRun(t, spec), "self-chain",
		`compute node "rules" chains 3 variables through self. (step_1, step_2, step_3)`,
		"rule-tree or evaluation rules")
}

func TestStructureSelfChainNegative(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "step_1", chainedVar("debt_to_income"))
	setStructureVar(spec, "step_2", chainedVar("step_1"))
	addStructureCompute(spec, "rules", "debt_to_income", "step_1", "step_2")
	requireNoLine(t, structureRun(t, spec), "self-chain")
}

func bandingPolicy() string {
	var b strings.Builder
	b.WriteString("score = inputs.get('task_outputs.bureau.SRC-0001.data.score') or 0\nband = 'band_none'\n")
	for i := 0; i < 30; i++ {
		kw := "elif"
		if i == 0 {
			kw = "if"
		}
		fmt.Fprintf(&b, "%s score > %d:\n    band = 'band_%02d'\n", kw, 900-i*10, i)
	}
	b.WriteString("result = band\n")
	return b.String()
}

func TestStructureLargePolicyVariable(t *testing.T) {
	spec := structureSpecMap(t)
	expr := bandingPolicy()
	if n := pyCodeChars(expr); n <= structureMaxPolicyCodeChars {
		t.Fatalf("fixture too small: %d", n)
	}
	setStructureVar(spec, "score_band", pyVar(expr))
	requireOneLine(t, structureRun(t, spec), "large-policy-variable",
		`variable "score_band" is`, "no loop, parsing or HTML", "move its thresholds and bands into mapping tables")
}

func TestStructureLargePolicyVariableNegative(t *testing.T) {
	cases := map[string]string{
		"iteration": "total = sum(a.get('balance') or 0 for a in (inputs.get('task_outputs.bureau.SRC-0001.data.accounts') or []))\n" + bandingPolicy(),
		"html":      "header = '<table>'\n" + bandingPolicy(),
		"parsing":   "import json\n" + bandingPolicy(),
		"comments":  strings.Repeat("# the bands come from the credit committee minutes of last quarter\n", 30) + "result = inputs.get('inputs.a') * 2",
	}
	for name, expr := range cases {
		spec := structureSpecMap(t)
		setStructureVar(spec, "big", pyVar(expr))
		if lines := structureLines(structureRun(t, spec), "large-policy-variable"); len(lines) != 0 {
			t.Errorf("%s must be exempt, got: %v", name, lines)
		}
	}
}

func TestStructureObjectReturn(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "typed_object", map[string]any{"type": "object", "returnValue": "result",
		"expression": "x = inputs.get('inputs.a') or 0\nresult = x * 2"})
	setStructureVar(spec, "inline_bundle", map[string]any{"type": "string",
		"expression":  "d = inputs.get('inputs.a')\nr = inputs.get('inputs.b')",
		"returnValue": "{'decision_key': d, 'reasons': r}"})
	setStructureVar(spec, "assigned_bundle", pyVar("score = inputs.get('task_outputs.bureau.SRC-0001.data.score') or 0\n"+
		"result = {\n    'band': 'A' if score > 700 else 'B',\n    'score': score,\n}"))
	setStructureVar(spec, "pair", map[string]any{"type": "string",
		"expression": "a = inputs.get('inputs.a')\nb = inputs.get('inputs.b')", "returnValue": "[a, b]"})
	out := structureRun(t, spec)
	lines := structureLines(out, "object-return")
	if len(lines) != 4 {
		t.Fatalf("want 4 object-return lines, got:\n%s", out)
	}
	byVar := map[string]string{}
	for _, ln := range lines {
		for _, name := range []string{"typed_object", "inline_bundle", "assigned_bundle", "pair"} {
			if strings.Contains(ln, fmt.Sprintf("variable %q", name)) {
				byVar[name] = ln
			}
		}
	}
	if !strings.Contains(byVar["inline_bundle"], "(decision_key, reasons)") ||
		!strings.Contains(byVar["inline_bundle"], "the decision itself belongs in a rule-tree") {
		t.Errorf("inline_bundle: %s", byVar["inline_bundle"])
	}
	if !strings.Contains(byVar["assigned_bundle"], "(band, score)") {
		t.Errorf("assigned_bundle: %s", byVar["assigned_bundle"])
	}
	if !strings.Contains(byVar["typed_object"], "one scalar per variable") {
		t.Errorf("typed_object: %s", byVar["typed_object"])
	}
}

func TestStructureObjectReturnNegative(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "label_lookup", pyVar(
		"result = {'A': 'approve', 'B': 'refer'}[inputs.get('task_outputs.score-band.score_band') or 'B']"))
	setStructureVar(spec, "parsed", map[string]any{"type": "object", "returnValue": "result",
		"expression": "import json\nresult = json.loads(inputs.get('task_outputs.bureau.SRC-0001.sourceData.raw') or '{}')"})
	setStructureVar(spec, "parties", map[string]any{"type": "array",
		"expression":  "a = inputs.get('inputs.applicant_tax_id')\nb = inputs.get('inputs.guarantor_tax_id')",
		"returnValue": "[{'tax_id': a}, {'tax_id': b}]"})
	setStructureVar(spec, "party_list_untyped", map[string]any{"type": "string",
		"expression":  "a = inputs.get('inputs.applicant_tax_id')\nb = inputs.get('inputs.guarantor_tax_id')",
		"returnValue": "[{'tax_id': a}, {'tax_id': b}]"})
	requireNoLine(t, structureRun(t, spec), "object-return")
}

// lint reads GET /v2/workflows/{id} (graph only, node.data.inputMappings as the canvas mirror)
// plus the task bodies of the end and compute nodes.
func structureAsWorkflow(t *testing.T, spec map[string]any, withBodies bool) (map[string]any, []map[string]any) {
	t.Helper()
	aliasOf := func(ref string) string { return ref + "-1a2b3c" }
	var refs []string
	for _, n := range spec["nodes"].([]any) {
		refs = append(refs, n.(map[string]any)["ref"].(string))
	}
	realias := func(v any) any {
		raw, _ := json.Marshal(v)
		s := string(raw)
		for _, r := range refs {
			s = strings.ReplaceAll(s, "task_outputs."+r+".", "task_outputs."+aliasOf(r)+".")
		}
		var out any
		_ = json.Unmarshal([]byte(s), &out)
		return out
	}
	var nodes []any
	var bodies []map[string]any
	for _, raw := range realias(spec["nodes"]).([]any) {
		n := raw.(map[string]any)
		ref := n["ref"].(string)
		alias := aliasOf(ref)
		label, _ := n["label"].(string)
		node := map[string]any{"nodeId": alias, "type": n["type"], "label": label, "taskAlias": alias, "data": map[string]any{}}
		if im, ok := n["inputMappings"]; ok {
			node["data"].(map[string]any)["inputMappings"] = im
		}
		nodes = append(nodes, node)
		if t := n["type"]; withBodies && (t == "end" || t == "compute-variables") {
			body := map[string]any{"alias": alias}
			for k, v := range n {
				if k != "ref" {
					body[k] = v
				}
			}
			bodies = append(bodies, body)
		}
	}
	wf := map[string]any{"id": "wf-1", "alias": "structure-clean", "nodes": nodes,
		"customVariables": realias(spec["customVariables"])}
	return wf, bodies
}

func structureRunWorkflow(wf map[string]any, bodies []map[string]any) string {
	var buf bytes.Buffer
	adviseWorkflowStructure(structureGraphFromWorkflow(wf, bodies), &buf)
	return buf.String()
}

func TestStructureWorkflowShapeCleanPrintsNothing(t *testing.T) {
	wf, bodies := structureAsWorkflow(t, structureSpecMap(t), true)
	if out := structureRunWorkflow(wf, bodies); out != "" {
		t.Fatalf("clean workflow must print nothing, got:\n%s", out)
	}
}

func TestStructureWorkflowShapeFindings(t *testing.T) {
	spec := structureSpecMap(t)
	setStructureVar(spec, "decision", pyVar(structurePolicyExpression))
	var names []string
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("ratio_%d", i)
		setStructureVar(spec, name, ratioVar(i))
		names = append(names, name)
	}
	addStructureCompute(spec, "decide", append(names, "decision")...)
	structureNodeByRef(spec, "decide")["label"] = "Decide"
	setStructureEndDecision(spec, "task_outputs.decide.decision")

	wf, bodies := structureAsWorkflow(t, spec, true)
	out := structureRunWorkflow(wf, bodies)
	requireOneLine(t, out, "decision-in-python",
		`end "End" (end-1a2b3c) takes decision_key from Python variable "decision" (node "Decide" (decide-1a2b3c))`)
	requireOneLine(t, out, "wide-compute-node", `compute node "Decide" (decide-1a2b3c) computes 8 variables`)

	// Task bodies that could not be fetched: the canvas mirror still carries decision_key.
	wf, _ = structureAsWorkflow(t, spec, false)
	out = structureRunWorkflow(wf, nil)
	requireOneLine(t, out, "decision-in-python", `Python variable "decision" (node "Decide" (decide-1a2b3c))`)
	requireNoLine(t, out, "wide-compute-node")
}

func TestStructureCapsLinesPerPractice(t *testing.T) {
	spec := structureSpecMap(t)
	for i := 0; i < 8; i++ {
		setStructureVar(spec, fmt.Sprintf("reason_%d", i), pyVar(fmt.Sprintf("result = 'reason_%d'", i)))
	}
	out := structureRun(t, spec)
	lines := structureLines(out, "constant")
	if len(lines) != structurePracticeCap+1 {
		t.Fatalf("want %d constant lines (cap + summary), got:\n%s", structurePracticeCap+1, out)
	}
	if !strings.Contains(lines[len(lines)-1], "3 more with the same fix: reason_5, reason_6, reason_7") {
		t.Errorf("summary line: %s", lines[len(lines)-1])
	}
	if !strings.HasPrefix(out, "# structure advisory: 8 finding(s).") {
		t.Errorf("header: %s", strings.SplitN(out, "\n", 2)[0])
	}
}

func TestStructureNeverPanicsOnOddInput(t *testing.T) {
	adviseWorkflowStructure(nil, &bytes.Buffer{})
	adviseWorkflowStructure(structureGraphFromSpec(&composeSpec{}), nil)
	var buf bytes.Buffer
	adviseWorkflowStructure(structureGraphFromSpec(&composeSpec{
		CustomVariables: map[string]any{"x": "not a map", "y": map[string]any{"expression": 42}},
		Tasks: []map[string]any{
			{"type": "end", "inputMappings": map[string]any{"decision_key": 7}},
			{"type": "compute-variables", "selectedVariables": "nope"},
		},
	}), &buf)
	adviseWorkflowStructure(structureGraphFromWorkflow(map[string]any{"nodes": []any{"x", nil}}, nil), &buf)
	if buf.Len() != 0 {
		t.Fatalf("odd input must stay silent, got:\n%s", buf.String())
	}
}

func TestAnalyzePythonVariable(t *testing.T) {
	cases := []struct {
		name, expr, rv string
		kind           pyShapeKind
		path, lit      string
	}{
		{"passthrough", "result = inputs.get('inputs.x')", "", shapePassthrough, "inputs.x", ""},
		{"comments and subscript", "# read it\nresult = inputs['inputs.x']  # trailing", "result", shapePassthrough, "inputs.x", ""},
		{"cast", "result = str(inputs.get('inputs.x'))", "result", shapePassthrough, "inputs.x", ""},
		{"default kept", "result = inputs.get('task_outputs.policy.decision_key') or 'refer'", "result", shapePassthrough, "task_outputs.policy.decision_key", ""},
		{"named return value", "a = inputs.get('inputs.a')", "a", shapePassthrough, "inputs.a", ""},
		{"return statement", "return inputs.get('inputs.x')", "", shapePassthrough, "inputs.x", ""},
		{"index", "d = inputs.get('task_outputs.b.SRC-1.data', {})\nresult = d.get('items', [])[0]", "result", shapeExtract, "task_outputs.b.SRC-1.data.items[0]", ""},
		{"multi-line parens", "result = (\n    inputs.get('inputs.app')\n    or {}\n).get('x')", "result", shapeExtract, "inputs.app.x", ""},
		{"string with hash", "result = 'a#b'", "result", shapeConstant, "", `"a#b"`},
		{"negative number", "result = -1", "result", shapeConstant, "", "-1"},
		{"none", "result = None", "result", shapeConstant, "", "None"},
		{"empty dict", "result = {}", "result", shapeConstant, "", "{}"},
		{"inert only", "\"\"\"doc\"\"\"\nimport math\npass", "result", shapeEmpty, "", ""},
		{"blank", "   \n ", "result", shapeEmpty, "", ""},
		{"two dependencies", "result = inputs.get('inputs.a') or inputs.get('inputs.b')", "result", shapeTransform, "", ""},
		{"arithmetic", "result = inputs.get('inputs.a') + 1", "result", shapeTransform, "", ""},
		{"f-string", "result = f\"{inputs.get('inputs.a')}\"", "result", shapeTransform, "", ""},
		{"dynamic key", "result = inputs.get(key)", "result", shapeTransform, "", ""},
		{"flat key", "result = inputs.get('amount')", "result", shapeTransform, "", ""},
		{"key with a space", "x = inputs.get('inputs.a')\nresult = x.get('field name')", "result", shapeTransform, "", ""},
		{"branch", "if True:\n    result = 1", "result", shapeTransform, "", ""},
		{"unknown name", "result = foo", "result", shapeTransform, "", ""},
		{"too many statements", "a = 1\nb = 2\nc = 3\nd = 4\nresult = inputs.get('inputs.a')", "result", shapeTransform, "", ""},
		{"non-empty dict", "result = {'a': 1}", "result", shapeTransform, "", ""},
		{"unterminated string", "result = 'oops", "result", shapeTransform, "", ""},
		{"method call", "result = (inputs.get('inputs.a') or '').strip()", "result", shapeTransform, "", ""},
	}
	for _, c := range cases {
		got := analyzePythonVariable(c.expr, c.rv)
		if got.kind != c.kind {
			t.Errorf("%s: kind %d, want %d", c.name, got.kind, c.kind)
			continue
		}
		if c.path != "" && got.deepPath() != c.path {
			t.Errorf("%s: path %q, want %q", c.name, got.deepPath(), c.path)
		}
		if c.lit != "" && got.lit != c.lit {
			t.Errorf("%s: lit %q, want %q", c.name, got.lit, c.lit)
		}
	}
}

func TestPyExemptKind(t *testing.T) {
	cases := map[string]string{
		"result = '<div>' + x + '</div>'":                     "html",
		"import json\nresult = 1":                             "parsing",
		"from xml.etree import ElementTree as ET\nresult = 1": "parsing",
		"result = json.loads(raw)":                            "parsing",
		"first, last = name.split(' ', 1)":                    "parsing",
		"result = [a for a in items]":                         "iteration",
		"while x:\n    x -= 1":                                "iteration",
		"# for every band\nresult = 1 if score > 700 else 0":  "",
		"result = 'approve' if score > 700 else 'decline'":    "",
	}
	for expr, want := range cases {
		if got := pyExemptKind(expr); got != want {
			t.Errorf("pyExemptKind(%q) = %q, want %q", expr, got, want)
		}
	}
}

// apply prints the group from composeWorkflowBody, before any request, in every mode.
func TestComposeWorkflowBodyPrintsStructureAdvisory(t *testing.T) {
	spec := func(decisionSrc string) *composeSpec {
		return &composeSpec{
			Label:      "Structure apply",
			Category:   "EVALUATION",
			ExtraNodes: []map[string]any{{"ref": "start", "type": "start", "label": "Start"}},
			CustomVariables: map[string]any{
				"decision": map[string]any{
					"type": "string", "returnValue": "result",
					"dependencies": []any{"task_outputs.fetch.body.score"},
					"expression":   "s = inputs.get('task_outputs.fetch.body.score') or 0\nresult = 'approve' if s > 700 else 'decline'",
				},
			},
			Tasks: []map[string]any{
				{"ref": "fetch", "type": "http", "label": "Fetch", "method": "GET", "url": "https://example.test/x"},
				{"ref": "decide", "type": "compute-variables", "label": "Decide", "selectedVariables": []any{"decision"}},
				{"ref": "end", "type": "end", "label": "End",
					"inputMappings": map[string]any{"decision_key": decisionSrc}},
			},
			Edges: []map[string]any{
				{"from": "start", "to": "fetch"}, {"from": "fetch", "to": "decide"}, {"from": "decide", "to": "end"},
			},
		}
	}
	var err error
	stderr := captureStderr(t, func() {
		_, err = composeWorkflowBody(nil, spec("task_outputs.decide.decision"), true, false, true, false, false, true, newComposeCapture())
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if !strings.Contains(stderr, `[structure] decision-in-python: end "end" takes decision_key from Python variable "decision" (node "decide")`) {
		t.Fatalf("apply must print the structure advisory, got:\n%s", stderr)
	}

	stderr = captureStderr(t, func() {
		_, err = composeWorkflowBody(nil, spec("task_outputs.fetch.body.decision"), true, false, true, false, false, true, newComposeCapture())
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if strings.Contains(stderr, "[structure] decision-in-python") {
		t.Fatalf("decision taken from a task output must not be flagged, got:\n%s", stderr)
	}
}

func TestGuardedExtractionReadsAsTheDeepPath(t *testing.T) {
	cases := []struct {
		name, expr string
		want       pyShapeKind
	}{
		{"guard on the container", "d = inputs.get(\"task_outputs.doc.extraction\") or {}\nresult = d.get(\"name\") if d else None", shapeExtract},
		{"is not None guard", "x = inputs.get(\"task_outputs.doc.extraction\")\nresult = x.get(\"name\") if x is not None else None", shapeExtract},
		{"inverted guard", "x = inputs.get(\"task_outputs.doc.extraction\")\nresult = None if x is None else x[\"name\"]", shapeExtract},
		{"guard on the value itself", "result = inputs.get(\"task_outputs.doc.extraction\") if inputs.get(\"task_outputs.doc.extraction\") else \"\"", shapePassthrough},
		{"guard on another value is a gate", "result = inputs.get(\"task_outputs.doc.name\") if inputs.get(\"task_outputs.doc.isSuccess\") else None", shapeTransform},
		{"a computed fallback is a transform", "x = inputs.get(\"task_outputs.doc.extraction\")\nresult = x.get(\"name\") if x else x.get(\"alias\")", shapeTransform},
		{"a guard deeper than the value is a transform", "x = inputs.get(\"task_outputs.doc.extraction\")\nresult = x if x.get(\"name\") else None", shapeTransform},
	}
	for _, tc := range cases {
		if got := analyzePythonVariable(tc.expr, "result"); got.kind != tc.want {
			t.Errorf("%s: got kind %d, want %d", tc.name, got.kind, tc.want)
		}
	}
}
