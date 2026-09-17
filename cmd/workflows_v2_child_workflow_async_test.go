package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// childTask builds a child-workflow task body. Callers add the async fields.
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

// A spec with no dispatchMode at all is the overwhelming majority of what is
// already published; it must keep passing untouched.
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
	// The message has to name the valid values: "async" is the obvious wrong
	// guess and the author needs to be told the real spelling.
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

// Async dispatch batches over a LIST. Without inputExpression the node resolves
// a dict and fails at runtime, so this is a hard error rather than a warning:
// the author believes they shipped async and would not find out until a run.
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

// maxConcurrency and failurePolicy do not survive into a platform batch. Warn,
// do not reject: the author may be flipping between modes and we must not
// delete their settings (that breaks the apply -> export -> apply round trip).
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
	// The fields must still be on the body afterwards.
	task := spec.Tasks[0]
	if _, ok := task["maxConcurrency"]; !ok {
		t.Fatal("preflight must not delete maxConcurrency")
	}
}

// invalidRowPolicy on an inline node does nothing. Warn so it is not mistaken
// for row validation that inline mode simply does not do.
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

// The structural validator is reached by `tasks-v2 create` / `create-version`,
// which never go through compose preflight, so the same rules live there too.
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
