package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// Characterization tests for the apply assembly.
//
// These pin the OBSERVABLE assembly semantics (graph node ids, edge endpoints +
// handles, inputMappings/template/customVar ref rewrites, and the per-node task
// bodies) for a representative spec that exercises every element assembly
// touches: a conditional with authored branches + labeled edges, a task node
// whose inputMappings reference other refs, an end node with endConfig, an extra
// (start) node, and custom variables.
//
// They are written against composeWorkflowBody in preview/dry mode: assembly
// must keep producing ref placeholders and canonical `task_outputs.<ref>`
// references, which is exactly what buildFlatSpecForServer sends to
// POST /v2/workflows/apply.

// richSplitSpec returns a fresh spec exercising every assembly element. Fresh
// maps each call: compose mutates the spec in place.
//
// Topological POST order of Tasks is fetch, route, score, end (score depends on
// fetch; end depends on score; route is edge-ordered after fetch and before
// score). The start extra node is posted last. So with a server that mints
// srv-task-N in POST order: fetch=1, route=2, score=3, end=4, start=5.
func richSplitSpec() *composeSpec {
	desc := ""
	return &composeSpec{
		Label:       "Rich split",
		Category:    "EVALUATION",
		Description: &desc,
		ExtraNodes: []map[string]any{
			{"ref": "start", "type": "start", "label": "Start"},
		},
		Tasks: []map[string]any{
			{"ref": "fetch", "type": "http", "label": "Fetch",
				"url":           "https://api.example.com/x",
				"inputMappings": map[string]any{"id": "inputs.id"}},
			{"ref": "route", "type": "conditional", "label": "Route",
				"branches": []any{
					map[string]any{"id": "approve", "label": "Approve", "isElse": false, "order": 0,
						"conditions": map[string]any{"operator": "AND", "items": []any{
							map[string]any{"field": "amount", "operator": "gt", "value": "task_outputs.fetch.amount", "valueType": "variable"},
						}}},
					map[string]any{"id": "deny", "label": "Deny", "isElse": true, "order": 1},
				}},
			{"ref": "score", "type": "http", "label": "Score",
				"url":           "https://score.example.com",
				"inputMappings": map[string]any{"amount": "task_outputs.fetch.amount", "raw": "fetch.raw"}},
			{"ref": "end", "type": "end", "label": "End",
				"endConfig": map[string]any{"outputJson": `{"s":"{{task_outputs.score.body}}"}`, "decisionConfig": nil}},
		},
		CustomVariables: map[string]any{
			"risk": map[string]any{"type": "number",
				"expression":   "risk = task_outputs.fetch.amount * 2",
				"returnValue":  "risk",
				"dependencies": []any{"task_outputs.fetch.amount"},
				// Formula mode: the Hub recompiles formulaText on save, so a ref
				// left here breaks the variable the moment a human edits it.
				"editorMode": "simple",
				"simpleConfig": map[string]any{
					"type":        "formula",
					"formulaText": "$task_outputs.fetch.amount * 2",
				},
				"dependencyTypes": map[string]any{"task_outputs.fetch.amount": "number"}},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "fetch"},
			{"from": "fetch", "to": "route"},
			{"from": "route", "to": "score", "sourceHandle": "approve"},
			{"from": "route", "to": "end", "sourceHandle": "deny"},
			{"from": "score", "to": "end"},
		},
	}
}

func nodesOf(t *testing.T, wf map[string]any) []map[string]any {
	t.Helper()
	raw, ok := wf["nodes"].([]map[string]any)
	if ok {
		return raw
	}
	// Also accept []any (post-marshal round-trips).
	anyList, ok := wf["nodes"].([]any)
	if !ok {
		t.Fatalf("wf[nodes] is not a slice: %T", wf["nodes"])
	}
	out := make([]map[string]any, 0, len(anyList))
	for _, n := range anyList {
		out = append(out, n.(map[string]any))
	}
	return out
}

func edgesOf(t *testing.T, wf map[string]any) []map[string]any {
	t.Helper()
	raw, ok := wf["edges"].([]map[string]any)
	if ok {
		return raw
	}
	anyList, ok := wf["edges"].([]any)
	if !ok {
		t.Fatalf("wf[edges] is not a slice: %T", wf["edges"])
	}
	out := make([]map[string]any, 0, len(anyList))
	for _, e := range anyList {
		out = append(out, e.(map[string]any))
	}
	return out
}

