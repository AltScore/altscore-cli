package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestAdviseVariableTitlesFlagsOnlyUntitled(t *testing.T) {
	f, ok := adviseVariableTitles(map[string]any{
		"debt_over_limit":             map[string]any{"title": "Porcentaje Deuda sobre Cupo"},
		"cupo_recomendado":            map[string]any{"title": "Cupo Recomendado"},
		"pjex_cat_final_limit_amount": map[string]any{},
		"final_decision":              map[string]any{"title": "   "},
	})
	if !ok {
		t.Fatal("expected a finding when some variables are untitled")
	}
	if f.Count != 2 || f.Total != 4 {
		t.Fatalf("want 2 of 4, got %d of %d", f.Count, f.Total)
	}
	// Sorted, so the sample is stable across runs (map iteration is not).
	if got := strings.Join(f.Sample, ","); got != "final_decision,pjex_cat_final_limit_amount" {
		t.Fatalf("unexpected sample: %q", got)
	}
}

func TestAdviseVariableTitlesSilentWhenAllTitled(t *testing.T) {
	// Afecor's shape: every custom variable carries a title. The advisory must
	// say nothing at all rather than emit a zero-count line.
	if _, ok := adviseVariableTitles(map[string]any{
		"age":               map[string]any{"title": "Edad"},
		"antig_edad_afecor": map[string]any{"title": "Antigüedad AFECOR"},
	}); ok {
		t.Fatal("expected no finding when every variable has a title")
	}
	if _, ok := adviseVariableTitles(nil); ok {
		t.Fatal("expected no finding for a workflow with no custom variables")
	}
}

// endNodeWithPDF builds the apply-spec shape, where endConfig sits inline on the node.
func endNodeWithPDF(pdf map[string]any) []any {
	return []any{map[string]any{
		"type":      "end",
		"endConfig": map[string]any{"pdfConfig": pdf},
	}}
}

func TestAdvisePDFSectionsFlagsUntitledAndPythonHTML(t *testing.T) {
	nodes := endNodeWithPDF(map[string]any{
		"enabled":           true,
		"includeAllSources": true,
		"sourcesConfig": []any{
			// La Fabril's shape: no title, body is one placeholder fed by Python.
			map[string]any{
				"type":       "htmlBlock",
				"title":      "",
				"components": []any{map[string]any{"content": "{rules_html}"}},
			},
			// Afecor's shape: a retitled override of an auto-wired section.
			map[string]any{
				"type":      "scorecard",
				"title":     "Desgloce Puntaje",
				"taskAlias": "scorecard-921a52",
			},
			// Authored HTML with placeholders: the recommended shape, not flagged.
			map[string]any{
				"type":       "htmlBlock",
				"title":      "Resumen",
				"components": []any{map[string]any{"content": "<div><b>Cupo:</b> {cupo}</div>"}},
			},
		},
	})
	got := advisePDFSections(nodes, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 findings (untitled + python-html), got %d", len(got))
	}
	byPractice := map[string]readabilityFinding{}
	for _, f := range got {
		byPractice[f.Practice] = f
	}
	if f := byPractice["pdf-section-titles"]; f.Count != 1 || f.Total != 3 {
		t.Fatalf("pdf-section-titles: want 1 of 3, got %d of %d", f.Count, f.Total)
	}
	if f := byPractice["pdf-python-html"]; f.Count != 1 || f.Sample[0] != "{rules_html}" {
		t.Fatalf("pdf-python-html: want 1 flagging {rules_html}, got %d %v", f.Count, f.Sample)
	}
}

func TestAdvisePDFSectionsReadsTaskBodyNotJustNode(t *testing.T) {
	// GET /v2/workflows/{id} returns end nodes WITHOUT endConfig -- pdfConfig
	// only exists on the fetched task. Reading the node alone would make this
	// check a silent no-op on every saved workflow, which is the regression
	// this test exists to prevent.
	nodes := []any{map[string]any{
		"type":      "end",
		"taskAlias": "end-3af1fd",
		"data":      map[string]any{"inputMappings": map[string]any{}},
	}}
	if got := advisePDFSections(nodes, nil); len(got) != 0 {
		t.Fatalf("no task body available: want 0 findings, got %d", len(got))
	}
	endTasks := []map[string]any{{
		"alias": "end-3af1fd",
		"endConfig": map[string]any{"pdfConfig": map[string]any{
			"enabled": true,
			"sourcesConfig": []any{map[string]any{
				"type":       "htmlBlock",
				"components": []any{map[string]any{"content": "{cover_html}"}},
			}},
		}},
	}}
	got := advisePDFSections(nodes, endTasks)
	if len(got) != 2 {
		t.Fatalf("with the task body: want 2 findings, got %d", len(got))
	}
}

