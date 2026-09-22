package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func childTask(ref string, extra map[string]any) map[string]any {
	t := map[string]any{
		"ref":        ref,
		"type":       "child-workflow",
		"label":      "Subflujo",
		"executorId": "preaprobacion-v2",
	}
	for k, v := range extra {
		t[k] = v
	}
	return t
}

func childSpec(task map[string]any) *composeSpec {
	return &composeSpec{
		Label:      "HQ-1719 async subflow",
		Category:   "OTHER",
		ExtraNodes: startNode,
		Tasks:      []map[string]any{task},
		Edges:      []map[string]any{{"from": "start", "to": task["ref"]}},
	}
}

func TestPreflight_ChildWorkflowWithoutDispatchModeIsUnchanged(t *testing.T) {
	spec := childSpec(childTask("sub", map[string]any{
		"runInBatch":      true,
		"inputExpression": "task_outputs.build.rows",
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestPreflight_ChildWorkflowAsyncBatchPasses(t *testing.T) {
	spec := childSpec(childTask("sub", map[string]any{
		"dispatchMode":     "async-batch",
		"invalidRowPolicy": "skip",
		"inputExpression":  "task_outputs.build.rows",
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestPreflight_ChildWorkflowRejectsUnknownDispatchMode(t *testing.T) {
	spec := childSpec(childTask("sub", map[string]any{
		"dispatchMode":    "async",
		"inputExpression": "task_outputs.build.rows",
	}))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatal("expected an error for dispatchMode=async")
	}
	if !strings.Contains(err.Error(), "async-batch") || !strings.Contains(err.Error(), "inline") {
		t.Fatalf("error should name both valid modes, got %v", err)
	}
}

func TestPreflight_ChildWorkflowRejectsUnknownInvalidRowPolicy(t *testing.T) {
	spec := childSpec(childTask("sub", map[string]any{
		"dispatchMode":     "async-batch",
		"invalidRowPolicy": "ignore",
		"inputExpression":  "task_outputs.build.rows",
	}))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatal("expected an error for invalidRowPolicy=ignore")
	}
	if !strings.Contains(err.Error(), "fail") || !strings.Contains(err.Error(), "skip") {
		t.Fatalf("error should name both valid policies, got %v", err)
	}
}

func TestPreflight_ChildWorkflowAsyncBatchRequiresInputExpression(t *testing.T) {
	spec := childSpec(childTask("sub", map[string]any{
		"dispatchMode": "async-batch",
	}))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatal("expected an error for async-batch without inputExpression")
	}
	if !strings.Contains(err.Error(), "inputExpression") {
		t.Fatalf("error should name inputExpression, got %v", err)
	}
}

func TestPreflight_ChildWorkflowAsyncBatchWarnsOnInertFields(t *testing.T) {
	spec := childSpec(childTask("sub", map[string]any{
		"dispatchMode":    "async-batch",
		"inputExpression": "task_outputs.build.rows",
		"maxConcurrency":  25,
		"failurePolicy":   "fail-fast",
	}))
	stderr := captureStderr(t, func() {
		if err := preflightTasks(spec); err != nil {
			t.Fatalf("inert fields must warn, not fail: %v", err)
		}
	})
	if !strings.Contains(stderr, "maxConcurrency") || !strings.Contains(stderr, "failurePolicy") {
		t.Fatalf("expected a warning naming both inert fields, got %q", stderr)
	}
	task := spec.Tasks[0]
	if _, ok := task["maxConcurrency"]; !ok {
		t.Fatal("preflight must not delete maxConcurrency")
	}
}

func TestPreflight_ChildWorkflowWarnsInvalidRowPolicyOutsideAsync(t *testing.T) {
	spec := childSpec(childTask("sub", map[string]any{
		"invalidRowPolicy": "skip",
		"runInBatch":       true,
		"inputExpression":  "task_outputs.build.rows",
	}))
	stderr := captureStderr(t, func() {
		if err := preflightTasks(spec); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})
	if !strings.Contains(stderr, "invalidRowPolicy") {
		t.Fatalf("expected a warning about invalidRowPolicy, got %q", stderr)
	}
}

func TestStructural_ChildWorkflowAsyncRules(t *testing.T) {
	cases := []struct {
		name    string
		task    map[string]any
		wantErr string
	}{
		{
			name:    "unknown dispatchMode",
			task:    map[string]any{"type": "child-workflow", "dispatchMode": "nope", "inputExpression": "inputs.rows"},
			wantErr: "dispatchMode",
		},
		{
			name:    "unknown invalidRowPolicy",
			task:    map[string]any{"type": "child-workflow", "invalidRowPolicy": "nope"},
			wantErr: "invalidRowPolicy",
		},
		{
			name:    "async without inputExpression",
			task:    map[string]any{"type": "child-workflow", "dispatchMode": "async-batch"},
			wantErr: "inputExpression",
		},
		{
			name: "valid async",
			task: map[string]any{
				"type": "child-workflow", "dispatchMode": "async-batch",
				"invalidRowPolicy": "fail", "inputExpression": "inputs.rows",
			},
			wantErr: "",
		},
		{
			name:    "no dispatchMode at all still fine",
			task:    map[string]any{"type": "child-workflow"},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, marshalErr := json.Marshal(tc.task)
			if marshalErr != nil {
				t.Fatalf("marshal: %v", marshalErr)
			}
			err := validateTaskV2BodyStructural(body, nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error naming %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error should name %q, got %v", tc.wantErr, err)
			}
		})
	}
}
