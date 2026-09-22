package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// EVERY node -- start, end and conditional included -- needs a taskAlias: the Hub renders a
// node without one as GET /v2/tasks/null. Short field names are renamed in place, not rejected.
func validateWorkflowV2Body(body *json.RawMessage) error {
	if body == nil || len(*body) == 0 {
		return nil
	}
	var wf map[string]any
	if err := json.Unmarshal(*body, &wf); err != nil {
		// Not our problem -- the API will reject with a clear parse error.
		return nil
	}

	var problems []string
	mutated := false

	endCount := 0

	if rawNodes, ok := wf["nodes"]; ok && rawNodes != nil {
		nodes, _ := rawNodes.([]any)
		for i, n := range nodes {
			nm, ok := n.(map[string]any)
			if !ok {
				continue
			}

			if renamed, err := renameToCamel(nm, "id", "nodeId", fmt.Sprintf("nodes[%d]", i)); err != nil {
				problems = append(problems, err.Error())
			} else if renamed {
				mutated = true
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
			if strings.ToLower(nodeType) == "end" {
				endCount++
			}

			taskAlias, _ := nm["taskAlias"].(string)
			taskID, _ := nm["taskId"].(string)
			if taskAlias == "" && taskID == "" {
				problems = append(problems, fmt.Sprintf(
					"nodes[%d] (%q, type=%q): no taskAlias/taskId -- every node (incl. start/end/conditional) needs a backing task. "+
						"Use 'altscore workflows-v2 apply' which creates tasks for all nodes atomically.",
					i, label, nodeType))
			}
		}
	}

	if endCount > 1 {
		problems = append(problems, fmt.Sprintf(
			"%d 'end' nodes -- a workflow must have exactly one end node; converge all paths (conditional branches, relationship handles) to a single end",
			endCount))
	}

	if rawEdges, ok := wf["edges"]; ok && rawEdges != nil {
		edges, _ := rawEdges.([]any)
		for i, e := range edges {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if renamed, err := renameToCamel(em, "source", "sourceNodeId", fmt.Sprintf("edges[%d]", i)); err != nil {
				problems = append(problems, err.Error())
			} else if renamed {
				mutated = true
			}
			if renamed, err := renameToCamel(em, "target", "targetNodeId", fmt.Sprintf("edges[%d]", i)); err != nil {
				problems = append(problems, err.Error())
			} else if renamed {
				mutated = true
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("workflow body validation failed:\n  - %s\n\nPrefer the declarative path:\n  altscore workflows-v2 apply --body @spec.json --dry-run\nwhich creates the underlying /v2/tasks records, wires nodes to them, and detects create-vs-update automatically.",
			strings.Join(problems, "\n  - "))
	}

	if mutated {
		rewritten, err := json.Marshal(wf)
		if err != nil {
			return fmt.Errorf("re-encode normalized workflow body: %w", err)
		}
		*body = json.RawMessage(rewritten)
	}
	return nil
}

func renameToCamel(m map[string]any, shortKey, camelKey, context string) (bool, error) {
	shortVal, hasShort := m[shortKey]
	if !hasShort {
		return false, nil
	}
	camelVal, hasCamel := m[camelKey]
	if hasCamel {
		if fmt.Sprint(shortVal) != fmt.Sprint(camelVal) {
			return false, fmt.Errorf(
				"%s: both %q and %q are present with conflicting values (%v vs %v) -- keep only %q",
				context, shortKey, camelKey, shortVal, camelVal, camelKey)
		}
		delete(m, shortKey)
		return true, nil
	}
	m[camelKey] = shortVal
	delete(m, shortKey)
	return true, nil
}

// The oracle owns every reference-integrity rule, so a local-only lint reports as clean
// workflows the Hub flags.
func makeWfv2LintCmd() *cobra.Command {
	var localOnly bool
	cmd := &cobra.Command{
		Use:   "lint <id>",
		Short: "Inspect an existing v2 workflow: the server validation oracle plus local structural checks",
		Long: `Lint a saved workflow.

From the server oracle (POST /v2/workflows/validate, one call, task bodies
resolved server-side from the persisted repository honoring this version's
status):
  - references to a task alias that is not in the graph, or not upstream
    (inputMappings, entity fields, PDF sources, PDF HTML body, standard output,
    outputJson, task config)
  - conditional branch / edge handle coherence, per-item handle mismatches
  - input variables never consumed, source inputs never fed, identity keys not
    mapped or not registered

Locally (the checks the oracle does not make):
  - ANY node missing taskAlias/taskId, start and end included (would fail to load in the Hub)
  - edges with an empty or unmatched sourceNodeId/targetNodeId
  - duplicate nodeIds
  - orphan nodes, and nodes with no incoming or no outgoing edge
  - missing start / end, or more than one end

Each issue carries "source": "server" or "local", and a server finding also
carries its "code". A local finding is dropped when the oracle already reported
the same thing, so the two sources never double-report.

"serverValidation" is "ok", "skipped" (oracle unreachable -- older backend,
offline, or a 5xx; the local checks still ran) or "local-only" (--local-only).
"skippedNodeIds" names nodes whose task body the oracle could not resolve: a
clean report that skipped nodes is not a clean bill of health.

Separately, and to stderr only, lint emits a HANDOFF READABILITY advisory: the
things that decide whether the analyst who inherits this workflow can read it.
A workflow that trips all of them runs identically, which is why nothing else
catches them:
  - custom variables with no "title" (the Hub falls back to the raw name)
  - custom variables whose whole output vocabulary is a small integer code
    (2/1/0/-1) -- the variable has already made the decision, in a vocabulary
    only its author can read. The usual source is a v1 port carrying its
    *_indicator fields across unchanged
  - PDF sections with no title, and htmlBlock bodies that are a single
    {placeholder} fed by a Python expression (the markup is not editable
    without Python)
  - evaluation rules whose description is blank or records the rule's
    provenance ("Ported from ...") rather than what it checks
Findings are aggregated -- one line per practice with a capped sample, never
one line per variable. It NEVER contributes an issue and never moves the exit
code. See 'altscore workflows-v2 schema-guide handoffReadability'.

Exits with non-zero status if any issue is found, from either source. Pass
--local-only for the pre-#101 behaviour.`,
		Example: `  altscore workflows-v2 lint <id>
  altscore workflows-v2 lint <id> --local-only`,
		Args: cobra.ExactArgs(1),
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
			switch {
			case localOnly:
				report.ServerValidation = lintServerLocalOnly
			default:
				resp, reason := fetchWorkflowValidation(c, wf)
				if resp == nil {
					// Fail open: the oracle's health never turns lint into an
					// error of its own.
					report.ServerValidation = lintServerSkipped
					dimNote(cmd.ErrOrStderr(), "server validation skipped: "+reason+"; reporting local checks only")
					break
				}
				report.ServerValidation = lintServerOK
				report.SkippedNodeIDs = resp.SkippedNodeIDs
				mergeServerFindings(&report, resp.Findings)
			}
			if cv, ok := wf["customVariables"].(map[string]any); ok {
				adviseExtractionProbes(cv, asSlice(wf["nodes"]))
			}
			wfAlias, _ := wf["alias"].(string)
			adviseHandoffReadability(wf,
				fetchEndTaskBodies(c, asSlice(wf["nodes"])),
				fetchWorkflowRules(c, wfAlias),
				cmd.ErrOrStderr())
			raw, _ := json.Marshal(report)
			if err := output.RawJSON(json.RawMessage(raw)); err != nil {
				return err
			}
			if len(report.Issues) > 0 {
				errs := 0
				for _, issue := range report.Issues {
					if issue.Severity == "error" {
						errs++
					}
				}
				return fmt.Errorf("workflow has %d issue(s): %d error(s), %d warning(s)",
					len(report.Issues), errs, len(report.Issues)-errs)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&localOnly, "local-only", false,
		"skip the server validation oracle and report only the local structural checks. "+
			"The oracle owns every reference-integrity rule, so this hides dangling task refs, "+
			"unfed source inputs and unconsumed input variables -- use it only when a script "+
			"cannot tolerate the wider finding set")
	return cmd
}

const (
	lintServerOK        = "ok"
	lintServerSkipped   = "skipped"
	lintServerLocalOnly = "local-only"
)

type lintIssue struct {
	Severity string `json:"severity"` // "error" or "warning"
	// "local" (this binary's structural pass) or "server" (a /v2/workflows/validate finding).
	Source string `json:"source"`
	// The server finding code (TASK_REFERENCE_NOT_IN_GRAPH, ...); empty on a local issue.
	Code   string `json:"code,omitempty"`
	NodeID string `json:"nodeId,omitempty"`
	EdgeID string `json:"edgeId,omitempty"`
	// The code the oracle emits for this same problem, "" when the check is local-only. Not
	// serialized: mergeServerFindings uses it to drop the local copy when the oracle answered.
	serverCode string
	Message    string `json:"message"`
}

type lintReport struct {
	WorkflowID string `json:"workflowId"`
	Alias      string `json:"alias,omitempty"`
	Status     string `json:"status,omitempty"`
	NodeCount  int    `json:"nodeCount"`
	EdgeCount  int    `json:"edgeCount"`
	// "ok", "skipped" or "local-only" -- never absent, so a clean report is always
	// distinguishable from one whose oracle never ran.
	ServerValidation string `json:"serverValidation"`
	// Nodes whose task body the oracle could not resolve, and therefore did not check.
	SkippedNodeIDs []string    `json:"skippedNodeIds,omitempty"`
	Issues         []lintIssue `json:"issues"`
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
	endCount := 0

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
			endCount++
		}
		ta, _ := nm["taskAlias"].(string)
		tid, _ := nm["taskId"].(string)
		if ta == "" && tid == "" {
			report.Issues = append(report.Issues, lintIssue{
				Severity:   "error",
				NodeID:     id,
				serverCode: "NODE_MISSING_TASK_REFERENCE",
				Message:    fmt.Sprintf("type=%q has no taskAlias/taskId -- Hub will hit GET /v2/tasks/null", nodeType),
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
		report.Issues = append(report.Issues, lintIssue{Severity: "warning", serverCode: "MISSING_START_NODE", Message: "no node of type 'start'"})
	}
	if !hasEnd {
		report.Issues = append(report.Issues, lintIssue{Severity: "warning", serverCode: "NO_END_NODES", Message: "no node of type 'end'"})
	}
	if endCount > 1 {
		report.Issues = append(report.Issues, lintIssue{
			Severity:   "error",
			serverCode: "MULTIPLE_END_NODES",
			Message:    fmt.Sprintf("%d 'end' nodes -- a workflow must have exactly one end node; converge all paths (conditional branches, relationship handles) to a single end", endCount),
		})
	}

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
			report.Issues = append(report.Issues, lintIssue{Severity: "error", EdgeID: eid, serverCode: "EDGE_REFERENCES_MISSING_NODE", Message: fmt.Sprintf("sourceNodeId %q does not match any node", src)})
		} else {
			hasOutgoing[src] = true
		}
		if tgt == "" {
			report.Issues = append(report.Issues, lintIssue{Severity: "error", EdgeID: eid, Message: fmt.Sprintf("edges[%d]: missing targetNodeId", i)})
		} else if _, ok := seenIDs[tgt]; !ok {
			report.Issues = append(report.Issues, lintIssue{Severity: "error", EdgeID: eid, serverCode: "EDGE_REFERENCES_MISSING_NODE", Message: fmt.Sprintf("targetNodeId %q does not match any node", tgt)})
		} else {
			hasIncoming[tgt] = true
		}
	}

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
		nodeRef := fmt.Sprintf("nodeId=%q type=%q", id, nodeType)
		if !incoming && !outgoing {
			sev := "error"
			report.Issues = append(report.Issues, lintIssue{
				Severity: sev,
				NodeID:   id,
				Message:  fmt.Sprintf("%s is fully disconnected (no edges in or out) -- unreachable", nodeRef),
			})
			continue
		}
		if needsIn && !incoming {
			sev := "error"
			report.Issues = append(report.Issues, lintIssue{
				Severity: sev,
				NodeID:   id,
				Message:  fmt.Sprintf("%s has no incoming edge -- unreachable from start", nodeRef),
			})
		}
		if needsOut && !outgoing {
			sev := "error"
			report.Issues = append(report.Issues, lintIssue{
				Severity: sev,
				NodeID:   id,
				Message:  fmt.Sprintf("%s has no outgoing edge -- execution will dead-end here", nodeRef),
			})
		}
	}

	// Stamped in one place rather than on each literal above, so a check added later
	// cannot ship without its source.
	for i := range report.Issues {
		report.Issues[i].Source = "local"
	}
	return report
}

// The GET body is posted verbatim because the oracle NEEDS `status`: it resolves each node's
// task body from the repository under the drafts-float / published-pin rule for that status.
func fetchWorkflowValidation(c *client.Client, wf map[string]any) (*validationResponse, string) {
	if c == nil || wf == nil {
		return nil, "no client"
	}
	body, err := json.Marshal(map[string]any{"workflow": wf})
	if err != nil {
		return nil, fmt.Sprintf("could not encode the workflow (%v)", err)
	}
	data, status, derr := c.Do("POST", "borrower_central", "/v2/workflows/validate", json.RawMessage(body))
	switch {
	case status == http.StatusNotFound:
		return nil, "POST /v2/workflows/validate not found (older backend)"
	case status >= 500:
		return nil, fmt.Sprintf("the server answered HTTP %d", status)
	case status >= 400:
		// The endpoint is there and rejected the request: a contract mismatch,
		// not an old backend. Say so plainly instead of blaming the deploy.
		detail := preflightResponseDetail(data, derr, status)
		if detail != "" {
			return nil, fmt.Sprintf("the server rejected the validation request (HTTP %d) -- possible contract mismatch: %s", status, detail)
		}
		return nil, fmt.Sprintf("the server rejected the validation request (HTTP %d) -- possible contract mismatch", status)
	case derr != nil:
		return nil, fmt.Sprintf("%v", derr)
	case status < 200 || status >= 300, len(data) == 0:
		return nil, fmt.Sprintf("unexpected response (HTTP %d, %d bytes)", status, len(data))
	}
	var resp validationResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, "unrecognized /v2/workflows/validate response"
	}
	return &resp, ""
}

// A local issue is dropped only when it declared a serverCode and the oracle reported that
// same code for the same node or edge.
func mergeServerFindings(report *lintReport, findings []validationFinding) {
	if report == nil {
		return
	}
	covered := func(issue lintIssue) bool {
		if issue.serverCode == "" {
			return false
		}
		for _, f := range findings {
			if f.Code != issue.serverCode {
				continue
			}
			// A graph-wide finding (no ids on either side) matches; otherwise the
			// ids must agree, so one node's problem never silences another's.
			if f.NodeID == issue.NodeID && f.EdgeID == issue.EdgeID {
				return true
			}
			if f.NodeID == "" && f.EdgeID == "" {
				return true
			}
		}
		return false
	}

	kept := make([]lintIssue, 0, len(report.Issues)+len(findings))
	for _, issue := range report.Issues {
		if covered(issue) {
			continue
		}
		kept = append(kept, issue)
	}
	for _, f := range findings {
		severity := "warning"
		if strings.EqualFold(f.Severity, "error") {
			severity = "error"
		}
		kept = append(kept, lintIssue{
			Severity: severity,
			Source:   "server",
			Code:     f.Code,
			NodeID:   f.NodeID,
			EdgeID:   f.EdgeID,
			Message:  f.Message,
		})
	}
	report.Issues = kept
}

// The expression is normalized before matching, so this only describes the canonical
// `result = inputs.get("<path>")` shape.
var extractionProbeRe = regexp.MustCompile(`^result\s*=\s*inputs\.get\(\s*["']([^"']+)["']\s*\)$`)

// A path with no dot is a flat input key, not a scoped extraction, and is left alone.
var extractionProbePathRe = regexp.MustCompile(`^(?:task_outputs\.)?[A-Za-z0-9_-]+\.[A-Za-z0-9_.\[\]*-]+$`)

// Deliberately conservative: any extra statement, operator or call defeats the match, so only
// genuine probes are flagged.
func isPureExtractionProbe(def map[string]any) (string, bool) {
	if def == nil {
		return "", false
	}
	expr, _ := def["expression"].(string)
	if expr == "" {
		return "", false
	}
	if rv, _ := def["returnValue"].(string); strings.TrimSpace(rv) != "result" {
		return "", false
	}

	norm := strings.TrimSpace(expr)
	norm = strings.TrimSuffix(norm, ";")
	// A genuine probe is a single statement, so any statement separator breaks the shape.
	if strings.ContainsAny(norm, "\n;") {
		return "", false
	}
	norm = strings.Join(strings.Fields(norm), " ")

	m := extractionProbeRe.FindStringSubmatch(norm)
	if m == nil {
		return "", false
	}
	path := m[1]

	if !extractionProbePathRe.MatchString(path) {
		return "", false
	}
	return path, true
}

func findExtractionProbeVars(customVariables map[string]any) map[string]string {
	out := map[string]string{}
	for name, raw := range customVariables {
		def, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if path, isProbe := isPureExtractionProbe(def); isProbe {
			out[name] = path
		}
	}
	return out
}

func computeVarSelectors(nodes []any) map[string]string {
	out := map[string]string{}
	for _, n := range nodes {
		nm, ok := n.(map[string]any)
		if !ok {
			continue
		}
		if t := strings.ToLower(fmt.Sprint(nm["type"])); t != "compute-variables" {
			continue
		}
		nodeRef, _ := nm["nodeId"].(string)
		if nodeRef == "" {
			nodeRef, _ = nm["ref"].(string)
		}
		if nodeRef == "" {
			nodeRef, _ = nm["alias"].(string)
		}
		for _, sv := range asSlice(nm["selectedVariables"]) {
			if vn, ok := sv.(string); ok {
				if _, seen := out[vn]; !seen {
					out[vn] = nodeRef
				}
			}
		}
	}
	return out
}

func adviseExtractionProbes(customVariables map[string]any, nodes []any) {
	probes := findExtractionProbeVars(customVariables)
	if len(probes) == 0 {
		return
	}
	selectors := computeVarSelectors(nodes)
	for name, path := range probes {
		directPath := path
		if !strings.HasPrefix(directPath, "task_outputs.") {
			directPath = "task_outputs." + directPath
		}
		probeNode := selectors[name]
		via := ""
		if probeNode != "" {
			via = fmt.Sprintf(" (selected by compute-variables node %q)", probeNode)
		}
		fmt.Fprintf(os.Stderr,
			"# advisory: customVariable %q%s looks like an extraction probe "+
				"-- its expression is a pure pass-through `result = inputs.get(%q)` with no computation. "+
				"The cleaner design is to wire the scoped value DIRECTLY into the consuming node's inputMappings: "+
				"reference %q there (per-item scope resolves it identically -- the consuming node reachable only "+
				"through the rel-<id>/deal-<id> handle runs scoped, so task_outputs.<alias> is that one item). "+
				"Reserve custom variables for values a rule or scorecard actually evaluates, not for plain extraction. "+
				"(advisory only; this never fails lint or blocks apply.)\n",
			name, via, path, directPath)
	}
}
