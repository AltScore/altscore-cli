package cmd

import (
	"encoding/json"
	"fmt"
)

// exportRefMode picks the identity a flattened node's `ref` carries.
type exportRefMode int

const (
	// refFromSpecRef emits the task's stored specRef, else its alias, else the
	// node id. `export --format apply-spec` uses it so a re-apply matches every
	// task by (workflowAlias, specRef) and reports it unchanged. A task without
	// a specRef (authored in the Hub) falls back to its alias and re-applies as
	// a new task until the server learns to adopt a task by its alias.
	refFromSpecRef exportRefMode = iota
	// refFromAlias emits the task alias, else the node id. `diff <a> <b>` uses
	// it: aliases survive version bumps, while a specRef first appears when the
	// CLI applies over a Hub-authored task, so alias identity keeps a node
	// matched across that boundary.
	refFromAlias
)

// bundleToApplySpec converts a /v2/workflows/{id}/export bundle into the FLAT
// spec shape that `workflows-v2 apply` consumes, so a live workflow can be
// edited and re-applied without hand-reconstructing the spec.
//
// The export bundle (see borrower-central
// app/usecase/workflows_v2/export_workflow.py) is shaped as:
//
//	{
//	  "sourceAlias": "...",
//	  "workflow": {label, description, category, nodes[], edges[],
//	               inputVariables, customVariables, ...},
//	  "tasks":    [ {alias, specRef?, type, label, ...full task body...}, ... ],
//	  "evaluationRules": [...], "scorecards": [...], ... (entity arrays)
//	}
//
// Each workflow node references its backing task by `taskAlias`. apply, by
// contrast, wants ONE flat node entry per graph node carrying the task body
// fields INLINE plus `type`, `label`, `position` and a spec-local `ref` (see
// the composeSpec docs in workflows_v2_apply.go). bundleToApplySpec performs
// that inversion: it indexes tasks by alias, then for every workflow node
// merges the matching task body into the node entry.
//
// Ref recovery: a task the CLI applied carries its original spec-local ref as
// `specRef`, and the server identifies tasks by (workflowAlias, specRef). In
// refFromSpecRef mode that value becomes the node's `ref`, so exporting a
// workflow and applying the result reports every task unchanged. Edges in the
// bundle name node ids, which are not always the alias (the Hub assigns its
// own), so every endpoint is mapped through the node it points at; an endpoint
// that matches no node is an error rather than a dangling edge.
//
// References inside bodies (`task_outputs.<alias>...`) are left as they are:
// the alias is stable across version bumps and the server resolves it, so the
// round trip stays faithful even though refs and bodies name the same task
// differently. The credit-decisioning entity arrays (scorecards / ruleTrees /
// ...) are NOT inlined -- apply references them by code from each task's
// *Config and re-scopes the live entities, so they are intentionally dropped
// from the spec.
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

	// Index tasks by their server alias so each node can pull its body.
	taskByAlias := make(map[string]map[string]any, len(b.Tasks))
	for _, t := range b.Tasks {
		if alias, _ := t["alias"].(string); alias != "" {
			taskByAlias[alias] = t
		}
	}

	// Fields on a task body that are bundle/identity bookkeeping, not part of
	// the spec node contract. `specRef`/`alias` become `ref`; type/label are set
	// from the node so they stay authoritative even if a task body lacks them.
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
	for _, rn := range rawNodes {
		node, _ := rn.(map[string]any)
		if node == nil {
			continue
		}
		nodeType, _ := node["type"].(string)
		label, _ := node["label"].(string)
		nodeID, _ := node["nodeId"].(string)
		taskAlias, _ := node["taskAlias"].(string)

		entry := map[string]any{}
		body := taskByAlias[taskAlias]
		for k, v := range body {
			if taskDropFields[k] {
				continue
			}
			entry[k] = v
		}

		// type/label/ref are authoritative from the node.
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

		// A pinned position keeps apply's auto-layout off, so the canvas
		// survives the round trip.
		if pos, ok := node["position"]; ok && pos != nil {
			entry["position"] = pos
		}

		// node.data.inputMappings is the canvas mirror of the task body's
		// inputMappings. Prefer the task body's copy (already inlined above);
		// fall back to the node's mirror when the task body didn't carry one.
		if _, has := entry["inputMappings"]; !has {
			if data, _ := node["data"].(map[string]any); data != nil {
				if im, ok := data["inputMappings"]; ok {
					entry["inputMappings"] = im
				}
			}
		}

		specNodes = append(specNodes, entry)
	}

	// Edges: the bundle names node ids, the spec names refs. Map every endpoint
	// through the node it points at; handles, label and id travel unchanged.
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
	// alias: prefer the bundle's sourceAlias (the live workflow's alias) so a
	// re-apply targets the SAME workflow (update path) rather than minting a
	// new one. The export workflow object itself strips workflowAlias.
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
