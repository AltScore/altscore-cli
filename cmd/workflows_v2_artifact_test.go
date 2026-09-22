package cmd

import "testing"

func TestArtifact_MirrorCarriesTheTaskType(t *testing.T) {
	if !validTaskTypes["artifact"] {
		t.Fatal("artifact must validate offline from the compiled-in validTaskTypes mirror")
	}
}

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

func TestArtifact_IsNotDeprecated(t *testing.T) {
	if deprecatedTaskTypes["artifact"] {
		t.Fatal("artifact is a current task type and must not be in deprecatedTaskTypes")
	}
}
