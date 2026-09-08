package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// liveTypeSpec is a minimal one-node spec, parameterized by that node's type.
func liveTypeSpec(taskType string) *composeSpec {
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

// The compiled-in validTaskTypes map is only a mirror of the backend enum.
// When fetchLiveTaskTypes is wired (composeWorkflowBody does this), preflight
// must accept a type the live backend reports even if this binary predates
// it -- and still reject types unknown to both.
//
// This UNKNOWN-type policy is deliberately NOT the deprecated-type policy
// tested below. The two differ because the CLI's knowledge differs: it cannot
// tell an invalid type from a valid one that is newer than this build without
// asking the backend, so an unverifiable unknown fails open. It needs nobody's
// help to know a type is retired, so a deprecated one fails closed. Keep both
// behaviours; they are not in tension.
func TestPreflightTasks_LiveBackendTypeFallback(t *testing.T) {
	// No hook (unit-test / offline mode): an unverifiable type warns and proceeds
	// rather than hard-blocking. The backend enforces the TaskType enum on write,
	// so a real typo is still rejected there -- and with no --no-preflight flag,
	// rejecting here would leave a stale mirror as an unrecoverable block.
	// A DEPRECATED type is the one exception (see the test below).
	fetchLiveTaskTypes = nil
	var offlineErr error
	stderr := captureStderr(t, func() { offlineErr = preflightTasks(liveTypeSpec("brand-new-node")) })
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
	if err := preflightTasks(liveTypeSpec("brand-new-node")); err != nil {
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
	if err := preflightTasks(liveTypeSpec("still-bogus")); err == nil || !strings.Contains(err.Error(), "unknown task type") {
		t.Fatalf("type unknown to a reachable backend must stay fatal, got: %v", err)
	}

	// Hook errors out (returns nil): unverifiable, so warn + proceed, no crash.
	fetchLiveTaskTypes = func() map[string]bool { return nil }
	var nilErr error
	stderr = captureStderr(t, func() { nilErr = preflightTasks(liveTypeSpec("also-bogus")) })
	if nilErr != nil {
		t.Fatalf("nil live list must warn and proceed, got: %v", nilErr)
	}
	if !strings.Contains(stderr, "live backend could not be consulted") {
		t.Fatalf("nil live list must warn on stderr, got: %q", stderr)
	}
}

// Deprecated types are the deliberate exception to the warn-and-proceed policy
// above, and the refusal is UNCONDITIONAL: with no hook wired there is nothing
// to consult, and the compiled-in deprecatedTaskTypes map is enough on its own.
// This is the offline case a delivery engineer actually hits -- no network, a
// stale binary -- and it is exactly where the type must still be refused.
func TestPreflightTasks_DeprecatedTypeIsFatalWithNoLiveHook(t *testing.T) {
	fetchLiveTaskTypes = nil
	for _, typ := range retiredTaskTypes {
		var err error
		stderr := captureStderr(t, func() { err = preflightTasks(liveTypeSpec(typ)) })
		if err == nil {
			t.Fatalf("%q must be refused with no live hook wired, got nil", typ)
		}
		if !strings.Contains(err.Error(), "DEPRECATED") {
			t.Errorf("%q: refusal must say the type is deprecated, got: %v", typ, err)
		}
		// Never routed through warnUnverifiedVocabularyValue: that path tells
		// the author the value is merely unverified and lets it through.
		if strings.Contains(stderr, "live backend could not be consulted") {
			t.Errorf("%q was routed through the unverified-vocabulary warning: %q", typ, stderr)
		}
	}
}

// Even a backend that still lists the type under `values` does not make it
// authorable: the local refusal comes first and does not defer to the live
// vocabulary. `webhook` is the real case -- BC keeps parsing it.
func TestPreflightTasks_DeprecatedTypeIsFatalEvenWhenLiveBackendListsIt(t *testing.T) {
	fetchLiveTaskTypes = func() map[string]bool {
		return map[string]bool{"webhook": true, "http": true, "end": true, "start": true}
	}
	defer func() { fetchLiveTaskTypes = nil }()
	err := preflightTasks(liveTypeSpec("webhook"))
	if err == nil || !strings.Contains(err.Error(), "DEPRECATED") {
		t.Fatalf("a live-listed deprecated type must still be refused, got: %v", err)
	}
}

// The refusal names what to author instead, per type, so the author does not
// have to go read the schema-guide to find out.
func TestDeprecatedTaskTypeErrorNamesTheReplacement(t *testing.T) {
	fetchLiveTaskTypes = nil
	for typ, want := range map[string]string{
		"create-borrower": `"customer" task with operation=write`,
		"fetch-entity":    `operation=read`,
		"data-store":      `"data-store-write" or "data-store-query"`,
		"pdf-report":      `endConfig on the "end" task`,
		"soap":            `"http" task`,
	} {
		err := preflightTasks(liveTypeSpec(typ))
		if err == nil {
			t.Fatalf("%q must be refused", typ)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q refusal must mention %q, got: %v", typ, want, err)
		}
	}
	// A type with no successor says so rather than inventing one.
	err := preflightTasks(liveTypeSpec("html-template"))
	if err == nil || !strings.Contains(err.Error(), "no replacement") {
		t.Errorf("html-template has no replacement and the message must say so, got: %v", err)
	}
}

// tasks-v2 create / create-version never go through compose, so the structural
// validator is their only gate. Its switch has a case only for types with
// structural rules, which is why this check sits before it.
func TestValidateTaskV2BodyStructural_RefusesDeprecatedTypes(t *testing.T) {
	for _, typ := range retiredTaskTypes {
		body := json.RawMessage(`{"type":"` + typ + `","label":"X","alias":"x"}`)
		if err := validateTaskV2BodyStructural(body, nil); err == nil {
			t.Errorf("tasks-v2 create must refuse type %q", typ)
		} else if !strings.Contains(err.Error(), "DEPRECATED") {
			t.Errorf("%q: unexpected refusal message: %v", typ, err)
		}
		// validateTaskV2Body wraps the structural pass; both create paths use it.
		if err := validateTaskV2Body(body, nil); err == nil {
			t.Errorf("validateTaskV2Body must refuse type %q", typ)
		}
	}
	// A current type with no structural rules still passes.
	if err := validateTaskV2BodyStructural(json.RawMessage(`{"type":"customer","label":"X"}`), nil); err != nil {
		t.Errorf("a current type must still pass the structural validator: %v", err)
	}
}
