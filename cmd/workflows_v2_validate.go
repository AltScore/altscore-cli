package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// validateWorkflowV2Body checks a workflow create/autosave/update body for
// the two failure modes that produce silent garbage:
//  1. Snake-case node/edge field names (id/source/target) -- API requires
//     camelCase aliases (nodeId/sourceNodeId/targetNodeId).
//  2. Orphan nodes (any type without taskAlias/taskId).
//
// EVERY node -- including start, end, and conditional -- must have a
// taskAlias. The Hub creates trivial backing tasks for start/end (type-only,
// no other config) so it can render them. A node without a taskAlias produces
// GET /v2/tasks/null -> 404 in the Hub UI.
//
// Returns a single error aggregating every problem found so the agent can
// fix everything at once instead of round-tripping per issue.
func validateWorkflowV2Body(body json.RawMessage) error {
	if len(body) == 0 {
		return nil
	}
	var wf map[string]any
	if err := json.Unmarshal(body, &wf); err != nil {
		// Not our problem -- the API will reject with a clear parse error.
		return nil
	}

	// Warn (don't block) when the body has an `alias` field. The server
	// derives the workflow's alias from `label` and silently drops any alias
	// supplied in the body -- both on create and on update. Surfacing this
	// here saves the trap where a caller stamps entities with the alias they
	// THOUGHT the workflow would have, only to find the slugifier picked a
	// different one. The compose dry-run preview also shows the slug, but
	// users go through `workflows-v2 update` after compose for ad-hoc edits
	// and hit the same drop silently.
	if v, has := wf["alias"]; has {
		if s, _ := v.(string); s != "" {
			fmt.Fprintf(os.Stderr,
				"# warning: body has \"alias\": %q -- the API will silently drop it. The workflow's alias is server-derived from `label` (slugified). Use `compose --dry-run` to preview the slug.\n",
				s)
		}
	}

	var problems []string

	if rawNodes, ok := wf["nodes"]; ok && rawNodes != nil {
		nodes, _ := rawNodes.([]any)
		for i, n := range nodes {
			nm, ok := n.(map[string]any)
			if !ok {
				continue
			}

			// Old field-name pitfalls
			if _, hasID := nm["id"]; hasID {
				if _, hasNodeID := nm["nodeId"]; !hasNodeID {
					problems = append(problems, fmt.Sprintf("nodes[%d]: uses 'id' -- the API requires 'nodeId'", i))
				}
			}

			nodeID, _ := nm["nodeId"].(string)
			label := nodeID
			if l, _ := nm["label"].(string); l != "" {
				label = l
			}

			nodeType, _ := nm["type"].(string)
			if nodeType == "" {
				problems = append(problems, fmt.Sprintf("nodes[%d] (%q): missing 'type'", i, label))
				continue
			}

			taskAlias, _ := nm["taskAlias"].(string)
			taskID, _ := nm["taskId"].(string)
			if taskAlias == "" && taskID == "" {
				problems = append(problems, fmt.Sprintf(
					"nodes[%d] (%q, type=%q): no taskAlias/taskId -- every node (incl. start/end/conditional) needs a backing task. "+
						"Use 'altscore workflows-v2 compose' which creates tasks for all nodes atomically.",
					i, label, nodeType))
			}
		}
	}

	if rawEdges, ok := wf["edges"]; ok && rawEdges != nil {
		edges, _ := rawEdges.([]any)
		for i, e := range edges {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if _, hasSrc := em["source"]; hasSrc {
				if _, ok := em["sourceNodeId"]; !ok {
					problems = append(problems, fmt.Sprintf("edges[%d]: uses 'source' -- the API requires 'sourceNodeId'", i))
				}
			}
			if _, hasTgt := em["target"]; hasTgt {
				if _, ok := em["targetNodeId"]; !ok {
					problems = append(problems, fmt.Sprintf("edges[%d]: uses 'target' -- the API requires 'targetNodeId'", i))
				}
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("workflow body validation failed:\n  - %s\n\nFor greenfield workflows prefer:\n  altscore workflows-v2 compose --body @spec.json --dry-run\nwhich creates the underlying /v2/tasks records and wires nodes to them automatically.",
		strings.Join(problems, "\n  - "))
}

// makeWfv2LintCmd inspects an existing workflow for the same set of issues
// validateWorkflowV2Body checks at write time, plus structural problems that
// only show up after save (orphan edges, duplicate node ids, missing start/end).
func makeWfv2LintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint <id>",
		Short: "Inspect an existing v2 workflow for orphan nodes, dangling edges, and missing start/end",
		Long: `Lint a saved workflow. Reports:
  - non-start/non-end nodes missing taskAlias/taskId (would fail to load in the Hub)
  - edges pointing at non-existent nodeIds
  - duplicate nodeIds
  - missing start or end nodes
  - tasks referenced by nodes that no longer exist on the tenant (best-effort)

Exits with non-zero status if any issue is found.`,
		Example: `  altscore workflows-v2 lint <id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+args[0], nil)
			if err != nil {
				return err
			}
			var wf map[string]any
			if err := json.Unmarshal(data, &wf); err != nil {
				return fmt.Errorf("parse workflow: %w", err)
			}

			report := lintWorkflowV2(wf)
			// Silent-PDF check: an end node with data-source ancestors but
			// endConfig.pdfConfig.enabled != true silently skips report
			// rendering. Caught only by reading each end node's backing task
			// body (the workflow body has nodes, not their config), so this
			// runs in RunE rather than inside the pure lintWorkflowV2.
			lintEndNodesPDF(c, wf, &report)
			// inputMappings namespace check: every value's head must be a
			// reserved scope (inputs/custom/system/task_outputs/...) or the
			// runtime resolver fails with "Unknown variable namespace" at
			// execute. Walks each task's persisted body via /v2/tasks/{alias},
			// since the workflow body's data.inputMappings mirror is only
			// populated for compose-built workflows.
			lintTaskInputMappingsNamespace(c, wf, &report)
			raw, _ := json.Marshal(report)
			if err := output.RawJSON(json.RawMessage(raw)); err != nil {
				return err
			}
			if len(report.Issues) > 0 {
				return fmt.Errorf("workflow has %d issue(s)", len(report.Issues))
			}
			return nil
		},
	}
}

// pdfDataSourceTaskTypes mirrors the Hub's auto-mapping list. End nodes with
// any ancestor of these types get their endConfig.pdfConfig auto-wired by
// compose; an end with such ancestors but pdfConfig.enabled != true is the
// "silent PDF" failure mode the user hit before commit 3d77e33.
var pdfDataSourceTaskTypes = map[string]bool{
	"altdata-enrichment": true,
	"scorecard":          true,
	"mapping-table":      true,
	"rule-tree":          true,
	"evaluate-rules":     true,
}

// lintEndNodesPDF appends warnings to the report for every end node whose
// backing task has data-source ancestors but doesn't have
// endConfig.pdfConfig.enabled=true. Best-effort: HTTP failures fall through
// silently so lint stays useful even when a single task GET 404s.
func lintEndNodesPDF(c interface {
	Do(method, module, path string, body any) (json.RawMessage, int, error)
}, wf map[string]any, report *lintReport) {
	if c == nil {
		return
	}
	nodes := asSlice(wf["nodes"])
	if len(nodes) == 0 {
		return
	}
	// Build parents map from edges so we can BFS upstream from each end node.
	parents := map[string][]string{}
	for _, e := range asSlice(wf["edges"]) {
		em, _ := e.(map[string]any)
		src, _ := em["sourceNodeId"].(string)
		tgt, _ := em["targetNodeId"].(string)
		if src == "" || tgt == "" {
			continue
		}
		parents[tgt] = append(parents[tgt], src)
	}
	// nodeId -> task type, for ancestor-type lookup.
	nodeType := map[string]string{}
	for _, n := range nodes {
		nm, _ := n.(map[string]any)
		id, _ := nm["nodeId"].(string)
		t, _ := nm["type"].(string)
		if id != "" {
			nodeType[id] = strings.ToLower(t)
		}
	}

	for _, n := range nodes {
		nm, _ := n.(map[string]any)
		id, _ := nm["nodeId"].(string)
		t, _ := nm["type"].(string)
		if strings.ToLower(t) != "end" || id == "" {
			continue
		}
		// Walk transitive ancestors; stop early if we find a data-source type.
		hasDataSource := false
		visited := map[string]bool{id: true}
		queue := append([]string{}, parents[id]...)
		for len(queue) > 0 && !hasDataSource {
			cur := queue[0]
			queue = queue[1:]
			if visited[cur] {
				continue
			}
			visited[cur] = true
			if pdfDataSourceTaskTypes[nodeType[cur]] {
				hasDataSource = true
				break
			}
			queue = append(queue, parents[cur]...)
		}
		if !hasDataSource {
			continue
		}
		// Read the end task's backing body to inspect endConfig.
		alias, _ := nm["taskAlias"].(string)
		if alias == "" {
			continue
		}
		taskData, _, err := c.Do("GET", "borrower_central", "/v2/tasks/"+alias, nil)
		if err != nil {
			continue
		}
		var task map[string]any
		if err := json.Unmarshal(taskData, &task); err != nil {
			continue
		}
		endCfg, _ := task["endConfig"].(map[string]any)
		pdfCfg, _ := endCfg["pdfConfig"].(map[string]any)
		enabled, _ := pdfCfg["enabled"].(bool)
		if !enabled {
			report.Issues = append(report.Issues, lintIssue{
				Severity: "warning",
				NodeID:   id,
				Message: fmt.Sprintf(
					"end node has data-source ancestors (PDF report would be useful) but endConfig.pdfConfig.enabled is not true on backing task %q -- PDF generation will silently skip. Re-compose with the latest CLI, or add the section/components manually via api POST /v2/tasks/%s.",
					alias, alias),
			})
			continue
		}
		// Even when enabled, an empty sourcesConfig produces no PDF.
		sources, _ := pdfCfg["sourcesConfig"].([]any)
		if len(sources) == 0 {
			report.Issues = append(report.Issues, lintIssue{
				Severity: "warning",
				NodeID:   id,
				Message: fmt.Sprintf(
					"end node %q has endConfig.pdfConfig.enabled=true but sourcesConfig is empty -- PDF will render with no sections. Add at least one section.",
					alias),
			})
		}
	}
}

// runtimeReservedScopes are the leading namespace tokens the v2 runtime
// resolver accepts in inputMappings values. Anything else fails at execute
// time with "Unknown variable namespace: <head>". Mirrors
// reservedMappingScopes in cmd/workflows_v2_compose.go -- if you add to one,
// add to the other.
var runtimeReservedScopes = map[string]bool{
	"inputs":               true,
	"custom":               true,
	"system":               true,
	"task_outputs":         true,
	"task_outputs_by_type": true,
}

// lintTaskInputMappingsNamespace fetches each task's persisted body and
// flags any inputMappings value whose head segment isn't a reserved runtime
// namespace. This catches workflows where rewriteRefsInMappings produced
// (or pre-fix compose persisted) bare "<task-alias>.<output>" references --
// they survive lint's structural pass but blow up on execute with "Unknown
// variable namespace: <task-alias>".
//
// Best-effort: HTTP failures fall through silently so a single 404 doesn't
// mask other issues.
func lintTaskInputMappingsNamespace(c interface {
	Do(method, module, path string, body any) (json.RawMessage, int, error)
}, wf map[string]any, report *lintReport) {
	if c == nil {
		return
	}
	for _, n := range asSlice(wf["nodes"]) {
		nm, _ := n.(map[string]any)
		nodeID, _ := nm["nodeId"].(string)
		nodeType, _ := nm["type"].(string)
		alias, _ := nm["taskAlias"].(string)
		if nodeID == "" || alias == "" {
			continue
		}
		taskData, _, err := c.Do("GET", "borrower_central", "/v2/tasks/"+alias, nil)
		if err != nil {
			continue
		}
		var task map[string]any
		if err := json.Unmarshal(taskData, &task); err != nil {
			continue
		}
		mappings, _ := task["inputMappings"].(map[string]any)
		for k, v := range mappings {
			s, _ := v.(string)
			dot := strings.Index(s, ".")
			if dot <= 0 {
				continue
			}
			head := s[:dot]
			if runtimeReservedScopes[head] {
				continue
			}
			report.Issues = append(report.Issues, lintIssue{
				Severity: "error",
				NodeID:   nodeID,
				Message: fmt.Sprintf(
					"type=%q task=%q inputMappings[%q]=%q has head %q which is not a valid runtime namespace. "+
						"Cross-task references must use task_outputs.<server-alias>.<output-field>; the runtime resolver fails at execute time with 'Unknown variable namespace: %s'. "+
						"Re-compose with the latest CLI (which topologically orders task creation and rewrites bare refs to the long form), or PATCH the task via /v2/tasks/%s with the corrected mapping.",
					nodeType, alias, k, s, head, head, alias),
			})
		}
	}
}

type lintIssue struct {
	Severity string `json:"severity"` // "error" or "warning"
	NodeID   string `json:"nodeId,omitempty"`
	EdgeID   string `json:"edgeId,omitempty"`
	Message  string `json:"message"`
}

type lintReport struct {
	WorkflowID string      `json:"workflowId"`
	Alias      string      `json:"alias,omitempty"`
	Status     string      `json:"status,omitempty"`
	NodeCount  int         `json:"nodeCount"`
	EdgeCount  int         `json:"edgeCount"`
	Issues     []lintIssue `json:"issues"`
}

func lintWorkflowV2(wf map[string]any) lintReport {
	report := lintReport{Issues: []lintIssue{}}
	report.WorkflowID, _ = wf["id"].(string)
	report.Alias, _ = wf["alias"].(string)
	report.Status, _ = wf["status"].(string)

	nodes := asSlice(wf["nodes"])
	edges := asSlice(wf["edges"])
	report.NodeCount = len(nodes)
	report.EdgeCount = len(edges)

	seenIDs := map[string]int{}
	hasStart, hasEnd := false, false

	for i, n := range nodes {
		nm, _ := n.(map[string]any)
		id, _ := nm["nodeId"].(string)
		if id == "" {
			report.Issues = append(report.Issues, lintIssue{
				Severity: "error",
				Message:  fmt.Sprintf("nodes[%d]: missing nodeId", i),
			})
			continue
		}
		seenIDs[id]++
		nodeType := strings.ToLower(fmt.Sprint(nm["type"]))
		if nodeType == "start" {
			hasStart = true
		}
		if nodeType == "end" {
			hasEnd = true
		}
		ta, _ := nm["taskAlias"].(string)
		tid, _ := nm["taskId"].(string)
		if ta == "" && tid == "" {
			report.Issues = append(report.Issues, lintIssue{
				Severity: "error",
				NodeID:   id,
				Message:  fmt.Sprintf("type=%q has no taskAlias/taskId -- Hub will hit GET /v2/tasks/null", nodeType),
			})
		}
	}

	for id, count := range seenIDs {
		if count > 1 {
			report.Issues = append(report.Issues, lintIssue{
				Severity: "error",
				NodeID:   id,
				Message:  fmt.Sprintf("duplicate nodeId (%d occurrences)", count),
			})
		}
	}

	if !hasStart {
		report.Issues = append(report.Issues, lintIssue{Severity: "warning", Message: "no node of type 'start'"})
	}
	if !hasEnd {
		report.Issues = append(report.Issues, lintIssue{Severity: "warning", Message: "no node of type 'end'"})
	}

	// Track edge endpoints so we can detect truly disconnected nodes (orphans).
	hasIncoming := map[string]bool{}
	hasOutgoing := map[string]bool{}

	for i, e := range edges {
		em, _ := e.(map[string]any)
		eid, _ := em["id"].(string)
		src, _ := em["sourceNodeId"].(string)
		tgt, _ := em["targetNodeId"].(string)
		if src == "" {
			report.Issues = append(report.Issues, lintIssue{Severity: "error", EdgeID: eid, Message: fmt.Sprintf("edges[%d]: missing sourceNodeId", i)})
		} else if _, ok := seenIDs[src]; !ok {
			report.Issues = append(report.Issues, lintIssue{Severity: "error", EdgeID: eid, Message: fmt.Sprintf("sourceNodeId %q does not match any node", src)})
		} else {
			hasOutgoing[src] = true
		}
		if tgt == "" {
			report.Issues = append(report.Issues, lintIssue{Severity: "error", EdgeID: eid, Message: fmt.Sprintf("edges[%d]: missing targetNodeId", i)})
		} else if _, ok := seenIDs[tgt]; !ok {
			report.Issues = append(report.Issues, lintIssue{Severity: "error", EdgeID: eid, Message: fmt.Sprintf("targetNodeId %q does not match any node", tgt)})
		} else {
			hasIncoming[tgt] = true
		}
	}

	// Flag orphan nodes (no edges in OR out). Start nodes are exempt from the
	// incoming-edge requirement; end nodes are exempt from outgoing. Comment
	// nodes are decorative -- the Hub allows them anywhere -- but a fully
	// disconnected comment is dead weight; flag it as a 'warning' (not error)
	// so agents see it but it doesn't block compose.
	for _, n := range nodes {
		nm, _ := n.(map[string]any)
		id, _ := nm["nodeId"].(string)
		nodeType := strings.ToLower(fmt.Sprint(nm["type"]))
		if id == "" {
			continue
		}
		incoming, outgoing := hasIncoming[id], hasOutgoing[id]
		needsIn := nodeType != "start"
		needsOut := nodeType != "end" && nodeType != "exception"
		// Identify by id+type so a workflow with multiple nodes of the same
		// type (common for multi-source altdata-enrichment, multi-step
		// compute-variables, etc.) lets the operator click the right node.
		nodeRef := fmt.Sprintf("nodeId=%q type=%q", id, nodeType)
		if !incoming && !outgoing {
			sev := "warning"
			if nodeType == "comment" {
				sev = "warning" // comments are decorative; warn but don't block
			} else {
				sev = "error" // any non-comment orphan is a real problem
			}
			report.Issues = append(report.Issues, lintIssue{
				Severity: sev,
				NodeID:   id,
				Message:  fmt.Sprintf("%s is fully disconnected (no edges in or out) -- unreachable", nodeRef),
			})
			continue
		}
		if needsIn && !incoming {
			sev := "warning"
			if nodeType != "comment" {
				sev = "error"
			}
			report.Issues = append(report.Issues, lintIssue{
				Severity: sev,
				NodeID:   id,
				Message:  fmt.Sprintf("%s has no incoming edge -- unreachable from start", nodeRef),
			})
		}
		if needsOut && !outgoing {
			sev := "warning"
			if nodeType != "comment" {
				sev = "error"
			}
			report.Issues = append(report.Issues, lintIssue{
				Severity: sev,
				NodeID:   id,
				Message:  fmt.Sprintf("%s has no outgoing edge -- execution will dead-end here", nodeRef),
			})
		}
	}

	return report
}
