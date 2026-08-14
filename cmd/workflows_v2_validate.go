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

// validateWorkflowV2Body normalizes a workflow create/autosave/update body
// and checks for the failure modes that produce silent garbage:
//  1. Snake-case / short node-edge field names (id/source/target) are RENAMED
//     in place to the camelCase aliases the API expects (nodeId/sourceNodeId/
//     targetNodeId) rather than rejected. We only error when both forms are
//     present with conflicting values.
//  2. Orphan nodes (any type without taskAlias/taskId).
//
// EVERY node -- including start, end, and conditional -- must have a
// taskAlias. The Hub creates trivial backing tasks for start/end (type-only,
// no other config) so it can render them. A node without a taskAlias produces
// GET /v2/tasks/null -> 404 in the Hub UI.
//
// The body is mutated in place when a rename happens. Returns a single error
// aggregating every remaining problem so the agent can fix everything at once
// instead of round-tripping per issue.
func validateWorkflowV2Body(body *json.RawMessage) error {
	if body == nil || len(*body) == 0 {
		return nil
	}
	var wf map[string]any
	if err := json.Unmarshal(*body, &wf); err != nil {
		// Not our problem -- the API will reject with a clear parse error.
		return nil
	}

	// As of BC #1291, the create endpoint honors an explicit `alias` field
	// when provided (and falls back to slugifying `label` when absent). No
	// warning is needed -- caller intent is respected. We keep this block
	// intentionally empty so the historical comment trail is searchable;
	// the prior "silently drops alias" warning was removed once the BC
	// contract changed.

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

			// Old field-name pitfall: 'id' is the short/snake form; the API
			// requires the camelCase 'nodeId'. Rename in place rather than
			// reject. Only error when both are present with conflicting values.
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

	// A workflow must converge to exactly one end node. Conditional branches and
	// parallel fan-out (e.g. relationship per-item handles) must all converge to
	// a single end rather than terminating at separate end nodes.
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
			// 'source'/'target' are the short forms; the API requires
			// 'sourceNodeId'/'targetNodeId'. Rename in place rather than reject.
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

	// Persist any in-place renames back to the caller's body.
	if mutated {
		rewritten, err := json.Marshal(wf)
		if err != nil {
			return fmt.Errorf("re-encode normalized workflow body: %w", err)
		}
		*body = json.RawMessage(rewritten)
	}
	return nil
}

// renameToCamel moves the value at the short key (e.g. "id") to the canonical
// camelCase key (e.g. "nodeId") and drops the short key. It returns true when
// a rename happened. If both keys are present with conflicting values it
// returns an error instead of guessing; equal values (or an absent short key)
// are a no-op rename. context is a short label like "nodes[2]" for the error.
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
		// Same value on both keys: just drop the redundant short form.
		delete(m, shortKey)
		return true, nil
	}
	m[camelKey] = shortVal
	delete(m, shortKey)
	return true, nil
}

// makeWfv2LintCmd inspects an existing workflow. Two sources, merged into one
// report: borrower-central's validation oracle (POST /v2/workflows/validate --
// the same one apply's pre-flight and the Hub builder call) plus the local
// structural checks that cover what the oracle does not (orphan nodes,
// duplicate nodeIds, malformed edge endpoints).
//
// Until #101 this command was local-only, and that was the defect: the oracle
// owns every reference-integrity rule (dangling `task_outputs.<alias>` in
// inputMappings, PDF sources, outputJson, task config; unconsumed input
// variables; identity keys), so `lint` reported a clean workflow while the Hub
// builder -- which does call it -- listed the real problems. A Bolivariano KYB
// review hit exactly that: `lint` clean, five live defects, one of them a
// dangling task ref that had survived 47 versions.
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
					// Fail open, exactly like apply's pre-flight: the oracle's
					// health never turns lint into an error of its own.
					report.ServerValidation = lintServerSkipped
					dimNote(cmd.ErrOrStderr(), "server validation skipped: "+reason+"; reporting local checks only")
					break
				}
				report.ServerValidation = lintServerOK
				report.SkippedNodeIDs = resp.SkippedNodeIDs
				mergeServerFindings(&report, resp.Findings)
			}
			// Non-blocking advisory: flag customVariables that are pure
			// pass-through extraction probes (a compute-variables node + a
			// custom var whose expression merely extracts a scoped scalar).
			// Emitted to stderr; it NEVER contributes to report.Issues, so it
			// cannot change the exit code below.
			if cv, ok := wf["customVariables"].(map[string]any); ok {
				adviseExtractionProbes(cv, asSlice(wf["nodes"]))
			}
			// Non-blocking readability advisory for client handoff: variable
			// titles, PDF section titles / Python-built HTML, rule descriptions.
			// Same construction as the probe advisory above -- stderr only, never
			// an Issue. Both enrichment fetches fail open, so a workflow whose end
			// task or rules cannot be read reports FEWER practices rather than
			// reporting them wrongly.
			wfAlias, _ := wf["alias"].(string)
			adviseHandoffReadability(wf,
				fetchEndTaskBodies(c, asSlice(wf["nodes"])),
				fetchWorkflowRules(c, wfAlias),
				cmd.ErrOrStderr())
			// (Silent-PDF lint removed: borrower-central's end_activity
			// now auto-resolves PDF sources from the ancestor graph at
			// runtime when pdfConfig.enabled=true but sourcesConfig is
			// empty. The "enabled+empty" state is now a valid shape, so
			// the old warning would mislead.)
			// (inputMappings namespace check removed: BC's variable resolver
			// now accepts bare-alias refs `<alias>.<field>` as implicit
			// `task_outputs.<alias>.<field>`. Create-time validation in BC
			// catches structurally-broken mapping values synchronously, and
			// ValidateInputMappingsUC catches unknown-alias refs at workflow
			// validate time with full graph context.)
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

