package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func resetLiveConditionOperators() {
	fetchLiveConditionOperators = nil
	liveConditionOperators = nil
	liveConditionOperatorsFetched = false
}

func condGroupWith(operator string) map[string]any {
	return map[string]any{
		"operator": "AND",
		"items": []any{
			map[string]any{"field": "score", "operator": operator, "value": "700"},
			map[string]any{"field": "risk", "operator": operator, "value": "5"},
		},
	}
}

func TestValidateConditionGroup_LiveBackendOperatorFallback(t *testing.T) {
	defer resetLiveConditionOperators()

	resetLiveConditionOperators()
	err := validateConditionGroup(condGroupWith("bigger_than_ish"), "branches[0].conditions")
	if err == nil || !strings.Contains(err.Error(), "not a known condition operator") {
		t.Fatalf("without live hook, unknown operator must be fatal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "offline or an older backend") {
		t.Fatalf("offline rejection must cite the offline fallback, got: %v", err)
	}

	resetLiveConditionOperators()
	fetchLiveConditionOperators = func() map[string]bool {
		t.Fatalf("compiled-in operator must not trigger a live fetch")
		return nil
	}
	if err := validateConditionGroup(condGroupWith("gt"), "branches[0].conditions"); err != nil {
		t.Fatalf("compiled-in operator must be accepted, got: %v", err)
	}

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

func TestFetchServerConditionOperators_Parse(t *testing.T) {
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
	if len(got) != 8 {
		t.Fatalf("expected 8 flattened strings from the workflow subsection, got %d: %v", len(got), sortedBoolMapKeys(got))
	}
}

func TestConditionOperators_MirrorCarriesTheArrayFamily(t *testing.T) {
	defer resetLiveConditionOperators()
	resetLiveConditionOperators()

	fetchLiveConditionOperators = func() map[string]bool {
		t.Fatalf("compiled-in operator must not trigger a live fetch")
		return nil
	}
	for _, operator := range []string{
		"array_contains_any", "arrayContainsAny",
		"array_contains_none", "arrayContainsNone",
		"array_contains_all", "arrayContainsAll",
	} {
		if err := validateConditionGroup(condGroupWith(operator), "branches[0].conditions"); err != nil {
			t.Fatalf("%s must validate offline from the mirror, got: %v", operator, err)
		}
	}
}
