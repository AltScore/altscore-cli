package cmd

import (
	"strings"
	"testing"
)

// The compiled-in validTaskTypes map is only a mirror of the backend enum.
// When fetchLiveTaskTypes is wired (composeWorkflowBody does this), preflight
// must accept a type the live backend reports even if this binary predates
// it -- and still reject types unknown to both.
func TestPreflightTasks_LiveBackendTypeFallback(t *testing.T) {
	spec := func(taskType string) *composeSpec {
		return &composeSpec{
			Label:      "Live type",
			Category:   "EVALUATION",
			ExtraNodes: startNode,
			Tasks: []map[string]any{
				{"ref": "n1", "type": taskType, "label": "Newer node"},
				{"ref": "e1", "type": "end", "label": "End"},
			},
			Edges: []map[string]any{{"from": "start", "to": "n1"}, {"from": "n1", "to": "e1"}},
		}
	}

	// No hook (unit-test / offline mode): unknown type stays fatal.
	fetchLiveTaskTypes = nil
	if err := preflightTasks(spec("brand-new-node")); err == nil || !strings.Contains(err.Error(), "unknown task type") {
		t.Fatalf("without live hook, unknown type must be fatal, got: %v", err)
	}

	// Hook reports the type: preflight accepts (warn-only).
	calls := 0
	fetchLiveTaskTypes = func() map[string]bool {
		calls++
		return map[string]bool{"brand-new-node": true}
	}
	defer func() { fetchLiveTaskTypes = nil }()
	if err := preflightTasks(spec("brand-new-node")); err != nil {
		t.Fatalf("live-known type must be accepted, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("live list must be fetched exactly once, got %d", calls)
	}

	// Hook does NOT report the type: fatal as before.
	fetchLiveTaskTypes = func() map[string]bool { return map[string]bool{} }
	if err := preflightTasks(spec("still-bogus")); err == nil || !strings.Contains(err.Error(), "unknown task type") {
		t.Fatalf("type unknown to both must stay fatal, got: %v", err)
	}

	// Hook errors out (returns nil): fatal as before, no crash.
	fetchLiveTaskTypes = func() map[string]bool { return nil }
	if err := preflightTasks(spec("also-bogus")); err == nil || !strings.Contains(err.Error(), "unknown task type") {
		t.Fatalf("nil live list must fall back to fatal, got: %v", err)
	}
}
