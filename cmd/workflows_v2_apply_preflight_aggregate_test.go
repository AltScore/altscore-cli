package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func threeBadRefSpec() *composeSpec {
	return &composeSpec{
		Label:      "Aggregate check",
		Alias:      "aggregate-check",
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			{"ref": "step_one", "type": "http", "label": "One", "url": "https://x.test", "method": "GET"},
			{"ref": "stepTwo", "type": "http", "label": "Two", "url": "https://x.test", "method": "GET"},
			{"ref": "Step Three", "type": "http", "label": "Three", "url": "https://x.test", "method": "GET"},
			{"ref": "end", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "step_one"},
			{"from": "step_one", "to": "stepTwo"},
			{"from": "stepTwo", "to": "Step Three"},
			{"from": "Step Three", "to": "end"},
		},
	}
}

func TestPreflightTasks_ReportsEveryBadRefInOneError(t *testing.T) {
	err := preflightTasks(threeBadRefSpec())
	if err == nil {
		t.Fatal("preflight must reject refs outside ^[a-z0-9][a-z0-9-]*$ before any request")
	}
	msg := err.Error()
	if got := strings.Count(msg, "has invalid characters"); got != 3 {
		t.Fatalf("want all 3 bad refs reported in one error, got %d:\n%s", got, msg)
	}
	if !strings.HasPrefix(msg, "spec has 3 problems") {
		t.Errorf("a multi-problem error should open with the count, got:\n%s", msg)
	}
	for bad, fix := range map[string]string{"step_one": "step-one", "stepTwo": "step-two", "Step Three": "step-three"} {
		want := `node ref "` + bad + `" has invalid characters: use "` + fix + `"`
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	if got := strings.Count(msg, "rename it in edges[] (from/to) and every reference too"); got != 3 {
		t.Errorf("every bad ref needs the rename instruction, got %d:\n%s", got, msg)
	}
}

func TestPreflightTasks_BadRefsAndTwoEndsReportedTogether(t *testing.T) {
	spec := threeBadRefSpec()
	spec.Tasks = append(spec.Tasks, map[string]any{"ref": "end-two", "type": "end", "label": "End two"})
	spec.Edges = append(spec.Edges, map[string]any{"from": "step_one", "to": "end-two"})

	err := preflightTasks(spec)
	if err == nil {
		t.Fatal("preflight must reject bad refs and a second end node")
	}
	msg := err.Error()
	for _, want := range []string{
		"spec has 4 problems",
		`node ref "step_one" has invalid characters`,
		`node ref "stepTwo" has invalid characters`,
		`node ref "Step Three" has invalid characters`,
		"spec has 2 'end' nodes (end, end-two)",
		"exactly ONE end node",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestPreflightTasks_CleanSpecUnaffected(t *testing.T) {
	spec := &composeSpec{
		Label:      "Clean",
		Alias:      "clean",
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			{"ref": "fetch-data", "type": "http", "label": "Fetch", "url": "https://x.test", "method": "GET"},
			{"ref": "end", "type": "end", "label": "End",
				"inputMappings": map[string]any{"status": "task_outputs.fetch-data.status"}},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "fetch-data"},
			{"from": "fetch-data", "to": "end"},
		},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("a valid spec must pass preflight, got: %v", err)
	}
}

func TestPreflightTasks_SingleProblemKeepsItsOwnMessage(t *testing.T) {
	spec := threeBadRefSpec()
	spec.Tasks[0]["ref"], spec.Tasks[1]["ref"], spec.Tasks[2]["ref"] = "step-one", "step-two", "step_three"
	spec.Edges = []map[string]any{
		{"from": "start", "to": "step-one"},
		{"from": "step-one", "to": "step-two"},
		{"from": "step-two", "to": "step_three"},
		{"from": "step_three", "to": "end"},
	}
	err := preflightTasks(spec)
	if err == nil || !strings.HasPrefix(err.Error(), `node ref "step_three" has invalid characters: use "step-three"`) {
		t.Fatalf("one problem should read as its own message, got: %v", err)
	}
}

func TestPreflightTasks_NodeLocalProblemsAcrossTasksAggregate(t *testing.T) {
	spec := &composeSpec{
		Label:      "Two bad tasks",
		Alias:      "two-bad-tasks",
		Category:   "OTHER",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			docExtractionTask("dx", map[string]any{}, nil),
			{"ref": "call", "type": "http", "label": "Call"},
			{"ref": "end", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "dx"},
			{"from": "dx", "to": "call"},
			{"from": "call", "to": "end"},
		},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatal("preflight must reject both tasks")
	}
	msg := err.Error()
	for _, want := range []string{"extractionSchema", "exactly one document source", `node ref="call": http task requires 'url'`} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestKebabRef(t *testing.T) {
	for in, want := range map[string]string{
		"end_approve":  "end-approve",
		"identityGate": "identity-gate",
		"ABC_Check":    "abc-check",
		"Route Step":   "route-step",
		"a__b":         "a-b",
		"step2Review":  "step2-review",
	} {
		if got := kebabRef(in); got != want {
			t.Errorf("kebabRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInvalidRefProblems_SuggestionNeverCollides(t *testing.T) {
	problems := invalidRefProblems([]string{"End"}, map[string]bool{"end": true, "End": true})
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), `use "end-2"`) {
		t.Fatalf("the suggestion must not collide with an existing ref, got: %v", problems)
	}
}

func notFoundFindingsBody() string {
	return `{"code":"UnprocessableEntity","message":"rejected","details":{"errorSubCode":"APPLY_VALIDATION_FAILED",
		"validation":{"valid":false,"skippedNodeIds":[],"refs":{"policy-a1b2c3":"policy"},"findings":[
			{"code":"RULE_TREE_NOT_FOUND","severity":"error","nodeId":"policy-a1b2c3","edgeId":null,
			 "params":{"entityClass":"rule_tree","code":"policy_tree"},
			 "message":"This step uses the rule tree 'policy_tree', which does not exist on this tenant. The run fails at this step. Create it, or import the bundle that carries it."},
			{"code":"SCORECARD_NOT_FOUND","severity":"error","nodeId":"policy-a1b2c3","edgeId":null,"params":{"code":"sc_base"},"message":"scorecard 'sc_base' missing"},
			{"code":"MAPPING_TABLE_NOT_FOUND","severity":"error","nodeId":"policy-a1b2c3","edgeId":null,"params":{"code":"band_map"},"message":"mapping table 'band_map' missing"},
			{"code":"EVALUATION_RULE_NOT_FOUND","severity":"error","nodeId":"policy-a1b2c3","edgeId":null,"params":{"code":"R-001"},"message":"evaluation rule 'R-001' missing"},
			{"code":"TASK_REFERENCE_NOT_UPSTREAM","severity":"error","nodeId":"policy-a1b2c3","edgeId":null,"params":{},"message":"not upstream"}]},
		"plan":{"mode":"create","workflowAlias":"loan-intake","workflow":{"nodes":[],"edges":[]},"tasks":[]}}}`
}

func TestServerFindings_NotFoundCarriesTheCreateCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(notFoundFindingsBody()))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	wants := []string{
		`[ERROR] RULE_TREE_NOT_FOUND (node "policy"): This step uses the rule tree 'policy_tree'`,
		`Fix: altscore rule-trees create --workflow-alias loan-intake --body @<file> whose "code" is "policy_tree", then dry-run again.`,
		`Fix: altscore scorecards create --workflow-alias loan-intake --body @<file> whose "code" is "sc_base"`,
		`Fix: altscore mapping-tables create --workflow-alias loan-intake --body @<file> whose "code" is "band_map"`,
		`Fix: altscore evaluation-rules create --workflow-alias loan-intake --body @<file> whose "code" is "R-001"`,
	}

	cmd, _, errb := serverApplyTestCmd()
	res, err := applyViaServer(c, cmd, map[string]any{"alias": "loan-intake"}, serverApplyOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run must not abort on findings: %v", err)
	}
	printServerApplySummary(errb, res)
	assertNotFoundRendering(t, errb.String(), wants)

	cmd, _, errb = serverApplyTestCmd()
	if _, err := applyViaServer(c, cmd, map[string]any{"alias": "loan-intake"}, serverApplyOptions{}); err == nil {
		t.Fatal("a real apply must fail on APPLY_VALIDATION_FAILED")
	}
	assertNotFoundRendering(t, errb.String(), wants)
}

