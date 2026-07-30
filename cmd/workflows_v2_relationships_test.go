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

// TestPreflightTasks_RelationshipsNoBorrowerOK: borrower_id is now OPTIONAL.
// The backend resolves the anchor borrower from the workflow primary borrower
// (_primary_borrower_id, set by an upstream customer/create-borrower node or a
// borrower_id workflow input), so a relationships task with items but no
// borrower_id must PASS preflight.
func TestPreflightTasks_RelationshipsNoBorrowerOK(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel no borrower",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"items": []any{map[string]any{"contact_id": "c-1"}},
			}, nil),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept relationships without borrower_id (backend-resolved), got: %v", err)
	}
}

// TestPreflightTasks_PackageIO: package-io is a valid backend task type and must
// pass preflight (regression for the missing-from-validTaskTypes bug that made
// the CLI reject it as an "unknown task type").
func TestPreflightTasks_PackageIO(t *testing.T) {
	spec := &composeSpec{
		Label:      "Package IO",
		Category:   "OTHER",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			{
				"ref":           "pkg",
				"type":          "package-io",
				"label":         "Package IO",
				"packageConfig": map[string]any{"mode": "read", "alias": "x"},
			},
		},
		Edges: []map[string]any{{"from": "start", "to": "pkg"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept package-io, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsEmptyItems: borrower wired, but no items
// inline and no items mapping -- reject.
func TestPreflightTasks_RelationshipsEmptyItems(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel no items",
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
		Label:      "Rel missing contact",
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
		Label:      "Rel two legal reps",
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

// TestPreflightTasks_RelationshipsInvalidKind: a relationship value the BACKEND
// disowns is rejected. The live hook is wired so preflight has real authority --
// that is the only branch that rejects. With no hook the CLI cannot tell
// "invalid" from "newer than this build", so it warns and proceeds (covered by
// TestCheckRelationshipKind_LiveBackendFallback).
func TestPreflightTasks_RelationshipsInvalidKind(t *testing.T) {
	fetchLiveRelationshipKinds = func() map[string]bool {
		return map[string]bool{
			"shareholder": true, "employee": true, "family": true,
			"other": true, "unspecified": true,
		}
	}
	defer func() {
		fetchLiveRelationshipKinds = nil
		liveRelationshipKinds = nil
		liveRelationshipKindsFetched = false
	}()
	liveRelationshipKinds = nil
	liveRelationshipKindsFetched = false

	spec := &composeSpec{
		Label:      "Rel invalid kind",
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

// TestPreflightTasks_RelationshipsUpsertHappyPath: upsertContacts=true allows
// items without contact_id as long as they carry tax_id (or identity_value).
// The KYC husband+wife shape.
func TestPreflightTasks_RelationshipsUpsertHappyPath(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel upsert",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"borrower_id":    "b-husband",
				"upsertContacts": true,
				"items": []any{
					map[string]any{
						"tax_id":       "27-32456789-4",
						"persona":      "individual",
						"label":        "Jane Doe",
						"relationship": "family",
					},
				},
			}, nil),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept upsert path with tax_id, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsUpsertCustomIdentityKey: defaultIdentityKey
// lets the item carry the value under a non-tax_id field name.
func TestPreflightTasks_RelationshipsUpsertCustomIdentityKey(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel upsert email",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"borrower_id":        "b-husband",
				"upsertContacts":     true,
				"defaultIdentityKey": "email",
				"items": []any{
					map[string]any{
						"email":        "wife@example.com",
						"persona":      "individual",
						"relationship": "family",
					},
				},
			}, nil),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept defaultIdentityKey shorthand, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsUpsertExplicitIdentityKey: per-item
// identity_key + identity_value overrides defaultIdentityKey.
func TestPreflightTasks_RelationshipsUpsertExplicitIdentityKey(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel upsert explicit",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"borrower_id":    "b-husband",
				"upsertContacts": true,
				"items": []any{
					map[string]any{
						"identity_key":   "phone",
						"identity_value": "+5491100000000",
						"persona":        "individual",
						"relationship":   "family",
					},
				},
			}, nil),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept explicit identity_key+identity_value, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsUpsertMissingIdentity: upsertContacts=true
// but item has neither contact_id nor any identity -- reject.
func TestPreflightTasks_RelationshipsUpsertMissingIdentity(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel upsert no identity",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"borrower_id":    "b-husband",
				"upsertContacts": true,
				"items": []any{
					map[string]any{
						"persona":      "individual",
						"relationship": "family",
					},
				},
			}, nil),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject item with no identity under upsert, got nil")
	}
	if !strings.Contains(err.Error(), "identity_value") {
		t.Errorf("error should mention identity_value, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsUpsertOffHintsAtFlag: missing contact_id
// without upsertContacts should mention the flag so the user can opt in.
func TestPreflightTasks_RelationshipsUpsertOffHintsAtFlag(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel no contact no upsert",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"borrower_id": "b-husband",
				"items": []any{
					map[string]any{
						"tax_id":       "27-32456789-4",
						"relationship": "family",
					},
				},
			}, nil),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject missing contact_id when upsert is off, got nil")
	}
	if !strings.Contains(err.Error(), "upsertContacts") {
		t.Errorf("error should hint at upsertContacts flag, got: %v", err)
	}
}

