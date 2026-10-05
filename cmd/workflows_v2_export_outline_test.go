package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApplySpecOutlineKeepsShapeAndDropsBodies(t *testing.T) {
	body := strings.Repeat("y", 5000)
	spec := map[string]any{
		"alias": "kyc-flow", "label": "KYC", "category": "EVALUATION",
		"inputVariables": map[string]any{
			"tax_id": map[string]any{"name": "tax_id", "type": "string", "required": true, "title": "Tax id", "description": ""},
		},
		"customVariables": map[string]any{
			"debt": map[string]any{"type": "number", "expression": body},
		},
		"nodes": []any{
			map[string]any{"ref": "start", "type": "start"},
			map[string]any{"ref": "pull", "type": "altdata-enrichment", "label": "Pull", "sourcesConfig": body},
			map[string]any{"ref": "done", "type": "end", "endConfig": map[string]any{
				"outputJson":     `{"decision": "{{task_outputs.pull.x}}", "debt": "{{custom.debt}}"}`,
				"standardOutput": map[string]any{"enabled": true},
			}},
		},
		"edges": []any{
			map[string]any{"from": "start", "to": "pull", "id": "e1"},
			map[string]any{"from": "pull", "to": "done", "sourceHandle": "ok"},
		},
	}
	out := applySpecOutline(spec)
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), body) {
		t.Fatal("the outline must not carry task bodies or expressions")
	}
	if got := out.InputVariables["tax_id"]; got["type"] != "string" || got["required"] != true || got["title"] != "Tax id" || len(got) != 3 {
		t.Errorf("input variables keep type, required, title (empty description dropped), got %v", got)
	}
	if out.CustomVariables["debt"] != "number" {
		t.Errorf("custom variables keep their type, got %v", out.CustomVariables)
	}
	if len(out.Nodes) != 3 || out.Nodes[1].Ref != "pull" || out.Nodes[1].Type != "altdata-enrichment" {
		t.Errorf("nodes keep ref, type, label, got %+v", out.Nodes)
	}
	if len(out.Edges) != 2 || out.Edges[1].SourceHandle != "ok" {
		t.Errorf("edges keep from, to, sourceHandle, got %+v", out.Edges)
	}
	if len(out.EndOutput) != 1 || strings.Join(out.EndOutput[0].OutputJSONKeys, ",") != "debt,decision" || !out.EndOutput[0].StandardOutputEnabled {
		t.Errorf("the end node's output keys are what a parent reads, got %+v", out.EndOutput)
	}
}

func TestApplySpecOutlinePreviewsANonObjectOutputJson(t *testing.T) {
	spec := map[string]any{"nodes": []any{map[string]any{"ref": "end", "type": "end", "endConfig": map[string]any{"outputJson": "{{task_outputs.x}}"}}}}
	out := applySpecOutline(spec)
	if len(out.EndOutput) != 1 || out.EndOutput[0].OutputJSONPreview != "{{task_outputs.x}}" {
		t.Errorf("a template that is not a JSON object is previewed, got %+v", out.EndOutput)
	}
}
