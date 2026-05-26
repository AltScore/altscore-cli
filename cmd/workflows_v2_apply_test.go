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
