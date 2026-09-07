package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// serverApplySpec is start -> fetch -> enrich -> end with every reference
// surface the server path must carry through unchanged: an inputMappings long
// form, an http body template, a PDF sourcesConfig taskAlias and a custom
// variable keyed by a dependency.
func serverApplySpec() *composeSpec {
	return &composeSpec{
		Alias:    "smoke-apply",
		Label:    "Smoke Apply",
		Category: "EVALUATION",
		InputVariables: map[string]any{
			"person_id": map[string]any{"type": "string"},
		},
		CustomVariables: map[string]any{
			"total": map[string]any{
				"type":            "number",
				"expression":      "inputs['task_outputs.fetch.body.total'] * 2",
				"dependencies":    []any{"task_outputs.fetch.body.total"},
				"dependencyTypes": map[string]any{"task_outputs.fetch.body.total": "number"},
			},
		},
		ExtraNodes: []map[string]any{{"ref": "start", "type": "start", "label": "Start"}},
		Tasks: []map[string]any{
			{"ref": "fetch", "type": "http", "label": "Fetch", "url": "https://example.test/{{inputs.person_id}}", "method": "GET",
				"inputMappings": map[string]any{"person_id": "inputs.person_id"}},
			{"ref": "enrich", "type": "http", "label": "Enrich", "url": "https://example.test/enrich", "method": "POST",
				"body":          `{"total": "{{task_outputs.fetch.body.total}}"}`,
				"inputMappings": map[string]any{"total": "task_outputs.fetch.body.total"}},
			{"ref": "end", "type": "end", "label": "End",
				"endConfig": map[string]any{"pdfConfig": map[string]any{"enabled": false,
					"sourcesConfig": []any{map[string]any{"taskAlias": "fetch", "type": "http"}}}}},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "fetch"},
			{"from": "fetch", "to": "enrich"},
			{"from": "enrich", "to": "end", "sourceHandle": "ok"},
		},
	}
}

func serverApplyTestCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	return cmd, &out, &errb
}

