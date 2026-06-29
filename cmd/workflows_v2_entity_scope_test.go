package cmd

import (
	"strings"
	"testing"
)

// entity.<root>.<group>.<key>[.<subkey>] is a backend pass-through mapping
// scope. The CLI must accept it as a reserved namespace exactly like inputs /
// custom / system / task_outputs / task_outputs_by_type, with no deep grammar
// validation. The branch-root form entity.<alias>:<handle>.<...> carries a ":"
// in a LATER segment; the head before the first "." is still "entity", so the
// reserved-scope short-circuit covers it without any special ":" handling.

// TestReservedMappingScopes_IncludesEntity is the canonical guard: if someone
// removes entity from the map, every other assertion here also breaks, but this
// pins the intent.
func TestReservedMappingScopes_IncludesEntity(t *testing.T) {
	if !reservedMappingScopes["entity"] {
		t.Fatalf("entity must be a reserved mapping scope")
	}
}

// TestMappingDependencyRef_EntityIsReserved proves entity.* mapping values
// carry no spec-local task dependency (so the topological sort + rewriter treat
// them as a reserved namespace, never as a task ref). Covers the plain form and
// the rel/deal branch-root form whose ":" lives after the first ".".
func TestMappingDependencyRef_EntityIsReserved(t *testing.T) {
	cases := []string{
		"entity.borrower.identities.tax_id",
		"entity.deal.deal_fields.loan_amount.amount",
		"entity.rel_node:rel-abc.identities.cedula",
		"entity.attach-deal:deal-123.deal_fields.x",
	}
	for _, in := range cases {
		if got := mappingDependencyRef(in); got != "" {
			t.Errorf("mappingDependencyRef(%q) = %q, want \"\" (reserved scope, no task dep)", in, got)
		}
	}
}

// TestRewriteRefsInMappings_EntityPassthrough proves the rewriter neither errors
// on nor mutates an entity.* value, even when the spec has real task refs in the
// same mapping (the entity head must short-circuit before the unknown-ref error
// path). The branch-root form with a ":" in a later segment must also survive
// untouched.
func TestRewriteRefsInMappings_EntityPassthrough(t *testing.T) {
	refMap := map[string]string{"fetch": "fetch-server-abc123"}
	mappings := map[string]any{
		"tax_id":   "entity.borrower.identities.tax_id",
		"cedula":   "entity.rel_node:rel-abc.identities.cedula",
		"loan_amt": "entity.deal.deal_fields.loan_amount.amount",
		"credit":   "task_outputs.fetch.score", // real ref -> rewritten to server alias
	}
	out, err := rewriteRefsInMappings(mappings, refMap)
	if err != nil {
		t.Fatalf("rewriteRefsInMappings must accept entity.* values, got error: %v", err)
	}
	for k, want := range map[string]string{
		"tax_id":   "entity.borrower.identities.tax_id",
		"cedula":   "entity.rel_node:rel-abc.identities.cedula",
		"loan_amt": "entity.deal.deal_fields.loan_amount.amount",
	} {
		if got, _ := out[k].(string); got != want {
			t.Errorf("entity.* value for %q was mutated: got %q, want %q", k, got, want)
		}
	}
	// The genuine spec ref still gets rewritten to its server alias, proving the
	// entity short-circuit didn't disable normal rewriting.
	if got, _ := out["credit"].(string); got != "task_outputs.fetch-server-abc123.score" {
		t.Errorf("real task ref was not rewritten: got %q", got)
	}
}

// TestRewriteRefsInTemplate_EntityPassthrough proves {{entity.*}} placeholders
// in template strings (http body/url, end outputJson) are left untouched and
// never trigger the unknown-head error.
func TestRewriteRefsInTemplate_EntityPassthrough(t *testing.T) {
	in := `{"taxId": "{{entity.borrower.identities.tax_id}}", "cedula": "{{entity.rel_node:rel-abc.identities.cedula}}"}`
	out, err := rewriteRefsInTemplate(in, map[string]string{}, nil)
	if err != nil {
		t.Fatalf("rewriteRefsInTemplate must accept {{entity.*}}, got error: %v", err)
	}
	if out != in {
		t.Errorf("entity.* template placeholders were mutated:\n got: %q\nwant: %q", out, in)
	}
}

// TestPreflightTasks_EntityMappingAccepted is the end-to-end regression for the
// original failure: an inputMappings value in the entity.* namespace previously
// failed preflight's mapping-namespace check ("unknown leading segment") because
// entity was not reserved. It must now PASS, including the branch-root ":" form.
func TestPreflightTasks_EntityMappingAccepted(t *testing.T) {
	spec := &composeSpec{
		Label:      "Entity mapping",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"items": []any{map[string]any{"contact_id": "c-1"}},
			}, map[string]any{
				"borrower_id": "entity.borrower.identities.tax_id",
				"cedula":      "entity.rel_node:rel-abc.identities.cedula",
				"loan_amt":    "entity.deal.deal_fields.loan_amount.amount",
			}),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	if err := preflightTasks(spec); err != nil {
		t.Fatalf("preflight should accept entity.* inputMappings (reserved pass-through scope), got: %v", err)
	}
}

// TestPreflightTasks_UnknownScopeStillRejected guards that widening the reserved
// set to include entity did NOT accidentally accept arbitrary unknown heads: a
// typo like "entityy.*" must still fail, and the error must enumerate entity as
// a valid namespace so the user sees the fix.
func TestPreflightTasks_UnknownScopeStillRejected(t *testing.T) {
	spec := &composeSpec{
		Label:      "Unknown scope",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"items": []any{map[string]any{"contact_id": "c-1"}},
			}, map[string]any{
				"borrower_id": "entityy.borrower.identities.tax_id", // typo
			}),
		},
		Edges: []map[string]any{{"from": "start", "to": "rel"}},
	}
	err := preflightTasks(spec)
	if err == nil {
		t.Fatalf("preflight should reject an unknown leading segment, got nil")
	}
	if !strings.Contains(err.Error(), "entity") {
		t.Errorf("error should list entity as a valid namespace, got: %v", err)
	}
}
