package cmd

import (
	"strings"
	"testing"
)

// computeSpec builds a minimal start -> compute -> end spec whose compute node
// carries the given inputMappings.
func computeMappingSpec(mappings map[string]any) *composeSpec {
	return &composeSpec{
		Label:      "Compute mappings",
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			{"ref": "fetch", "type": "http", "label": "Fetch", "method": "GET", "url": "https://example.test/x"},
			{
				"ref":               "compute",
				"type":              "compute-variables",
				"label":             "Indicadores",
				"selectedVariables": []any{"judicial_process_indicator"},
				"inputMappings":     mappings,
			},
			{"ref": "e1", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "fetch"},
			{"from": "fetch", "to": "compute"},
			{"from": "compute", "to": "e1"},
		},
	}
}

// A compute-variables node's inputMappings ARE readable by its own custom
// variables as bare dependencies. The CLI used to warn that they are "NOT
// visible to the expression DSL", which is false and is the sentence that talks
// an author out of the only shape that reads an entity field -- or that lets one
// variable be shared by four nodes reading four different sources.
//
// So the three real shapes must now compose in silence.
func TestPreflightTasks_ComputeMappingsToRealReferencesDoNotWarn(t *testing.T) {
	for _, mapping := range []map[string]any{
		{"lookup_tax_id": "entity.borrower.identities.tax_id"},
		{"src": "task_outputs.fetch.ECU-PUB-0023.sourceData"},
		{"authorization": "inputs.bureau_authorization"},
	} {
		var err error
		stderr := captureStderr(t, func() { err = preflightTasks(computeMappingSpec(mapping)) })
		if err != nil {
			t.Fatalf("preflight must accept %v, got: %v", mapping, err)
		}
		if strings.Contains(stderr, "compute-variables inputMappings") {
			t.Errorf("mapping %v must not warn, got: %q", mapping, stderr)
		}
		if strings.Contains(stderr, "NOT visible to the expression DSL") {
			t.Errorf("the retired false warning came back for %v: %q", mapping, stderr)
		}
	}
}

// `__static__::<json>` carries no dot and DOES resolve: `resolve` handles it
// before `_split_reference`, `task_schemas` accepts it explicitly as the literal
// escape, and the Hub generates it. A per-node constant is exactly the
// shared-variable case this change exists for, so warning on it would be a
// second false warning in the place the first one was removed from.
func TestPreflightTasks_StaticLiteralMappingDoesNotWarn(t *testing.T) {
	for _, literal := range []string{`__static__::true`, `__static__::"CONYUGE"`, `__static__::5`} {
		stderr := captureStderr(t, func() {
			_ = preflightTasks(computeMappingSpec(map[string]any{"flag": literal}))
		})
		if strings.Contains(stderr, "compute-variables inputMappings") {
			t.Errorf("%s must not warn, got: %q", literal, stderr)
		}
	}
}

// The lenient bare-alias form resolves too (`ScopedWorkflowContext.resolve`
// rewrites `<alias>.<field>` to `task_outputs.<alias>.<field>`), and apply emits
// it deliberately. It carries a dot so the dotless rule already covers it --
// pinned anyway, because it is the form most likely to look wrong to a reader.
func TestPreflightTasks_BareAliasMappingDoesNotWarn(t *testing.T) {
	stderr := captureStderr(t, func() {
		_ = preflightTasks(computeMappingSpec(map[string]any{"src": "fetch.ECU-PUB-0023.sourceData"}))
	})
	if strings.Contains(stderr, "compute-variables inputMappings") {
		t.Errorf("bare-alias form must not warn, got: %q", stderr)
	}
}

// The shape that IS dead, and that the old blanket warning buried in noise: a
// value naming no namespace. ScopedWorkflowContext._split_reference returns an
// empty root_key for a dotless reference and resolve() returns None without
// raising, so the key never reaches resolvedInputs. The mapping-namespace check
// skips these (`dot <= 0` continues), so this warning is the only thing that
// reports them.
func TestPreflightTasks_DotlessComputeMappingWarns(t *testing.T) {
	var err error
	stderr := captureStderr(t, func() {
		err = preflightTasks(computeMappingSpec(map[string]any{"tax_idd": "tax_idd"}))
	})
	if err != nil {
		t.Fatalf("a dotless mapping is a warning, not a hard error, got: %v", err)
	}
	for _, want := range []string{"compute-variables inputMappings", "tax_idd", "names no namespace"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("warning must mention %q, got: %q", want, stderr)
		}
	}
}

// Map iteration order is random in Go, so a node with several dead mappings
// would otherwise emit them in a different order on every run -- which turns a
// diffable preflight into noise.
func TestPreflightTasks_DeadComputeMappingsWarnInSortedOrder(t *testing.T) {
	stderr := captureStderr(t, func() {
		_ = preflightTasks(computeMappingSpec(map[string]any{
			"zebra": "zebra", "alpha": "alpha", "mango": "mango",
		}))
	})
	a, m, z := strings.Index(stderr, "alpha"), strings.Index(stderr, "mango"), strings.Index(stderr, "zebra")
	if a < 0 || m < 0 || z < 0 {
		t.Fatalf("all three dead mappings must be reported, got: %q", stderr)
	}
	if !(a < m && m < z) {
		t.Errorf("warnings must be sorted by key, got: %q", stderr)
	}
}
