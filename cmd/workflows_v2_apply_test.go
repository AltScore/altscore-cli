package cmd

import (
	"strings"
	"testing"
)

// TestRewriteRefsInTaskTemplates_EndPdfConfig covers the formerly-missing
// rewrite of endConfig.pdfConfig.sourcesConfig[].taskAlias. Without this
// rewrite, a spec like
//
//	{"taskAlias": "score"}   // spec-local ref
//
// shipped to the server verbatim and the runtime renderer couldn't resolve
// the section. The fix adds an explicit walk over sourcesConfig in the
// "end" branch of rewriteRefsInTaskTemplates.
func TestRewriteRefsInTaskTemplates_EndPdfConfig(t *testing.T) {
	refMap := map[string]string{
		"score": "scorecard-059a48",
		"tree":  "rbol-de-decisi-n-55e977",
	}
	task := map[string]any{
		"type": "end",
		"endConfig": map[string]any{
			"outputJson": "",
			"pdfConfig": map[string]any{
				"enabled": true,
				"sourcesConfig": []any{
					map[string]any{"type": "htmlBlock"},
					map[string]any{"type": "scorecard", "taskAlias": "score"},
					map[string]any{"type": "rule-tree", "taskAlias": "tree"},
					map[string]any{"type": "scorecard", "taskAlias": "scorecard-already-server"},
				},
			},
		},
	}
	if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
		t.Fatalf("rewriteRefsInTaskTemplates: %v", err)
	}
	endCfg := task["endConfig"].(map[string]any)
	pdfCfg := endCfg["pdfConfig"].(map[string]any)
	sources := pdfCfg["sourcesConfig"].([]any)
	cases := []struct {
		idx  int
		want string
		key  string
	}{
		{1, "scorecard-059a48", "taskAlias"},
		{2, "rbol-de-decisi-n-55e977", "taskAlias"},
		{3, "scorecard-already-server", "taskAlias"}, // not in refMap; should pass through
	}
	for _, c := range cases {
		got, _ := sources[c.idx].(map[string]any)[c.key].(string)
		if got != c.want {
			t.Errorf("sources[%d].%s = %q, want %q", c.idx, c.key, got, c.want)
		}
	}
	// Entry without taskAlias (htmlBlock) must be left untouched.
	if _, has := sources[0].(map[string]any)["taskAlias"]; has {
		t.Errorf("sources[0] should not have gained a taskAlias key")
	}
}

// TestValidateNoResidualSpecRefs covers the defensive validator that catches
// any string at a non-excluded path which still equals a spec-local ref --
// i.e., a ref-bearing field the rewriter doesn't yet walk. Establishes the
// bar for future ref-bearing additions: when this fires in CI, either the
// rewriter is missing a path or the field belongs in the exclusion list.
func TestValidateNoResidualSpecRefs(t *testing.T) {
	refMap := map[string]string{
		"score": "scorecard-059a48",
		"fetch": "altdata-enrichment-1234",
	}
	t.Run("clean body passes", func(t *testing.T) {
		body := map[string]any{
			"type":  "end",
			"label": "Fin",
			"endConfig": map[string]any{
				"pdfConfig": map[string]any{
					"sourcesConfig": []any{
						map[string]any{"type": "scorecard", "taskAlias": "scorecard-059a48"},
					},
				},
			},
		}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err != nil {
			t.Errorf("unexpected error on clean body: %v", err)
		}
	})
	t.Run("residual ref at unknown path fails", func(t *testing.T) {
		// Simulate a hypothetical future ref-bearing field the rewriter
		// doesn't know about. The validator should flag it by path.
		body := map[string]any{
			"type":             "end",
			"someNewRefField":  "score", // not excluded; not rewritten
		}
		err := validateNoResidualSpecRefs(body, refMap, "test")
		if err == nil {
			t.Fatal("expected validator to flag residual ref")
		}
		msg := err.Error()
		if !strings.Contains(msg, "someNewRefField") {
			t.Errorf("error should name the offending path; got: %v", err)
		}
		if !strings.Contains(msg, "scorecard-059a48") {
			t.Errorf("error should name the expected server alias; got: %v", err)
		}
	})
	t.Run("excluded user-text fields pass", func(t *testing.T) {
		body := map[string]any{
			"label":       "score",            // legitimate user text
			"description": "Computes score",   // legitimate user text
			"comment":     "fetch upstream",   // legitimate user text
			"endConfig": map[string]any{
				"pdfConfig": map[string]any{
					"title":    "score breakdown",
					"subtitle": "fetch results",
				},
			},
		}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err != nil {
			t.Errorf("excluded fields should not trip validator: %v", err)
		}
	})
	t.Run("residual ref inside array element fails", func(t *testing.T) {
		body := map[string]any{
			"endConfig": map[string]any{
				"pdfConfig": map[string]any{
					"sourcesConfig": []any{
						map[string]any{"type": "scorecard", "taskAlias": "score"},
					},
				},
			},
		}
		err := validateNoResidualSpecRefs(body, refMap, "test")
		if err == nil {
			t.Fatal("expected validator to flag ref nested in array")
		}
		if !strings.Contains(err.Error(), "sourcesConfig[0].taskAlias") {
			t.Errorf("error should include bracketed path; got: %v", err)
		}
	})
	t.Run("empty refMap is a no-op", func(t *testing.T) {
		body := map[string]any{"taskAlias": "score"}
		if err := validateNoResidualSpecRefs(body, map[string]string{}, "test"); err != nil {
			t.Errorf("empty refMap should short-circuit cleanly: %v", err)
		}
	})
}

