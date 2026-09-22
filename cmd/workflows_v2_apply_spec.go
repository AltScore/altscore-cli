package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

type composeSpec struct {
	Label    string `json:"label"`
	Alias    string `json:"alias,omitempty"`
	Category string `json:"category"`
	// Pointer so an explicit "" (blank the description) is distinguishable from
	// an omitted field (leave the existing description untouched on update).
	Description     *string          `json:"description,omitempty"`
	Status          string           `json:"status,omitempty"`
	InputVariables  map[string]any   `json:"inputVariables,omitempty"`
	CustomVariables map[string]any   `json:"customVariables,omitempty"`
	Config          map[string]any   `json:"config,omitempty"`
	Nodes           []map[string]any `json:"nodes,omitempty"`
	Edges           []map[string]any `json:"edges"`
	Notes           []map[string]any `json:"notes,omitempty"`

	// Internal buckets filled by splitting Nodes (type=="start" -> ExtraNodes, the rest
	// -> Tasks). No JSON tags: never read from user input.
	Tasks      []map[string]any `json:"-"`
	ExtraNodes []map[string]any `json:"-"`

	// Node types the apply TARGET already carries, nil on create. Only the deprecation
	// gate reads it, so the zero value has to be the strict one.
	ExistingNodeTypes map[string]bool `json:"-"`
}

func detectLegacySpecShape(body []byte) error {
	var peek map[string]json.RawMessage
	if err := json.Unmarshal(body, &peek); err != nil {
		// Let the caller's main Unmarshal produce the proper parse error.
		return nil
	}
	countArray := func(key string) int {
		raw, ok := peek[key]
		if !ok {
			return 0
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return 0
		}
		return len(arr)
	}
	nTasks := countArray("tasks")
	nExtra := countArray("extraNodes")
	if nTasks == 0 && nExtra == 0 {
		return nil
	}
	var got []string
	if nTasks > 0 {
		got = append(got, fmt.Sprintf("tasks[] (%d entries)", nTasks))
	}
	if nExtra > 0 {
		got = append(got, fmt.Sprintf("extraNodes[] (%d entries)", nExtra))
	}
	return fmt.Errorf(`spec uses the removed two-bucket shape: %s.
the `+"`tasks[]`"+` and `+"`extraNodes[]`"+` keys were removed -- use the flat `+"`nodes[]`"+` shape instead.

how to migrate (per-entry bodies are unchanged, only the wrapping key changes):

  before:
    {
      "tasks":      [{"ref":"fetch","type":"altdata-enrichment","...":"..."},
                     {"ref":"score","type":"scorecard","...":"..."}],
      "extraNodes": [{"ref":"start","type":"start","label":"Start"},
                     {"ref":"end","type":"end","endConfig":{"...":"..."}}]
    }

  after:
    {
      "nodes": [
        {"ref":"start","type":"start","label":"Start"},
        {"ref":"fetch","type":"altdata-enrichment","...":"..."},
        {"ref":"score","type":"scorecard","...":"..."},
        {"ref":"end","type":"end","endConfig":{"...":"..."}}
      ]
    }

order inside nodes[] is cosmetic; edges still drive execution order.
move every entry verbatim -- ref, type, label, inputMappings, endConfig,
htmlSections, sourcesConfig, every field carries over unchanged`,
		strings.Join(got, " and "))
}

func localRef(entry map[string]any, fallback string) string {
	if v, _ := entry["ref"].(string); v != "" {
		return v
	}
	if v, _ := entry["alias"].(string); v != "" {
		return v
	}
	if v, _ := entry["nodeId"].(string); v != "" {
		return v
	}
	return fallback
}

func edgeEndpoints(e map[string]any) (from, to string) {
	from, _ = e["from"].(string)
	if from == "" {
		from, _ = e["sourceNodeId"].(string)
	}
	to, _ = e["to"].(string)
	if to == "" {
		to, _ = e["targetNodeId"].(string)
	}
	return from, to
}

func readSpecHTMLSections(node map[string]any) []map[string]any {
	raw, ok := node["htmlSections"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
