package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type exportRefMode int

const (
	// Stored specRef, else alias, else node id: a re-apply then matches every task by
	// (workflowAlias, specRef) and reports it unchanged.
	refFromSpecRef exportRefMode = iota
	// Alias, else node id: aliases survive version bumps, while a specRef only appears
	// once the CLI applies over a Hub-authored task.
	refFromAlias
)

// The credit-decisioning entity arrays are dropped on purpose: apply references them by
// code from each task's *Config and re-scopes the live entities.
func bundleToApplySpec(bundle json.RawMessage, mode exportRefMode) (map[string]any, error) {
	var b struct {
		SourceAlias string           `json:"sourceAlias"`
		Workflow    map[string]any   `json:"workflow"`
		Tasks       []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(bundle, &b); err != nil {
		return nil, fmt.Errorf("parse export bundle: %w", err)
	}
	if b.Workflow == nil {
		return nil, fmt.Errorf("export bundle has no .workflow object (is this a v2 export?)")
	}

	taskByAlias := make(map[string]map[string]any, len(b.Tasks))
	for _, t := range b.Tasks {
		if alias, _ := t["alias"].(string); alias != "" {
			taskByAlias[alias] = t
		}
	}

	taskDropFields := map[string]bool{
		"alias":         true,
		"type":          true,
		"label":         true,
		"specRef":       true,
		"workflowAlias": true,
	}

	rawNodes, _ := b.Workflow["nodes"].([]any)
	specNodes := make([]map[string]any, 0, len(rawNodes))
	refByNodeID := make(map[string]string, len(rawNodes))
	deprecatedNodes := map[string]bool{}
	for _, rn := range rawNodes {
		node, _ := rn.(map[string]any)
		if node == nil {
			continue
		}
		nodeType, _ := node["type"].(string)
		label, _ := node["label"].(string)
		nodeID, _ := node["nodeId"].(string)
		taskAlias, _ := node["taskAlias"].(string)
		if deprecatedTaskTypes[nodeType] {
			deprecatedNodes[nodeType] = true
		}

		entry := map[string]any{}
		body := taskByAlias[taskAlias]
		for k, v := range body {
			if taskDropFields[k] {
				continue
			}
			entry[k] = v
		}

		entry["type"] = nodeType
		if label != "" {
			entry["label"] = label
		} else if l, _ := entry["label"].(string); l == "" {
			entry["label"] = nodeID
		}
		ref := ""
		if mode == refFromSpecRef {
			ref, _ = body["specRef"].(string)
		}
		if ref == "" {
			ref = taskAlias
		}
		if ref == "" {
			ref = nodeID
		}
		entry["ref"] = ref
		if nodeID != "" {
			refByNodeID[nodeID] = ref
		}
		if taskAlias != "" {
			refByNodeID[taskAlias] = ref
		}

		// A pinned position keeps apply's auto-layout off, so the canvas survives the trip.
		if pos, ok := node["position"]; ok && pos != nil {
			entry["position"] = pos
		}

		// node.data.inputMappings is the canvas mirror; the task body's copy wins.
		if _, has := entry["inputMappings"]; !has {
			if data, _ := node["data"].(map[string]any); data != nil {
				if im, ok := data["inputMappings"]; ok {
					entry["inputMappings"] = im
				}
			}
		}

		specNodes = append(specNodes, entry)
	}

	// Only for an apply-spec: nobody re-applies a diff comparison.
	if mode == refFromSpecRef && len(deprecatedNodes) > 0 {
		fmt.Fprintf(os.Stderr,
			"# WARNING: this apply-spec carries DEPRECATED node type(s): %s. "+
				"Re-applying it over the workflow it came from carries them forward unchanged, but "+
				"applying it under a NEW alias is refused -- there they would be newly authored. "+
				"Replace them as you migrate. "+
				"Run 'altscore workflows-v2 schema-guide taskTypes' for the live palette.\n",
			strings.Join(sortedKeys(deprecatedNodes), ", "),
		)
	}

	rawEdges, _ := b.Workflow["edges"].([]any)
	specEdges := make([]map[string]any, 0, len(rawEdges))
	for _, re := range rawEdges {
		edge, _ := re.(map[string]any)
		if edge == nil {
			continue
		}
		out := map[string]any{}
		for _, side := range [][2]string{{"sourceNodeId", "from"}, {"targetNodeId", "to"}} {
			id, _ := edge[side[0]].(string)
			if id == "" {
				continue
			}
			ref, ok := refByNodeID[id]
			if !ok {
				return nil, fmt.Errorf("export bundle edge %v: %s %q matches no node", edge["id"], side[0], id)
			}
			out[side[1]] = ref
		}
		for _, k := range []string{"sourceHandle", "targetHandle", "label", "id"} {
			if v, ok := edge[k]; ok && v != nil {
				out[k] = v
			}
		}
		specEdges = append(specEdges, out)
	}

	spec := map[string]any{
		"nodes": specNodes,
		"edges": specEdges,
	}
	// sourceAlias makes a re-apply target the SAME workflow instead of minting a new one.
	if b.SourceAlias != "" {
		spec["alias"] = b.SourceAlias
	}
	for _, k := range []string{"label", "description", "category", "inputVariables", "customVariables"} {
		if v, ok := b.Workflow[k]; ok && v != nil {
			spec[k] = v
		}
	}
	return spec, nil
}