// The flat spec the server receives is the authored spec after normalization:
// one entry per node with its ref, graph fields and inline body, refs kept in
// the canonical long form, and none of the server-assigned keys.
func TestBuildFlatSpecForServer_RoundTripsTheAssembly(t *testing.T) {
	capture := newComposeCapture()
	wf, err := composeWorkflowBody(nil, serverApplySpec(), true, false, true, false, false, true, capture)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	flat, ok := buildFlatSpecForServer(wf, capture, "smoke-apply")
	if !ok {
		t.Fatal("buildFlatSpecForServer refused an ordinary spec")
	}
	if flat["alias"] != "smoke-apply" || flat["label"] != "Smoke Apply" || flat["category"] != "EVALUATION" {
		t.Errorf("top-level fields: %v %v %v", flat["alias"], flat["label"], flat["category"])
	}
	if _, has := flat["inputVariables"]; !has {
		t.Error("inputVariables missing")
	}

	nodes := flat["nodes"].([]map[string]any)
	byRef := map[string]map[string]any{}
	for _, n := range nodes {
		byRef[n["ref"].(string)] = n
	}
	if len(byRef) != 4 {
		t.Fatalf("expected 4 nodes, got %d: %v", len(nodes), nodes)
	}
	for ref, n := range byRef {
		for _, k := range []string{"alias", "nodeId", "taskId", "taskVersion", "specRef", "workflowAlias", "data"} {
			if _, has := n[k]; has {
				t.Errorf("node %s carries server-owned key %q", ref, k)
			}
		}
		if n["type"] == nil || n["label"] == nil {
			t.Errorf("node %s lacks type/label: %v", ref, n)
		}
		if _, has := n["position"]; !has {
			t.Errorf("node %s lacks the auto-layout position", ref)
		}
	}
	// start has a body-less entry; the server mints its backing task.
	if byRef["start"]["type"] != "start" {
		t.Errorf("start node: %v", byRef["start"])
	}
	fetch := byRef["fetch"]
	if fetch["url"] != "https://example.test/{{inputs.person_id}}" || fetch["method"] != "GET" {
		t.Errorf("fetch body not carried: %v", fetch)
	}
	if im, _ := fetch["inputMappings"].(map[string]any); im["person_id"] != "inputs.person_id" {
		t.Errorf("fetch inputMappings: %v", fetch["inputMappings"])
	}
	enrich := byRef["enrich"]
	if body, _ := enrich["body"].(string); !strings.Contains(body, "task_outputs.fetch.body.total") {
		t.Errorf("enrich body must keep the canonical long-form ref, got %q", body)
	}
	if im, _ := enrich["inputMappings"].(map[string]any); im["total"] != "task_outputs.fetch.body.total" {
		t.Errorf("enrich inputMappings: %v", enrich["inputMappings"])
	}
	end := byRef["end"]
	endConfig, _ := end["endConfig"].(map[string]any)
	if endConfig == nil {
		t.Fatalf("end body not carried: %v", end)
	}

	edges := flat["edges"].([]map[string]any)
	if len(edges) != 3 {
		t.Fatalf("expected 3 edges, got %v", edges)
	}
	for _, e := range edges {
		if _, has := e["id"]; has {
			t.Errorf("auto-generated edge id must not travel: %v", e)
		}
		if _, has := e["sourceNodeId"]; has {
			t.Errorf("edges use from/to refs, got %v", e)
		}
	}
	if edges[2]["from"] != "enrich" || edges[2]["to"] != "end" || edges[2]["sourceHandle"] != "ok" {
		t.Errorf("third edge: %v", edges[2])
	}
	// customVariables travel with refs intact (the server resolves them).
	cvs := flat["customVariables"].(map[string]any)
	total := cvs["total"].(map[string]any)
	deps, _ := total["dependencyTypes"].(map[string]any)
	if _, has := deps["task_outputs.fetch.body.total"]; !has {
		t.Errorf("customVariables dependencyTypes rewritten client-side: %v", deps)
	}
}

// An explicit node alias is server-assigned on the new path, so such a spec
// stays on the client-side pipeline instead of being rejected by the server.
func TestBuildFlatSpecForServer_RefusesExplicitNodeAlias(t *testing.T) {
	spec := serverApplySpec()
	spec.Tasks[0]["alias"] = "fetch-explicit"
	capture := newComposeCapture()
	wf, err := composeWorkflowBody(nil, spec, true, false, true, false, false, true, capture)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if _, ok := buildFlatSpecForServer(wf, capture, "smoke-apply"); ok {
		t.Fatal("expected the explicit node alias to refuse the server path")
	}
}

