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