// TestDetectLegacySpecShape_RejectsTasksKey: a spec using the removed
// `tasks[]` two-bucket shape is rejected with a migration error that
// names the offending key and shows the rewrite.
func TestDetectLegacySpecShape_RejectsTasksKey(t *testing.T) {
	body := []byte(`{
		"label": "x",
		"category": "EVALUATION",
		"tasks": [{"ref":"fetch","type":"altdata-enrichment"}]
	}`)
	err := detectLegacySpecShape(body)
	if err == nil {
		t.Fatal("expected error rejecting tasks[] input; got nil")
	}
	msg := err.Error()
	for _, want := range []string{"tasks[] (1 entries)", "nodes[]", "before:", "after:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q; got: %s", want, msg)
		}
	}
}

// TestDetectLegacySpecShape_RejectsExtraNodesKey: same for an extraNodes-only spec.
func TestDetectLegacySpecShape_RejectsExtraNodesKey(t *testing.T) {
	body := []byte(`{
		"label": "x",
		"extraNodes": [{"ref":"start","type":"start"},{"ref":"end","type":"end"}]
	}`)
	err := detectLegacySpecShape(body)
	if err == nil {
		t.Fatal("expected error rejecting extraNodes[] input; got nil")
	}
	if !strings.Contains(err.Error(), "extraNodes[] (2 entries)") {
		t.Errorf("expected entry count in error; got: %s", err.Error())
	}
}

// TestDetectLegacySpecShape_RejectsBoth: both keys present cited together.
func TestDetectLegacySpecShape_RejectsBoth(t *testing.T) {
	body := []byte(`{
		"tasks":      [{"ref":"a","type":"altdata-enrichment"}],
		"extraNodes": [{"ref":"start","type":"start"}]
	}`)
	err := detectLegacySpecShape(body)
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tasks[] (1 entries) and extraNodes[] (1 entries)") {
		t.Errorf("expected combined citation; got: %s", msg)
	}
}

// TestDetectLegacySpecShape_PassesNodesShape: a spec using only nodes[] is
// not flagged by the legacy detector.
func TestDetectLegacySpecShape_PassesNodesShape(t *testing.T) {
	body := []byte(`{
		"label": "x",
		"nodes": [
			{"ref":"start","type":"start"},
			{"ref":"fetch","type":"altdata-enrichment"},
			{"ref":"end","type":"end"}
		]
	}`)
	if err := detectLegacySpecShape(body); err != nil {
		t.Fatalf("nodes[] spec should pass detector; got: %v", err)
	}
}

// TestDetectLegacySpecShape_IgnoresEmptyLegacyArrays: empty `tasks: []` or
// `extraNodes: []` keys are tolerated (treated as absent) so callers can
// emit either omit-or-empty without spurious errors.
func TestDetectLegacySpecShape_IgnoresEmptyLegacyArrays(t *testing.T) {
	body := []byte(`{"tasks": [], "extraNodes": [], "nodes": [{"ref":"start","type":"start"}]}`)
	if err := detectLegacySpecShape(body); err != nil {
		t.Fatalf("empty legacy arrays should be tolerated; got: %v", err)
	}
}