func TestAdvisePDFSectionsIgnoresDisabledAndEmpty(t *testing.T) {
	// pdfConfig.enabled=false: nothing renders, so nothing to advise on.
	if got := advisePDFSections(endNodeWithPDF(map[string]any{
		"enabled":       false,
		"sourcesConfig": []any{map[string]any{"type": "htmlBlock", "title": ""}},
	}), nil); len(got) != 0 {
		t.Fatalf("disabled pdf: want 0 findings, got %d", len(got))
	}
	// enabled + empty sourcesConfig is the RECOMMENDED overlay shape: the
	// runtime auto-wires every data-source ancestor. Warning here is the exact
	// mistake the removed silent-PDF lint made.
	if got := advisePDFSections(endNodeWithPDF(map[string]any{
		"enabled":           true,
		"includeAllSources": true,
		"sourcesConfig":     []any{},
	}), nil); len(got) != 0 {
		t.Fatalf("enabled+empty is valid: want 0 findings, got %d", len(got))
	}
}

func TestAdviseRuleDescriptions(t *testing.T) {
	rules := []any{
		map[string]any{"code": "KYB-002", "description": "Ported from kyb v2 rule KYB-002 alertLevel=-1"},
		map[string]any{"code": "FIN-EC-005", "description": ""},
		map[string]any{"code": "KYC-EC-004", "description": nil},
		map[string]any{"code": "KYC-EC-001", "description": "El aplicante no registra procesos en el Consejo de la Judicatura"},
	}
	f, ok := adviseRuleDescriptions(rules)
	if !ok {
		t.Fatal("expected a finding")
	}
	if f.Count != 3 || f.Total != 4 {
		t.Fatalf("want 3 of 4, got %d of %d", f.Count, f.Total)
	}
	if strings.Contains(strings.Join(f.Sample, ","), "KYC-EC-001") {
		t.Fatal("a description that states the business rule must not be flagged")
	}
}

func TestAdviseRuleDescriptionsSilentWhenClean(t *testing.T) {
	if _, ok := adviseRuleDescriptions([]any{
		map[string]any{"code": "A", "description": "El aplicante tiene mas de 65 anios"},
	}); ok {
		t.Fatal("expected no finding when every rule states its business rule")
	}
	// nil means "could not fetch", NOT "no rules": staying silent is the point.
	if _, ok := adviseRuleDescriptions(nil); ok {
		t.Fatal("expected no finding when rules could not be fetched")
	}
}

func TestProvenanceDescriptionDoesNotOverfire(t *testing.T) {
	flagged := []string{
		"Ported from kyb v2 rule KYB-002",
		"Migrated from the legacy engine",
		"Copied from scoring_pj v3",
		"Rule carried over from v1",
		"Replaces a legacy rule in the old engine",
	}
	for _, d := range flagged {
		if !provenanceDescription.MatchString(d) {
			t.Errorf("should be flagged as provenance: %q", d)
		}
	}
	// Real business descriptions that merely contain "from" must survive.
	clean := []string{
		"El aplicante no registra procesos en el Consejo de la Judicatura",
		"Cliente con deuda vencida reportada from SRI en los ultimos 12 meses",
		"Score proveniente del buro de credito",
		"Rechaza si el monto solicitado supera el cupo disponible",
	}
	for _, d := range clean {
		if provenanceDescription.MatchString(d) {
			t.Errorf("business description wrongly flagged as provenance: %q", d)
		}
	}
}

func TestAdviseHandoffReadabilityAggregatesAndStaysSilent(t *testing.T) {
	// 218 untitled variables must produce ONE line, not 218.
	cv := map[string]any{}
	for i := 0; i < 218; i++ {
		cv[string(rune('a'+i%26))+strings.Repeat("x", i/26+1)] = map[string]any{}
	}
	var buf bytes.Buffer
	adviseHandoffReadability(map[string]any{"customVariables": cv}, nil, nil, &buf)
	out := buf.String()
	if lines := strings.Count(out, "\n"); lines > 3 {
		t.Fatalf("advisory must aggregate; got %d lines:\n%s", lines, out)
	}
	if !strings.Contains(out, "[variable-titles]") {
		t.Fatalf("missing the variable-titles finding: %s", out)
	}

	// A clean workflow prints nothing at all.
	buf.Reset()
	adviseHandoffReadability(map[string]any{
		"customVariables": map[string]any{"age": map[string]any{"title": "Edad"}},
	}, nil, nil, &buf)
	if buf.Len() != 0 {
		t.Fatalf("clean workflow must print nothing, got: %s", buf.String())
	}

	// A nil writer must not panic.
	adviseHandoffReadability(map[string]any{"customVariables": cv}, nil, nil, nil)
}
