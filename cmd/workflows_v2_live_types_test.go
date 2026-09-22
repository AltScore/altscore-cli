package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

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

func TestPreflightTasks_LiveBackendTypeFallback(t *testing.T) {
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

	fetchLiveTaskTypes = func() map[string]bool {
		return map[string]bool{"http": true, "end": true, "start": true}
	}
	if err := preflightTasks(liveTypeSpec("still-bogus")); err == nil || !strings.Contains(err.Error(), "unknown task type") {
		t.Fatalf("type unknown to a reachable backend must stay fatal, got: %v", err)
	}

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
		if strings.Contains(stderr, "live backend could not be consulted") {
			t.Errorf("%q was routed through the unverified-vocabulary warning: %q", typ, stderr)
		}
	}
}

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
	err := preflightTasks(liveTypeSpec("html-template"))
	if err == nil || !strings.Contains(err.Error(), "no replacement") {
		t.Errorf("html-template has no replacement and the message must say so, got: %v", err)
	}
}

func TestValidateTaskV2BodyStructural_RefusesDeprecatedTypes(t *testing.T) {
	for _, typ := range retiredTaskTypes {
		body := json.RawMessage(`{"type":"` + typ + `","label":"X","alias":"x"}`)
		if err := validateTaskV2BodyStructural(body, nil); err == nil {
			t.Errorf("tasks-v2 create must refuse type %q", typ)
		} else if !strings.Contains(err.Error(), "DEPRECATED") {
			t.Errorf("%q: unexpected refusal message: %v", typ, err)
		}
		if err := validateTaskV2Body(body, nil); err == nil {
			t.Errorf("validateTaskV2Body must refuse type %q", typ)
		}
	}
	if err := validateTaskV2BodyStructural(json.RawMessage(`{"type":"customer","label":"X"}`), nil); err != nil {
		t.Errorf("a current type must still pass the structural validator: %v", err)
	}
}