// 404 and 405 (POST on a path an older backend only knows as GET /{id}) mean
// the endpoint does not exist: the sentinel lets the caller fall back.
func TestApplyViaServer_UnavailableOnOlderBackend(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"NotFound","message":"Method not allowed or the endpoint does not exist"}`))
		}))
		c := newTestClient(t, srv.URL)
		cmd, _, _ := serverApplyTestCmd()
		_, err := applyViaServer(c, cmd, map[string]any{"alias": "a"}, serverApplyOptions{})
		srv.Close()
		if err != errServerApplyUnavailable {
			t.Errorf("status %d: expected errServerApplyUnavailable, got %v", status, err)
		}
	}
}

// A 2xx is parsed; the request carries the spec and the options verbatim.
func TestApplyViaServer_SendsOptionsAndParsesResult(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v2/workflows/apply" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"mode":"create","dryRun":false,"workflowAlias":"smoke-apply",
			"workflow":{"id":"wf-1","status":"DRAFT","nodes":[],"edges":[]},
			"tasks":[{"ref":"fetch","alias":"fetch-a1b2c3","taskId":"t1","version":1,"action":"created","droppedFields":["htmlSections"]}],
			"validation":{"valid":true,"findings":[{"code":"INPUT_VARIABLE_NEVER_CONSUMED","severity":"warning","nodeId":null,"edgeId":null,"params":{},"message":"unused"}],"skippedNodeIds":[]},
			"publish":{"requested":true,"published":false,"errors":[]},
			"lock":{"acquired":true,"released":true}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, _, errb := serverApplyTestCmd()
	yes := true
	res, err := applyViaServer(c, cmd, map[string]any{"alias": "smoke-apply"}, serverApplyOptions{
		DryRun: false, Publish: &yes, ForceLock: true, ClientID: "apply-1",
	})
	if err != nil {
		t.Fatalf("applyViaServer: %v", err)
	}
	spec, _ := got["spec"].(map[string]any)
	if spec["alias"] != "smoke-apply" || got["publish"] != true || got["forceLock"] != true || got["clientId"] != "apply-1" || got["dryRun"] != false {
		t.Errorf("request body: %v", got)
	}
	if res.Mode != "create" || len(res.Tasks) != 1 || res.Tasks[0].Alias != "fetch-a1b2c3" || !res.Lock.Released {
		t.Errorf("result: %+v", res)
	}

	printServerApplySummary(errb, res)
	out := errb.String()
	for _, want := range []string{"1 created", "fetch -> fetch-a1b2c3 (created, v1)", "dropped field(s)", "htmlSections", "[WARN] INPUT_VARIABLE_NEVER_CONSUMED", "publish requested but not published"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q; got:\n%s", want, out)
		}
	}
}

// A 422 APPLY_VALIDATION_FAILED renders the server's findings and reports that
// nothing was created.
func TestApplyViaServer_ValidationFailurePrintsFindings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"code":"UnprocessableEntity","message":"rejected","details":{"errorSubCode":"APPLY_VALIDATION_FAILED",
			"validation":{"valid":false,"skippedNodeIds":[],"findings":[
				{"code":"NO_END_NODES","severity":"error","nodeId":null,"edgeId":null,"params":{},"message":"no end node"},
				{"code":"SOURCE_INPUT_NOT_FED","severity":"warning","nodeId":"fetch-a1","edgeId":null,"params":{},"message":"unfed"}]}}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, _, errb := serverApplyTestCmd()
	_, err := applyViaServer(c, cmd, map[string]any{"alias": "a"}, serverApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing was created") || !strings.Contains(err.Error(), "1 error(s)") {
		t.Fatalf("expected a validation error naming the count, got %v", err)
	}
	out := errb.String()
	for _, want := range []string{"[ERROR] NO_END_NODES", "[WARN] SOURCE_INPUT_NOT_FED", `(node "fetch-a1")`} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, out)
		}
	}
}

// A 400 APPLY_SPEC_INVALID renders the structural findings the same way.
func TestApplyViaServer_SpecInvalidPrintsFindings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"BadRequestError","message":"invalid","details":{"errorSubCode":"APPLY_SPEC_INVALID",
			"findings":[{"code":"APPLY_SPEC_INVALID","severity":"error","nodeId":"fetch","edgeId":null,"params":{},"message":"'alias' is server-assigned"}]}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, _, errb := serverApplyTestCmd()
	_, err := applyViaServer(c, cmd, map[string]any{}, serverApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "1 finding(s)") {
		t.Fatalf("expected a spec error, got %v", err)
	}
	if !strings.Contains(errb.String(), `[ERROR] APPLY_SPEC_INVALID (node "fetch"): 'alias' is server-assigned`) {
		t.Errorf("stderr:\n%s", errb.String())
	}
}

// A lock held by someone else names the holder and points at --force-lock.
func TestApplyViaServer_LockConflictNamesHolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"LOCK_CONFLICT","message":"Workflow is being edited by someone","details":{"lockedBy":{"userId":"u2","email":"someone@example.test"},"lockedAt":"2026-09-07T10:00:00Z"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, _, _ := serverApplyTestCmd()
	_, err := applyViaServer(c, cmd, map[string]any{"alias": "a"}, serverApplyOptions{})
	if err == nil {
		t.Fatal("expected a lock error")
	}
	for _, want := range []string{"someone@example.test", "since 2026-09-07T10:00:00Z", "--force-lock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// A publish-rule rejection says what was written and where.
func TestDescribeServerApplyError_PublishRejected(t *testing.T) {
	var errb bytes.Buffer
	err := describeServerApplyError(&errb, http.StatusUnprocessableEntity, json.RawMessage(`{"code":"VALIDATION_ERROR","message":"x","details":{"errorSubCode":"APPLY_PUBLISH_REJECTED","errors":["Node 'a': unfed"],"applied":{"workflowId":"draft-1","status":"DRAFT","tasks":[]}}}`))
	if err == nil || !strings.Contains(err.Error(), "DRAFT draft-1") {
		t.Fatalf("expected the draft id in the error, got %v", err)
	}
	if !strings.Contains(errb.String(), "Node 'a': unfed") {
		t.Errorf("stderr:\n%s", errb.String())
	}
	// Unknown envelopes still surface code, message and subcode.
	err = describeServerApplyError(&errb, 423, json.RawMessage(`{"code":"LOCKED","message":"Workflow is locked by another user","details":{"errorSubCode":"X"}}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 423 LOCKED: Workflow is locked by another user [errorSubCode=X]") {
		t.Errorf("generic rendering: %v", err)
	}
}

