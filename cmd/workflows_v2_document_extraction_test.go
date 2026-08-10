package cmd

import (
	"strings"
	"testing"
)

// document-extraction is a backend task type as of borrower-central #1794. It
// must be in the compiled-in mirror so preflight accepts it without a live
// meta round-trip: an unknown-locally type is a HARD error whenever the backend
// is reachable and does not list it, and only warns-and-proceeds when it does.
func TestValidTaskTypes_DocumentExtraction(t *testing.T) {
	if !validTaskTypes["document-extraction"] {
		t.Fatalf("document-extraction must be in validTaskTypes")
	}
}

func docExtractionTask(ref string, cfg map[string]any, mappings map[string]any) map[string]any {
	task := map[string]any{
		"ref":                      ref,
		"type":                     "document-extraction",
		"label":                    "Extract",
		"documentExtractionConfig": cfg,
	}
	if mappings != nil {
		task["inputMappings"] = mappings
	}
	return task
}

func scalarSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"articulo_7_texto": map[string]any{"type": "string"},
		},
	}
}

func docExtractionSpec(task map[string]any) *composeSpec {
	return &composeSpec{
		Label:      "Doc extraction",
		Category:   "OTHER",
		ExtraNodes: startNode,
		Tasks:      []map[string]any{task},
		Edges:      []map[string]any{{"from": "start", "to": task["ref"]}},
	}
}

func TestPreflightTasks_DocumentExtractionAccepted(t *testing.T) {
	spec := docExtractionSpec(docExtractionTask("dx", map[string]any{
		"documentUrl":      "https://example.test/doc.pdf",
		"extractionSchema": scalarSchema(),
	}, nil))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept a well-formed document-extraction task, got: %v", err)
	}
}

// The document source is runtime resolvable, and wiring it through
// inputMappings is the RECOMMENDED shape (it keeps the ref out of the config
// body, which the per-type ref rewriter would never rewrite). Preflight must
// not report that as a missing source.
func TestPreflightTasks_DocumentExtractionSourceFromMapping(t *testing.T) {
	for _, key := range []string{"documentUrl", "document_url"} {
		spec := docExtractionSpec(docExtractionTask("dx", map[string]any{
			"extractionSchema": scalarSchema(),
		}, map[string]any{key: "task_outputs.start.workflow_started"}))
		if err := preflightTasks(spec); err != nil {
			t.Fatalf("preflight should accept a source mapped as %q, got: %v", key, err)
		}
	}
}

func TestPreflightTasks_DocumentExtractionMissingSource(t *testing.T) {
	spec := docExtractionSpec(docExtractionTask("dx", map[string]any{
		"extractionSchema": scalarSchema(),
	}, nil))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject a document-extraction task with no source")
	}
	if !strings.Contains(err.Error(), "exactly one document source") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreflightTasks_DocumentExtractionAmbiguousSource(t *testing.T) {
	// One in the config, one mapped: the count spans both, since the runtime
	// sees a single merged context and raises AMBIGUOUS_DOCUMENT_SOURCE.
	spec := docExtractionSpec(docExtractionTask("dx", map[string]any{
		"documentUrl":      "https://example.test/doc.pdf",
		"extractionSchema": scalarSchema(),
	}, map[string]any{"rawText": "task_outputs.start.workflow_started"}))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject two document sources")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreflightTasks_DocumentExtractionEmptySchema(t *testing.T) {
	// BC validates extractionSchema at RUN time only, so without this check an
	// apply persists the node, returns 201 and fails on the first execution.
	spec := docExtractionSpec(docExtractionTask("dx", map[string]any{
		"documentUrl": "https://example.test/doc.pdf",
	}, nil))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject an absent extractionSchema")
	}
	if !strings.Contains(err.Error(), "extractionSchema") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ocr-tools extraction targets are scalars and list[string] only, so a nested
// shape yields nothing instead of failing loudly. Rejected for that provider
// and accepted for llm, which handles nested objects natively.
func TestPreflightTasks_DocumentExtractionNestedSchemaByProvider(t *testing.T) {
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"articulo_7_texto": map[string]any{"type": "string"},
			"comparecientes": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "object"},
			},
		},
	}

	spec := docExtractionSpec(docExtractionTask("dx", map[string]any{
		"documentUrl":      "https://example.test/doc.pdf",
		"extractionSchema": nested,
		"provider":         "ocr-tools",
	}, nil))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject a nested schema under provider ocr-tools")
	}
	if !strings.Contains(err.Error(), "comparecientes") {
		t.Fatalf("error should name the offending field, got: %v", err)
	}

	spec = docExtractionSpec(docExtractionTask("dx", map[string]any{
		"documentUrl":      "https://example.test/doc.pdf",
		"extractionSchema": nested,
		"provider":         "llm",
	}, nil))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept a nested schema under provider llm, got: %v", err)
	}
}

func TestFirstNestedSchemaProperty(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{"scalars only", scalarSchema(), ""},
		{
			"object property",
			map[string]any{"properties": map[string]any{"a": map[string]any{"type": "object"}}},
			"a",
		},
		{
			"array of objects",
			map[string]any{"properties": map[string]any{
				"a": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			}},
			"a",
		},
		{
			"array of strings is a supported target",
			map[string]any{"properties": map[string]any{
				"a": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			}},
			"",
		},
		{
			// Deterministic: map iteration order must not decide which field
			// the error names.
			"several nested, lowest name wins",
			map[string]any{"properties": map[string]any{
				"zeta":  map[string]any{"type": "object"},
				"alpha": map[string]any{"type": "object"},
			}},
			"alpha",
		},
		{"no properties", map[string]any{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstNestedSchemaProperty(tc.schema); got != tc.want {
				t.Fatalf("firstNestedSchemaProperty = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCamelToSnake(t *testing.T) {
	cases := map[string]string{
		"documentUrl":    "document_url",
		"documentBase64": "document_base64",
		"rawText":        "raw_text",
		"filename":       "filename",
	}
	for in, want := range cases {
		if got := camelToSnake(in); got != want {
			t.Fatalf("camelToSnake(%q) = %q, want %q", in, got, want)
		}
	}
}
