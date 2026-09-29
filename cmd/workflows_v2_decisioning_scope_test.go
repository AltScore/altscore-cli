package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/AltScore/altscore-cli/internal/client"
)

type fakeDecisioningBC struct {
	entities map[string][]map[string]any
	patched  []string
}

// Honours the list endpoints' code and workflow-alias filters the way BC's CRUD.query does.
func newFakeDecisioningBC(t *testing.T, entities map[string][]map[string]any) (*client.Client, *fakeDecisioningBC) {
	t.Helper()
	seedEntities(t, map[string]map[string]any{})
	f := &fakeDecisioningBC{entities: entities}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/"), "/")
		resource := parts[0]
		switch {
		case r.Method == http.MethodGet && len(parts) == 1:
			q := r.URL.Query()
			owner, scoped := q["workflow-alias"]
			out := []map[string]any{}
			for _, e := range f.entities[resource] {
				if e["code"] != q.Get("code") {
					continue
				}
				if scoped && e["workflowAlias"] != owner[0] {
					continue
				}
				out = append(out, e)
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPatch && len(parts) == 2:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			alias, _ := body["workflowAlias"].(string)
			f.patched = append(f.patched, resource+"/"+parts[1]+"->"+alias)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return newTestClient(t, srv.URL), f
}

func decisioningEntity(id, code, owner string, extra map[string]any) map[string]any {
	e := map[string]any{"id": id, "code": code}
	if owner != "" {
		e["workflowAlias"] = owner
	}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

// Every code exists under both workflows, and the other workflow's copy is listed first, so a
// tenant-wide by-code lookup lands on it.
func sameCodesUnderTwoWorkflows() map[string][]map[string]any {
	both := func(resource, code string, extra map[string]any) []map[string]any {
		return []map[string]any{
			decisioningEntity(resource+"-other", code, "other-wf", extra),
			decisioningEntity(resource+"-target", code, "target-wf", extra),
		}
	}
	return map[string][]map[string]any{
		"rule-trees":       both("rt", "RT", map[string]any{"rules": []any{map[string]any{"ruleCode": "DR_R001"}}}),
		"evaluation-rules": both("er", "DR_R001", nil),
		"scorecards":       both("sc", "SC", map[string]any{"rules": []any{map[string]any{"mappingTableCode": "MT"}}}),
		"mapping-tables":   both("mt", "MT", nil),
	}
}

func onlyUnderOtherWorkflow() map[string][]map[string]any {
	return map[string][]map[string]any{
		"rule-trees":       {decisioningEntity("rt-other", "RT", "other-wf", map[string]any{"rules": []any{map[string]any{"ruleCode": "DR_R001"}}})},
		"evaluation-rules": {decisioningEntity("er-other", "DR_R001", "other-wf", nil)},
		"scorecards":       {decisioningEntity("sc-other", "SC", "other-wf", map[string]any{"rules": []any{map[string]any{"mappingTableCode": "MT"}}})},
		"mapping-tables":   {decisioningEntity("mt-other", "MT", "other-wf", nil)},
	}
}

func decisioningTasks() map[string]func() map[string]any {
	return map[string]func() map[string]any{
		"rule-tree": func() map[string]any {
			return map[string]any{"type": "rule-tree", "label": "Decide", "ruleTreeConfig": map[string]any{
				"ruleTreeCode": "RT", "outputVariable": "decision",
			}}
		},
		"evaluate-rules": func() map[string]any {
			return map[string]any{"type": "evaluate-rules", "label": "Rules", "rulesConfig": []any{
				map[string]any{"ruleCode": "DR_R001"},
			}}
		},
		"scorecard": func() map[string]any {
			return map[string]any{"type": "scorecard", "label": "Score", "scorecardConfig": map[string]any{
				"scorecardCode": "SC",
			}}
		},
		"mapping-table": func() map[string]any {
			return map[string]any{"type": "mapping-table", "label": "Bucket", "mappingTableConfig": map[string]any{
				"entries": []any{map[string]any{
					"mappingTableCode": "MT", "inputVariable": "inputs.amount", "outputVariable": "bucket", "order": 0,
				}},
			}}
		},
	}
}

func TestNormalizeTaskBody_SameCodeUnderTwoWorkflowsResolvesTheAppliedOne(t *testing.T) {
	for name, task := range decisioningTasks() {
		t.Run(name, func(t *testing.T) {
			c, _ := newFakeDecisioningBC(t, sameCodesUnderTwoWorkflows())
			opts := &composeNormalizeOpts{PredictedAlias: "target-wf"}
			if err := normalizeTaskBody(c, task(), opts, false); err != nil {
				t.Fatalf("each workflow owns its own copy of the code, pre-flight must pass: %v", err)
			}
		})
	}
}

func TestNormalizeTaskBody_CodeOwnedOnlyByAnotherWorkflowIsRefused(t *testing.T) {
	for name, task := range decisioningTasks() {
		t.Run(name, func(t *testing.T) {
			c, _ := newFakeDecisioningBC(t, onlyUnderOtherWorkflow())
			opts := &composeNormalizeOpts{PredictedAlias: "target-wf"}
			err := normalizeTaskBody(c, task(), opts, false)
			if err == nil {
				t.Fatal("a code that only another workflow owns must still be refused")
			}
			if !strings.Contains(err.Error(), `is currently owned by workflow "other-wf", but this apply targets workflow "target-wf"`) {
				t.Fatalf("expected the ownership refusal, got: %v", err)
			}
		})
	}
}

func TestNormalizeRuleTreeTask_NestedRulesResolveUnderTheTreesOwnAlias(t *testing.T) {
	c, _ := newFakeDecisioningBC(t, map[string][]map[string]any{
		"rule-trees": {decisioningEntity("rt-1", "RT", "", map[string]any{"rules": []any{map[string]any{"ruleCode": "DR_R001"}}})},
		"evaluation-rules": {
			decisioningEntity("er-other", "DR_R001", "other-wf", nil),
			decisioningEntity("er-target", "DR_R001", "target-wf", nil),
		},
	})
	task := decisioningTasks()["rule-tree"]()
	if err := normalizeRuleTreeTask(c, task, &composeNormalizeOpts{PredictedAlias: "target-wf"}, false); err != nil {
		t.Fatalf("an unscoped tree reads its rules under the applied workflow: %v", err)
	}
}

func TestReconcileEntityScopes_LeavesAnotherWorkflowsSameCodeEntityAlone(t *testing.T) {
	entities := sameCodesUnderTwoWorkflows()
	entities["rule-trees"][1]["rules"] = []any{
		map[string]any{"ruleCode": "DR_R001"},
		map[string]any{"ruleCode": "DR_R002"},
	}
	entities["evaluation-rules"] = append(entities["evaluation-rules"], decisioningEntity("er-unscoped", "DR_R002", "", nil))
	c, f := newFakeDecisioningBC(t, entities)

	spec := &composeSpec{}
	for _, task := range decisioningTasks() {
		spec.Tasks = append(spec.Tasks, task())
	}
	var errOut bytes.Buffer
	if err := reconcileEntityScopes(c, spec, "target-wf", true, &errOut); err != nil {
		t.Fatal(err)
	}

	want := []string{"evaluation-rules/er-unscoped->target-wf"}
	sort.Strings(f.patched)
	if strings.Join(f.patched, ",") != strings.Join(want, ",") {
		t.Fatalf("only the unscoped rule may be stamped, even with --allow-steal-ownership\n got: %v\nwant: %v\nstderr:\n%s", f.patched, want, errOut.String())
	}
	if strings.Contains(errOut.String(), "REFUSED") {
		t.Errorf("nothing owned by the applied workflow should be refused:\n%s", errOut.String())
	}
}

func TestReconcileEntityScopes_CodeOwnedOnlyByAnotherWorkflowIsRefused(t *testing.T) {
	c, f := newFakeDecisioningBC(t, onlyUnderOtherWorkflow())
	spec := &composeSpec{Tasks: []map[string]any{decisioningTasks()["evaluate-rules"]()}}
	var errOut bytes.Buffer
	if err := reconcileEntityScopes(c, spec, "target-wf", false, &errOut); err != nil {
		t.Fatal(err)
	}
	if len(f.patched) != 0 {
		t.Fatalf("another workflow's entity must not be re-stamped without --allow-steal-ownership, patched: %v", f.patched)
	}
	if !strings.Contains(errOut.String(), `REFUSED to re-scope evaluation-rules "DR_R001" (id=er-other)`) {
		t.Errorf("expected the refusal line, got:\n%s", errOut.String())
	}
}

func TestLookupEntity_ResolvesWithinTheWorkflowAliasFirst(t *testing.T) {
	cases := []struct {
		name   string
		owners []string
		wantID string
	}{
		{"owned only by the applied workflow", []string{"target-wf"}, "target-wf"},
		{"unscoped only", []string{""}, "unscoped"},
		{"owned only by another workflow", []string{"other-wf"}, "other-wf"},
		{"same code under both workflows", []string{"other-wf", "target-wf"}, "target-wf"},
	}
	for _, resource := range []string{"evaluation-rules", "rule-trees", "scorecards", "mapping-tables"} {
		for _, tc := range cases {
			t.Run(resource+"/"+tc.name, func(t *testing.T) {
				var rows []map[string]any
				for _, owner := range tc.owners {
					id := owner
					if id == "" {
						id = "unscoped"
					}
					rows = append(rows, decisioningEntity(id, "CODE", owner, nil))
				}
				c, _ := newFakeDecisioningBC(t, map[string][]map[string]any{resource: rows})
				got, _ := lookupEntity(c, resource, "CODE", "target-wf", false)
				if id, _ := got["id"].(string); id != tc.wantID {
					t.Fatalf("resolved %v, want id=%s", got, tc.wantID)
				}
			})
		}
	}
}

func TestPrintScopeConflicts_SameCodeUnderTwoWorkflowsIsNotAConflict(t *testing.T) {
	c, _ := newFakeDecisioningBC(t, sameCodesUnderTwoWorkflows())
	spec := &composeSpec{}
	for _, task := range decisioningTasks() {
		spec.Tasks = append(spec.Tasks, task())
	}
	var out bytes.Buffer
	printScopeConflicts(&out, c, spec, "target-wf")
	if out.Len() != 0 {
		t.Fatalf("every code resolves to the applied workflow's own copy, --diff must not report a re-stamp:\n%s", out.String())
	}
}