// TestDetectLegacySpecShape_IgnoresMalformedJSON: detector defers to the
// main unmarshal for parse errors -- doesn't return its own.
func TestDetectLegacySpecShape_IgnoresMalformedJSON(t *testing.T) {
	if err := detectLegacySpecShape([]byte("not json")); err != nil {
		t.Errorf("malformed JSON should defer to caller's unmarshal; got: %v", err)
	}
}

// TestRewriteRefsInTaskTemplates_EndStandardOutput covers the rewrite of
// endConfig.standardOutput template refs (sections, score sub-fields, and
// author-added fields) from spec-local refs to server aliases -- the same
// treatment outputJson gets. Without it an authored standardOutput ships with
// stale spec refs and the runtime resolves each section to empty.
func TestRewriteRefsInTaskTemplates_EndStandardOutput(t *testing.T) {
	refMap := map[string]string{
		"score": "scorecard-059a48",
		"tree":  "rbol-de-decisi-n-55e977",
	}
	task := map[string]any{
		"type": "end",
		"endConfig": map[string]any{
			"standardOutput": map[string]any{
				"enabled":   true,
				"scorecard": "{{task_outputs.score.rows}}",
				"rules":     "{{task_outputs.tree.rules}}",
				"decision":  "{{task_outputs.score.decision}}",
				"score": map[string]any{
					"key":      "score",
					"label":    "Score",
					"value":    "{{task_outputs.score.total}}",
					"maxValue": float64(1000),
				},
				"fields": map[string]any{
					"tax_id": "{{inputs.tax_id}}",
					"note":   "approved",
				},
			},
		},
	}
	if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
		t.Fatalf("rewriteRefsInTaskTemplates: %v", err)
	}
	std := task["endConfig"].(map[string]any)["standardOutput"].(map[string]any)
	if got := std["scorecard"]; got != "{{task_outputs.scorecard-059a48.rows}}" {
		t.Errorf("scorecard = %q, want rewritten server alias", got)
	}
	if got := std["rules"]; got != "{{task_outputs.rbol-de-decisi-n-55e977.rules}}" {
		t.Errorf("rules = %q, want rewritten server alias", got)
	}
	if got := std["decision"]; got != "{{task_outputs.scorecard-059a48.decision}}" {
		t.Errorf("decision = %q", got)
	}
	score := std["score"].(map[string]any)
	if got := score["value"]; got != "{{task_outputs.scorecard-059a48.total}}" {
		t.Errorf("score.value = %q, want rewritten", got)
	}
	// Literals survive untouched.
	if got := score["key"]; got != "score" {
		t.Errorf("score.key literal changed: %q", got)
	}
	fields := std["fields"].(map[string]any)
	if got := fields["note"]; got != "approved" {
		t.Errorf("fields.note literal changed: %q", got)
	}
	// inputs.* is a reserved scope, left as-is.
	if got := fields["tax_id"]; got != "{{inputs.tax_id}}" {
		t.Errorf("fields.tax_id = %q, want unchanged", got)
	}
}

