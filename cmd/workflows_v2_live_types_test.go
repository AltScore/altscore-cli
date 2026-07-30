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

	// No hook (unit-test / offline mode): an unverifiable type warns and proceeds
	// rather than hard-blocking. The backend enforces the TaskType enum on write,
	// so a real typo is still rejected there -- and with no --no-preflight flag,
	// rejecting here would leave a stale mirror as an unrecoverable block.
	fetchLiveTaskTypes = nil
	var offlineErr error
	stderr := captureStderr(t, func() { offlineErr = preflightTasks(spec("brand-new-node")) })
	if offlineErr != nil {
		t.Fatalf("without live hook, unknown type must warn and proceed, got: %v", offlineErr)
	}
	if !strings.Contains(stderr, "brand-new-node") ||
		!strings.Contains(stderr, "live backend could not be consulted") {
		t.Fatalf("offline acceptance must warn on stderr, got: %q", stderr)
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

	// Hook returns a real vocabulary that does NOT list the type: the backend is
	// reachable and disowns it, so preflight has authority and stays fatal. A
	// populated map is what a reachable backend actually yields --
	// fetchServerTaskTypes returns nil, not an empty map, when the list is empty.
	fetchLiveTaskTypes = func() map[string]bool {
		return map[string]bool{"http": true, "end": true, "start": true}
	}
	if err := preflightTasks(spec("still-bogus")); err == nil || !strings.Contains(err.Error(), "unknown task type") {
		t.Fatalf("type unknown to a reachable backend must stay fatal, got: %v", err)
	}

	// Hook errors out (returns nil): unverifiable, so warn + proceed, no crash.
	fetchLiveTaskTypes = func() map[string]bool { return nil }
	var nilErr error
	stderr = captureStderr(t, func() { nilErr = preflightTasks(spec("also-bogus")) })
	if nilErr != nil {
		t.Fatalf("nil live list must warn and proceed, got: %v", nilErr)
	}
	if !strings.Contains(stderr, "live backend could not be consulted") {
		t.Fatalf("nil live list must warn on stderr, got: %q", stderr)
	}
}