// With the server plan both sides carry real aliases: a relabel is `~`, not
// `-` + `+`, and the ref-map heuristic is skipped.
func TestDiffWorkflowWith_AliasIdentityKeepsRelabelAsChange(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v2/workflows/wf-1" {
			atomic.AddInt32(&gets, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"wf-1","alias":"smoke-apply","label":"Smoke Apply","status":"ACTIVE",
				"nodes":[{"nodeId":"start-1a2b3c","taskAlias":"start-1a2b3c","type":"start","label":"Start"},
				         {"nodeId":"fetch-9f9f9f","taskAlias":"fetch-9f9f9f","type":"http","label":"Old Fetch Label"}],
				"edges":[{"id":"e1","sourceNodeId":"start-1a2b3c","targetNodeId":"fetch-9f9f9f"}],
				"inputVariables":{},"customVariables":{}}`))
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	planned := map[string]any{
		"alias": "smoke-apply", "label": "Smoke Apply",
		"nodes": []any{
			map[string]any{"nodeId": "start-1a2b3c", "taskAlias": "start-1a2b3c", "type": "start", "label": "Start"},
			map[string]any{"nodeId": "fetch-9f9f9f", "taskAlias": "fetch-9f9f9f", "type": "http", "label": "New Fetch Label"},
		},
		"edges": []any{
			map[string]any{"id": "x", "sourceNodeId": "start-1a2b3c", "targetNodeId": "fetch-9f9f9f"},
		},
		"inputVariables": map[string]any{}, "customVariables": map[string]any{},
	}
	c := newTestClient(t, srv.URL)
	cmd, out, _ := serverApplyTestCmd()
	spec := &composeSpec{Alias: "smoke-apply", Label: "Smoke Apply"}
	if err := diffWorkflowWith(c, cmd, spec, planned, map[string]any{"id": "wf-1"}, "smoke-apply", diffIdentityByAlias); err != nil {
		t.Fatalf("diff: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "~ nodes[]: `fetch-9f9f9f` (http) -- label") {
		t.Errorf("relabel must diff as a change under alias identity; got:\n%s", got)
	}
	if strings.Contains(got, "+ nodes[]") || strings.Contains(got, "- nodes[]") {
		t.Errorf("no node should be added/removed on a relabel; got:\n%s", got)
	}
	if strings.Contains(got, "edges[]") {
		t.Errorf("edge endpoints keyed by alias must match; got:\n%s", got)
	}
	if atomic.LoadInt32(&gets) != 1 {
		t.Errorf("expected exactly one GET of the current workflow, got %d", gets)
	}
}
