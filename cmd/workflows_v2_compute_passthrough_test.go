package cmd

import "testing"

func TestPassthroughComputeExpr_Flags(t *testing.T) {
	// Pure extractions -> flagged (anti-pattern).
	flagged := []string{
		`result = inputs.get("task_outputs.enrich.X.data.pdEc_sri_esActivo")`,
		`result = inputs.is_active`,
		`inputs.get("is_active")`,
		`is_active = task_outputs.enrich.ECU-PUB-0002.data.is_active`,
		`result = custom.tier`,
		"# just a passthrough\nresult = inputs.is_active",
		`result = context.get('borrower_id')`,
	}
	for _, e := range flagged {
		if ref, ok := passthroughComputeExpr(e); !ok {
			t.Errorf("expected passthrough for %q, got ok=false", e)
		} else if ref == "" {
			t.Errorf("passthrough %q returned empty ref", e)
		}
	}
}

func TestPassthroughComputeExpr_DoesNotFlag(t *testing.T) {
	// Real computation -> never flagged.
	ok := []string{
		`result = inputs.get("x") or 0`,                       // fallback
		`result = float(inputs.get("x"))`,                     // coercion
		`result = 1 if inputs.get("x") else 0`,                // conditional
		`result = inputs.a + inputs.b`,                        // arithmetic
		`result = inputs.get("x") > 100`,                      // comparison
		`result = inputs.get("a") and inputs.get("b")`,        // boolean
		"src = inputs.get(\"x\")\nresult = src * 2",           // multi-statement
		`result = round(100 * inputs.amount / inputs.income)`, // expression
		`result = "STATIC"`,                                   // literal
	}
	for _, e := range ok {
		if ref, flagged := passthroughComputeExpr(e); flagged {
			t.Errorf("did NOT expect passthrough for %q, but flagged ref=%q", e, ref)
		}
	}
}
