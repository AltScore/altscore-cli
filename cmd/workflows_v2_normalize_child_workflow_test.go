package cmd

import "testing"

func TestNormalizeChildWorkflowTaskSkipsCoverageWhenAnExpressionFeedsTheChild(t *testing.T) {
	task := map[string]any{
		"executorId":      "kyc-individual",
		"inputExpression": "task_outputs.pick-primary.borrower",
		"inputMappings":   map[string]any{},
	}
	if err := normalizeChildWorkflowTask(nil, task, false); err != nil {
		t.Fatalf("expression-fed child must not be checked against inputMappings, got: %v", err)
	}
}

func TestNormalizeChildWorkflowTaskStillChecksCoverageWithoutAnExpression(t *testing.T) {
	for name, expr := range map[string]any{"absent": nil, "blank": "   "} {
		t.Run(name, func(t *testing.T) {
			task := map[string]any{
				"executorId":    "kyc-individual",
				"inputMappings": map[string]any{},
			}
			if expr != nil {
				task["inputExpression"] = expr
			}
			if err := normalizeChildWorkflowTask(nil, task, false); err == nil {
				t.Fatalf("a mapping-fed child must reach the coverage check (which needs a client here)")
			}
		})
	}
}
