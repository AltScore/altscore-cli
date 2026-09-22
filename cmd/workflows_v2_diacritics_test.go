package cmd

import (
	"strings"
	"testing"
)

func TestDiacriticsMissing_FlagsFoldedSpanishOnlyInASCIIStrings(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Cedula de inscripcion al RFC de la empresa no vigente", []string{"cédula", "inscripción"}},
		{"Garantias inmuebles sin valor catastral extraido", []string{"garantías"}},
		{"Cédula de inscripción al RFC", nil},
		{"Opinion of counsel received; revision pending; decision recorded", nil},
		{"Fetch ECU bureau", nil},
		{"Solicitud de credito", []string{"crédito"}},
		{"", nil},
	}
	for _, tc := range cases {
		got := diacriticsMissing(tc.in)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("diacriticsMissing(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAdviseDiacritics_AggregatesAndStaysSilentOnCleanText(t *testing.T) {
	f, ok := adviseDiacritics([]string{
		"Revision Empresa",
		"Cedula de inscripcion al RFC",
		"Opinion de cumplimiento fiscal vigente",
		"Garantias entregadas pero excluidas",
		"Semaforización Documental",
		"",
	})
	if !ok {
		t.Fatal("expected a finding")
	}
	// "Opinion" is also an English word, so that string is deliberately not counted.
	if f.Practice != "diacritics" || f.Count != 2 || f.Total != 5 {
		t.Errorf("finding = %+v, want practice diacritics, 2 of 5", f)
	}
	line := f.line()
	for _, want := range []string{"[diacritics] 2 of 5", "cédula", "garantías", "Cédula, Dirección, Garantías"} {
		if !strings.Contains(line, want) {
			t.Errorf("line missing %q: %s", want, line)
		}
	}

	if _, ok := adviseDiacritics([]string{"Cédula vigente", "Garantías", "Fetch ECU bureau"}); ok {
		t.Error("clean text must not produce a finding")
	}
	if _, ok := adviseDiacritics(nil); ok {
		t.Error("no text, no finding")
	}
}

func TestHumanStringsFromSpec_ReadsLabelsTitlesAndPDFSections(t *testing.T) {
	desc := "Evalua el checklist"
	spec := &composeSpec{
		Label:       "Validacion Documentos",
		Description: &desc,
		CustomVariables: map[string]any{
			"x": map[string]any{"title": "Antiguedad del RFC"},
		},
		InputVariables: map[string]any{
			"rfc": map[string]any{"title": "RFC de la empresa"},
		},
		Nodes: []map[string]any{
			{"ref": "a", "type": "evaluate-rules", "label": "Revision Empresa",
				"inputSchema": map[string]any{"f": map[string]any{"title": "Dias desde emision"}}},
			{"ref": "end", "type": "end", "label": "Fin", "endConfig": map[string]any{
				"pdfConfig": map[string]any{
					"title": "Revision Documentacion",
					"sourcesConfig": []any{
						map[string]any{"type": "htmlBlock", "title": "Garantias Excluidas",
							"components": []any{map[string]any{"content": "<h3>Motivo de exclusion</h3>"}}},
					},
				},
			}},
		},
	}
	got := humanStringsFromSpec(spec)
	joined := strings.Join(got, "|")
	for _, want := range []string{"Validacion Documentos", "Evalua el checklist", "Antiguedad del RFC", "RFC de la empresa",
		"Revision Empresa", "Dias desde emision", "Revision Documentacion", "Garantias Excluidas", "Motivo de exclusion"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	// revision and exclusion are English words too, so those two strings are not flagged.
	f, ok := adviseDiacritics(got)
	if !ok || f.Count != 5 {
		t.Errorf("expected exactly 5 flagged strings, got %+v", f)
	}
}

func TestHumanStringsFromWorkflow_IncludesEndTasksAndRules(t *testing.T) {
	wf := map[string]any{
		"label":           "Validacion",
		"nodes":           []any{map[string]any{"type": "end", "label": "Fin"}},
		"customVariables": map[string]any{"v": map[string]any{"title": "Numero de garantias"}},
	}
	endTasks := []map[string]any{{"endConfig": map[string]any{"pdfConfig": map[string]any{"title": "Resumen del Expediente"}}}}
	rules := []any{map[string]any{"label": "Cedula vigente", "description": "Emitido hace 91 dias o menos.", "alertMessage": "Cédula vencida"}}
	got := humanStringsFromWorkflow(wf, endTasks, rules)
	joined := strings.Join(got, "|")
	for _, want := range []string{"Validacion", "Fin", "Numero de garantias", "Resumen del Expediente", "Cedula vigente", "Emitido hace 91 dias o menos.", "Cédula vencida"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	f, ok := adviseDiacritics(got)
	if !ok || f.Count != 4 {
		t.Errorf("expected 4 flagged (Validacion, Numero de garantias, Cedula vigente, dias), got %+v", f)
	}
}
