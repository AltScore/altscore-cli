package cmd

import (
	"strings"
	"testing"
)

// endNode returns the single end task from a spec for assertions.
func endTask(spec *composeSpec) map[string]any {
	for _, t := range spec.Tasks {
		if tt, _ := t["type"].(string); tt == "end" {
			return t
		}
	}
	return nil
}

func baseSpecWithCustomers(n int) *composeSpec {
	tasks := []map[string]any{}
	for i := 0; i < n; i++ {
		ref := "customer"
		if i > 0 {
			ref = "customer" + string(rune('1'+i))
		}
		tasks = append(tasks, map[string]any{"ref": ref, "type": "customer", "label": "C"})
	}
	tasks = append(tasks, map[string]any{
		"ref": "end", "type": "end", "label": "End",
		"inputMappings": map[string]any{},
	})
	return &composeSpec{Tasks: tasks}
}

func TestApplyAutoEndDefaults_SingleCustomer(t *testing.T) {
	spec := baseSpecWithCustomers(1)
	applyAutoEndDefaults(spec)

	end := endTask(spec)
	im := end["inputMappings"].(map[string]any)
	if got := im["borrower_id"]; got != "task_outputs.customer.borrower_id" {
		t.Errorf("borrower_id = %v, want task_outputs.customer.borrower_id", got)
	}
	if got := im["billable_id"]; got != "task_outputs.customer.borrower_id" {
		t.Errorf("billable_id = %v, want task_outputs.customer.borrower_id", got)
	}
	pdf := end["endConfig"].(map[string]any)["pdfConfig"].(map[string]any)
	if pdf["enabled"] != true || pdf["pdfGenerationRequired"] != true {
		t.Errorf("pdf not forced: %v", pdf)
	}
}

func TestApplyAutoEndDefaults_CallerValuesWin(t *testing.T) {
	spec := baseSpecWithCustomers(1)
	end := endTask(spec)
	end["inputMappings"].(map[string]any)["borrower_id"] = "inputs.explicit"
	end["endConfig"] = map[string]any{"pdfConfig": map[string]any{"title": "Custom"}}

	applyAutoEndDefaults(spec)

	im := end["inputMappings"].(map[string]any)
	if im["borrower_id"] != "inputs.explicit" {
		t.Errorf("caller borrower_id overwritten: %v", im["borrower_id"])
	}
	if im["billable_id"] != "task_outputs.customer.borrower_id" {
		t.Errorf("billable_id not filled: %v", im["billable_id"])
	}
	pdf := end["endConfig"].(map[string]any)["pdfConfig"].(map[string]any)
	if pdf["title"] != "Custom" { // preserved
		t.Errorf("pdf title lost: %v", pdf["title"])
	}
	if pdf["enabled"] != true || pdf["pdfGenerationRequired"] != true { // forced anyway
		t.Errorf("pdf not forced over caller cfg: %v", pdf)
	}
}

func TestApplyAutoEndDefaults_AmbiguousCustomerWarnsAndSkipsWiring(t *testing.T) {
	for _, n := range []int{0, 2} {
		spec := baseSpecWithCustomers(n)
		stderr := captureStderr(t, func() { applyAutoEndDefaults(spec) })

		end := endTask(spec)
		im := end["inputMappings"].(map[string]any)
		if _, has := im["borrower_id"]; has {
			t.Errorf("n=%d: borrower_id should NOT be wired when ambiguous", n)
		}
		// PDF is still forced regardless of customer ambiguity.
		pdf := end["endConfig"].(map[string]any)["pdfConfig"].(map[string]any)
		if pdf["enabled"] != true {
			t.Errorf("n=%d: pdf should still be forced", n)
		}
		if !strings.Contains(stderr, "skipped auto-wiring") {
			t.Errorf("n=%d: expected warning, got %q", n, stderr)
		}
	}
}

func TestNormalizeEntityWriteTask_DealContactIdentityValue(t *testing.T) {
	task := map[string]any{
		"type": "deal",
		"contacts": []any{
			map[string]any{"id": "0", "tax_id": "{{inputs.a}}"},                           // -> identity_value from tax_id
			map[string]any{"id": "1", "identity_key": "email", "email": "{{inputs.b}}"},   // -> from email
			map[string]any{"id": "2", "tax_id": "x", "identity_value": "{{inputs.keep}}"}, // caller wins
		},
	}
	if err := normalizeEntityWriteTask(task, &composeNormalizeOpts{AutoDefaults: true}); err != nil {
		t.Fatal(err)
	}
	cs := task["contacts"].([]any)
	c0 := cs[0].(map[string]any)
	if c0["identity_key"] != "tax_id" || c0["identity_value"] != "{{inputs.a}}" {
		t.Errorf("c0 = %v", c0)
	}
	c1 := cs[1].(map[string]any)
	if c1["identity_value"] != "{{inputs.b}}" {
		t.Errorf("c1 identity_value = %v, want {{inputs.b}}", c1["identity_value"])
	}
	c2 := cs[2].(map[string]any)
	if c2["identity_value"] != "{{inputs.keep}}" {
		t.Errorf("c2 caller identity_value overwritten: %v", c2["identity_value"])
	}
}

func TestNormalizeEntityWriteTask_DealContactsSkippedWhenOptOut(t *testing.T) {
	task := map[string]any{
		"type":     "deal",
		"contacts": []any{map[string]any{"id": "0", "tax_id": "{{inputs.a}}"}},
	}
	if err := normalizeEntityWriteTask(task, &composeNormalizeOpts{AutoDefaults: false}); err != nil {
		t.Fatal(err)
	}
	c0 := task["contacts"].([]any)[0].(map[string]any)
	if _, has := c0["identity_value"]; has {
		t.Errorf("identity_value should not be filled when AutoDefaults is off: %v", c0)
	}
}
