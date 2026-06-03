package cmd

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// was written. Used to assert the advisory surfaces on stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestAdviseExtractionProbes_SurfacesOnStderr proves the advisory is emitted to
// stderr, names the variable and the selecting compute-variables node, and
// recommends the direct task_outputs.* wire. It also confirms a computed var in
// the same workflow does NOT trigger any advisory line.
func TestAdviseExtractionProbes_SurfacesOnStderr(t *testing.T) {
	cv := map[string]any{
		"scoped_id": map[string]any{
			"expression":  `result = inputs.get("attach-deal.deal_contact_id")`,
			"returnValue": "result",
		},
		"dti": map[string]any{
			"expression":  `result = inputs.get("task_outputs.a.debt") / inputs.get("task_outputs.a.income")`,
			"returnValue": "result",
		},
	}
	nodes := []any{
		map[string]any{"nodeId": "probe", "type": "compute-variables", "selectedVariables": []any{"scoped_id"}},
	}
	out := captureStderr(t, func() { adviseExtractionProbes(cv, nodes) })

	if !strings.Contains(out, `# advisory:`) {
		t.Errorf("expected an advisory line, got: %q", out)
	}
	if !strings.Contains(out, `"scoped_id"`) {
		t.Errorf("advisory should name the probe variable, got: %q", out)
	}
	if !strings.Contains(out, `compute-variables node "probe"`) {
		t.Errorf("advisory should cross-reference the selecting node, got: %q", out)
	}
	if !strings.Contains(out, `task_outputs.attach-deal.deal_contact_id`) {
		t.Errorf("advisory should recommend the direct task_outputs.* wire, got: %q", out)
	}
	if strings.Contains(out, `"dti"`) {
		t.Errorf("computed var 'dti' must NOT trigger an advisory, got: %q", out)
	}
}

// TestAdviseExtractionProbes_NoProbesIsSilent confirms a workflow with no probe
// vars produces no stderr output (so it never clutters clean runs).
func TestAdviseExtractionProbes_NoProbesIsSilent(t *testing.T) {
	cv := map[string]any{
		"dti": map[string]any{
			"expression":  `result = inputs.get("task_outputs.a.debt") / inputs.get("task_outputs.a.income")`,
			"returnValue": "result",
		},
	}
	out := captureStderr(t, func() { adviseExtractionProbes(cv, nil) })
	if out != "" {
		t.Errorf("expected no advisory output, got: %q", out)
	}
}

// TestIsPureExtractionProbe_Positive covers the genuine pass-through extraction
// probes the advisory targets: a single `result = inputs.get("...")` over a
// scoped per-item path, with returnValue "result" and no other computation.
func TestIsPureExtractionProbe_Positive(t *testing.T) {
	cases := []struct {
		name     string
		def      map[string]any
		wantPath string
	}{
		{
			name: "task_outputs path",
			def: map[string]any{
				"expression":  `result = inputs.get("task_outputs.attach-deal.deal_contact_id")`,
				"returnValue": "result",
			},
			wantPath: "task_outputs.attach-deal.deal_contact_id",
		},
		{
			name: "bare-alias shortcut",
			def: map[string]any{
				"expression":  `result = inputs.get("attach-deal.deal_contact_id")`,
				"returnValue": "result",
			},
			wantPath: "attach-deal.deal_contact_id",
		},
		{
			name: "single quotes",
			def: map[string]any{
				"expression":  `result = inputs.get('task_outputs.rel.contact_id')`,
				"returnValue": "result",
			},
			wantPath: "task_outputs.rel.contact_id",
		},
		{
			name: "extra whitespace and trailing semicolon",
			def: map[string]any{
				"expression":  `   result   =   inputs.get( "task_outputs.fetch.borrower_id" ) ;`,
				"returnValue": "result",
			},
			wantPath: "task_outputs.fetch.borrower_id",
		},
		{
			name: "deeply nested path",
			def: map[string]any{
				"expression":  `result = inputs.get("task_outputs.src.a.b.c")`,
				"returnValue": "result",
			},
			wantPath: "task_outputs.src.a.b.c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, ok := isPureExtractionProbe(tc.def)
			if !ok {
				t.Fatalf("expected probe to be flagged, was not")
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
		})
	}
}