// TestRewriteRefsInTaskTemplates_EndStandardOutputUnknownRef ensures an unknown
// task ref in a standardOutput template fails loudly, same as outputJson.
func TestRewriteRefsInTaskTemplates_EndStandardOutputUnknownRef(t *testing.T) {
	task := map[string]any{
		"type": "end",
		"endConfig": map[string]any{
			"standardOutput": map[string]any{
				"scorecard": "{{task_outputs.nonexistent.rows}}",
			},
		},
	}
	err := rewriteRefsInTaskTemplates(task, map[string]string{"score": "scorecard-1"})
	if err == nil {
		t.Fatal("expected error for unknown standardOutput ref, got nil")
	}
	if !strings.Contains(err.Error(), "standardOutput.scorecard") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

// TestValidateStandardOutputShape covers the structural validation surfaced
// before apply ships the body.
func TestValidateStandardOutputShape(t *testing.T) {
	cases := []struct {
		name    string
		std     map[string]any
		wantErr string // substring; "" = no error
	}{
		{"valid", map[string]any{"enabled": true, "scorecard": "{{task_outputs.s.rows}}", "score": map[string]any{"value": "{{task_outputs.s.t}}"}, "fields": map[string]any{"a": "b"}}, ""},
		{"enabled not bool", map[string]any{"enabled": "yes"}, "enabled must be a boolean"},
		{"section is list", map[string]any{"scorecard": []any{1, 2}}, "scorecard must be a variable template string"},
		{"unknown top key", map[string]any{"scoreCard": "x"}, `unknown field "scoreCard"`},
		{"score not object", map[string]any{"score": "x"}, "score must be an object"},
		{"score unknown key", map[string]any{"score": map[string]any{"maximum": 1}}, `score has unknown field "maximum"`},
		{"fields not object", map[string]any{"fields": []any{}}, "fields must be an object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateStandardOutputShape(c.std)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

// --- workflow alias pre-flight ------------------------------------------

// The workflow alias is the LAST thing the create path validates: BC only
// rejects a non-kebab-case alias at POST /v2/workflows, after every task has
// already been POSTed and cannot be un-created. checkWorkflowAlias moves that
// rejection ahead of the first HTTP call.
func TestCheckWorkflowAlias(t *testing.T) {
	cases := []struct {
		name    string
		alias   string
		wantErr string // substring; "" = accepted
	}{
		{"omitted -- server slugifies the label", "", ""},
		{"kebab", "over-the-road-v2", ""},
		{"single word", "kyb", ""},
		{"leading digit", "0089-kyb", ""},
		{"exactly 100 chars", strings.Repeat("a", 100), ""},
		{"underscores", "over_the_road_v2", "over-the-road-v2"},
		{"uppercase", "OverTheRoad", "not kebab-case"},
		{"spaces", "over the road", "over-the-road"},
		{"leading dash", "-otr", "not kebab-case"},
		{"slash", "otr/v2", "otr-v2"},
		{"101 chars", strings.Repeat("a", 101), "not kebab-case"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkWorkflowAlias(c.alias)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("checkWorkflowAlias(%q) = %v, want nil", c.alias, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkWorkflowAlias(%q) = nil, want an error", c.alias)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error should contain %q, got: %v", c.wantErr, err)
			}
			// Every rejection must state the regex so the fix is obvious.
			if !strings.Contains(err.Error(), "^[a-z0-9][a-z0-9-]*$") {
				t.Errorf("error should quote the alias regex, got: %v", err)
			}
		})
	}
}

// End-to-end guard: the underscore alias must die in preflightTasks, i.e.
// before composeWorkflowBody POSTs anything. On the pre-change code this spec
// cleared preflight entirely and leaked one task row per node.
func TestPreflightTasks_RejectsNonKebabWorkflowAlias(t *testing.T) {
	spec := &composeSpec{
		Label:      "Over the road v2",
		Alias:      "over_the_road_v2",
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			{"ref": "end", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{{"from": "start", "to": "end"}},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight must reject a non-kebab-case workflow alias before any task is POSTed")
	}
	if !strings.Contains(err.Error(), "over_the_road_v2") || !strings.Contains(err.Error(), "over-the-road-v2") {
		t.Errorf("error should name the bad alias and suggest the kebab form, got: %v", err)
	}
}

// --- node ref / workflow variable name collisions -----------------------

// A node ref that is also a workflow variable name breaks apply's exact-string
// ref rewriting: validateNoResidualSpecRefs then reads a field holding the
// VARIABLE's bare name as an un-rewritten node ref and aborts in the POST loop,
// after tasks exist. Catch it in phase 1 instead.
func TestPreflightTasks_RefVariableCollision(t *testing.T) {
	cases := []struct {
		name    string
		inputs  map[string]any
		customs map[string]any
		wantErr string // substring; "" = accepted
	}{
		{
			name:    "no collision",
			inputs:  map[string]any{"raw_score": map[string]any{"type": "number"}},
			wantErr: "",
		},
		{
			name:    "ref collides with an input variable",
			inputs:  map[string]any{"score": map[string]any{"type": "number"}},
			wantErr: `"score" (workflow.inputVariables)`,
		},
		{
			name:    "ref collides with a custom variable",
			customs: map[string]any{"score": map[string]any{"expression": "result = 1"}},
			wantErr: `"score" (workflow.customVariables)`,
		},
		{
			name:    "collision on a graph-only node ref",
			inputs:  map[string]any{"start": map[string]any{"type": "string"}},
			wantErr: `"start" (workflow.inputVariables)`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := &composeSpec{
				Label:           "Scoring engine",
				Category:        "EVALUATION",
				InputVariables:  c.inputs,
				CustomVariables: c.customs,
				ExtraNodes:      startNode,
				Tasks: []map[string]any{
					{"ref": "score", "type": "http", "label": "Score", "url": "https://x.test", "method": "GET"},
					{"ref": "end", "type": "end", "label": "End"},
				},
				Edges: []map[string]any{
					{"from": "start", "to": "score"},
					{"from": "score", "to": "end"},
				},
			}
			err := preflightTasks(spec)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("preflight should accept a spec with no ref/variable collision, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("preflight must reject the ref/variable collision before any task is POSTed")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error should name the colliding name and its scope %q, got: %v", c.wantErr, err)
			}
			if !strings.Contains(err.Error(), "Rename one of the two") {
				t.Errorf("error should tell the author how to fix it, got: %v", err)
			}
		})
	}
}

// Both scopes colliding at once are reported together, in a deterministic order
// (map iteration is random, so the message must be sorted).
func TestCheckRefVariableCollisions_ReportsAllSorted(t *testing.T) {
	spec := &composeSpec{
		InputVariables:  map[string]any{"zeta": map[string]any{}},
		CustomVariables: map[string]any{"alpha": map[string]any{}},
	}
	refs := map[string]bool{"alpha": true, "zeta": true, "unrelated": true}
	for i := 0; i < 20; i++ {
		err := checkRefVariableCollisions(spec, refs)
		if err == nil {
			t.Fatalf("both collisions must be reported")
		}
		want := `"alpha" (workflow.customVariables), "zeta" (workflow.inputVariables)`
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("collisions must be listed sorted; want %q in: %v", want, err)
		}
	}
}

