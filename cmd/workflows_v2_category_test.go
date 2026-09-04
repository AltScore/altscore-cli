package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// The compiled-in validTaskTypes map mirrors the backend TaskType enum by hand.
// A missing entry makes `apply` reject a category node as an unknown type AFTER
// the earlier tasks in the compose loop have already been created, and no
// rollback path exists -- so the mirror is the thing worth pinning.
func TestCategory_MirrorCarriesTheTaskType(t *testing.T) {
	if !validTaskTypes["category"] {
		t.Fatal("category must validate offline from the compiled-in validTaskTypes mirror")
	}
}

func categoryBody(cfg map[string]any) json.RawMessage {
	body, _ := json.Marshal(map[string]any{"type": "category", "categoryConfig": cfg})
	return body
}

func TestCategory_StructuralValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     map[string]any
		wantErr string
	}{
		{
			name: "assign with a json value is accepted",
			cfg: map[string]any{
				"operation":   "assign",
				"categoryKey": "segmentation",
				"valueFormat": "json",
				"valueFields": []any{"grupo_precio_comercial", "subcanal"},
			},
		},
		{
			name: "read with the string default is accepted",
			cfg:  map[string]any{"operation": "read", "categoryKey": "segmentation"},
		},
		{
			name:    "a missing categoryKey is rejected",
			cfg:     map[string]any{"operation": "read"},
			wantErr: "categoryConfig.categoryKey",
		},
		{
			name:    "a missing operation is rejected",
			cfg:     map[string]any{"categoryKey": "segmentation"},
			wantErr: "categoryConfig.operation",
		},
		{
			name:    "an unknown operation is rejected",
			cfg:     map[string]any{"operation": "unassign", "categoryKey": "segmentation"},
			wantErr: "must be \"read\" or \"assign\"",
		},
		{
			name:    "json without valueFields is rejected",
			cfg:     map[string]any{"operation": "assign", "categoryKey": "segmentation", "valueFormat": "json"},
			wantErr: "categoryConfig.valueFields",
		},
		{
			name:    "an unknown entityRoot is rejected",
			cfg:     map[string]any{"operation": "read", "categoryKey": "segmentation", "entityRoot": "asset"},
			wantErr: "categoryConfig.entityRoot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTaskV2BodyStructural(categoryBody(tc.cfg))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got none", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected an error mentioning %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// The runtime reads the TOP-LEVEL inputMappings; schema-guide examples document
// the nested form. Either spelling has to reach the runtime or the assigned
// value is silently empty.
func TestCategory_NormalizeMirrorsInputMappingsBothWays(t *testing.T) {
	t.Run("nested to top level", func(t *testing.T) {
		task := map[string]any{
			"type": "category",
			"categoryConfig": map[string]any{
				"operation":     "assign",
				"categoryKey":   "segmentation",
				"inputMappings": map[string]any{"subcanal": "inputs.subcanal"},
			},
		}
		if err := normalizeTaskBody(nil, task, nil, true); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if got := asMap(task["inputMappings"])["subcanal"]; got != "inputs.subcanal" {
			t.Fatalf("top-level inputMappings not mirrored, got %v", task["inputMappings"])
		}
	})

	t.Run("top level to nested", func(t *testing.T) {
		task := map[string]any{
			"type":          "category",
			"inputMappings": map[string]any{"subcanal": "inputs.subcanal"},
			"categoryConfig": map[string]any{
				"operation":   "assign",
				"categoryKey": "segmentation",
			},
		}
		if err := normalizeTaskBody(nil, task, nil, true); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		nested := asMap(asMap(task["categoryConfig"])["inputMappings"])
		if got := nested["subcanal"]; got != "inputs.subcanal" {
			t.Fatalf("nested inputMappings not mirrored, got %v", task["categoryConfig"])
		}
	})
}

// A node ref and a category key are separate namespaces, and "segmentation" is
// a natural name in both. The residual-ref validator runs INSIDE the POST loop,
// so a false positive there aborts after tasks have already been created and
// nothing rolls them back -- which is the one failure apply must never have.
func TestCategory_KeyAndValueFieldsAreNotTreatedAsResidualRefs(t *testing.T) {
	body := map[string]any{
		"type": "category",
		"categoryConfig": map[string]any{
			"operation":   "assign",
			"categoryKey": "segmentation",
			"valueFields": []any{"subcanal", "grupo_precio_comercial"},
		},
	}
	refMap := map[string]string{
		"segmentation":           "segmentation-server-1234",
		"subcanal":               "subcanal-server-5678",
		"grupo_precio_comercial": "grupo-server-9012",
	}
	if err := validateNoResidualSpecRefs(body, refMap, "compose"); err != nil {
		t.Fatalf("a category key or value field that reads like a node ref must not abort apply, got: %v", err)
	}
}

// The exclusion must not blind the validator to a real residual ref elsewhere
// in the same body: inputMappings still has to be rewritten.
func TestCategory_AResidualRefInInputMappingsStillAborts(t *testing.T) {
	body := map[string]any{
		"type": "category",
		"categoryConfig": map[string]any{
			"operation":   "assign",
			"categoryKey": "segmentation",
		},
		"inputMappings": map[string]any{"subcanal": "lookup"},
	}
	refMap := map[string]string{"lookup": "lookup-server-1234"}
	if err := validateNoResidualSpecRefs(body, refMap, "compose"); err == nil {
		t.Fatal("an unrewritten node ref in inputMappings must still abort")
	}
}