func nodeByLabel(t *testing.T, wf map[string]any, label string) map[string]any {
	t.Helper()
	for _, n := range nodesOf(t, wf) {
		if l, _ := n["label"].(string); l == label {
			return n
		}
	}
	t.Fatalf("no node with label %q; nodes=%v", label, wf["nodes"])
	return nil
}

func nodeInputMappings(t *testing.T, node map[string]any) map[string]any {
	t.Helper()
	data, _ := node["data"].(map[string]any)
	if data == nil {
		return map[string]any{}
	}
	im, _ := data["inputMappings"].(map[string]any)
	return im
}

// edgeWithHandle finds the edge carrying the given sourceHandle.
func edgeWithHandle(t *testing.T, wf map[string]any, handle string) map[string]any {
	t.Helper()
	for _, e := range edgesOf(t, wf) {
		if h, _ := e["sourceHandle"].(string); h == handle {
			return e
		}
	}
	t.Fatalf("no edge with sourceHandle %q", handle)
	return nil
}

// TestCharacterization_DryAssembly_RefPlaceholders pins the ref-placeholder
// artifacts the dry (preview) assembly produces -- the exact artifacts the
// server pre-flight validates.
func TestCharacterization_DryAssembly_RefPlaceholders(t *testing.T) {
	capture := newComposeCapture()
	// c=nil: the fixture uses only http/conditional/end/start types + a
	// compiled-in operator ("gt"), so assembly needs no network.
	wf, err := composeWorkflowBody(nil, richSplitSpec(), true, false, true, false, false, true, capture)
	if err != nil {
		t.Fatalf("dry assembly failed: %v", err)
	}

	// Graph node ids are the spec-local refs (placeholders), not server aliases.
	if got := nodeByLabel(t, wf, "Fetch")["nodeId"]; got != "fetch" {
		t.Errorf("Fetch nodeId: want ref placeholder %q, got %v", "fetch", got)
	}
	score := nodeByLabel(t, wf, "Score")
	if got := score["nodeId"]; got != "score" {
		t.Errorf("Score nodeId: want %q, got %v", "score", got)
	}
	if got := score["taskAlias"]; got != "score" {
		t.Errorf("Score taskAlias: want %q, got %v", "score", got)
	}

	// A task node referencing another ref keeps the ref in both long and bare form.
	im := nodeInputMappings(t, score)
	if im["amount"] != "task_outputs.fetch.amount" {
		t.Errorf("Score inputMappings.amount: want long ref, got %v", im["amount"])
	}
	if im["raw"] != "fetch.raw" {
		t.Errorf("Score inputMappings.raw: want bare ref, got %v", im["raw"])
	}

	// Labeled conditional edge keeps its handle and ref endpoints.
	approve := edgeWithHandle(t, wf, "approve")
	if approve["sourceNodeId"] != "route" || approve["targetNodeId"] != "score" {
		t.Errorf("approve edge endpoints: want route->score, got %v->%v", approve["sourceNodeId"], approve["targetNodeId"])
	}

	// Custom variable keeps the ref in expression + dependencies.
	cv, _ := wf["customVariables"].(map[string]any)["risk"].(map[string]any)
	if !strings.Contains(cv["expression"].(string), "task_outputs.fetch.amount") {
		t.Errorf("risk expression should keep ref: %v", cv["expression"])
	}
	deps := cv["dependencies"].([]any)
	if deps[0] != "task_outputs.fetch.amount" {
		t.Errorf("risk dependency should keep ref: %v", deps[0])
	}

	// Every backing task body is captured, keyed by its placeholder, and holds refs.
	if len(capture.tasks) != 5 {
		t.Fatalf("want 5 captured task bodies (start,fetch,route,score,end); got %d (%v)", len(capture.tasks), keysOf(capture.tasks))
	}
	scoreBody := capture.tasks["score"]
	if scoreBody == nil {
		t.Fatalf("no captured task body for placeholder 'score'; keys=%v", keysOf(capture.tasks))
	}
	if !strings.Contains(string(scoreBody), "task_outputs.fetch.amount") {
		t.Errorf("captured score body should carry the ref: %s", scoreBody)
	}
	if !strings.Contains(string(scoreBody), `"specRef":"score"`) {
		t.Errorf("captured score body should carry specRef=score: %s", scoreBody)
	}
	endBody := capture.tasks["end"]
	if !strings.Contains(string(endBody), "task_outputs.score.body") {
		t.Errorf("captured end body should carry the outputJson ref: %s", endBody)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
