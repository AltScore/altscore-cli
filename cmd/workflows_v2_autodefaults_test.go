package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	if pdf["title"] != "Custom" {
		t.Errorf("pdf title lost: %v", pdf["title"])
	}
	if pdf["enabled"] != true || pdf["pdfGenerationRequired"] != true {
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
			map[string]any{"id": "0", "tax_id": "{{inputs.a}}"},
			map[string]any{"id": "1", "identity_key": "email", "email": "{{inputs.b}}"},
			map[string]any{"id": "2", "tax_id": "x", "identity_value": "{{inputs.keep}}"},
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

func TestNormalizeEntityWriteTask_PersonaLiteralPreserved(t *testing.T) {
	task := map[string]any{"type": "customer", "operation": "write", "persona": "business"}
	if err := normalizeEntityWriteTask(task, &composeNormalizeOpts{}); err != nil {
		t.Fatal(err)
	}
	if task["persona"] != "business" {
		t.Errorf("persona literal overwritten: %v", task["persona"])
	}
}

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

func TestApplyAutoEndDefaults_PdfConfigFillsOnlyAbsentKeys(t *testing.T) {
	const absent = "<absent>"
	cases := []struct {
		name         string
		pdf          map[string]any
		wantEnabled  any
		wantRequired any
	}{
		{"pdfConfig omitted entirely", nil, true, true},
		{"pdfConfig present but empty", map[string]any{}, true, true},
		{"unrelated key only -- no opinion on enabled", map[string]any{"title": "Custom"}, true, true},
		{"explicit enabled=false wins", map[string]any{"enabled": false}, false, absent},
		{"explicit enabled=false keeps an explicit required", map[string]any{"enabled": false, "pdfGenerationRequired": true}, false, true},
		{"explicit enabled=true still defaults required", map[string]any{"enabled": true}, true, true},
		{"explicit required=false wins", map[string]any{"pdfGenerationRequired": false}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := baseSpecWithCustomers(1)
			end := endTask(spec)
			if c.pdf != nil {
				end["endConfig"] = map[string]any{"pdfConfig": c.pdf}
			}

			applyAutoEndDefaults(spec)

			pdf := endTask(spec)["endConfig"].(map[string]any)["pdfConfig"].(map[string]any)
			if got, has := pdf["enabled"]; !has || got != c.wantEnabled {
				t.Errorf("pdfConfig.enabled = %v (present=%v), want %v", got, has, c.wantEnabled)
			}
			got, has := pdf["pdfGenerationRequired"]
			if c.wantRequired == absent {
				if has {
					t.Errorf("pdfGenerationRequired must not be injected next to enabled=false, got %v", got)
				}
				return
			}
			if !has || got != c.wantRequired {
				t.Errorf("pdfConfig.pdfGenerationRequired = %v (present=%v), want %v", got, has, c.wantRequired)
			}
		})
	}
}

func TestApplyAutoEndDefaults_PdfOptOutKeepsBorrowerWiring(t *testing.T) {
	spec := baseSpecWithCustomers(1)
	endTask(spec)["endConfig"] = map[string]any{"pdfConfig": map[string]any{"enabled": false}}

	applyAutoEndDefaults(spec)

	end := endTask(spec)
	im := end["inputMappings"].(map[string]any)
	if im["borrower_id"] != "task_outputs.customer.borrower_id" {
		t.Errorf("borrower_id wiring lost: %v", im["borrower_id"])
	}
	if pdf := end["endConfig"].(map[string]any)["pdfConfig"].(map[string]any); pdf["enabled"] != false {
		t.Errorf("pdfConfig.enabled = %v, want false", pdf["enabled"])
	}
}

func defaultsBlockSpec(end map[string]any, applicant map[string]any) *composeSpec {
	customer := map[string]any{"ref": "applicant", "type": "customer", "label": "Applicant", "operation": "write",
		"key": "person_id", "inputMappings": map[string]any{"person_id": "inputs.person_id"}}
	for k, v := range applicant {
		customer[k] = v
	}
	endNode := map[string]any{"ref": "end", "type": "end", "label": "End"}
	for k, v := range end {
		endNode[k] = v
	}
	return &composeSpec{
		Alias:          "defaults-block",
		Label:          "Defaults block",
		Category:       "EVALUATION",
		InputVariables: map[string]any{"person_id": map[string]any{"type": "string", "required": true}},
		ExtraNodes:     []map[string]any{{"ref": "start", "type": "start", "label": "Start"}},
		Tasks:          []map[string]any{customer, endNode},
		Edges:          []map[string]any{{"from": "start", "to": "applicant"}, {"from": "applicant", "to": "end"}},
	}
}

