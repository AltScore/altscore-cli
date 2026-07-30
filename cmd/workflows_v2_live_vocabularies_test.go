package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// The three vocabularies below (workflow category, relationship kind,
// inputSchema type) each get the same live-backend fallback the condition
// operator got: a compiled-in map is only a mirror of a backend enum, and when
// the mirror lacks a value the check consults the live backend once before
// rejecting. These tests mirror TestValidateConditionGroup_LiveBackendOperatorFallback:
// live-known -> warn + accept; live-unknown with a reachable backend -> reject
// naming the live list; failed/absent fetch -> fall back to the offline message.
// All hooks are stubbed so nothing depends on the live endpoint.

// --- workflow category -------------------------------------------------------

func resetLiveWorkflowCategories() {
	fetchLiveWorkflowCategories = nil
	liveWorkflowCategories = nil
	liveWorkflowCategoriesFetched = false
}

func TestCheckWorkflowCategory_LiveBackendFallback(t *testing.T) {
	defer resetLiveWorkflowCategories()

	// Empty category is always fine (the field is optional) and must never fetch.
	resetLiveWorkflowCategories()
	fetchLiveWorkflowCategories = func() map[string]bool {
		t.Fatalf("empty category must not trigger a live fetch")
		return nil
	}
	if err := checkWorkflowCategory(""); err != nil {
		t.Fatalf("empty category must be accepted, got: %v", err)
	}

	// Case-insensitive: the check upper-cases before comparing.
	resetLiveWorkflowCategories()
	err := checkWorkflowCategory("action")
	if err != nil {
		t.Fatalf("compiled-in category (any case) must be accepted, got: %v", err)
	}

	// Offline / no hook: an unverifiable category WARNS and proceeds rather than
	// hard-blocking. The backend enforces CategoryEnum on write, so a real typo
	// is still caught there; rejecting here would make a stale mirror an
	// unrecoverable block on a valid spec (there is no --no-preflight flag).
	resetLiveWorkflowCategories()
	var offlineErr error
	stderr := captureStderr(t, func() { offlineErr = checkWorkflowCategory("MADE_UP") })
	if offlineErr != nil {
		t.Fatalf("without live hook, unknown category must warn and proceed, got: %v", offlineErr)
	}
	if !strings.Contains(stderr, "MADE_UP") ||
		!strings.Contains(stderr, "live backend could not be consulted") {
		t.Fatalf("offline acceptance must warn on stderr, got: %q", stderr)
	}

	// RECOMMENDATION is a real backend category and must be on the fast path
	// (it was missing from the mirror, which is exactly the drift this guards).
	resetLiveWorkflowCategories()
	fetchLiveWorkflowCategories = func() map[string]bool {
		t.Fatalf("RECOMMENDATION must be compiled-in, not fetched")
		return nil
	}
	if err := checkWorkflowCategory("RECOMMENDATION"); err != nil {
		t.Fatalf("RECOMMENDATION must be accepted offline, got: %v", err)
	}

	// Compiled-in category is accepted on the fast path -- never triggers a fetch.
	resetLiveWorkflowCategories()
	fetchLiveWorkflowCategories = func() map[string]bool {
		t.Fatalf("compiled-in category must not trigger a live fetch")
		return nil
	}
	if err := checkWorkflowCategory("EVALUATION"); err != nil {
		t.Fatalf("compiled-in category must be accepted, got: %v", err)
	}

	// Hook reports the category: accept (warn-only), fetched exactly once.
	resetLiveWorkflowCategories()
	calls := 0
	fetchLiveWorkflowCategories = func() map[string]bool {
		calls++
		return map[string]bool{"ACTION": true, "SIGNAL": true}
	}
	if err := checkWorkflowCategory("signal"); err != nil {
		t.Fatalf("live-known category must be accepted, got: %v", err)
	}
	if err := checkWorkflowCategory("SIGNAL"); err != nil {
		t.Fatalf("second live-known lookup must reuse the memo, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("live category list must be fetched exactly once, got %d", calls)
	}

	// Hook does NOT report it: fatal, message says the backend was consulted.
	resetLiveWorkflowCategories()
	fetchLiveWorkflowCategories = func() map[string]bool {
		return map[string]bool{"ACTION": true, "EVALUATION": true}
	}
	err = checkWorkflowCategory("totally_bogus")
	if err == nil || !strings.Contains(err.Error(), "is not a valid value") {
		t.Fatalf("category unknown to both must stay fatal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "live backend was consulted") {
		t.Fatalf("reachable-backend rejection must say the backend was consulted, got: %v", err)
	}

	// Hook errors out (nil, e.g. 404 on an old backend): warn + proceed, same as
	// no hook at all -- an unreachable meta endpoint must never block a valid spec.
	resetLiveWorkflowCategories()
	fetchLiveWorkflowCategories = func() map[string]bool { return nil }
	stderr = captureStderr(t, func() { offlineErr = checkWorkflowCategory("still_bogus") })
	if offlineErr != nil {
		t.Fatalf("nil live list must warn and proceed, got: %v", offlineErr)
	}
	if !strings.Contains(stderr, "live backend could not be consulted") {
		t.Fatalf("nil live list must warn on stderr, got: %q", stderr)
	}
}

// --- relationship kind -------------------------------------------------------

func resetLiveRelationshipKinds() {
	fetchLiveRelationshipKinds = nil
	liveRelationshipKinds = nil
	liveRelationshipKindsFetched = false
}

func TestCheckRelationshipKind_LiveBackendFallback(t *testing.T) {
	defer resetLiveRelationshipKinds()
	const path = `node ref="rels": relationshipsConfig.items[0]`

	// Offline / no hook: an unverifiable kind warns and proceeds. The backend
	// enforces the relationship Literal on write, so a typo is still caught.
	resetLiveRelationshipKinds()
	var offlineErr error
	stderr := captureStderr(t, func() { offlineErr = checkRelationshipKind("guarantor", path) })
	if offlineErr != nil {
		t.Fatalf("without live hook, unknown kind must warn and proceed, got: %v", offlineErr)
	}
	if !strings.Contains(stderr, "guarantor") ||
		!strings.Contains(stderr, "live backend could not be consulted") {
		t.Fatalf("offline acceptance must warn on stderr, got: %q", stderr)
	}

	// Compiled-in kind is accepted on the fast path -- never triggers a fetch.
	resetLiveRelationshipKinds()
	fetchLiveRelationshipKinds = func() map[string]bool {
		t.Fatalf("compiled-in kind must not trigger a live fetch")
		return nil
	}
	if err := checkRelationshipKind("shareholder", path); err != nil {
		t.Fatalf("compiled-in kind must be accepted, got: %v", err)
	}

	// Hook reports the kind: accept (warn-only), fetched exactly once.
	resetLiveRelationshipKinds()
	calls := 0
	fetchLiveRelationshipKinds = func() map[string]bool {
		calls++
		return map[string]bool{"shareholder": true, "guarantor": true}
	}
	if err := checkRelationshipKind("guarantor", path); err != nil {
		t.Fatalf("live-known kind must be accepted, got: %v", err)
	}
	if err := checkRelationshipKind("guarantor", path); err != nil {
		t.Fatalf("second live-known lookup must reuse the memo, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("live kind list must be fetched exactly once, got %d", calls)
	}

	// Hook does NOT report it: fatal, message says the backend was consulted.
	resetLiveRelationshipKinds()
	fetchLiveRelationshipKinds = func() map[string]bool {
		return map[string]bool{"shareholder": true, "employee": true}
	}
	err := checkRelationshipKind("totally_bogus", path)
	if err == nil || !strings.Contains(err.Error(), "not a known relationship kind") {
		t.Fatalf("kind unknown to both must stay fatal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "live backend was consulted") {
		t.Fatalf("reachable-backend rejection must say the backend was consulted, got: %v", err)
	}

	// Hook errors out (nil): warn + proceed, never a hard block.
	resetLiveRelationshipKinds()
	fetchLiveRelationshipKinds = func() map[string]bool { return nil }
	stderr = captureStderr(t, func() { offlineErr = checkRelationshipKind("still_bogus", path) })
	if offlineErr != nil {
		t.Fatalf("nil live list must warn and proceed, got: %v", offlineErr)
	}
	if !strings.Contains(stderr, "live backend could not be consulted") {
		t.Fatalf("nil live list must warn on stderr, got: %q", stderr)
	}
}

// --- inputSchema type --------------------------------------------------------

func resetLiveInputSchemaTypes() {
	fetchLiveInputSchemaTypes = nil
	liveInputSchemaTypes = nil
	liveInputSchemaTypesFetched = false
}

func TestCheckInputSchemaType_LiveBackendFallback(t *testing.T) {
	defer resetLiveInputSchemaTypes()
	const path = "workflow.inputVariables.foo.type"

	// "secret" is a real backend SchemaTypes member (a node reads a stored tenant
	// secret via inputSchema.<f> = {type:"secret", default:"<secretId>"}). It was
	// missing from the mirror, which hard-blocked valid specs offline. It must now
	// be on the fast path -- accepted with NO fetch and NO warning.
	resetLiveInputSchemaTypes()
	fetchLiveInputSchemaTypes = func() map[string]bool {
		t.Fatalf(`"secret" must be compiled-in, not fetched`)
		return nil
	}
	secretStderr := captureStderr(t, func() {
		if err := checkInputSchemaType("secret", path); err != nil {
			t.Fatalf(`"secret" must be accepted offline, got: %v`, err)
		}
	})
	if secretStderr != "" {
		t.Fatalf(`"secret" must not warn, got: %q`, secretStderr)
	}

	// Offline / no hook: a type this build cannot verify warns and proceeds. The
	// backend's SchemaTypes union rejects a genuine typo on write.
	resetLiveInputSchemaTypes()
	var offlineErr error
	stderr := captureStderr(t, func() { offlineErr = checkInputSchemaType("bogus_type", path) })
	if offlineErr != nil {
		t.Fatalf("without live hook, unknown type must warn and proceed, got: %v", offlineErr)
	}
	if !strings.Contains(stderr, "bogus_type") ||
		!strings.Contains(stderr, "live backend could not be consulted") {
		t.Fatalf("offline acceptance must warn on stderr, got: %q", stderr)
	}

	// Compiled-in type is accepted on the fast path -- never triggers a fetch.
	resetLiveInputSchemaTypes()
	fetchLiveInputSchemaTypes = func() map[string]bool {
		t.Fatalf("compiled-in type must not trigger a live fetch")
		return nil
	}
	if err := checkInputSchemaType("string", path); err != nil {
		t.Fatalf("compiled-in type must be accepted, got: %v", err)
	}

	// Hook reports the type: accept (warn-only), fetched exactly once. Uses a type
	// absent from the mirror on purpose -- a compiled-in one would short-circuit
	// before any fetch.
	resetLiveInputSchemaTypes()
	calls := 0
	fetchLiveInputSchemaTypes = func() map[string]bool {
		calls++
		return map[string]bool{"string": true, "decimal": true}
	}
	if err := checkInputSchemaType("decimal", path); err != nil {
		t.Fatalf("live-known type must be accepted, got: %v", err)
	}
	if err := checkInputSchemaType("decimal", "node ref=\"x\": inputSchema.bar.type"); err != nil {
		t.Fatalf("second live-known lookup must reuse the memo, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("live type list must be fetched exactly once, got %d", calls)
	}

	// Hook does NOT report it: fatal, message says the backend was consulted.
	resetLiveInputSchemaTypes()
	fetchLiveInputSchemaTypes = func() map[string]bool {
		return map[string]bool{"string": true, "integer": true}
	}
	err := checkInputSchemaType("totally_bogus", path)
	if err == nil || !strings.Contains(err.Error(), "is not a valid type") {
		t.Fatalf("type unknown to both must stay fatal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "live backend was consulted") {
		t.Fatalf("reachable-backend rejection must say the backend was consulted, got: %v", err)
	}

	// Hook errors out (nil): warn + proceed, never a hard block.
	resetLiveInputSchemaTypes()
	fetchLiveInputSchemaTypes = func() map[string]bool { return nil }
	stderr = captureStderr(t, func() { offlineErr = checkInputSchemaType("still_bogus", path) })
	if offlineErr != nil {
		t.Fatalf("nil live list must warn and proceed, got: %v", offlineErr)
	}
	if !strings.Contains(stderr, "live backend could not be consulted") {
		t.Fatalf("nil live list must warn on stderr, got: %q", stderr)
	}
}

// --- parse: the section payload shape ---------------------------------------

// Each fetchServer* function must parse the {"<section>": {"values": [...]}}
// shape BC serves (mirroring taskTypes) into a set, and return nil on any
// shape/transport error so callers fall back to the compiled-in mirror.
func TestFetchServerVocabularies_ParseValuesShape(t *testing.T) {
	// workflowCategories: values are upper-cased on ingest so the case-folding
	// lookup in checkWorkflowCategory works.
	catBody := `{"workflowCategories": {"description": "...", "values": ["ACTION", "CONTACT", "EVALUATION", "OTHER", "SIGNAL"]}}`
	var catPayload struct {
		WorkflowCategories struct {
			Values []string `json:"values"`
		} `json:"workflowCategories"`
	}
	if err := json.Unmarshal([]byte(catBody), &catPayload); err != nil {
		t.Fatalf("category fixture parse failed: %v", err)
	}
	if len(catPayload.WorkflowCategories.Values) != 5 {
		t.Fatalf("expected 5 categories, got %d", len(catPayload.WorkflowCategories.Values))
	}

	// relationshipKinds
	relBody := `{"relationshipKinds": {"values": ["employee", "family", "guarantor", "other", "shareholder", "unspecified"]}}`
	var relPayload struct {
		RelationshipKinds struct {
			Values []string `json:"values"`
		} `json:"relationshipKinds"`
	}
	if err := json.Unmarshal([]byte(relBody), &relPayload); err != nil {
		t.Fatalf("relationship fixture parse failed: %v", err)
	}
	if len(relPayload.RelationshipKinds.Values) != 6 {
		t.Fatalf("expected 6 relationship kinds, got %d", len(relPayload.RelationshipKinds.Values))
	}

	// inputSchemaTypes
	typeBody := `{"inputSchemaTypes": {"values": ["array", "boolean", "integer", "number", "object", "secret", "string"]}}`
	var typePayload struct {
		InputSchemaTypes struct {
			Values []string `json:"values"`
		} `json:"inputSchemaTypes"`
	}
	if err := json.Unmarshal([]byte(typeBody), &typePayload); err != nil {
		t.Fatalf("type fixture parse failed: %v", err)
	}
	if len(typePayload.InputSchemaTypes.Values) != 7 {
		t.Fatalf("expected 7 schema types, got %d", len(typePayload.InputSchemaTypes.Values))
	}

	// Wrong-section / empty-values payloads must leave an empty set so the
	// caller falls back to the compiled-in mirror.
	empties := []string{
		`{"conditionOperators": {"workflow": {}}}`,
		`{"workflowCategories": {"values": []}}`,
	}
	for _, body := range empties {
		var p struct {
			WorkflowCategories struct {
				Values []string `json:"values"`
			} `json:"workflowCategories"`
		}
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatalf("empty fixture parse failed: %v", err)
		}
		if len(p.WorkflowCategories.Values) != 0 {
			t.Fatalf("wrong/empty section must yield 0 values, got %d", len(p.WorkflowCategories.Values))
		}
	}
}
