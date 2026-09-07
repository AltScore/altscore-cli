package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// --diff assembles without posting, so the body names spec-local refs while
// the tenant names live aliases. The smoke test of v0.33.0 showed a no-op
// re-apply reported as three custom-variable changes because of it. The ref
// here (`q1`) deliberately differs from the label slug (`consulta-turso`) so a
// hash-stripping comparison cannot pass by accident.

func diffLiveWorkflow() string {
	return `{
	  "id": "e775bb30-0000-0000-0000-000000000000",
	  "label": "Diff refs", "category": "EVALUATION", "status": "ACTIVE", "version": 3,
	  "nodes": [
	    {"nodeId": "start-aaaaaa", "taskAlias": "start-aaaaaa", "type": "start", "label": "Start", "data": {}},
	    {"nodeId": "consulta-turso-8f21c3", "taskAlias": "consulta-turso-8f21c3", "type": "http", "label": "Consulta Turso",
	     "data": {"inputMappings": {"id": "inputs.id"}}},
	    {"nodeId": "enrich-33ea43", "taskAlias": "enrich-33ea43", "type": "http", "label": "Enrich",
	     "data": {"inputMappings": {"raw": "task_outputs.consulta-turso-8f21c3.body", "bare": "consulta-turso-8f21c3.rows"}}},
	    {"nodeId": "end-bbbbbb", "taskAlias": "end-bbbbbb", "type": "end", "label": "End", "data": {"isEndNode": true}}
	  ],
	  "edges": [
	    {"id": "e1", "sourceNodeId": "start-aaaaaa", "targetNodeId": "consulta-turso-8f21c3"},
	    {"id": "e2", "sourceNodeId": "consulta-turso-8f21c3", "targetNodeId": "enrich-33ea43"},
	    {"id": "e3", "sourceNodeId": "enrich-33ea43", "targetNodeId": "end-bbbbbb"}
	  ],
	  "inputVariables": {},
	  "customVariables": {
	    "total": {
	      "type": "string", "enforceType": true,
	      "expression": "result = inputs['task_outputs.consulta-turso-8f21c3.body']",
	      "returnValue": "result",
	      "dependencies": ["task_outputs.consulta-turso-8f21c3.body"],
	      "dependencyTypes": {"task_outputs.consulta-turso-8f21c3.body": "string"}
	    }
	  }
	}`
}

// diffAssembledBody is what composeWorkflowBody yields in --diff mode: the
// spec-local ref stands in for every task alias.
func diffAssembledBody(expression string) map[string]any {
	return map[string]any{
		"label": "Diff refs", "category": "EVALUATION",
		"nodes": []map[string]any{
			{"nodeId": "start", "type": "start", "label": "Start"},
			{"nodeId": "q1", "taskAlias": "q1", "type": "http", "label": "Consulta Turso",
				"inputMappings": map[string]any{"id": "inputs.id"}},
			{"nodeId": "enrich", "taskAlias": "enrich", "type": "http", "label": "Enrich",
				"inputMappings": map[string]any{"raw": "task_outputs.q1.body", "bare": "q1.rows"}},
			{"nodeId": "end", "taskAlias": "end", "type": "end", "label": "End", "data": map[string]any{"isEndNode": true}},
		},
		"edges": []map[string]any{
			{"id": "start->q1", "sourceNodeId": "start", "targetNodeId": "q1"},
			{"id": "q1->enrich", "sourceNodeId": "q1", "targetNodeId": "enrich"},
			{"id": "enrich->end", "sourceNodeId": "enrich", "targetNodeId": "end"},
		},
		"inputVariables": map[string]any{},
		"customVariables": map[string]any{
			"total": map[string]any{
				"type": "string", "enforceType": true,
				"expression":      expression,
				"returnValue":     "result",
				"dependencies":    []any{"task_outputs.q1.body"},
				"dependencyTypes": map[string]any{"task_outputs.q1.body": "string"},
			},
		},
	}
}

func runDiff(t *testing.T, assembled map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v2/workflows/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(diffLiveWorkflow()))
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	spec := &composeSpec{Label: "Diff refs", Category: "EVALUATION"}
	existing := map[string]any{"id": "e775bb30-0000-0000-0000-000000000000", "version": float64(3)}
	if err := diffWorkflow(c, cmd, spec, assembled, existing, "diff-refs"); err != nil {
		t.Fatalf("diff: %v", err)
	}
	return out.String()
}

func TestDiff_NoOpReapplyWithTaskOutputRefsIsClean(t *testing.T) {
	out := runDiff(t, diffAssembledBody("result = inputs['task_outputs.q1.body']"))
	if !strings.Contains(out, "no changes") {
		t.Fatalf("a no-op re-apply must diff clean; got:\n%s", out)
	}
	if strings.Contains(out, "customVariables") || strings.Contains(out, "inputMappings") {
		t.Errorf("no variable or mapping change may be reported; got:\n%s", out)
	}
}

// The rewrite must not hide a real change behind the alias substitution.
func TestDiff_RealCustomVariableChangeStillReported(t *testing.T) {
	out := runDiff(t, diffAssembledBody("result = inputs['task_outputs.q1.body'].upper()"))
	if !strings.Contains(out, "~ customVariables[]: `total`") {
		t.Fatalf("a changed expression must be reported; got:\n%s", out)
	}
	if !strings.Contains(out, "~ expression:") {
		t.Errorf("the changed field must be named; got:\n%s", out)
	}
	if strings.Contains(out, "~ dependencies:") || strings.Contains(out, "~ dependencyTypes:") {
		t.Errorf("unchanged fields must not be dragged in by the alias difference; got:\n%s", out)
	}
}

// The input body is not mutated: diff is a preview and the caller may still
// print the assembled body afterwards.
func TestResolveAssembledRefsForDiff_DoesNotMutateInput(t *testing.T) {
	assembled := diffAssembledBody("result = 1")
	var current map[string]any
	if err := jsonUnmarshalString(diffLiveWorkflow(), &current); err != nil {
		t.Fatal(err)
	}
	resolved := resolveAssembledRefsForDiff(assembled, current)
	got := resolved["customVariables"].(map[string]any)["total"].(map[string]any)["dependencies"].([]any)[0]
	if got != "task_outputs.consulta-turso-8f21c3.body" {
		t.Errorf("resolved copy must carry the live alias, got %v", got)
	}
	orig := assembled["customVariables"].(map[string]any)["total"].(map[string]any)["dependencies"].([]any)[0]
	if orig != "task_outputs.q1.body" {
		t.Errorf("input was mutated: %v", orig)
	}
	// A node the tenant lacks keeps its spec-local ref.
	refMap := diffRefMap(
		[]map[string]any{{"nodeId": "nuevo", "taskAlias": "nuevo", "label": "Nuevo"}},
		toMapSlice(current["nodes"]))
	if len(refMap) != 0 {
		t.Errorf("a new node must not resolve to anything, got %v", refMap)
	}
}