// TestIsPureExtractionProbe_Negative covers everything that must NOT be flagged:
// any computation, conditional, multiple statements, a non-"result" returnValue,
// a flat (unscoped) input key, or a normal non-extraction custom var.
func TestIsPureExtractionProbe_Negative(t *testing.T) {
	cases := []struct {
		name string
		def  map[string]any
	}{
		{
			name: "arithmetic on the extracted value",
			def: map[string]any{
				"expression":  `result = inputs.get("task_outputs.fetch.amount") * 1.1`,
				"returnValue": "result",
			},
		},
		{
			name: "comparison",
			def: map[string]any{
				"expression":  `result = inputs.get("task_outputs.fetch.score") > 700`,
				"returnValue": "result",
			},
		},
		{
			name: "conditional expression",
			def: map[string]any{
				"expression":  `result = inputs.get("task_outputs.fetch.x") if inputs.get("task_outputs.fetch.x") else 0`,
				"returnValue": "result",
			},
		},
		{
			name: "two statements",
			def: map[string]any{
				"expression":  "x = inputs.get(\"task_outputs.fetch.x\")\nresult = x + 1",
				"returnValue": "result",
			},
		},
		{
			name: "two statements via semicolon",
			def: map[string]any{
				"expression":  `x = inputs.get("task_outputs.fetch.x"); result = x`,
				"returnValue": "result",
			},
		},
		{
			name: "calls another function",
			def: map[string]any{
				"expression":  `result = float(inputs.get("task_outputs.fetch.x"))`,
				"returnValue": "result",
			},
		},
		{
			name: "nested inputs.get (two extractions added)",
			def: map[string]any{
				"expression":  `result = inputs.get("task_outputs.a.x") + inputs.get("task_outputs.b.y")`,
				"returnValue": "result",
			},
		},
		{
			name: "returnValue is not result",
			def: map[string]any{
				"expression":  `total = inputs.get("task_outputs.fetch.amount")`,
				"returnValue": "total",
			},
		},
		{
			name: "no returnValue",
			def: map[string]any{
				"expression": `result = inputs.get("task_outputs.fetch.amount")`,
			},
		},
		{
			name: "flat input key (no alias.field scope)",
			def: map[string]any{
				"expression":  `result = inputs.get("external_id")`,
				"returnValue": "result",
			},
		},
		{
			name: "normal computed custom var (sum of inputs)",
			def: map[string]any{
				"expression":  `result = inputs.get("task_outputs.a.debt") + inputs.get("task_outputs.a.other_debt")`,
				"returnValue": "result",
			},
		},
		{
			name: "bare literal expression",
			def: map[string]any{
				"expression":  `640`,
				"returnValue": "",
			},
		},
		{
			name:  "empty def",
			def:   map[string]any{},
		},
		{
			name:  "nil def",
			def:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if path, ok := isPureExtractionProbe(tc.def); ok {
				t.Errorf("expected NOT flagged, but was flagged with path %q", path)
			}
		})
	}
}

// TestFindExtractionProbeVars verifies the map-level scan returns only the
// probe variables, leaving computed ones alone, and tolerates malformed entries.
func TestFindExtractionProbeVars(t *testing.T) {
	cv := map[string]any{
		"scoped_id": map[string]any{
			"expression":  `result = inputs.get("task_outputs.attach-deal.deal_contact_id")`,
			"returnValue": "result",
		},
		"dti": map[string]any{
			"expression":  `result = inputs.get("task_outputs.a.debt") / inputs.get("task_outputs.a.income")`,
			"returnValue": "result",
		},
		"not_an_object": "garbage",
	}
	got := findExtractionProbeVars(cv)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 probe, got %d: %v", len(got), got)
	}
	if got["scoped_id"] != "task_outputs.attach-deal.deal_contact_id" {
		t.Errorf("scoped_id path = %q, want task_outputs.attach-deal.deal_contact_id", got["scoped_id"])
	}
	if _, ok := got["dti"]; ok {
		t.Errorf("computed var 'dti' must not be flagged")
	}
}

// TestComputeVarSelectors cross-references which compute-variables node selects
// a variable, across both fetched (nodeId) and spec (ref) node shapes.
func TestComputeVarSelectors(t *testing.T) {
	nodes := []any{
		map[string]any{
			"nodeId":            "probe",
			"type":              "compute-variables",
			"selectedVariables": []any{"scoped_id"},
		},
		map[string]any{
			"ref":  "score",
			"type": "scorecard",
		},
	}
	got := computeVarSelectors(nodes)
	if got["scoped_id"] != "probe" {
		t.Errorf("selector for scoped_id = %q, want probe", got["scoped_id"])
	}

	// spec shape uses ref instead of nodeId.
	specNodes := []any{
		map[string]any{
			"ref":               "probe2",
			"type":              "compute-variables",
			"selectedVariables": []any{"scoped_other"},
		},
	}
	got2 := computeVarSelectors(specNodes)
	if got2["scoped_other"] != "probe2" {
		t.Errorf("selector for scoped_other = %q, want probe2", got2["scoped_other"])
	}
}
