package cmd

import (
	"reflect"
	"testing"
)

func TestDroppedSpecFields_DetectsSilentDrop(t *testing.T) {
	// The canonical bug: spec sets `contacts`, backend persists everything
	// else but silently drops `contacts`.
	specNode := map[string]any{
		"ref":           "create-borrower",
		"type":          "customer",
		"label":         "Create borrower",
		"operation":     "write",
		"contacts":      []any{map[string]any{"email": "a@b.com"}},
		"specRef":       "create-borrower",
		"workflowAlias": "wf",
	}
	persisted := map[string]any{
		"type":      "customer",
		"label":     "Create borrower",
		"operation": "write",
		// contacts dropped by the server
	}
	got := droppedSpecFields(specNode, persisted)
	want := []string{"contacts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("droppedSpecFields = %v, want %v", got, want)
	}
}

func TestDroppedSpecFields_NullCountsAsDropped(t *testing.T) {
	specNode := map[string]any{
		"type":          "http",
		"label":         "Call",
		"url":           "https://x",
		"someConfig":    map[string]any{"k": 1},
	}
	persisted := map[string]any{
		"url":        "https://x",
		"someConfig": nil, // present but nulled
	}
	got := droppedSpecFields(specNode, persisted)
	want := []string{"someConfig"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDroppedSpecFields_NoFalsePositives(t *testing.T) {
	specNode := map[string]any{
		"ref":           "fetch",
		"type":          "altdata-enrichment",
		"label":         "Fetch",
		"sourcesConfig": []any{map[string]any{"sourceId": "X"}},
		"emptyStr":      "",            // empty -> not verified
		"emptyArr":      []any{},       // empty -> not verified
		"emptyMap":      map[string]any{}, // empty -> not verified
		"position":      map[string]any{"x": 1.0}, // excluded
		"data":          map[string]any{"inputMappings": map[string]any{}}, // excluded
	}
	persisted := map[string]any{
		"type":          "altdata-enrichment",
		"label":         "Fetch",
		"sourcesConfig": []any{map[string]any{"sourceId": "X"}},
		// server added its own defaults, dropped nothing the spec set
		"version": 1,
	}
	got := droppedSpecFields(specNode, persisted)
	if len(got) != 0 {
		t.Fatalf("expected no dropped fields, got %v", got)
	}
}

func TestCoerceNodeSlice(t *testing.T) {
	asMaps := []map[string]any{{"taskAlias": "a"}}
	if got := coerceNodeSlice(asMaps); len(got) != 1 || got[0]["taskAlias"] != "a" {
		t.Errorf("[]map path: %v", got)
	}
	asAny := []any{map[string]any{"taskAlias": "b"}, "not-a-map"}
	if got := coerceNodeSlice(asAny); len(got) != 1 || got[0]["taskAlias"] != "b" {
		t.Errorf("[]any path: %v", got)
	}
	if got := coerceNodeSlice(nil); got != nil {
		t.Errorf("nil path: %v", got)
	}
}

func TestIsEmptyVerifyValue(t *testing.T) {
	cases := []struct {
		v    any
		want bool
	}{
		{nil, true},
		{"", true},
		{"x", false},
		{[]any{}, true},
		{[]any{1}, false},
		{map[string]any{}, true},
		{map[string]any{"a": 1}, false},
		{0, false},
		{false, false},
	}
	for _, c := range cases {
		if got := isEmptyVerifyValue(c.v); got != c.want {
			t.Errorf("isEmptyVerifyValue(%#v) = %v, want %v", c.v, got, c.want)
		}
	}
}
