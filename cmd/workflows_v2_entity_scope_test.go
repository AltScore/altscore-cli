package cmd

import (
	"strings"
	"testing"
)

func TestReservedMappingScopes_IncludesEntity(t *testing.T) {
	if !reservedMappingScopes["entity"] {
		t.Fatalf("entity must be a reserved mapping scope")
	}
}

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

func TestRewriteRefsInMappings_EntityPassthrough(t *testing.T) {
	refMap := map[string]string{"fetch": "fetch-server-abc123"}
	mappings := map[string]any{
		"tax_id":   "entity.borrower.identities.tax_id",
		"cedula":   "entity.rel_node:rel-abc.identities.cedula",
		"loan_amt": "entity.deal.deal_fields.loan_amount.amount",
		"credit":   "task_outputs.fetch.score",
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
	if got, _ := out["credit"].(string); got != "task_outputs.fetch-server-abc123.score" {
		t.Errorf("real task ref was not rewritten: got %q", got)
	}
}

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

func TestPreflightTasks_UnknownScopeStillRejected(t *testing.T) {
	spec := &composeSpec{
		Label:      "Unknown scope",
		Category:   "ACTION",
		ExtraNodes: startNode,
		Tasks: []map[string]any{
			relTask("rel", map[string]any{
				"items": []any{map[string]any{"contact_id": "c-1"}},
			}, map[string]any{
				"borrower_id": "entityy.borrower.identities.tax_id",
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
