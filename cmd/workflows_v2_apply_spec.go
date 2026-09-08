package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The apply spec: its typed shape, legacy-shape detection and per-node helpers.

type composeSpec struct {
	Label    string `json:"label"`
	Alias    string `json:"alias,omitempty"`
	Category string `json:"category"`
	// Pointer so an explicit "" (blank the description) is distinguishable from
	// an omitted field (leave the existing description untouched on update).
	Description     *string        `json:"description,omitempty"`
	Status          string         `json:"status,omitempty"`
	InputVariables  map[string]any `json:"inputVariables,omitempty"`
	CustomVariables map[string]any `json:"customVariables,omitempty"`
	Config          map[string]any `json:"config,omitempty"`
	// Nodes is the workflow's graph -- one flat list, one entry per node
	// (start, end, every task type). Apply dispatches each entry by `type`
	// at parse time. This is the only accepted input shape.
	//
	// The legacy two-bucket shape (`tasks[]` + `extraNodes[]`) was removed:
	// it half-worked, with fields like inputMappings / endConfig /
	// htmlSections on extraNodes entries getting silently stripped or only
	// partially honored depending on the field. detectLegacySpecShape()
	// catches that input early and emits a one-shot rewrite suggestion.
	Nodes []map[string]any `json:"nodes,omitempty"`
	Edges []map[string]any `json:"edges"`
	Notes []map[string]any `json:"notes,omitempty"`

	// Tasks and ExtraNodes are INTERNAL buckets used by the downstream
	// build pipeline -- populated by splitting Nodes at parse time.
	// No JSON tags: never read from user input. The internal split is
	// type=="start" -> ExtraNodes (graph-only); everything else
	// (including end) -> Tasks (backing task created). Renaming these
	// to taskNodes/graphOnlyNodes is a future cleanup; the user-facing
	// contract (Nodes) is what matters here.
	Tasks      []map[string]any `json:"-"`
	ExtraNodes []map[string]any `json:"-"`

	// ExistingNodeTypes is INTERNAL: the set of node types the apply TARGET
	// already carries, filled by apply from the workflow it looked up by alias
	// (nil on the create path, and nil when that lookup failed). No JSON tag:
	// never read from user input.
	//
	// Only the deprecation gate reads it, and only to mirror the backend's
	// diff-based rule -- a retired type already in the stored graph is a
	// carry-forward, not new authoring (see deprecatedTaskTypeRefused). The
	// zero value therefore has to be the STRICT one: a code path that forgets
	// to fill this refuses every deprecated type, exactly as before.
	ExistingNodeTypes map[string]bool `json:"-"`
}

// detectLegacySpecShape rejects specs that use the removed
// `tasks[]` + `extraNodes[]` two-bucket shape. Returns an error with a
// concrete `nodes[]` rewrite when either key is present and non-empty.
// Called pre-unmarshal so the message can cite the user's input verbatim.
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

// localRef returns the spec-local reference for a task or extraNode entry,
// in priority order: explicit `ref`, then `alias` (for tasks) or `nodeId`
// (for nodes), falling back to the supplied default.
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

// edgeEndpoints reads the source/target ref or nodeId of an edge entry,
// preferring the spec-side `from`/`to` shortcuts.
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

// readSpecHTMLSections extracts the spec-only `htmlSections` field from an
// extraNode. Returns nil when the field is absent or shaped wrong (compose
// silently ignores malformed entries; the spec validator would have caught
// truly broken JSON before we got here).
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
