package cmd

import (
	"encoding/json"
	"sort"
)

// workflowOutline is what a sibling or a parent needs to know about a workflow: its inputs,
// graph, variables and what its end node returns. The apply-spec inlines every task body and
// ran from 30 KB to over 400 KB.
type workflowOutline struct {
	Alias           any                       `json:"alias,omitempty"`
	Label           any                       `json:"label,omitempty"`
	Category        any                       `json:"category,omitempty"`
	InputVariables  map[string]map[string]any `json:"inputVariables"`
	CustomVariables map[string]any            `json:"customVariables"`
	Nodes           []outlineNode             `json:"nodes"`
	Edges           []outlineEdge             `json:"edges"`
	EndOutput       []outlineEndOutput        `json:"endOutput,omitempty"`
}

type outlineNode struct {
	Ref   any `json:"ref"`
	Type  any `json:"type"`
	Label any `json:"label,omitempty"`
}

type outlineEdge struct {
	From         any `json:"from"`
	To           any `json:"to"`
	SourceHandle any `json:"sourceHandle,omitempty"`
}

// outputJsonKeys are the top-level keys of the end node's custom output (what a parent reads
// from this workflow); outputJsonPreview stands in when outputJson is not a JSON object.
type outlineEndOutput struct {
	Ref                   any      `json:"ref"`
	OutputJSONKeys        []string `json:"outputJsonKeys,omitempty"`
	OutputJSONPreview     string   `json:"outputJsonPreview,omitempty"`
	StandardOutputEnabled bool     `json:"standardOutputEnabled,omitempty"`
}

func applySpecOutline(spec map[string]any) workflowOutline {
	out := workflowOutline{
		Alias:           spec["alias"],
		Label:           spec["label"],
		Category:        spec["category"],
		InputVariables:  map[string]map[string]any{},
		CustomVariables: map[string]any{},
		Nodes:           []outlineNode{},
		Edges:           []outlineEdge{},
	}
	if vars, ok := spec["inputVariables"].(map[string]any); ok {
		for name, v := range vars {
			m, _ := v.(map[string]any)
			kept := map[string]any{}
			for _, k := range []string{"type", "required", "title", "default"} {
				if val, ok := m[k]; ok && val != nil && val != "" {
					kept[k] = val
				}
			}
			out.InputVariables[name] = kept
		}
	}
	if vars, ok := spec["customVariables"].(map[string]any); ok {
		for name, v := range vars {
			m, _ := v.(map[string]any)
			out.CustomVariables[name] = m["type"]
		}
	}
	for _, n := range asMapSlice(spec["nodes"]) {
		out.Nodes = append(out.Nodes, outlineNode{Ref: n["ref"], Type: n["type"], Label: n["label"]})
		if n["type"] != "end" {
			continue
		}
		end := outlineEndOutput{Ref: n["ref"]}
		cfg, _ := n["endConfig"].(map[string]any)
		if std, ok := cfg["standardOutput"].(map[string]any); ok {
			end.StandardOutputEnabled, _ = std["enabled"].(bool)
		}
		if raw, ok := cfg["outputJson"].(string); ok && raw != "" {
			var obj map[string]any
			if json.Unmarshal([]byte(raw), &obj) == nil {
				for k := range obj {
					end.OutputJSONKeys = append(end.OutputJSONKeys, k)
				}
				sort.Strings(end.OutputJSONKeys)
			} else {
				end.OutputJSONPreview = guideSnippet(raw, "", 200)
			}
		}
		out.EndOutput = append(out.EndOutput, end)
	}
	for _, e := range asMapSlice(spec["edges"]) {
		out.Edges = append(out.Edges, outlineEdge{From: e["from"], To: e["to"], SourceHandle: e["sourceHandle"]})
	}
	return out
}
