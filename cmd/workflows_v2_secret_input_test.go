package cmd

import (
	"strings"
	"testing"
)

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
				// No inputMapping on purpose: the value comes from `default` (the secretId), resolved at runtime.
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

// Nothing in BC dereferences a workflow-level secret, so the runtime sees the literal secretId.
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
