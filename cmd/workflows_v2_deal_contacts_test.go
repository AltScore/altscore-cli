package cmd

import (
	"strings"
	"testing"
)

// dealTask builds a deal write task body for these tests. contacts is the
// inline contacts list (sibling to upsertContacts) that drives deal-<id>
// handles and, with upsertContacts on, borrower-by-identity upsert.
func dealTask(ref string, upsertContacts bool, contacts []any) map[string]any {
	t := map[string]any{
		"ref":       ref,
		"type":      "deal",
		"label":     "Attach deal contacts",
		"operation": "write",
		"lookupBy":  "external_id",
		"key":       "external_id",
		"inputSchema": map[string]any{
			"external_id": map[string]any{"type": "string", "required": true},
		},
		"inputMappings": map[string]any{"external_id": "inputs.external_id"},
	}
	if upsertContacts {
		t["upsertContacts"] = true
	}
	if contacts != nil {
		t["contacts"] = contacts
	}
	return t
}

// dealSpec wraps a deal task in a minimal preflight-passing spec.
func dealSpec(label string, deal map[string]any) *composeSpec {
	return &composeSpec{
		Label:      label,
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks:      []map[string]any{deal},
		Edges:      []map[string]any{{"from": "start", "to": deal["ref"]}},
	}
}

// TestPreflightTasks_DealContactsBorrowerIdPasses: upsertContacts off and each
// row carries borrower_id -- the existing-borrower attach path. Passes.
func TestPreflightTasks_DealContactsBorrowerIdPasses(t *testing.T) {
	spec := dealSpec("Deal borrower ids", dealTask("attach-deal", false, []any{
		map[string]any{"id": "0", "borrower_id": "brw_customer", "role_key": "customer", "is_primary": true},
		map[string]any{"id": "1", "borrower_id": "brw_guarantor", "role_key": "guarantor"},
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept borrower_id rows with upsert off, got: %v", err)
	}
}

// TestPreflightTasks_DealContactsUpsertTaxIdPasses: upsertContacts on, row
// carries tax_id shorthand + persona. The customer-upsert path. Passes.
func TestPreflightTasks_DealContactsUpsertTaxIdPasses(t *testing.T) {
	spec := dealSpec("Deal upsert tax_id", dealTask("attach-deal", true, []any{
		map[string]any{"tax_id": "20-12345678-9", "persona": "individual", "role_key": "customer", "is_primary": true},
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept tax_id+persona under upsert, got: %v", err)
	}
}

// TestPreflightTasks_DealContactsUpsertExplicitIdentityPasses: explicit
// identity_key + identity_value + persona resolves. Passes.
func TestPreflightTasks_DealContactsUpsertExplicitIdentityPasses(t *testing.T) {
	spec := dealSpec("Deal upsert explicit", dealTask("attach-deal", true, []any{
		map[string]any{"identity_key": "email", "identity_value": "co@example.com", "persona": "business", "role_key": "guarantor"},
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept explicit identity_key+identity_value+persona, got: %v", err)
	}
}

// TestPreflightTasks_DealContactsUpsertBorrowerIdShortCircuits: a borrower_id
// row under upsert needs no identity/persona -- it short-circuits. Passes.
func TestPreflightTasks_DealContactsUpsertBorrowerIdShortCircuits(t *testing.T) {
	spec := dealSpec("Deal upsert mixed", dealTask("attach-deal", true, []any{
		map[string]any{"borrower_id": "brw_customer", "role_key": "customer"},
		map[string]any{"tax_id": "20-99999999-9", "persona": "individual", "role_key": "guarantor"},
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should let a borrower_id row short-circuit under upsert, got: %v", err)
	}
}

// TestPreflightTasks_DealContactsUpsertMissingIdentity: upsertContacts on but
// row has neither borrower_id nor any identity -- reject. This is the
// silent-skip bug the preflight catches.
func TestPreflightTasks_DealContactsUpsertMissingIdentity(t *testing.T) {
	spec := dealSpec("Deal upsert no identity", dealTask("attach-deal", true, []any{
		map[string]any{"persona": "individual", "role_key": "customer"},
	}))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject upsert row with no identity, got nil")
	}
	if !strings.Contains(err.Error(), "identity_value") {
		t.Errorf("error should mention identity_value, got: %v", err)
	}
}

// TestPreflightTasks_DealContactsUpsertMissingPersona: identity present but no
// persona -- reject (can't create the borrower without it).
func TestPreflightTasks_DealContactsUpsertMissingPersona(t *testing.T) {
	spec := dealSpec("Deal upsert no persona", dealTask("attach-deal", true, []any{
		map[string]any{"tax_id": "20-12345678-9", "role_key": "customer"},
	}))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject identity row missing persona, got nil")
	}
	if !strings.Contains(err.Error(), "persona") {
		t.Errorf("error should mention persona, got: %v", err)
	}
}

// TestPreflightTasks_DealContactsMissingBorrowerIdHintsAtFlag: missing
// borrower_id with upsert off should mention the upsertContacts flag.
func TestPreflightTasks_DealContactsMissingBorrowerIdHintsAtFlag(t *testing.T) {
	spec := dealSpec("Deal no borrower no upsert", dealTask("attach-deal", false, []any{
		map[string]any{"tax_id": "20-12345678-9", "persona": "individual", "role_key": "customer"},
	}))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject missing borrower_id when upsert is off, got nil")
	}
	if !strings.Contains(err.Error(), "upsertContacts") {
		t.Errorf("error should hint at upsertContacts flag, got: %v", err)
	}
}

// TestPreflightTasks_DealNoContactsPasses: a deal task with no inline contacts
// at all is unaffected by the new preflight. Passes.
func TestPreflightTasks_DealNoContactsPasses(t *testing.T) {
	spec := dealSpec("Deal no contacts", dealTask("attach-deal", false, nil))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept a deal task with no inline contacts, got: %v", err)
	}
}

// TestPreflightTasks_DealContactsSourcesConfigRejected: the legacy
// deal_contacts (plural) / deal_contact (singular) sourcesConfig contact-
// attachment paths are no longer supported. A deal node carrying such a
// sourcesConfig entry must be rejected with a migration message pointing at
// the inline `contacts` field.
func TestPreflightTasks_DealContactsSourcesConfigRejected(t *testing.T) {
	for _, srcType := range []string{"deal_contacts", "deal_contact"} {
		deal := dealTask("attach-deal", false, nil)
		deal["sourcesConfig"] = []any{
			map[string]any{"type": srcType, "key": "contacts", "label": "Contacts"},
		}
		spec := dealSpec("Deal sources "+srcType, deal)
		err := preflightTasks(spec)
		if err == nil {
			t.Fatalf("preflight should reject %s sourcesConfig, got nil", srcType)
		}
		if !strings.Contains(err.Error(), "no longer supported") ||
			!strings.Contains(err.Error(), "inline 'contacts' field") {
			t.Errorf("error for %s should explain migration to inline contacts, got: %v", srcType, err)
		}
	}
}
