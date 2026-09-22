package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestValidateWorkflowV2Body_RenamesCamelCase(t *testing.T) {
	body := json.RawMessage(`{
		"nodes": [
			{"id": "start", "type": "start", "taskAlias": "start"},
			{"id": "end", "type": "end", "taskAlias": "end"}
		],
		"edges": [
			{"source": "start", "target": "end"}
		]
	}`)
	if err := validateWorkflowV2Body(&body); err != nil {
		t.Fatalf("expected rename to succeed, got error: %v", err)
	}
	var wf map[string]any
	if err := json.Unmarshal(body, &wf); err != nil {
		t.Fatalf("re-parse normalized body: %v", err)
	}
	nodes := asSlice(wf["nodes"])
	for i, n := range nodes {
		nm := n.(map[string]any)
		if _, has := nm["id"]; has {
			t.Errorf("nodes[%d]: short key 'id' was not dropped", i)
		}
		if _, has := nm["nodeId"]; !has {
			t.Errorf("nodes[%d]: 'nodeId' missing after rename", i)
		}
	}
	edge := asSlice(wf["edges"])[0].(map[string]any)
	if _, has := edge["source"]; has {
		t.Errorf("edge: short key 'source' was not dropped")
	}
	if _, has := edge["target"]; has {
		t.Errorf("edge: short key 'target' was not dropped")
	}
	if edge["sourceNodeId"] != "start" || edge["targetNodeId"] != "end" {
		t.Errorf("edge endpoints not renamed: %v", edge)
	}
}