// --- residual-ref safety net vs mapping-table inputVariable -------------

// mappingTableConfig.entries[].inputVariable is dual-mode: BC resolves a
// `task_outputs.`-prefixed value as a node reference and ANY other value as a
// flat context key. rewriteTaskRefs rewrites the first form; the second is a
// variable name and must survive untouched. Before the fix the bare form was
// reported as a residual spec-local ref and killed apply mid-POST.
func TestRewriteTaskRefs_MappingTableInputVariableModes(t *testing.T) {
	refMap := map[string]string{"score": "score-service-49dcec"}
	task := map[string]any{
		"type":          "mapping-table",
		"label":         "Band",
		"inputMappings": map[string]any{"score": "task_outputs.score.weight"},
		"mappingTableConfig": map[string]any{
			"entries": []any{
				// Bare name -> the workflow/task variable called "score".
				map[string]any{"inputVariable": "score", "outputVariable": "risk_band"},
				// Dotted name -> a genuine node reference, must be rewritten.
				map[string]any{"inputVariable": "task_outputs.score.weight", "outputVariable": "weight_band"},
			},
		},
	}
	if err := rewriteTaskRefs(task, refMap, `node ref="metrics-a"`); err != nil {
		t.Fatalf("a bare inputVariable is a context-variable name, not a residual ref: %v", err)
	}
	entries := task["mappingTableConfig"].(map[string]any)["entries"].([]any)
	if got := entries[0].(map[string]any)["inputVariable"]; got != "score" {
		t.Errorf("bare inputVariable must survive verbatim, got %v", got)
	}
	if got := entries[1].(map[string]any)["inputVariable"]; got != "task_outputs.score-service-49dcec.weight" {
		t.Errorf("dotted inputVariable must be rewritten to the server alias, got %v", got)
	}
}

// Excluding inputVariable must not blunt the safety net: a spec-local ref left
// in a field the rewriter genuinely forgot is still a hard error.
func TestValidateNoResidualSpecRefs_StillCatchesRealResidues(t *testing.T) {
	refMap := map[string]string{"score": "score-service-49dcec"}
	body := map[string]any{
		"type": "end",
		"endConfig": map[string]any{
			"pdfConfig": map[string]any{
				"someFutureField": "score",
			},
		},
	}
	err := validateNoResidualSpecRefs(body, refMap, `node ref="end"`)
	if err == nil {
		t.Fatalf("an un-rewritten ref at an unknown path must still fail loudly")
	}
	if !strings.Contains(err.Error(), "someFutureField") {
		t.Errorf("error should name the offending path, got: %v", err)
	}
}