// lintServerValidation values for lintReport.ServerValidation.
const (
	lintServerOK        = "ok"
	lintServerSkipped   = "skipped"
	lintServerLocalOnly = "local-only"
)

type lintIssue struct {
	Severity string `json:"severity"` // "error" or "warning"
	// Source is "local" (this binary's structural pass) or "server" (a
	// /v2/workflows/validate finding), so a reader knows which half to trust
	// when only one of them ran.
	Source string `json:"source"`
	// Code is the server finding code (MISSING_START_NODE,
	// TASK_REFERENCE_NOT_IN_GRAPH, ...). Empty on a local issue.
	Code   string `json:"code,omitempty"`
	NodeID string `json:"nodeId,omitempty"`
	EdgeID string `json:"edgeId,omitempty"`
	// serverCode is the finding code the oracle emits for this same problem, or
	// "" when the check is local-only. Not serialized: it exists so
	// mergeServerFindings can drop the local copy when the oracle answered.
	serverCode string
	Message    string `json:"message"`
}

type lintReport struct {
	WorkflowID string `json:"workflowId"`
	Alias      string `json:"alias,omitempty"`
	Status     string `json:"status,omitempty"`
	NodeCount  int    `json:"nodeCount"`
	EdgeCount  int    `json:"edgeCount"`
	// ServerValidation is "ok", "skipped" or "local-only" -- never absent, so a
	// clean report can always be told apart from one whose oracle never ran.
	ServerValidation string `json:"serverValidation"`
	// SkippedNodeIDs are the nodes whose task body the oracle could not resolve
	// and therefore did not check.
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
	// A workflow must converge to exactly one end node. Conditional branches and
	// parallel fan-out (e.g. relationship per-item handles) must all converge to
	// a single end rather than terminating at separate end nodes.
	if endCount > 1 {
		report.Issues = append(report.Issues, lintIssue{
			Severity:   "error",
			serverCode: "MULTIPLE_END_NODES",
			Message:    fmt.Sprintf("%d 'end' nodes -- a workflow must have exactly one end node; converge all paths (conditional branches, relationship handles) to a single end", endCount),
		})
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

	// Flag orphan nodes (no edges in OR out). Start nodes are exempt from the
	// incoming-edge requirement; end nodes are exempt from outgoing. Comment
	// nodes are decorative -- the Hub allows them anywhere -- but a fully
	// disconnected comment is dead weight; flag it as a 'warning' (not error)
	// so agents see it but it doesn't block apply.
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
			// Every orphan is a real problem now: borrower-central removed the
			// decorative `comment` task type (#1234), so standalone annotations
			// live in the top-level `notes` array rather than as graph nodes.
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

	// Stamped in one place rather than on each literal above, so a check added
	// later cannot ship without its source.
	for i := range report.Issues {
		report.Issues[i].Source = "local"
	}
	return report
}

// fetchWorkflowValidation asks borrower-central's validation oracle about an
// already-persisted definition. `wf` is the GET /v2/workflows/{id} body posted
// verbatim: the request schema ignores the extra keys (id, status, timestamps)
// and it NEEDS `status`, because the oracle resolves each node's task body from
// the persisted repository under the drafts-float / published-pin rule for that
// status. No `tasks` map is sent for the same reason -- the server resolving
// them is the whole point, and it is one request instead of N.
//
// Returns (nil, reason) on anything unusable, mirroring apply's pre-flight
// fail-open policy: the oracle's health must never turn a lint into an error of
// its own. The reason is already human-readable.
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

// mergeServerFindings folds the oracle's findings into the report and drops the
// local issues it already covers.
//
// Dropping is deliberate and narrow: only a local issue that declared a
// serverCode, and only when the oracle reported that same code for the same
// node or edge (or for the graph as a whole, where neither side carries an id).
// The server is the oracle for those checks -- that is this codebase's rule, not
// a preference -- and reporting one problem twice under two wordings is the kind
// of noise that teaches people to skim past a lint report.
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

// extractionProbeRe matches the body of a pure pass-through extraction custom
// variable: a single `inputs.get(...)` whose argument is a quoted path. We
// capture the path so the advisory can quote the exact ref to wire directly.
//
// The full expression is normalized (trimmed, internal whitespace collapsed)
// before matching, so this regex only needs to describe the canonical
// `result = inputs.get("<path>")` shape. Single or double quotes are accepted.
var extractionProbeRe = regexp.MustCompile(`^result\s*=\s*inputs\.get\(\s*["']([^"']+)["']\s*\)$`)

// extractionProbePathRe constrains the captured path to a per-item scoped
// reference: either `task_outputs.<alias>.<rest>` or the bare-alias shortcut
// `<alias>.<rest>`. A path with no dot (a flat input key) is NOT a scoped
// extraction and is left alone -- pulling a flat input into a custom variable
// is a different (and sometimes legitimate) shape.
var extractionProbePathRe = regexp.MustCompile(`^(?:task_outputs\.)?[A-Za-z0-9_-]+\.[A-Za-z0-9_.\[\]*-]+$`)

// isPureExtractionProbe reports whether a customVariable definition is a pure
// pass-through extraction probe: `result = inputs.get("task_outputs.<alias>.<path>")`
// (or the bare-alias shortcut `inputs.get("<alias>.<path>")`) with
// returnValue "result" and NO additional computation -- no arithmetic,
// comparisons, conditionals, loops, or any other call beyond the single
// inputs.get. When it matches, the second return value is the captured path
// the author should wire directly via inputMappings instead.
//
// The check is deliberately conservative: any extra statement, operator, or
// call defeats the match so we only flag genuine probes (a value a rule or
// scorecard actually computes is never flagged).
func isPureExtractionProbe(def map[string]any) (string, bool) {
	if def == nil {
		return "", false
	}
	expr, _ := def["expression"].(string)
	if expr == "" {
		return "", false
	}
	// returnValue must be exactly "result". An empty returnValue means the
	// wrapper returns None (not a probe surfacing a value); anything else
	// means the author is returning something other than the extracted scalar.
	if rv, _ := def["returnValue"].(string); strings.TrimSpace(rv) != "result" {
		return "", false
	}

	// Normalize: drop a trailing semicolon, trim, collapse internal runs of
	// whitespace to a single space so indentation/newline variations don't
	// defeat the match. A genuine probe is a single statement, so any
	// remaining statement separator (`;` mid-expression, a newline that
	// survived as a second statement) breaks the single-assignment shape and
	// is rejected below.
	norm := strings.TrimSpace(expr)
	norm = strings.TrimSuffix(norm, ";")
	// Reject multi-statement bodies outright: a newline or a `;` between
	// statements means there's more than the one extraction assignment.
	if strings.ContainsAny(norm, "\n;") {
		return "", false
	}
	norm = strings.Join(strings.Fields(norm), " ")

	m := extractionProbeRe.FindStringSubmatch(norm)
	if m == nil {
		return "", false
	}
	path := m[1]

	// The captured path must be a scoped per-item reference (alias.field),
	// not a flat input key, and must not itself smuggle in computation
	// (the inner-quote constraint in extractionProbeRe already forbids a
	// nested call, but we double-check the path shape here).
	if !extractionProbePathRe.MatchString(path) {
		return "", false
	}
	return path, true
}

// findExtractionProbeVars scans a workflow's top-level customVariables map and
// returns the names (sorted) of variables that are pure pass-through
// extraction probes, mapped to the scoped path each one extracts.
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

// computeVarSelectors cross-references which compute-variables node selects a
// given custom variable via selectedVariables, so the advisory can name the
// probe node. nodes may be []any (fetched workflow) or []map[string]any
// (compose spec); both are handled. Returns varName -> node ref/id that
// selects it (first match wins; empty when none).
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
		// Node identity: prefer nodeId (fetched) then ref/alias (spec).
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

// adviseExtractionProbes emits a NON-BLOCKING advisory (stderr, no error
// returned, exit code unaffected) for every customVariable that is a pure
// pass-through extraction probe. The advisory names the variable (and the
// compute-variables node that selects it, when cheaply cross-referenceable)
// and recommends wiring the scoped path directly into the consuming node's
// inputMappings instead -- per-item scope resolves `task_outputs.<alias>.<field>`
// identically there, and custom variables should be reserved for values a rule
// or scorecard actually evaluates.
//
// Both `workflows-v2 lint` and the `apply` preflight call this. It is advisory
// only by construction: it writes to stderr and returns nothing.
func adviseExtractionProbes(customVariables map[string]any, nodes []any) {
	probes := findExtractionProbeVars(customVariables)
	if len(probes) == 0 {
		return
	}
	selectors := computeVarSelectors(nodes)
	for name, path := range probes {
		// Surface a direct-wire path: prefer the explicit task_outputs.* form
		// so the recommendation is copy-pasteable regardless of which shortcut
		// the probe used.
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