// relReadTask builds a relationships READ task (operation:read + picks). No
// inline write items -- the read op resolves existing relationships.
func relReadTask(ref string, picks []any, inputMappings map[string]any) map[string]any {
	t := map[string]any{
		"ref":       ref,
		"type":      "relationships",
		"label":     "Read relationships",
		"operation": "read",
		"readRelationshipsConfig": map[string]any{
			"picks": picks,
		},
	}
	if inputMappings != nil {
		t["inputMappings"] = inputMappings
	}
	return t
}

// TestPreflightTasks_RelationshipsReadHappyPath: operation:read with valid picks
// passes preflight WITHOUT any relationshipsConfig.items (the write-only items
// requirement must not apply in read mode).
func TestPreflightTasks_RelationshipsReadHappyPath(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel read",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relReadTask("read-rel", []any{
				map[string]any{"id": "legalrep", "isLegalRepresentative": true, "take": "highest"},
				map[string]any{"id": "sh", "relationship": "shareholder", "take": "lowest"},
				map[string]any{"id": "any", "take": "highest"},
			}, nil),
		},
		Edges: []map[string]any{{"from": "start", "to": "read-rel"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept a read relationships task with picks and no items, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsReadNoPicksOK: read mode with zero picks is
// warn-only (node produces no output), not a hard compose error.
func TestPreflightTasks_RelationshipsReadNoPicksOK(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel read empty",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relReadTask("read-rel", []any{}, nil),
		},
		Edges: []map[string]any{{"from": "start", "to": "read-rel"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept a read relationships task with zero picks, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsReadBadTake: an invalid tie-break is rejected.
func TestPreflightTasks_RelationshipsReadBadTake(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel read bad take",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relReadTask("read-rel", []any{
				map[string]any{"id": "p", "take": "middle"},
			}, nil),
		},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject take=middle, got nil")
	}
	if !strings.Contains(err.Error(), "take") {
		t.Errorf("error should mention take, got: %v", err)
	}
}

// TestPreflightTasks_RelationshipsReadBadKind: an invalid pick relationship kind
// is rejected (same enum as write items).
func TestPreflightTasks_RelationshipsReadBadKind(t *testing.T) {
	spec := &composeSpec{
		Label:      "Rel read bad kind",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relReadTask("read-rel", []any{
				map[string]any{"id": "p", "relationship": "cousin"},
			}, nil),
		},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject relationship=cousin, got nil")
	}
	if !strings.Contains(err.Error(), "shareholder/employee/family/other/unspecified") {
		t.Errorf("error should mention the kind enum, got: %v", err)
	}
}
