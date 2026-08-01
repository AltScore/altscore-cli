package cmd

import (
	"strings"
	"testing"
)

// mtTask builds a minimal mapping-table task whose single entry reads inVar.
// topLevel, when non-nil, seeds the node's caller-supplied inputMappings.
func mtTask(inVar string, topLevel map[string]any) map[string]any {
	t := map[string]any{
		"type":  "mapping-table",
		"label": "Tier",
		"mappingTableConfig": map[string]any{
			"entries": []any{
				map[string]any{
					"mappingTableCode": "claude-credit-tier-v1",
					"inputVariable":    inVar,
					"outputVariable":   "risk_tier",
				},
			},
		},
	}
	if topLevel != nil {
		t["inputMappings"] = topLevel
	}
	return t
}

func mtMappings(t *testing.T, task map[string]any) map[string]any {
	t.Helper()
	m, _ := task["inputMappings"].(map[string]any)
	if m == nil {
		t.Fatalf("task has no inputMappings: %#v", task)
	}
	return m
}

// Scoped entry inputVariable mirrors into a full-path top-level inputMapping.
func TestNormalizeMappingTable_ScopedInputVariable_MirrorsFullPath(t *testing.T) {
	task := mtTask("task_outputs.score-abc.total_score", nil)
	if err := normalizeMappingTableTask(nil, task, &composeNormalizeOpts{}, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	im := mtMappings(t, task)
	if got := im["total_score"]; got != "task_outputs.score-abc.total_score" {
		t.Errorf("mirrored mapping = %v, want full scoped path", got)
	}
}

// A bare entry inputVariable with no backing top-level mapping must fail loud
// (it would 400 at /v2/tasks and silently default-bucket at runtime).
func TestNormalizeMappingTable_BareInputVariable_NoMapping_Errors(t *testing.T) {
	task := mtTask("total_score", nil)
	err := normalizeMappingTableTask(nil, task, &composeNormalizeOpts{}, true)
	if err == nil {
		t.Fatal("expected error for unscoped bare inputVariable, got nil")
	}
	if !strings.Contains(err.Error(), "unscoped bare name") {
		t.Errorf("error message not actionable: %v", err)
	}
	// And it must NOT have fabricated a path-less self-reference.
	if im, _ := task["inputMappings"].(map[string]any); im["total_score"] == "total_score" {
		t.Errorf("fabricated path-less mapping despite error: %v", im)
	}
}

// A bare entry inputVariable is allowed when the caller supplied an explicit
// scoped top-level mapping (the runtime resolves it via the context fallback).
// The mirror must NOT clobber that caller value.
func TestNormalizeMappingTable_BareInputVariable_WithCallerMapping_OK(t *testing.T) {
	task := mtTask("total_score", map[string]any{
		"total_score": "task_outputs.score-abc.total_score",
	})
	if err := normalizeMappingTableTask(nil, task, &composeNormalizeOpts{}, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	im := mtMappings(t, task)
	if got := im["total_score"]; got != "task_outputs.score-abc.total_score" {
		t.Errorf("caller mapping clobbered: %v", got)
	}
}

// A bare inputVariable matching a declared workflow input is auto-wrapped to
// inputs.<name> and then mirrors cleanly.
func TestNormalizeMappingTable_BareInputVariable_WrapsWorkflowInput(t *testing.T) {
	task := mtTask("risk_in", nil)
	opts := &composeNormalizeOpts{InputVariables: map[string]any{"risk_in": map[string]any{"type": "number"}}}
	if err := normalizeMappingTableTask(nil, task, opts, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg := task["mappingTableConfig"].(map[string]any)
	entry := cfg["entries"].([]any)[0].(map[string]any)
	if entry["inputVariable"] != "inputs.risk_in" {
		t.Errorf("inputVariable not wrapped: %v", entry["inputVariable"])
	}
	im := mtMappings(t, task)
	if im["risk_in"] != "inputs.risk_in" {
		t.Errorf("mirrored mapping = %v, want inputs.risk_in", im["risk_in"])
	}
}

// mirrorEntryInputsToTopLevel never fabricates a path-less self-reference.
func TestMirrorEntryInputs_SkipsBareNames(t *testing.T) {
	task := map[string]any{"inputMappings": map[string]any{}}
	entries := []any{
		map[string]any{"inputVariable": "bare_name"},            // skip
		map[string]any{"inputVariable": "task_outputs.x.score"}, // mirror -> score
		map[string]any{"inputVariable": "inputs.amount"},        // mirror -> amount
	}
	mirrorEntryInputsToTopLevel(task, entries)
	im := task["inputMappings"].(map[string]any)
	if _, has := im["bare_name"]; has {
		t.Errorf("bare name was mirrored: %v", im)
	}
	if im["score"] != "task_outputs.x.score" {
		t.Errorf("score mapping = %v", im["score"])
	}
	if im["amount"] != "inputs.amount" {
		t.Errorf("amount mapping = %v", im["amount"])
	}
}

func TestIsScopedRef(t *testing.T) {
	cases := map[string]bool{
		"total_score":      false,
		"inputs.x":         true,
		"task_outputs.a.b": true,
		"custom.y":         true,
		"system.tenant":    true,
		`__static__::"A"`:  true,
		"":                 false,
	}
	for in, want := range cases {
		if got := isScopedRef(in); got != want {
			t.Errorf("isScopedRef(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLastDotSegment(t *testing.T) {
	cases := map[string]string{
		"task_outputs.a.total_score": "total_score",
		"inputs.amount":              "amount",
		"bare":                       "bare",
	}
	for in, want := range cases {
		if got := lastDotSegment(in); got != want {
			t.Errorf("lastDotSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPublishSuffix(t *testing.T) {
	if !strings.Contains(publishSuffix(true), "publish") {
		t.Errorf("publishSuffix(true) = %q", publishSuffix(true))
	}
	if !strings.Contains(publishSuffix(false), "DRAFT") {
		t.Errorf("publishSuffix(false) = %q", publishSuffix(false))
	}
}

// --- nested credit-decisioning entity scopes ----------------------------

// seedEntities primes lookupEntity's memo so the normalizers resolve entities
// with a nil client (no HTTP), and restores it when the test ends.
func seedEntities(t *testing.T, entities map[string]map[string]any) {
	t.Helper()
	prev := entityCache
	entityCache = entities
	t.Cleanup(func() { entityCache = prev })
}

// reconcileEntityScopes re-stamps every mapping table a scorecard's rules link
// to, but normalizeScorecardTask only ever checked the scorecard itself. A
// cross-owned bucket table reachable ONLY through the scorecard therefore
// cleared pre-flight, the workflow was created and published, and the re-stamp
// was refused on stderr with everything already persisted -- leaving the table
// on the old alias and invisible in the new workflow's Hub elements panel.
func TestNormalizeScorecardTask_NestedMappingTableScope(t *testing.T) {
	scorecardTask := func() map[string]any {
		return map[string]any{
			"type":            "scorecard",
			"label":           "Score",
			"scorecardConfig": map[string]any{"scorecardCode": "sc-code"},
		}
	}
	cases := []struct {
		name       string
		mtAlias    string // the mapping table's current workflowAlias
		allowSteal bool
		wantErr    string // substring; "" = accepted
	}{
		{"nested table already owned by this workflow", "target-wf", false, ""},
		{"nested table unscoped -- apply will claim it", "", false, ""},
		{"nested table owned by another workflow", "other-wf", false, `mapping-tables "mt-code"`},
		{"cross-owned but --allow-steal-ownership", "other-wf", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seedEntities(t, map[string]map[string]any{
				"scorecards|sc-code": {
					"id": "sc-1", "code": "sc-code", "workflowAlias": "target-wf",
					"rules": []any{map[string]any{"mappingTableCode": "mt-code"}},
				},
				"mapping-tables|mt-code": {
					"id": "mt-1", "code": "mt-code", "workflowAlias": c.mtAlias,
				},
			})
			opts := &composeNormalizeOpts{PredictedAlias: "target-wf", AllowStealOwnership: c.allowSteal}
			err := normalizeScorecardTask(nil, scorecardTask(), opts, false)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a cross-owned nested mapping table must fail pre-flight, before any task is POSTed")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error should name the nested mapping table (%s), got: %v", c.wantErr, err)
			}
			if !strings.Contains(err.Error(), "rules[0]") {
				t.Errorf("error should locate the offending rule, got: %v", err)
			}
			if !strings.Contains(err.Error(), "other-wf") {
				t.Errorf("error should name the current owner, got: %v", err)
			}
		})
	}
}

// Same invariant on the rule-tree side: reconcileEntityScopes re-stamps every
// evaluation-rule the tree references, so pre-flight owns that set too. The
// recursion existed but only checked decisionKey, never the workflow scope.
func TestNormalizeRuleTreeTask_NestedEvaluationRuleScope(t *testing.T) {
	ruleTreeTask := func() map[string]any {
		return map[string]any{
			"type":  "rule-tree",
			"label": "Decide",
			"ruleTreeConfig": map[string]any{
				"ruleTreeCode":   "rt-code",
				"outputVariable": "decision",
			},
		}
	}
	cases := []struct {
		name      string
		ruleAlias string
		wantErr   string
	}{
		{"nested rule already owned by this workflow", "target-wf", ""},
		{"nested rule unscoped", "", ""},
		{"nested rule owned by another workflow", "other-wf", `evaluation-rules "er-code"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seedEntities(t, map[string]map[string]any{
				"rule-trees|rt-code": {
					"id": "rt-1", "code": "rt-code", "workflowAlias": "target-wf",
					"rules": []any{map[string]any{"ruleCode": "er-code"}},
				},
				"evaluation-rules|er-code": {
					"id": "er-1", "code": "er-code", "workflowAlias": c.ruleAlias,
				},
			})
			opts := &composeNormalizeOpts{PredictedAlias: "target-wf"}
			err := normalizeRuleTreeTask(nil, ruleTreeTask(), opts, false)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a cross-owned nested evaluation-rule must fail pre-flight")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error should name the nested rule (%s), got: %v", c.wantErr, err)
			}
			if !strings.Contains(err.Error(), "rules[0]") {
				t.Errorf("error should locate the offending rule, got: %v", err)
			}
		})
	}
}
