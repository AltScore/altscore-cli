package cmd

import "testing"

func TestSlugifyWorkflowLabel_FoldsDiacriticsLikeTheServer(t *testing.T) {
	cases := map[string]string{
		"Customer Onboarding":                  "customer-onboarding",
		"Send Email (v2)":                      "send-email-v2",
		"KYC/AML Check":                        "kyc-aml-check",
		"Validación Documentos Persona Moral":  "validacion-documentos-persona-moral",
		"Verificación Documentos Adicional":    "verificacion-documentos-adicional",
		"Validación Documentos Persona Física": "validacion-documentos-persona-fisica",
		"Evaluación Ñandú":                     "evaluacion-nandu",
		"Ação":                                 "acao",
		"  ---Hello   World---  ":              "hello-world",
		"":                                     "workflow",
		"áéíóú":                                "aeiou",
		// NFKD does not decompose these, so BC keeps hyphenating them.
		"Straße": "stra-e",
		"Søren":  "s-ren",
		// A decomposed input (e + combining acute) folds like the composed one.
		"Evaluación": "evaluacion",
	}
	for in, want := range cases {
		if got := slugifyWorkflowLabel(in); got != want {
			t.Errorf("slugifyWorkflowLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
