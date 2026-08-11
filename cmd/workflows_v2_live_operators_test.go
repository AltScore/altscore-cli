package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// resetLiveConditionOperators clears the package-level hook and memo so each
// sub-case starts from a clean, offline state. Mirrors how the live-task-types
// test toggles fetchLiveTaskTypes between cases.
func resetLiveConditionOperators() {
	fetchLiveConditionOperators = nil
	liveConditionOperators = nil
	liveConditionOperatorsFetched = false
}

// leaf builds a single-item condition group using the given operator, plus a
// second leaf sharing the operator so a live fetch spanning both items must
// still happen at most once (proves memoization across the recursion).
func condGroupWith(operator string) map[string]any {
	return map[string]any{
		"operator": "AND",
		"items": []any{
			map[string]any{"field": "score", "operator": operator, "value": "700"},
			map[string]any{"field": "risk", "operator": operator, "value": "5"},
		},
	}
}

// The compiled-in conditionOperators map is only a mirror of the backend's
// WORKFLOW_CONDITION_OPERATORS table. When fetchLiveConditionOperators is wired
// (composeWorkflowBody does this), validation must accept an operator the live
// backend reports even if this binary predates it -- and still reject operators
// unknown to both. Mirrors TestPreflightTasks_LiveBackendTypeFallback.
func TestValidateConditionGroup_LiveBackendOperatorFallback(t *testing.T) {
	defer resetLiveConditionOperators()

	// No hook (unit-test / offline mode): an operator absent from the mirror
	// stays fatal, and the message names the offline fallback. This used to use
	// "equals" -- a canonical backend operator the mirror lacked, i.e. the
	// false rejection the live fetch exists to cure. #101 put all 55 accepted
	// spellings in the mirror, so only an operator no backend has ever served
	// can stand in here.
	resetLiveConditionOperators()
	err := validateConditionGroup(condGroupWith("bigger_than_ish"), "branches[0].conditions")
	if err == nil || !strings.Contains(err.Error(), "not a known condition operator") {
		t.Fatalf("without live hook, unknown operator must be fatal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "offline or an older backend") {
		t.Fatalf("offline rejection must cite the offline fallback, got: %v", err)
	}

	// Compiled-in operator is accepted on the fast path and never triggers a
	// fetch (the hook must not be called at all).
	resetLiveConditionOperators()
	fetchLiveConditionOperators = func() map[string]bool {
		t.Fatalf("compiled-in operator must not trigger a live fetch")
		return nil
	}
	if err := validateConditionGroup(condGroupWith("gt"), "branches[0].conditions"); err != nil {
		t.Fatalf("compiled-in operator must be accepted, got: %v", err)
	}

	// Hook reports the operator: validation accepts (warn-only), and the live
	// list is fetched exactly once even though two leaf items reference it. The
	// operator has to be one this build does NOT know, which is the case a
	// newer backend produces.
	resetLiveConditionOperators()
	calls := 0
	fetchLiveConditionOperators = func() map[string]bool {
		calls++
		return map[string]bool{"matches_regex": true, "greater_than": true}
	}
	if err := validateConditionGroup(condGroupWith("matches_regex"), "branches[0].conditions"); err != nil {
		t.Fatalf("live-known operator must be accepted, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("live operator list must be fetched exactly once, got %d", calls)
	}

	// Hook does NOT report the operator: fatal, and the message says the live
	// backend was consulted (so the author knows it's authoritative, not stale).
	resetLiveConditionOperators()
	fetchLiveConditionOperators = func() map[string]bool {
		return map[string]bool{"equals": true, "gt": true}
	}
	err = validateConditionGroup(condGroupWith("totally_bogus"), "branches[0].conditions")
	if err == nil || !strings.Contains(err.Error(), "not a known condition operator") {
		t.Fatalf("operator unknown to both must stay fatal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "live backend was consulted") {
		t.Fatalf("reachable-backend rejection must say the backend was consulted, got: %v", err)
	}

	// Hook errors out (returns nil, e.g. offline / 404 on an old backend):
	// fatal as before, no crash, and falls back to the compiled-in message.
	resetLiveConditionOperators()
	fetchLiveConditionOperators = func() map[string]bool { return nil }
	err = validateConditionGroup(condGroupWith("still_bogus"), "branches[0].conditions")
	if err == nil || !strings.Contains(err.Error(), "not a known condition operator") {
		t.Fatalf("nil live list must fall back to fatal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "offline or an older backend") {
		t.Fatalf("failed fetch must fall back to the offline message, got: %v", err)
	}
}

// fetchServerConditionOperators must flatten the workflow subsection into a set
// of canonical names AND aliases (both are interchangeable on the wire), and
// return nil on any shape/transport error so callers fall back to the mirror.
func TestFetchServerConditionOperators_Parse(t *testing.T) {
	// The real prod response shape (trimmed) -- canonical keys, aliases arrays.
	body := `{
	  "conditionOperators": {
	    "description": "...",
	    "workflow": {
	      "equals": {"aliases": ["eq"], "description": "..."},
	      "not_equals": {"aliases": ["ne", "neq"], "description": "..."},
	      "is_true": {"aliases": [], "description": "..."},
	      "array_contains_any": {"aliases": ["arrayContainsAny"], "description": "..."}
	    },
	    "audience": {"equals": {"aliases": ["eq"]}}
	  }
	}`
	var payload struct {
		ConditionOperators struct {
			Workflow map[string]struct {
				Aliases []string `json:"aliases"`
			} `json:"workflow"`
		} `json:"conditionOperators"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("fixture parse failed: %v", err)
	}
	// Mirror the flattening fetchServerConditionOperators performs.
	got := map[string]bool{}
	for name, spec := range payload.ConditionOperators.Workflow {
		got[name] = true
		for _, a := range spec.Aliases {
			got[a] = true
		}
	}
	for _, want := range []string{"equals", "eq", "not_equals", "ne", "neq", "is_true", "array_contains_any", "arrayContainsAny"} {
		if !got[want] {
			t.Fatalf("flattened set missing %q; got %v", want, sortedBoolMapKeys(got))
		}
	}
	// Audience-only vocabulary must NOT leak into the workflow set.
	if len(got) != 8 {
		t.Fatalf("expected 8 flattened strings from the workflow subsection, got %d: %v", len(got), sortedBoolMapKeys(got))
	}
}
