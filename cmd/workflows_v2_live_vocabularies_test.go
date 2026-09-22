package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func resetLiveWorkflowCategories() {
	fetchLiveWorkflowCategories = nil
	liveWorkflowCategories = nil
	liveWorkflowCategoriesFetched = false
}

func TestCheckWorkflowCategory_LiveBackendFallback(t *testing.T) {
	defer resetLiveWorkflowCategories()

	resetLiveWorkflowCategories()
	fetchLiveWorkflowCategories = func() map[string]bool {
		t.Fatalf("empty category must not trigger a live fetch")
		return nil
	}
	if err := checkWorkflowCategory(""); err != nil {
		t.Fatalf("empty category must be accepted, got: %v", err)
	}

	resetLiveWorkflowCategories()
	err := checkWorkflowCategory("action")
	if err != nil {
		t.Fatalf("compiled-in category (any case) must be accepted, got: %v", err)
	}

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

	resetLiveWorkflowCategories()
	fetchLiveWorkflowCategories = func() map[string]bool {
		t.Fatalf("RECOMMENDATION must be compiled-in, not fetched")
		return nil
	}
	if err := checkWorkflowCategory("RECOMMENDATION"); err != nil {
		t.Fatalf("RECOMMENDATION must be accepted offline, got: %v", err)
	}

	resetLiveWorkflowCategories()
	fetchLiveWorkflowCategories = func() map[string]bool {
		t.Fatalf("compiled-in category must not trigger a live fetch")
		return nil
	}
	if err := checkWorkflowCategory("EVALUATION"); err != nil {
		t.Fatalf("compiled-in category must be accepted, got: %v", err)
	}

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

func resetLiveRelationshipKinds() {
	fetchLiveRelationshipKinds = nil
	liveRelationshipKinds = nil
	liveRelationshipKindsFetched = false
}

func TestCheckRelationshipKind_LiveBackendFallback(t *testing.T) {
	defer resetLiveRelationshipKinds()
	const path = `node ref="rels": relationshipsConfig.items[0]`

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

	resetLiveRelationshipKinds()
	fetchLiveRelationshipKinds = func() map[string]bool {
		t.Fatalf("compiled-in kind must not trigger a live fetch")
		return nil
	}
	if err := checkRelationshipKind("shareholder", path); err != nil {
		t.Fatalf("compiled-in kind must be accepted, got: %v", err)
	}

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

func resetLiveInputSchemaTypes() {
	fetchLiveInputSchemaTypes = nil
	liveInputSchemaTypes = nil
	liveInputSchemaTypesFetched = false
}

func TestCheckInputSchemaType_LiveBackendFallback(t *testing.T) {
	defer resetLiveInputSchemaTypes()
	const path = "workflow.inputVariables.foo.type"

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

	resetLiveInputSchemaTypes()
	fetchLiveInputSchemaTypes = func() map[string]bool {
		t.Fatalf("compiled-in type must not trigger a live fetch")
		return nil
	}
	if err := checkInputSchemaType("string", path); err != nil {
		t.Fatalf("compiled-in type must be accepted, got: %v", err)
	}

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

func TestFetchServerVocabularies_ParseValuesShape(t *testing.T) {
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
