package cmd

import (
	"encoding/json"
	"fmt"
)

// bundleToApplySpec converts a /v2/workflows/{id}/export bundle into the FLAT
// spec shape that `workflows-v2 apply` consumes, so a live workflow can be
// edited and re-applied without hand-reconstructing the spec.
//
// The export bundle (see borrower-central
// app/usecase/workflows_v2/export_workflow.py) is shaped as:
//
//	{
//	  "workflow": {label, description, category, nodes[], edges[],
//	               inputVariables, customVariables, ...},
//	  "tasks":    [ {alias, type, label, ...full task body...}, ... ],
//	  "evaluationRules": [...], "scorecards": [...], ... (entity arrays)
//	}
//
// Each workflow node references its backing task by `taskAlias`. apply, by
// contrast, wants ONE flat node entry per graph node carrying the task body
// fields INLINE plus `type`, `label`, and a spec-local `ref` (see the
// composeSpec docs in workflows_v2_apply.go). bundleToApplySpec performs that
// inversion: it indexes tasks by alias, then for every workflow node merges
// the matching task body into the node entry.
//
// Ref recovery limitation: the bundle does not preserve the original
// spec-local refs the workflow was applied with -- it only carries
// server-assigned task aliases (slug-NNNNNN). We therefore emit each node's
// server alias as its `ref`. apply tolerates this: rewriteRefsInMappings /
// rewriteRefsInTemplate leave a head that is already a server alias untouched
// (isServerAlias), and the BC runtime resolves it against task_outputs. The
// round-trip is faithful; refs simply read like "fetch-ecu-a1b2c3" rather than
// "fetch". The credit-decisioning entity arrays (scorecards/ruleTrees/etc.)
// are NOT inlined -- apply references them by code from each task's *Config and
// re-scopes the live entities, so they are intentionally dropped from the spec.
func bundleToApplySpec(bundle json.RawMessage) (map[string]any, error) {
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
	// the spec node contract. `alias` becomes `ref`; type/label are set from
	// the node so they stay authoritative even if a task body lacks them.
	taskDropFields := map[string]bool{
		"alias":         true,
		"type":          true,
		"label":         true,
		"specRef":       true,
		"workflowAlias": true,
	}

	rawNodes, _ := b.Workflow["nodes"].([]any)
	specNodes := make([]map[string]any, 0, len(rawNodes))
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

		// Inline the backing task body (everything except start, which is
		// graph-only and has no task -- apply re-creates trivial backing
		// tasks for end/etc. itself).
		if taskAlias != "" {
			if body, ok := taskByAlias[taskAlias]; ok {
				for k, v := range body {
					if taskDropFields[k] {
						continue
					}
					entry[k] = v
				}
			}
		}

		// type/label/ref are authoritative from the node. ref prefers the
		// server task alias (stable, round-trips), then nodeId.
		entry["type"] = nodeType
		if label != "" {
			entry["label"] = label
		} else if l, _ := entry["label"].(string); l == "" {
			entry["label"] = nodeID
		}
		ref := taskAlias
		if ref == "" {
			ref = nodeID
		}
		entry["ref"] = ref

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

	// Edges: apply accepts sourceNodeId/targetNodeId directly and preserves
	// sourceHandle/label/id. Carry the bundle edges through, mapping to the
	// from/to shortcut shape apply documents while keeping handles.
	rawEdges, _ := b.Workflow["edges"].([]any)
	specEdges := make([]map[string]any, 0, len(rawEdges))
	for _, re := range rawEdges {
		edge, _ := re.(map[string]any)
		if edge == nil {
			continue
		}
		from, _ := edge["sourceNodeId"].(string)
		to, _ := edge["targetNodeId"].(string)
		out := map[string]any{}
		if from != "" {
			out["from"] = from
		}
		if to != "" {
			out["to"] = to
		}
		if sh, ok := edge["sourceHandle"]; ok && sh != nil {
			out["sourceHandle"] = sh
		}
		if th, ok := edge["targetHandle"]; ok && th != nil {
			out["targetHandle"] = th
		}
		if lbl, ok := edge["label"]; ok && lbl != nil {
			out["label"] = lbl
		}
		if id, ok := edge["id"]; ok && id != nil {
			out["id"] = id
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