func assertNotFoundRendering(t *testing.T, out string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "TASK_REFERENCE_NOT_UPSTREAM") && strings.Contains(line, "Fix:") {
			t.Errorf("only *_NOT_FOUND findings get a create command: %s", line)
		}
		// The quoted code must stay the first single-quoted token on the line.
		if strings.Contains(line, "RULE_TREE_NOT_FOUND") && strings.Index(line, "'policy_tree'") > strings.Index(line, "Fix:") {
			t.Errorf("the create command must follow the server's message: %s", line)
		}
	}
	if got := strings.Count(out, "[ERROR]"); got != 5 {
		t.Errorf("the create command must not add finding lines; want 5 [ERROR], got %d", got)
	}
}

func TestPrintFindingLines_NoAliasNoCreateCommand(t *testing.T) {
	var b bytes.Buffer
	printFindingLines(&b, "ERROR", []validationFinding{{Code: "RULE_TREE_NOT_FOUND", Severity: "error", Params: map[string]any{"code": "x"}, Message: "missing 'x'"}}, nil, "")
	if strings.Contains(b.String(), "Fix:") {
		t.Errorf("import has no workflow alias to name, so no command: %s", b.String())
	}
}

func TestPreflightTasks_MissingReturnValueAggregatesWithShapeProblems(t *testing.T) {
	spec := threeBadRefSpec()
	spec.CustomVariables = map[string]any{
		"ratio":    map[string]any{"expression": "a = 1\nresult = a / 2"},
		"single":   map[string]any{"expression": "value = 3"},
		"literal":  map[string]any{"expression": `inputs.get("x")`},
		"declared": map[string]any{"expression": "result = 1", "returnValue": "result"},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatal("preflight must reject a custom variable whose returnValue is empty")
	}
	msg := err.Error()
	for _, want := range []string{
		"spec has 5 problems",
		`customVariables["ratio"]: returnValue is empty`,
		`assigns (e.g. "result")`,
		`customVariables["single"]: returnValue is empty`,
		`assigns (e.g. "value")`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	for _, unwanted := range []string{`customVariables["literal"]`, `customVariables["declared"]`} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("%s must pass: a bare literal gets its returnValue from assembly, a declared one has it:\n%s", unwanted, msg)
		}
	}
}