func composeDefaultsBlock(t *testing.T, spec *composeSpec, autoDefaults bool) (block, stderr string) {
	t.Helper()
	capture := newComposeCapture()
	var err error
	stderr = captureStderr(t, func() {
		_, err = composeWorkflowBody(nil, spec, true, false, true, false, autoDefaults, true, capture)
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	var buf strings.Builder
	printAppliedDefaults(&buf, capture.defaults)
	return buf.String(), stderr
}

func TestAppliedDefaultsBlock_ListsEveryDefaultApplyFilled(t *testing.T) {
	block, _ := composeDefaultsBlock(t, defaultsBlockSpec(nil, nil), true)

	if n := strings.Count(block, "# defaults applied (confirm with the user or set them explicitly):"); n != 1 {
		t.Fatalf("want exactly one heading, got %d:\n%s", n, block)
	}
	for _, want := range []string{
		"#   end: endConfig.pdfConfig.enabled = true -- a PDF report is generated on every run",
		"#   end: endConfig.pdfConfig.pdfGenerationRequired = true -- a failed PDF render fails the run",
		`#   end: inputMappings.borrower_id = "task_outputs.applicant.borrower_id" -- `,
		`#   end: inputMappings.billable_id = "task_outputs.applicant.borrower_id" -- `,
		"#   end: endConfig.decisionConfig.enabled = false (server default) -- no decision is recorded on the execution",
		`#   applicant: persona = "individual" -- `,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "pdfConfig.enabled = true (server default)") {
		t.Errorf("apply filled the PDF itself; it must not be reported twice:\n%s", block)
	}
}

func TestAppliedDefaultsBlock_SilentWhenTheSpecSetsThem(t *testing.T) {
	end := map[string]any{
		"inputMappings": map[string]any{"borrower_id": "task_outputs.applicant.borrower_id", "billable_id": "task_outputs.applicant.borrower_id"},
		"endConfig": map[string]any{
			"pdfConfig":      map[string]any{"enabled": true, "pdfGenerationRequired": false},
			"decisionConfig": map[string]any{"enabled": true, "decisionType": "final"},
		},
	}
	for _, auto := range []bool{true, false} {
		block, stderr := composeDefaultsBlock(t, defaultsBlockSpec(end, map[string]any{"persona": "business"}), auto)
		if block != "" {
			t.Errorf("auto-defaults=%v: block must be silent when the spec sets every field, got:\n%s", auto, block)
		}
		if strings.Contains(stderr, "defaults applied") {
			t.Errorf("auto-defaults=%v: assembly must not print the block itself:\n%s", auto, stderr)
		}
	}
}

func TestAppliedDefaultsBlock_ReportsServerDefaultsWithoutAutoDefaults(t *testing.T) {
	block, _ := composeDefaultsBlock(t, defaultsBlockSpec(map[string]any{
		"endConfig": map[string]any{"decisionConfig": map[string]any{"enabled": true}},
	}, nil), false)

	for _, want := range []string{
		"#   end: endConfig.pdfConfig.enabled = true (server default) -- ",
		`#   end: endConfig.decisionConfig.decisionType = "final" (server default) -- `,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
	for _, absent := range []string{"pdfGenerationRequired", "inputMappings.borrower_id", "decisionConfig.enabled"} {
		if strings.Contains(block, absent) {
			t.Errorf("--no-auto-defaults: %q must not be listed:\n%s", absent, block)
		}
	}
}

func TestCanonicalEndAdvisoryAgreesWithTheDefaultsBlock(t *testing.T) {
	spec := func(pdf map[string]any) *composeSpec {
		endCfg := map[string]any{"decisionConfig": map[string]any{"enabled": true, "decisionType": "final"}}
		if pdf != nil {
			endCfg["pdfConfig"] = pdf
		}
		return &composeSpec{
			Alias:      "advisory-agrees",
			Label:      "Advisory agrees",
			Category:   "EVALUATION",
			ExtraNodes: []map[string]any{{"ref": "start", "type": "start", "label": "Start"}},
			Tasks: []map[string]any{
				{"ref": "policy", "type": "rule-tree", "label": "Policy",
					"ruleTreeConfig": map[string]any{"ruleTreeCode": "policy_tree", "outputVariable": "decision_key"}},
				{"ref": "end", "type": "end", "label": "End",
					"inputMappings": map[string]any{"decision_key": "task_outputs.policy.decision_key"},
					"endConfig":     endCfg},
			},
			Edges: []map[string]any{{"from": "start", "to": "policy"}, {"from": "policy", "to": "end"}},
		}
	}
	const missingPdf = "endConfig.pdfConfig.enabled=true"

	for _, auto := range []bool{true, false} {
		block, stderr := composeDefaultsBlock(t, spec(nil), auto)
		if strings.Contains(stderr, missingPdf) {
			t.Errorf("auto-defaults=%v: advisory calls the PDF missing while the run generates it:\n%s", auto, stderr)
		}
		if !strings.Contains(block, "end: endConfig.pdfConfig.enabled = true") {
			t.Errorf("auto-defaults=%v: block must report the PDF default:\n%s", auto, block)
		}
	}

	block, stderr := composeDefaultsBlock(t, spec(map[string]any{"enabled": false}), true)
	if !strings.Contains(stderr, missingPdf) {
		t.Errorf("an explicit enabled=false is a real gap the advisory must still name:\n%s", stderr)
	}
	if strings.Contains(block, "pdfConfig.enabled") {
		t.Errorf("an explicit enabled=false is not a default:\n%s", block)
	}
}

func TestNormalizeEntityWriteTask_RecordsDealContactDefaults(t *testing.T) {
	var applied appliedDefaults
	task := map[string]any{
		"type": "deal", "specRef": "deal",
		"contacts": []any{map[string]any{"id": "0", "tax_id": "{{inputs.a}}"}},
	}
	if err := normalizeEntityWriteTask(task, &composeNormalizeOpts{AutoDefaults: true, Defaults: &applied}); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	printAppliedDefaults(&buf, applied)
	for _, want := range []string{
		`#   deal: contacts[id=0].identity_key = "tax_id" -- `,
		`#   deal: contacts[id=0].identity_value = "{{inputs.a}}" -- copied from the contact's tax_id`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("block missing %q:\n%s", want, buf.String())
		}
	}
}
