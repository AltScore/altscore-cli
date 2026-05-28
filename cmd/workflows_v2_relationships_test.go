package cmd

import (
	"strings"
	"testing"
)

// relTask builds a relationships task body for these tests.
func relTask(ref string, cfg map[string]any, inputMappings map[string]any) map[string]any {
	t := map[string]any{
		"ref":   ref,
		"type":  "relationships",
		"label": "Attach contacts",
	}
	if cfg != nil {
		t["relationshipsConfig"] = cfg
	}
	if inputMappings != nil {
		t["inputMappings"] = inputMappings
	}
	return t
}

// startNode is the minimal start ExtraNode every preflight-passing spec needs.
var startNode = []map[string]any{{"ref": "start", "type": "start", "label": "Start"}}

// TestPreflightTasks_RelationshipsHappyPath: an inline-config spec with one
// borrower and three items passes preflight unchanged.
func TestPreflightTasks_RelationshipsHappyPath(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel happy",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"borrower_id": "b-1",
				"items": []any{
					map[string]any{"contact_id": "c-1", "relationship": "shareholder", "ownership_pct": 50.0},
					map[string]any{"contact_id": "c-2", "relationship": "employee"},
					map[string]any{"contact_id": "c-3", "relationship": "family", "is_legal_representative": true},
				},
			}, nil),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept a valid relationships task, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsVariableBound: items + borrower_id both wired
// via inputMappings; no inline config. Must pass.
func TestPreflightTasks_RelationshipsVariableBound(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel mapped",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", nil, map[string]any{
				"borrower_id": "inputs.borrower_id",
				"items":       "inputs.items",
			}),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept variable-bound items, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsMissingBorrower: no borrower_id source on
// either side -- reject. This is exactly the silent-zero-write bug the
// preflight is supposed to catch.
func TestPreflightTasks_RelationshipsMissingBorrower(t *testing.T) {
	spec := &composeSpec{
		Label:    "Rel no borrower",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"items": []any{map[string]any{"contact_id": "c-1"}},
			}, nil),
		},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject missing borrower_id, got nil")
	}
	if !strings.Contains(err.Error(), "borrower_id") {
		t.Errorf("error should mention borrower_id, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsEmptyItems: borrower wired, but no items
// inline and no items mapping -- reject.
func TestPreflightTasks_RelationshipsEmptyItems(t *testing.T) {
	spec := &composeSpec{
		Label:    "Rel no items",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{"borrower_id": "b-1"}, nil),
		},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject empty items + no mapping, got nil")
	}
	if !strings.Contains(err.Error(), "items") {
		t.Errorf("error should mention items, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsMissingContactId: an item without contact_id
// in the inline list is rejected.
func TestPreflightTasks_RelationshipsMissingContactId(t *testing.T) {
	spec := &composeSpec{
		Label:    "Rel missing contact",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"borrower_id": "b-1",
				"items": []any{
					map[string]any{"relationship": "family"},
				},
			}, nil),
		},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject item without contact_id, got nil")
	}
	if !strings.Contains(err.Error(), "contact_id") {
		t.Errorf("error should mention contact_id, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsTwoLegalReps: two items with
// is_legal_representative=true would clobber each other -- reject upfront.
func TestPreflightTasks_RelationshipsTwoLegalReps(t *testing.T) {
	spec := &composeSpec{
		Label:    "Rel two legal reps",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"borrower_id": "b-1",
				"items": []any{
					map[string]any{"contact_id": "c-1", "is_legal_representative": true},
					map[string]any{"contact_id": "c-2", "is_legal_representative": true},
				},
			}, nil),
		},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject two legal reps, got nil")
	}
	if !strings.Contains(err.Error(), "is_legal_representative") {
		t.Errorf("error should mention is_legal_representative, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsInvalidKind: relationship value not in the
// allowed enum is rejected.
func TestPreflightTasks_RelationshipsInvalidKind(t *testing.T) {
	spec := &composeSpec{
		Label:    "Rel invalid kind",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"borrower_id": "b-1",
				"items": []any{
					map[string]any{"contact_id": "c-1", "relationship": "spouse"},
				},
			}, nil),
		},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject invalid relationship kind, got nil")
	}
	if !strings.Contains(err.Error(), "shareholder") {
		t.Errorf("error should list valid kinds, got: %v", err)
	}
}
