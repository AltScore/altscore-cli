package cmd

import (
	"encoding/json"
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

// --- persona-as-task-property (vs forced workflow input) -----------------

// Default: no persona input declared and no persona mapping -> persona is set
// as a task literal ("individual"), and NOT wired as an input or inputSchema
// field. The runtime reads CustomerTaskData.persona.
func TestNormalizeEntityWriteTask_PersonaDefaultsToTaskLiteral(t *testing.T) {
	task := map[string]any{"type": "customer", "operation": "write", "key": "person_id"}
	if err := normalizeEntityWriteTask(task, &composeNormalizeOpts{}); err != nil {
		t.Fatal(err)
	}
	if task["persona"] != "individual" {
		t.Errorf("persona literal = %v, want individual", task["persona"])
	}
	if m, _ := task["inputMappings"].(map[string]any); m != nil {
		if _, has := m["persona"]; has {
			t.Errorf("persona must not be wired to an input by default: %v", m)
		}
	}
	if s, _ := task["inputSchema"].(map[string]any); s != nil {
		if _, has := s["persona"]; has {
			t.Errorf("persona must not be added to inputSchema by default: %v", s)
		}
	}
}

// Agent-set task literal persona is preserved (e.g. "business" for a RUC flow).
func TestNormalizeEntityWriteTask_PersonaLiteralPreserved(t *testing.T) {
	task := map[string]any{"type": "customer", "operation": "write", "persona": "business"}
	if err := normalizeEntityWriteTask(task, &composeNormalizeOpts{}); err != nil {
		t.Fatal(err)
	}
	if task["persona"] != "business" {
		t.Errorf("persona literal overwritten: %v", task["persona"])
	}
}

// Opt-in via a declared workflow input: persona keeps the input path -- the
// task gets an inputs.persona mapping and no literal. The inputSchema.persona
// display entry is NO LONGER authored here: BC's DerivedSchemaService derives
// it into derivedSchema server-side (#1526), and BC's create validator keeps a
// persona mapping even when inputSchema doesn't declare it (shape-check only,
// never prune). Only the load-bearing inputMappings wiring stays.
func TestNormalizeEntityWriteTask_PersonaInputPath(t *testing.T) {
	task := map[string]any{"type": "customer", "operation": "write"}
	opts := &composeNormalizeOpts{InputVariables: map[string]any{"persona": map[string]any{"type": "string"}}}
	if err := normalizeEntityWriteTask(task, opts); err != nil {
		t.Fatal(err)
	}
	if _, has := task["persona"]; has {
		t.Errorf("no task literal expected on the input path: %v", task["persona"])
	}
	m := task["inputMappings"].(map[string]any)
	if m["persona"] != "inputs.persona" {
		t.Errorf("persona mapping = %v, want inputs.persona", m["persona"])
	}
	if s, _ := task["inputSchema"].(map[string]any); s != nil {
		if _, has := s["persona"]; has {
			t.Errorf("inputSchema.persona must no longer be authored (server-derived): %v", s)
		}
	}
}

// A persona mapping to a non-input source (custom.*) is left untouched and no
// task literal is injected -- the mapping supplies persona at runtime.
func TestNormalizeEntityWriteTask_PersonaCustomMappingLeftAlone(t *testing.T) {
	task := map[string]any{
		"type":          "customer",
		"operation":     "write",
		"inputMappings": map[string]any{"persona": "custom.persona"},
	}
	if err := normalizeEntityWriteTask(task, &composeNormalizeOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, has := task["persona"]; has {
		t.Errorf("no task literal expected when persona is mapped: %v", task["persona"])
	}
	m := task["inputMappings"].(map[string]any)
	if m["persona"] != "custom.persona" {
		t.Errorf("persona mapping overwritten: %v", m["persona"])
	}
}

// composeSpec.Description is a pointer so an explicit "" (blank it) is
// distinguishable from an omitted field (leave untouched).
func TestComposeSpec_DescriptionPointerSemantics(t *testing.T) {
	var withEmpty composeSpec
	if err := json.Unmarshal([]byte(`{"label":"x","description":""}`), &withEmpty); err != nil {
		t.Fatal(err)
	}
	if withEmpty.Description == nil || *withEmpty.Description != "" {
		t.Errorf("explicit empty description should be non-nil empty string, got %v", withEmpty.Description)
	}
	var omitted composeSpec
	if err := json.Unmarshal([]byte(`{"label":"x"}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.Description != nil {
		t.Errorf("omitted description should be nil, got %v", *omitted.Description)
	}
}
