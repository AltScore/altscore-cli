package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// Characterization tests for the apply assembly -> post pipeline.
//
// These pin the OBSERVABLE assembly semantics (graph node ids, edge endpoints +
// handles, inputMappings/template/customVar ref rewrites, and the per-node task
// bodies) for a representative spec that exercises every element the split
// touches: a conditional with authored branches + labeled edges, a task node
// whose inputMappings reference other refs, an end node with endConfig, an extra
// (start) node, and custom variables.
//
// They are written against the STABLE entry points (composeWorkflowBody in
// preview/dry mode, and applyAssembleValidateAndPost end-to-end) so they hold
// across the assemble/post split: dry assembly must keep producing ref
// placeholders, and the real path must keep producing a body that differs from
// the validated one ONLY by the ref -> server-alias substitution.

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
				"dependencies": []any{"task_outputs.fetch.amount"}},
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

// recordingApplyServer serves the validate endpoint (valid:true) and records
// every POST /v2/tasks body in order, minting srv-task-N aliases.
func recordingApplyServer(t *testing.T, taskBodies *[]map[string]any, validateBodies *[]map[string]any) *httptest.Server {
	t.Helper()
	var n int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/workflows/validate":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			*validateBodies = append(*validateBodies, body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"valid":true,"findings":[]}`))
		case r.Method == "POST" && r.URL.Path == "/v2/tasks":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			*taskBodies = append(*taskBodies, body)
			k := atomic.AddInt32(&n, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"alias":"srv-task-` + strconv.Itoa(int(k)) + `","version":` + strconv.Itoa(int(k)) + `}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

// TestCharacterization_RealApply_AliasSubstitution pins the fully-substituted
// artifacts the real apply path posts: graph node ids/versions are server
// aliases, edges + handles carry through with substituted endpoints, and every
// inputMapping / template / customVar ref is rewritten to the server alias.
func TestCharacterization_RealApply_AliasSubstitution(t *testing.T) {
	var taskBodies, validateBodies []map[string]any
	srv := recordingApplyServer(t, &taskBodies, &validateBodies)
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	cmd, _ := preflightTestCmd()

	// noAutoDefaults=true keeps the end node deterministic (no borrower_id wiring).
	wf, err := applyAssembleValidateAndPost(c, cmd, richSplitSpec(), false, false, false, true, false)
	if err != nil {
		t.Fatalf("real apply failed: %v", err)
	}

	// Exactly one validate call carrying all 5 task bodies; 5 task POSTs.
	if len(validateBodies) != 1 {
		t.Fatalf("want exactly 1 validate call, got %d", len(validateBodies))
	}
	if vt, _ := validateBodies[0]["tasks"].(map[string]any); len(vt) != 5 {
		t.Errorf("validate payload should carry 5 task bodies, got %d", len(vt))
	}
	if len(taskBodies) != 5 {
		t.Fatalf("want 5 task POSTs, got %d", len(taskBodies))
	}

	// Graph ids are server aliases; taskVersion follows the server (POST order).
	fetch := nodeByLabel(t, wf, "Fetch")
	if fetch["nodeId"] != "srv-task-1" || fetch["taskAlias"] != "srv-task-1" {
		t.Errorf("Fetch node not aliased: %v", fetch)
	}
	if v, _ := fetch["taskVersion"].(int); v != 1 {
		t.Errorf("Fetch taskVersion: want server version 1, got %v", fetch["taskVersion"])
	}
	end := nodeByLabel(t, wf, "End")
	if end["nodeId"] != "srv-task-4" {
		t.Errorf("End nodeId: want srv-task-4, got %v", end["nodeId"])
	}
	if v, _ := end["taskVersion"].(int); v != 4 {
		t.Errorf("End taskVersion: want server version 4, got %v", end["taskVersion"])
	}

	// Score inputMappings rewritten to the fetch server alias (long + bare form).
	im := nodeInputMappings(t, nodeByLabel(t, wf, "Score"))
	if im["amount"] != "task_outputs.srv-task-1.amount" {
		t.Errorf("Score amount not substituted: %v", im["amount"])
	}
	if im["raw"] != "srv-task-1.raw" {
		t.Errorf("Score raw not substituted: %v", im["raw"])
	}

	// Labeled edge keeps its handle; endpoints substituted.
	approve := edgeWithHandle(t, wf, "approve")
	if approve["sourceNodeId"] != "srv-task-2" || approve["targetNodeId"] != "srv-task-3" {
		t.Errorf("approve edge endpoints not substituted: %v->%v", approve["sourceNodeId"], approve["targetNodeId"])
	}
	if id, _ := approve["id"].(string); id != "srv-task-2->srv-task-3" {
		t.Errorf("approve edge auto-id not rebuilt from aliases: %v", approve["id"])
	}

	// Custom variable rewritten to the fetch server alias.
	cv, _ := wf["customVariables"].(map[string]any)["risk"].(map[string]any)
	if !strings.Contains(cv["expression"].(string), "task_outputs.srv-task-1.amount") {
		t.Errorf("risk expression not substituted: %v", cv["expression"])
	}
	if cv["dependencies"].([]any)[0] != "task_outputs.srv-task-1.amount" {
		t.Errorf("risk dependency not substituted: %v", cv["dependencies"])
	}

	// The POSTED task bodies are fully substituted -- no residual spec refs.
	for _, b := range taskBodies {
		blob, _ := json.Marshal(b)
		s := string(blob)
		for _, residual := range []string{"task_outputs.fetch.", "task_outputs.score.", `"fetch.raw"`} {
			if strings.Contains(s, residual) {
				t.Errorf("posted task body carries residual ref %q: %s", residual, s)
			}
		}
	}
	// The posted score body specifically carries the substituted mapping.
	var scorePosted map[string]any
	for _, b := range taskBodies {
		if b["specRef"] == "score" {
			scorePosted = b
		}
	}
	if scorePosted == nil {
		t.Fatal("no posted body with specRef=score")
	}
	sim, _ := scorePosted["inputMappings"].(map[string]any)
	if sim["amount"] != "task_outputs.srv-task-1.amount" {
		t.Errorf("posted score inputMappings.amount not substituted: %v", sim["amount"])
	}
}

// TestCharacterization_ValidatedMatchesPostedModuloAlias asserts the split's
// core guarantee: the graph handed to the validator and the graph ultimately
// posted differ ONLY by the ref -> server-alias substitution. It compares the
// dry (validated) body against the real (posted) body node-by-node and
// edge-by-edge by stable label / handle, confirming topology + wiring shape are
// identical and only identifiers changed.
func TestCharacterization_ValidatedMatchesPostedModuloAlias(t *testing.T) {
	// Validated artifacts (what the pre-flight sees).
	dryCapture := newComposeCapture()
	dryWf, err := composeWorkflowBody(nil, richSplitSpec(), true, false, true, false, false, true, dryCapture)
	if err != nil {
		t.Fatalf("dry assembly failed: %v", err)
	}

	// Posted artifacts (what apply persists).
	var taskBodies, validateBodies []map[string]any
	srv := recordingApplyServer(t, &taskBodies, &validateBodies)
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	cmd, _ := preflightTestCmd()
	realWf, err := applyAssembleValidateAndPost(c, cmd, richSplitSpec(), false, false, false, true, false)
	if err != nil {
		t.Fatalf("real apply failed: %v", err)
	}

	// Same node + edge counts.
	if len(nodesOf(t, dryWf)) != len(nodesOf(t, realWf)) {
		t.Fatalf("node count drift: dry=%d real=%d", len(nodesOf(t, dryWf)), len(nodesOf(t, realWf)))
	}
	if len(edgesOf(t, dryWf)) != len(edgesOf(t, realWf)) {
		t.Fatalf("edge count drift: dry=%d real=%d", len(edgesOf(t, dryWf)), len(edgesOf(t, realWf)))
	}

	// ref -> alias derived from the two bodies, keyed by stable node label.
	refToAlias := map[string]string{}
	for _, dn := range nodesOf(t, dryWf) {
		label, _ := dn["label"].(string)
		rn := nodeByLabel(t, realWf, label)
		refToAlias[dn["nodeId"].(string)] = rn["nodeId"].(string)
	}
	// Every graph identifier must have changed to a server alias.
	for ref, alias := range refToAlias {
		if !strings.HasPrefix(alias, "srv-task-") {
			t.Errorf("node %q did not become a server alias: %q", ref, alias)
		}
	}

	// The labeled conditional edge keeps its handle; endpoints map through refToAlias.
	dryApprove := edgeWithHandle(t, dryWf, "approve")
	realApprove := edgeWithHandle(t, realWf, "approve")
	if refToAlias[dryApprove["sourceNodeId"].(string)] != realApprove["sourceNodeId"] {
		t.Errorf("approve edge source not consistent under substitution")
	}
	if refToAlias[dryApprove["targetNodeId"].(string)] != realApprove["targetNodeId"] {
		t.Errorf("approve edge target not consistent under substitution")
	}

	// Score inputMappings: same keys, values differ only by the fetch alias.
	dryIM := nodeInputMappings(t, nodeByLabel(t, dryWf, "Score"))
	realIM := nodeInputMappings(t, nodeByLabel(t, realWf, "Score"))
	if len(dryIM) != len(realIM) {
		t.Fatalf("Score inputMappings key drift: dry=%v real=%v", dryIM, realIM)
	}
	fetchAlias := refToAlias["fetch"]
	if strings.ReplaceAll(dryIM["amount"].(string), "fetch", fetchAlias) != realIM["amount"] {
		t.Errorf("Score amount not a pure fetch->alias rename: dry=%v real=%v", dryIM["amount"], realIM["amount"])
	}
}
