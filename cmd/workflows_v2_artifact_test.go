package cmd

import "testing"

// The compiled-in validTaskTypes map mirrors the backend TaskType enum by hand.
// A missing entry makes `apply` reject an artifact node as an unknown type AFTER
// the earlier tasks in the compose loop have already been created, and no
// rollback path exists -- so the mirror is the thing worth pinning.
func TestArtifact_MirrorCarriesTheTaskType(t *testing.T) {
	if !validTaskTypes["artifact"] {
		t.Fatal("artifact must validate offline from the compiled-in validTaskTypes mirror")
	}
}

// A node ref and an artifact alias are separate namespaces, and "usuarios" or
// "sucursales" is a natural name in both -- which is exactly why the backend
// field is spelled artifactAlias and not alias. The residual-ref validator runs
// INSIDE the POST loop, so a false positive there aborts after tasks have
// already been created and nothing rolls them back.
func TestArtifact_AliasAndColumnsAreNotTreatedAsResidualRefs(t *testing.T) {
	body := map[string]any{
		"type": "artifact",
		"artifactConfig": map[string]any{
			"artifactAlias": "usuarios",
			"columns":       []any{"email", "sucursal"},
		},
	}
	refMap := map[string]string{
		"usuarios": "usuarios-server-1234",
		"email":    "email-server-5678",
		"sucursal": "sucursal-server-9012",
	}
	if err := validateNoResidualSpecRefs(body, refMap, "compose"); err != nil {
		t.Fatalf("an artifact alias or column that reads like a node ref must not abort apply, got: %v", err)
	}
}

// The exclusion must not blind the validator to a real residual ref elsewhere
// in the same body: inputMappings still has to be rewritten.
func TestArtifact_AResidualRefInInputMappingsStillAborts(t *testing.T) {
	body := map[string]any{
		"type": "artifact",
		"artifactConfig": map[string]any{
			"artifactAlias": "usuarios",
		},
		"inputMappings": map[string]any{"artifactAlias": "lookup"},
	}
	refMap := map[string]string{"lookup": "lookup-server-1234"}
	if err := validateNoResidualSpecRefs(body, refMap, "compose"); err == nil {
		t.Fatal("an unrewritten node ref in inputMappings must still abort")
	}
}

// residualSpecRefExcludedFields is shared with the REWRITER and the dependency
// scanner, not just the validator, so moving the alias out of it has to be
// checked on that side too: the literal must survive rewriting unrenamed, and
// the mapping beside it must still be rewritten to the server-assigned alias.
func TestArtifact_RewriteLeavesTheLiteralAndStillRewritesTheMapping(t *testing.T) {
	task := map[string]any{
		"type": "artifact",
		"artifactConfig": map[string]any{
			"artifactAlias": "usuarios",
			"columns":       []any{"email"},
		},
		"inputMappings": map[string]any{"artifactAlias": "task_outputs.usuarios.alias"},
	}
	refMap := map[string]string{"usuarios": "usuarios-server-1234"}

	if err := rewriteTaskRefs(task, refMap, `node ref="probe"`); err != nil {
		t.Fatalf("rewriteTaskRefs: %v", err)
	}

	cfg := asMap(task["artifactConfig"])
	if got := cfg["artifactAlias"]; got != "usuarios" {
		t.Fatalf("the artifact alias literal must not be renamed by the rewriter, got %v", got)
	}
	mappings := asMap(task["inputMappings"])
	if got := mappings["artifactAlias"]; got != "task_outputs.usuarios-server-1234.alias" {
		t.Fatalf("the alias MAPPING must still be rewritten to the server alias, got %v", got)
	}
}

// artifact is a live type, not a retired one: apply must not refuse it with the
// deprecation message.
func TestArtifact_IsNotDeprecated(t *testing.T) {
	if deprecatedTaskTypes["artifact"] {
		t.Fatal("artifact is a current task type and must not be in deprecatedTaskTypes")
	}
}
