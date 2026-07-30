package cmd

import (
	"strings"
	"testing"
)

// A node reads a stored tenant secret by declaring it as a `secret`-typed
// inputSchema field whose `default` is the secretId from the Secrets panel:
//
//	inputSchema: {OPENAI_API_KEY: {type: "secret", default: "<secretId>"}}
//
// borrower-central's resolved_secret_inputs() swaps the secretId for the
// secret's value and merges it into the context the activity receives, so a
// compute-variables expression reads it as a BARE dependency name.
//
// `secret` was missing from validInputSchemaTypes, so composing this shape was
// hard-rejected offline. It must now pass preflight with no fetch and no
// warning. This test fails on the old mirror.
func TestPreflightTasks_SecretTypedInputSchemaAccepted(t *testing.T) {
	fetchLiveInputSchemaTypes = func() map[string]bool {
		t.Fatalf(`"secret" must be compiled-in, not fetched from the backend`)
		return nil
	}
	defer func() {
		fetchLiveInputSchemaTypes = nil
		liveInputSchemaTypes = nil
		liveInputSchemaTypesFetched = false
	}()
	liveInputSchemaTypes = nil
	liveInputSchemaTypesFetched = false

	spec := &composeSpec{
		Label:      "Secret input",
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			{
				"ref":   "compute",
				"type":  "compute-variables",
				"label": "Call OpenAI",
				// The secret field carries no inputMapping on purpose: its value
				// comes from `default` (the secretId), resolved at runtime.
				"inputSchema": map[string]any{
					"OPENAI_API_KEY": map[string]any{
						"type":    "secret",
						"default": "openai-api-key",
					},
					"prompt": map[string]any{"type": "string"},
				},
				"inputMappings":     map[string]any{"prompt": "inputs.prompt"},
				"selectedVariables": []any{"answer"},
			},
			{"ref": "e1", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "compute"},
			{"from": "compute", "to": "e1"},
		},
	}

	var err error
	stderr := captureStderr(t, func() { err = preflightTasks(spec) })
	if err != nil {
		t.Fatalf("secret-typed inputSchema field must pass preflight, got: %v", err)
	}
	if strings.Contains(stderr, "secret") && strings.Contains(stderr, "WARNING") {
		t.Fatalf("secret-typed field must not warn, got: %q", stderr)
	}
}

// Workflow-level inputVariables go through the same type check, so
// `type: secret` must be accepted there too. Note this only makes the value a
// typed input -- nothing in borrower-central dereferences a workflow-level
// secret against the secret store (only the task inputSchema path does), so the
// runtime sees the literal secretId. Accepted here because the backend accepts
// it; the semantics are a separate matter documented in the skill reference.
func TestComposeWorkflowInputVariable_SecretTypeAccepted(t *testing.T) {
	defer func() {
		fetchLiveInputSchemaTypes = nil
		liveInputSchemaTypes = nil
		liveInputSchemaTypesFetched = false
	}()
	liveInputSchemaTypes = nil
	liveInputSchemaTypesFetched = false
	fetchLiveInputSchemaTypes = nil

	if err := checkInputSchemaType("secret", "workflow.inputVariables.api_key.type"); err != nil {
		t.Fatalf("workflow-level secret input must be accepted, got: %v", err)
	}
}
