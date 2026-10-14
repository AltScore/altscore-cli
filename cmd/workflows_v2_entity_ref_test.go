package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// BC's DecisioningEntityResolver resolves a reference by the code under the workflow's
// alias, then the code tenant-wide, then the stored id. The preflight used to stop after
// the two by-code list queries, so `apply --publish` called every id-only reference
// (mappingTableId, scorecardId, ruleTreeId, ruleId) missing.
func TestLookupEntityRef_ResolvesInBCsOrder(t *testing.T) {
	cases := []struct {
		name     string
		entities []map[string]any
		code, id string
		wantID   string
	}{
		{"id only", []map[string]any{decisioningEntity("e-1", "CODE", "", nil)}, "", "e-1", "e-1"},
		{"id only, entity owned by the applied workflow", []map[string]any{decisioningEntity("e-1", "CODE", "target-wf", nil)}, "", "e-1", "e-1"},
		{"unknown code falls back to the id", []map[string]any{decisioningEntity("e-1", "CODE", "", nil)}, "GONE", "e-1", "e-1"},
		{"code under the alias wins over the id", []map[string]any{
			decisioningEntity("e-target", "CODE", "target-wf", nil),
			decisioningEntity("e-other", "OTHER", "other-wf", nil),
		}, "CODE", "e-other", "e-target"},
		{"code tenant-wide still wins over the id", []map[string]any{
			decisioningEntity("e-other", "CODE", "other-wf", nil),
			decisioningEntity("e-2", "OTHER", "", nil),
		}, "CODE", "e-2", "e-other"},
		{"neither", []map[string]any{decisioningEntity("e-1", "CODE", "", nil)}, "", "nope", ""},
	}
	for _, resource := range versionedDecisioningResources {
		for _, tc := range cases {
			t.Run(resource+"/"+tc.name, func(t *testing.T) {
				c, _ := newFakeDecisioningBC(t, map[string][]map[string]any{resource: tc.entities})
				got := lookupEntityRef(c, resource, tc.code, tc.id, "target-wf")
				gotID, _ := got["id"].(string)
				if gotID != tc.wantID {
					t.Fatalf("resolved %v, want id=%q", got, tc.wantID)
				}
			})
		}
	}
}

func idOnlyDecisioningTasks() map[string]func() map[string]any {
	return map[string]func() map[string]any{
		"mapping-table": func() map[string]any {
			return map[string]any{"type": "mapping-table", "label": "Bucket", "mappingTableConfig": map[string]any{
				"entries": []any{map[string]any{
					"mappingTableId": "mt-1", "inputVariable": "inputs.amount", "outputVariable": "bucket", "order": 0,
				}},
			}}
		},
		"scorecard": func() map[string]any {
			return map[string]any{"type": "scorecard", "label": "Score", "scorecardConfig": map[string]any{"scorecardId": "sc-1"}}
		},
		"rule-tree": func() map[string]any {
			return map[string]any{"type": "rule-tree", "label": "Decide", "ruleTreeConfig": map[string]any{
				"ruleTreeId": "rt-1", "outputVariable": "decision",
			}}
		},
		"evaluate-rules": func() map[string]any {
			return map[string]any{"type": "evaluate-rules", "label": "Rules", "rulesConfig": []any{map[string]any{"ruleId": "er-1"}}}
		},
	}
}

func idOnlyEntities(owner string) map[string][]map[string]any {
	return map[string][]map[string]any{
		"mapping-tables":   {decisioningEntity("mt-1", "MT", owner, nil)},
		"scorecards":       {decisioningEntity("sc-1", "SC", owner, map[string]any{"rules": []any{map[string]any{"mappingTableId": "mt-1"}}})},
		"rule-trees":       {decisioningEntity("rt-1", "RT", owner, map[string]any{"rules": []any{map[string]any{"ruleId": "er-1"}}})},
		"evaluation-rules": {decisioningEntity("er-1", "DR_R001", owner, nil)},
	}
}