func TestValidateWorkflowV2Body_ConflictingKeysError(t *testing.T) {
	body := json.RawMessage(`{
		"nodes": [
			{"id": "a", "nodeId": "b", "type": "start", "taskAlias": "a"}
		]
	}`)
	err := validateWorkflowV2Body(&body)
	if err == nil {
		t.Fatalf("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "conflicting") {
		t.Errorf("expected conflict error, got: %v", err)
	}

	body = json.RawMessage(`{
		"nodes": [
			{"id": "a", "nodeId": "a", "type": "start", "taskAlias": "a"}
		]
	}`)
	if err := validateWorkflowV2Body(&body); err != nil {
		t.Fatalf("equal id/nodeId should not error, got: %v", err)
	}
	var wf map[string]any
	_ = json.Unmarshal(body, &wf)
	nm := asSlice(wf["nodes"])[0].(map[string]any)
	if _, has := nm["id"]; has {
		t.Errorf("redundant 'id' should have been dropped")
	}
}

func TestRewriteRefsInTemplate_BareSingleToken(t *testing.T) {
	refMap := map[string]string{}
	localMappings := map[string]any{
		"borrower_id": "inputs.borrower_id",
		"tax_id":      "task_outputs.fetch.tax_id",
	}
	cases := []struct {
		in   string
		want string
	}{
		{"{{borrower_id}}", "{{inputs.borrower_id}}"},
		{`{"id":"{{tax_id}}"}`, `{"id":"{{task_outputs.fetch.tax_id}}"}`},
		{"{{unknown_token}}", "{{unknown_token}}"},
	}
	for _, c := range cases {
		got, err := rewriteRefsInTemplate(c.in, refMap, localMappings)
		if err != nil {
			t.Fatalf("rewriteRefsInTemplate(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("rewriteRefsInTemplate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRewriteRefsInTemplate_NoMappings(t *testing.T) {
	got, err := rewriteRefsInTemplate("{{borrower_id}}", map[string]string{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "{{borrower_id}}" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestNormalizeBatchBody(t *testing.T) {
	out, wrapped, err := normalizeBatchBody(json.RawMessage(`{"borrower_id":"abc"}`))
	if err != nil {
		t.Fatalf("flat body errored: %v", err)
	}
	if !wrapped {
		t.Errorf("flat body should have been wrapped")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("re-parse wrapped: %v", err)
	}
	arr, ok := m["inputs"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("expected inputs[] with one element, got %v", m["inputs"])
	}
	if got := arr[0].(map[string]any)["borrower_id"]; got != "abc" {
		t.Errorf("wrapped element lost data: %v", arr[0])
	}

	in := json.RawMessage(`{"inputs":[{"a":1},{"a":2}],"label":"x"}`)
	out, wrapped, err = normalizeBatchBody(in)
	if err != nil {
		t.Fatalf("inputs body errored: %v", err)
	}
	if wrapped {
		t.Errorf("body with inputs[] should not be wrapped")
	}
	if string(out) != string(in) {
		t.Errorf("body with inputs[] should pass through unchanged")
	}

	if _, _, err := normalizeBatchBody(json.RawMessage(`{"inputs":[]}`)); err == nil {
		t.Errorf("empty inputs should error")
	}
	if _, _, err := normalizeBatchBody(json.RawMessage(`{"inputs":{"a":1}}`)); err == nil {
		t.Errorf("non-array inputs should error")
	}
}

func TestDeriveAltdataInputKeysForCreate_NoOps(t *testing.T) {
	cases := []string{
		`{"type":"http","url":"https://x"}`,
		`{"type":"altdata-enrichment"}`,
		`{"type":"altdata-enrichment","sourcesConfig":[{"sourceId":"S"}],"inputKeys":{"personId":"{{personId}}"}}`,
	}
	for _, in := range cases {
		body := json.RawMessage(in)
		before := string(body)
		if err := deriveAltdataInputKeysForCreate(nil, &body); err != nil {
			t.Fatalf("no-op case %q errored: %v", in, err)
		}
		if string(body) != before {
			t.Errorf("body was mutated for no-op case %q: %q", in, string(body))
		}
	}
}

func findSubcommand(t *testing.T, group, name string) *cobra.Command {
	t.Helper()
	for _, g := range rootCmd.Commands() {
		if g.Name() != group {
			continue
		}
		for _, sub := range g.Commands() {
			if sub.Name() == name {
				return sub
			}
		}
	}
	return nil
}

func TestWorkflowsV2CreateHidden(t *testing.T) {
	create := findSubcommand(t, "workflows-v2", "create")
	if create == nil {
		t.Fatalf("workflows-v2 create subcommand should still exist")
	}
	if !create.Hidden {
		t.Errorf("workflows-v2 create should be Hidden")
	}
	if !strings.Contains(strings.ToLower(create.Long), "apply") {
		t.Errorf("create Long should point at apply, got: %q", create.Long)
	}
}

func TestExecuteByAliasVersionOptional(t *testing.T) {
	cmd := findSubcommand(t, "workflows-v2", "execute-by-alias")
	if cmd == nil {
		t.Fatalf("execute-by-alias subcommand not found")
	}
	if err := cmd.Args(cmd, []string{"my-wf"}); err != nil {
		t.Errorf("one arg (alias only) should be accepted: %v", err)
	}
	if err := cmd.Args(cmd, []string{"my-wf", "latest"}); err != nil {
		t.Errorf("two args (alias + version) should be accepted: %v", err)
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Errorf("zero args should be rejected")
	}
}

func TestDeriveAltdataInputKeysForCreate_FallbackError(t *testing.T) {
	body := json.RawMessage(`{"type":"altdata-enrichment","sourcesConfig":[{"sourceId":"ECU-PUB-0002","version":"v1"}]}`)
	err := deriveAltdataInputKeysForCreate(nil, &body)
	if err == nil {
		t.Fatalf("expected fallback error with no client, got nil")
	}
	if !strings.Contains(err.Error(), "could not derive") {
		t.Errorf("expected derive-failure guidance, got: %v", err)
	}
}

func TestValidateWorkflowV2Body_MultipleEndNodes(t *testing.T) {
	body := json.RawMessage(`{
		"nodes": [
			{"nodeId": "s", "type": "start", "taskAlias": "s"},
			{"nodeId": "e1", "type": "end", "taskAlias": "e1"},
			{"nodeId": "e2", "type": "end", "taskAlias": "e2"}
		]
	}`)
	err := validateWorkflowV2Body(&body)
	if err == nil || !strings.Contains(err.Error(), "exactly one end node") {
		t.Fatalf("expected single-end-node error, got: %v", err)
	}

	body = json.RawMessage(`{
		"nodes": [
			{"nodeId": "s", "type": "start", "taskAlias": "s"},
			{"nodeId": "c", "type": "conditional", "taskAlias": "c"},
			{"nodeId": "e1", "type": "end", "taskAlias": "e1"},
			{"nodeId": "e2", "type": "end", "taskAlias": "e2"}
		]
	}`)
	if err := validateWorkflowV2Body(&body); err == nil || !strings.Contains(err.Error(), "exactly one end node") {
		t.Fatalf("a conditional must NOT permit multiple end nodes, got: %v", err)
	}

	body = json.RawMessage(`{
		"nodes": [
			{"nodeId": "s", "type": "start", "taskAlias": "s"},
			{"nodeId": "e1", "type": "end", "taskAlias": "e1"}
		]
	}`)
	if err := validateWorkflowV2Body(&body); err != nil {
		t.Fatalf("single end node should not error, got: %v", err)
	}
}

func TestPreflightTasks_MultipleEndNodes(t *testing.T) {
	twoEnds := &composeSpec{
		Label:      "Two ends",
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			{"ref": "e1", "type": "end", "label": "End A"},
			{"ref": "e2", "type": "end", "label": "End B"},
		},
		Edges: []map[string]any{{"from": "start", "to": "e1"}, {"from": "start", "to": "e2"}},
	}
	err := preflightTasks(twoEnds)
	if err == nil || !strings.Contains(err.Error(), "exactly ONE end node") {
		t.Fatalf("preflight should reject two end nodes, got: %v", err)
	}

	oneEnd := &composeSpec{
		Label:      "One end",
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks:      []map[string]any{{"ref": "e1", "type": "end", "label": "End"}},
		Edges:      []map[string]any{{"from": "start", "to": "e1"}},
	}
	if err := preflightTasks(oneEnd); err != nil {
		t.Fatalf("preflight should accept a single end node, got: %v", err)
	}
}
