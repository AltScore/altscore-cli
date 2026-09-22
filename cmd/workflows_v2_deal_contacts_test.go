package cmd

import (
	"strings"
	"testing"
)

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

func dealSpec(label string, deal map[string]any) *composeSpec {
	return &composeSpec{
		Label:      label,
		Category:   "EVALUATION",
		ExtraNodes: startNode,
		Tasks:      []map[string]any{deal},
		Edges:      []map[string]any{{"from": "start", "to": deal["ref"]}},
	}
}

func TestPreflightTasks_DealContactsBorrowerIdPasses(t *testing.T) {
	spec := dealSpec("Deal borrower ids", dealTask("attach-deal", false, []any{
		map[string]any{"id": "0", "borrower_id": "brw_customer", "role_key": "customer", "is_primary": true},
		map[string]any{"id": "1", "borrower_id": "brw_guarantor", "role_key": "guarantor"},
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept borrower_id rows with upsert off, got: %v", err)
	}
}

func TestPreflightTasks_DealContactsUpsertTaxIdPasses(t *testing.T) {
	spec := dealSpec("Deal upsert tax_id", dealTask("attach-deal", true, []any{
		map[string]any{"tax_id": "20-12345678-9", "persona": "individual", "role_key": "customer", "is_primary": true},
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept tax_id+persona under upsert, got: %v", err)
	}
}

func TestPreflightTasks_DealContactsUpsertExplicitIdentityPasses(t *testing.T) {
	spec := dealSpec("Deal upsert explicit", dealTask("attach-deal", true, []any{
		map[string]any{"identity_key": "email", "identity_value": "co@example.com", "persona": "business", "role_key": "guarantor"},
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept explicit identity_key+identity_value+persona, got: %v", err)
	}
}

func TestPreflightTasks_DealContactsUpsertBorrowerIdShortCircuits(t *testing.T) {
	spec := dealSpec("Deal upsert mixed", dealTask("attach-deal", true, []any{
		map[string]any{"borrower_id": "brw_customer", "role_key": "customer"},
		map[string]any{"tax_id": "20-99999999-9", "persona": "individual", "role_key": "guarantor"},
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should let a borrower_id row short-circuit under upsert, got: %v", err)
	}
}

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

func TestPreflightTasks_DealNoContactsPasses(t *testing.T) {
	spec := dealSpec("Deal no contacts", dealTask("attach-deal", false, nil))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept a deal task with no inline contacts, got: %v", err)
	}
}

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

func dealReadTask(ref string, picks []any, leftoverContacts []any) map[string]any {
	t := map[string]any{
		"ref":       ref,
		"type":      "deal",
		"label":     "Read deal contacts",
		"operation": "read",
		"lookupBy":  "deal_id",
		"inputSchema": map[string]any{
			"deal_id": map[string]any{"type": "string", "required": true},
		},
		"inputMappings": map[string]any{"deal_id": "inputs.deal_id"},
	}
	if picks != nil {
		t["readDealContactsConfig"] = map[string]any{"picks": picks}
	}
	if leftoverContacts != nil {
		t["contacts"] = leftoverContacts
	}
	return t
}

func TestPreflightTasks_DealReadPicksPass(t *testing.T) {
	spec := dealSpec("Deal read picks", dealReadTask("read-deal", []any{
		map[string]any{"id": "g", "role_key": "guarantor", "take": "oldest"},
		map[string]any{"id": "p", "is_primary": true, "take": "newest"},
		map[string]any{"id": "any"},
	}, nil))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept read contact picks, got: %v", err)
	}
}

func TestPreflightTasks_DealReadIgnoresLeftoverContacts(t *testing.T) {
	spec := dealSpec("Deal read leftovers", dealReadTask("read-deal", []any{
		map[string]any{"id": "g", "role_key": "guarantor"},
	}, []any{
		map[string]any{"id": "c1", "role_key": "customer"},
	}))
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("read mode must not apply the write-mode contact rules, got: %v", err)
	}
}

func TestPreflightTasks_DealReadPickBadTakeFails(t *testing.T) {
	spec := dealSpec("Deal read bad take", dealReadTask("read-deal", []any{
		map[string]any{"id": "g", "role_key": "guarantor", "take": "highest"},
	}, nil))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatal("expected preflight to reject take=highest on a deal pick")
	}
	if !strings.Contains(err.Error(), "oldest") {
		t.Errorf("error should name the accepted values; got: %v", err)
	}
}

func TestPreflightTasks_DealReadPickNotObjectFails(t *testing.T) {
	spec := dealSpec("Deal read bad pick", dealReadTask("read-deal", []any{"guarantor"}, nil))
	err := preflightTasks(spec)
	if err == nil {
		t.Fatal("expected preflight to reject a non-object pick")
	}
	if !strings.Contains(err.Error(), "readDealContactsConfig.picks[0]") {
		t.Errorf("error should name the offending path; got: %v", err)
	}
}

func TestValidateNoResidualSpecRefs_DealRoleKeyIsALiteral(t *testing.T) {
	refMap := map[string]string{"customer": "borrower-ca89d5", "guarantor": "aval-77f0e1"}

	t.Run("write-side contacts[].role_key", func(t *testing.T) {
		body := map[string]any{
			"type": "deal",
			"contacts": []any{
				map[string]any{"id": "c1", "borrower_id": "brw_1", "role_key": "customer"},
			},
		}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err != nil {
			t.Errorf("role_key is a tenant role literal, not a ref: %v", err)
		}
	})

	t.Run("read-side picks[].role_key", func(t *testing.T) {
		body := map[string]any{
			"type":      "deal",
			"operation": "read",
			"readDealContactsConfig": map[string]any{
				"picks": []any{map[string]any{"id": "g", "role_key": "guarantor"}},
			},
		}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err != nil {
			t.Errorf("pick role_key is a tenant role literal, not a ref: %v", err)
		}
	})

	t.Run("a real missed ref on the same task still fails", func(t *testing.T) {
		body := map[string]any{
			"type":            "deal",
			"someNewRefField": "customer",
		}
		if err := validateNoResidualSpecRefs(body, refMap, "test"); err == nil {
			t.Error("excluding role_key must not blind the validator to other fields")
		}
	})
}