func TestNormalizeTaskBody_PublishAcceptsAnIdReference(t *testing.T) {
	for _, owner := range []string{"", "target-wf"} {
		for name, task := range idOnlyDecisioningTasks() {
			t.Run(name+"/owner="+owner, func(t *testing.T) {
				c, _ := newFakeDecisioningBC(t, idOnlyEntities(owner))
				opts := &composeNormalizeOpts{PredictedAlias: "target-wf", Publish: true}
				if err := normalizeTaskBody(c, task(), opts, false); err != nil {
					t.Fatalf("an existing entity referenced by id must pass the --publish preflight: %v", err)
				}
			})
		}
	}
	// A missing id is still refused under --publish.
	for name, task := range idOnlyDecisioningTasks() {
		t.Run(name+"/missing", func(t *testing.T) {
			c, _ := newFakeDecisioningBC(t, map[string][]map[string]any{})
			opts := &composeNormalizeOpts{PredictedAlias: "target-wf", Publish: true}
			err := normalizeTaskBody(c, task(), opts, false)
			if err == nil || !strings.Contains(err.Error(), "was not found on the tenant") {
				t.Fatalf("a missing entity must still block --publish, got %v", err)
			}
		})
	}
}

// Once an id resolves, it is held to the rule a code is: another workflow's entity is
// refused, and the refusal names the entity's code and every way forward.
func TestAnIdReferenceToAnotherWorkflowsEntityFollowsTheOwnershipRule(t *testing.T) {
	codes := map[string]string{"mapping-table": "MT", "scorecard": "SC", "rule-tree": "RT", "evaluate-rules": "DR_R001"}
	for name, task := range idOnlyDecisioningTasks() {
		t.Run(name, func(t *testing.T) {
			c, _ := newFakeDecisioningBC(t, idOnlyEntities("other-wf"))
			err := normalizeTaskBody(c, task(), &composeNormalizeOpts{PredictedAlias: "target-wf"}, false)
			if err == nil {
				t.Fatal("an id reference to another workflow's entity must be refused like a code reference")
			}
			for _, want := range []string{
				`(code="` + codes[name] + `") is currently owned by workflow "other-wf"`,
				"re-run apply with --allow-steal-ownership",
				"--skip-rescope --allow-steal-ownership",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal is missing %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestReconcileEntityScopes_StampsAnIdReference(t *testing.T) {
	c, f := newFakeDecisioningBC(t, idOnlyEntities(""))
	spec := &composeSpec{Tasks: []map[string]any{idOnlyDecisioningTasks()["mapping-table"]()}}
	var errOut bytes.Buffer
	if err := reconcileEntityScopes(c, spec, "target-wf", false, &errOut); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.patched, ",") != "mapping-tables/mt-1->target-wf" {
		t.Fatalf("an unscoped table referenced by id must be stamped, patched: %v\nstderr:\n%s", f.patched, errOut.String())
	}
}

// --skip-rescope skips only the re-stamp after the apply. The 1:1 ownership check is
// policy and still runs; the refusal says how to go on instead of talking about a
// transfer that --skip-rescope would never make.
func TestOwnershipRefusalUnderSkipRescopeSaysHowToProceed(t *testing.T) {
	for name, task := range decisioningTasks() {
		t.Run(name, func(t *testing.T) {
			c, _ := newFakeDecisioningBC(t, onlyUnderOtherWorkflow())
			opts := &composeNormalizeOpts{PredictedAlias: "target-wf", SkipRescope: true}
			err := normalizeTaskBody(c, task(), opts, false)
			if err == nil {
				t.Fatal("--skip-rescope alone must not lift the ownership check")
			}
			for _, want := range []string{
				`is currently owned by workflow "other-wf", but this apply targets workflow "target-wf"`,
				"--skip-rescope only skips the re-stamp",
				"--skip-rescope --allow-steal-ownership",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal is missing %q:\n%v", want, err)
				}
			}

			opts.AllowStealOwnership = true
			if err := normalizeTaskBody(c, task(), opts, false); err != nil {
				t.Fatalf("--skip-rescope --allow-steal-ownership references it without a transfer: %v", err)
			}
		})
	}
}
