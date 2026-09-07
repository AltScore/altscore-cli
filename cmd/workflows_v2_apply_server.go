package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// Server-side apply: POST /v2/workflows/apply.
//
// Borrower Central accepts the flat authoring spec verbatim and does what this
// CLI did client-side before v0.35.0 (post each task, substitute aliases, then
// the lock / draft / autosave / publish dance): it resolves every spec-local ref to
// a task alias BEFORE writing, validates the assembled graph with the oracle
// publish uses, creates or version-bumps the tasks by (workflowAlias, specRef),
// then creates / drafts+autosaves / publishes the workflow under the edit
// lock. A rejected spec writes nothing; a mid-write infrastructure failure is
// unwound server-side. `dryRun` returns the plan (exact ref -> alias map,
// assembled graph, findings) without taking the lock.
//
// The CLI still owns authoring sugar and rendering: it parses, normalizes and
// assembles exactly as before (composeWorkflowBody, posting nothing), then
// rebuilds the flat spec from the assembled graph plus the captured task bodies
// and sends it once. References inside bodies are already in the canonical
// long form (`task_outputs.<ref>`) after assembly with the identity map.
//
// Availability. An older backend answers 404 (or 405: POST on a path that only
// exists as GET /{workflow_id}). There is no client-side fallback since
// v0.35.0: apply reports the missing endpoint and stops.

const serverApplyPath = "/v2/workflows/apply"

// serverOwnedTaskKeys are body keys the server assigns or derives from the node
// entry itself. Sending them is either rejected (alias, nodeId, taskId,
// taskVersion) or pointless (specRef / workflowAlias: the server stamps its own;
// position: lifted onto the node).
var serverOwnedTaskKeys = map[string]bool{
	"alias": true, "nodeId": true, "taskId": true, "taskVersion": true,
	"specRef": true, "workflowAlias": true, "position": true,
}

type serverApplyOptions struct {
	DryRun bool
	// nil selects the server's default policy: an update over an ACTIVE version
	// publishes, a create or an adopted DRAFT stays DRAFT.
	Publish   *bool
	ForceLock bool
	ClientID  string
}

// serverApplyTask is one entry of the response's tasks[]: what happened to the
// task behind a spec ref. action is created | bumped | unchanged | referenced.
type serverApplyTask struct {
	Ref           string   `json:"ref"`
	Alias         string   `json:"alias"`
	TaskID        string   `json:"taskId"`
	Version       int      `json:"version"`
	Action        string   `json:"action"`
	DroppedFields []string `json:"droppedFields"`
}

type serverApplyResult struct {
	Mode          string             `json:"mode"`
	DryRun        bool               `json:"dryRun"`
	WorkflowAlias string             `json:"workflowAlias"`
	Workflow      json.RawMessage    `json:"workflow"`
	Tasks         []serverApplyTask  `json:"tasks"`
	Validation    validationResponse `json:"validation"`
	Publish       struct {
		Requested bool     `json:"requested"`
		Published bool     `json:"published"`
		Errors    []string `json:"errors"`
	} `json:"publish"`
	Lock struct {
		Acquired bool `json:"acquired"`
		Released bool `json:"released"`
	} `json:"lock"`
}

// buildFlatSpecForServer rebuilds the flat authoring spec the server accepts
// from the assembled graph (nodes keyed by their spec-local placeholder) and the
// task bodies the assembly pass captured. It refuses, naming the node, a spec
// the server path cannot express: an explicit node `alias` (task aliases are
// server-assigned; `taskAlias` references an existing task instead) or a node
// with neither a captured body nor a taskAlias.
func buildFlatSpecForServer(assembled map[string]any, capture *composeCapture, targetAlias string) (map[string]any, error) {
	if capture == nil {
		return nil, fmt.Errorf("apply: assembly recorded no task bodies")
	}
	nodes := []map[string]any{}
	for _, n := range asMapSlice(assembled["nodes"]) {
		placeholder, _ := n["nodeId"].(string)
		if placeholder == "" {
			return nil, fmt.Errorf("apply: assembled node %q has no id", n["label"])
		}
		if ref, ok := capture.refByNodeID[placeholder]; ok && ref != "" && ref != placeholder {
			return nil, fmt.Errorf("node ref=%q sets an explicit alias %q: task aliases are server-assigned; drop `alias`, or use `taskAlias` to reference an existing task", ref, placeholder)
		}
		flat := map[string]any{"ref": placeholder, "type": n["type"], "label": n["label"]}
		if pos, ok := n["position"]; ok && pos != nil {
			flat["position"] = pos
		}
		if raw, ok := capture.tasks[placeholder]; ok {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				return nil, fmt.Errorf("apply: node %q: unreadable task body: %w", placeholder, err)
			}
			for k, v := range body {
				if serverOwnedTaskKeys[k] || k == "type" || k == "label" {
					continue
				}
				flat[k] = v
			}
		} else if ta, _ := n["taskAlias"].(string); ta != "" {
			// Backed by a task that already exists: the server references it
			// without writing it.
			flat["taskAlias"] = ta
		} else {
			return nil, fmt.Errorf("node %q has neither a task body nor a taskAlias", placeholder)
		}
		nodes = append(nodes, flat)
	}

	edges := []map[string]any{}
	for _, e := range asMapSlice(assembled["edges"]) {
		src, _ := e["sourceNodeId"].(string)
		tgt, _ := e["targetNodeId"].(string)
		if src == "" || tgt == "" {
			return nil, fmt.Errorf("apply: assembled edge %v is missing an endpoint", e["id"])
		}
		flat := map[string]any{"from": src, "to": tgt}
		for _, k := range []string{"sourceHandle", "targetHandle", "label"} {
			if v, ok := e[k]; ok && v != nil {
				flat[k] = v
			}
		}
		// An auto-generated id tracks its endpoints; the server generates the
		// same one (plus the handle). Only an explicit id travels.
		if id, _ := e["id"].(string); id != "" && id != src+"->"+tgt {
			flat["id"] = id
		}
		edges = append(edges, flat)
	}

	flat := map[string]any{
		"alias": targetAlias,
		"label": assembled["label"],
		"nodes": nodes,
		"edges": edges,
	}
	for _, k := range []string{"category", "description", "inputVariables", "customVariables", "config", "notes"} {
		v, ok := assembled[k]
		if !ok {
			continue
		}
		// The assembly always emits `category`, "" when the spec has none; the
		// server takes absence, not an empty enum value.
		if k == "category" {
			if s, _ := v.(string); s == "" {
				continue
			}
		}
		flat[k] = v
	}
	return flat, nil
}

// applyViaServer sends the flat spec to POST /v2/workflows/apply. A 404/405
// without an APPLY_* subcode means the backend predates the endpoint and is
// reported as such (no client-side fallback since v0.35.0); a 400/422 prints
// the server's findings and returns a one-line error; a 2xx is parsed.
func applyViaServer(c *client.Client, cmd *cobra.Command, flat map[string]any, opts serverApplyOptions) (*serverApplyResult, error) {
	req := map[string]any{
		"spec":      flat,
		"dryRun":    opts.DryRun,
		"forceLock": opts.ForceLock,
	}
	if opts.Publish != nil {
		req["publish"] = *opts.Publish
	}
	if opts.ClientID != "" {
		req["clientId"] = opts.ClientID
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode apply request: %w", err)
	}
	data, status, err := c.DoKeepBody("POST", "borrower_central", serverApplyPath, json.RawMessage(body))
	if err != nil {
		return nil, fmt.Errorf("server apply: %w", err)
	}
	switch {
	case (status == http.StatusNotFound || status == http.StatusMethodNotAllowed) && !isApplyErrorEnvelope(data):
		// The route itself is missing (a 404/405 the endpoint produced would
		// carry an APPLY_* subcode): an older backend.
		return nil, fmt.Errorf("this backend has no POST /v2/workflows/apply (HTTP %d): upgrade Borrower Central, or use altscore v0.34.x, the last release with the client-side apply pipeline", status)
	case status >= 200 && status < 300:
		var res serverApplyResult
		if err := json.Unmarshal(data, &res); err != nil {
			return nil, fmt.Errorf("server apply: unreadable %d response: %w", status, err)
		}
		return &res, nil
	}
	if opts.DryRun {
		// A preview never aborts on findings: the server attaches the plan it
		// would have returned to a validation failure, so render it with the
		// errors instead of failing the preview.
		if res := planFromValidationFailure(data); res != nil {
			return res, nil
		}
	}
	return nil, describeServerApplyError(cmd.ErrOrStderr(), status, data)
}

// serverErrorEnvelope is Borrower Central's error body.
type serverErrorEnvelope struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func parseServerErrorEnvelope(data json.RawMessage) (serverErrorEnvelope, bool) {
	var env serverErrorEnvelope
	if err := json.Unmarshal(data, &env); err != nil || env.Code == "" {
		return serverErrorEnvelope{}, false
	}
	return env, true
}

// isApplyErrorEnvelope reports whether a response body is an error the apply
// endpoint itself produced (an APPLY_* subcode), as opposed to the framework's
// answer for a route that does not exist.
func isApplyErrorEnvelope(data json.RawMessage) bool {
	env, ok := parseServerErrorEnvelope(data)
	if !ok {
		return false
	}
	subCode, _ := env.Details["errorSubCode"].(string)
	return strings.HasPrefix(subCode, "APPLY_")
}

// planFromValidationFailure turns a 422 APPLY_VALIDATION_FAILED that carries
// `details.plan` (a dry run's plan) into a result the preview renderers accept,
// with the failing validation attached. nil when the body is anything else.
func planFromValidationFailure(data json.RawMessage) *serverApplyResult {
	env, ok := parseServerErrorEnvelope(data)
	if !ok {
		return nil
	}
	if subCode, _ := env.Details["errorSubCode"].(string); subCode != "APPLY_VALIDATION_FAILED" {
		return nil
	}
	planRaw, err := json.Marshal(env.Details["plan"])
	if err != nil || env.Details["plan"] == nil {
		return nil
	}
	var plan struct {
		Mode          string            `json:"mode"`
		WorkflowAlias string            `json:"workflowAlias"`
		Workflow      json.RawMessage   `json:"workflow"`
		Tasks         []serverApplyTask `json:"tasks"`
	}
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		return nil
	}
	res := &serverApplyResult{
		Mode:          plan.Mode,
		DryRun:        true,
		WorkflowAlias: plan.WorkflowAlias,
		Workflow:      plan.Workflow,
		Tasks:         plan.Tasks,
	}
	if validationRaw, err := json.Marshal(env.Details["validation"]); err == nil {
		_ = json.Unmarshal(validationRaw, &res.Validation)
	}
	res.Validation.Valid = false
	return res
}

// captureFromRefs builds the alias -> ref map printFindingLines uses to show
// the author's spec-local names instead of the minted aliases the server's
// findings carry.
func captureFromRefs(refs map[string]string) *composeCapture {
	if len(refs) == 0 {
		return nil
	}
	return &composeCapture{refByNodeID: refs}
}

// describeServerApplyError prints what the server found (findings, publish
// errors, the lock holder) and returns the error apply exits with. The subcode
// decides the rendering; unknown shapes fall back to the envelope's message.
func describeServerApplyError(w io.Writer, status int, data json.RawMessage) error {
	env, ok := parseServerErrorEnvelope(data)
	if !ok {
		detail := strings.TrimSpace(string(data))
		if len(detail) > 400 {
			detail = detail[:400] + "..."
		}
		if detail == "" {
			return fmt.Errorf("server apply: HTTP %d", status)
		}
		return fmt.Errorf("server apply: HTTP %d: %s", status, detail)
	}
	subCode, _ := env.Details["errorSubCode"].(string)

	switch {
	case subCode == "APPLY_SPEC_INVALID":
		findings := findingsFromAny(env.Details["findings"])
		printFindingLines(w, "ERROR", findings, nil)
		return fmt.Errorf("server rejected the spec with %d finding(s) (see above); nothing was created", len(findings))

	case subCode == "APPLY_VALIDATION_FAILED":
		validation, _ := env.Details["validation"].(map[string]any)
		findings := findingsFromAny(validation["findings"])
		errs, warns := partitionFindings(findings)
		capture := captureFromRefs(refsFromAny(validation["refs"]))
		printFindingLines(w, "ERROR", errs, capture)
		printFindingLines(w, "WARN", warns, capture)
		return fmt.Errorf("server pre-write validation failed with %d error(s) (see above); nothing was created", len(errs))

	case subCode == "APPLY_PUBLISH_REJECTED":
		applied, _ := env.Details["applied"].(map[string]any)
		wfID, _ := applied["workflowId"].(string)
		var lines []string
		if raw, ok := env.Details["errors"].([]any); ok {
			for _, e := range raw {
				lines = append(lines, fmt.Sprintf("  - %v", e))
			}
		}
		fmt.Fprintf(w, "# publish rejected the workflow; tasks and DRAFT %s were written and left in place:\n%s\n", wfID, strings.Join(lines, "\n"))
		return fmt.Errorf("workflow saved as DRAFT %s but publish rejected it (%d error(s), see above); fix the spec and re-apply, or fix the draft in the builder", wfID, len(lines))

	case env.Code == "LOCK_CONFLICT" || env.Code == "SELF_LOCK_CONFLICT":
		holder := "another session"
		since := ""
		if by, ok := env.Details["lockedBy"].(map[string]any); ok {
			if email, _ := by["email"].(string); email != "" {
				holder = email
			}
		}
		if at, _ := env.Details["lockedAt"].(string); at != "" {
			since = " since " + at
		}
		return fmt.Errorf("%s (held by %s%s). Ask them to close the builder tab, or pass --force-lock to take the lock -- that discards whatever that session has unsaved", env.Message, holder, since)

	case subCode == "APPLY_FAILED":
		rolledBack, _ := env.Details["rolledBack"].(bool)
		detail, _ := env.Details["error"].(string)
		if rolledBack {
			return fmt.Errorf("server apply failed mid-write and was rolled back (%s); nothing remains, retry", detail)
		}
		return fmt.Errorf("server apply failed mid-write and could NOT be fully rolled back (%s); inspect the tenant before retrying: %v", detail, env.Details["applied"])
	}

	msg := fmt.Sprintf("server apply: HTTP %d %s", status, env.Code)
	if env.Message != "" {
		msg += ": " + env.Message
	}
	if subCode != "" {
		msg += " [errorSubCode=" + subCode + "]"
	}
	return errors.New(msg)
}

// refsFromAny decodes the validation payload's alias -> ref map.
func refsFromAny(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for alias, ref := range m {
		if s, ok := ref.(string); ok && s != "" {
			out[alias] = s
		}
	}
	return out
}

// findingsFromAny decodes a findings list that arrived inside a generic error
// envelope (map[string]any values) into validationFinding.
func findingsFromAny(v any) []validationFinding {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var findings []validationFinding
	if err := json.Unmarshal(raw, &findings); err != nil {
		return nil
	}
	return findings
}

// printServerApplySummary writes the plan / outcome the server reported: one
// line per task (ref -> alias, what happened, version), dropped fields, warning
// findings, and the publish outcome.
func printServerApplySummary(w io.Writer, res *serverApplyResult) {
	counts := map[string]int{}
	for _, t := range res.Tasks {
		counts[t.Action]++
	}
	actions := make([]string, 0, len(counts))
	for action := range counts {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	parts := make([]string, 0, len(actions))
	for _, action := range actions {
		parts = append(parts, fmt.Sprintf("%d %s", counts[action], action))
	}
	verb := "apply"
	if res.DryRun {
		verb = "plan"
	}
	fmt.Fprintf(w, "# server %s: mode=%s alias=%s tasks: %s\n", verb, res.Mode, res.WorkflowAlias, strings.Join(parts, ", "))
	for _, t := range res.Tasks {
		version := ""
		if t.Version > 0 {
			version = fmt.Sprintf(", v%d", t.Version)
		}
		fmt.Fprintf(w, "#   %s -> %s (%s%s)\n", t.Ref, t.Alias, t.Action, version)
		if len(t.DroppedFields) > 0 {
			fmt.Fprintf(w, "#     WARNING: the server dropped field(s) the task schema does not declare: %s\n", strings.Join(t.DroppedFields, ", "))
		}
	}
	errs, warns := partitionFindings(res.Validation.Findings)
	capture := captureFromRefs(res.Validation.Refs)
	if len(errs) > 0 {
		fmt.Fprintf(w, "# server validation: %d error(s) -- a real apply would be REFUSED and write nothing:\n", len(errs))
		printFindingLines(w, "ERROR", errs, capture)
	}
	if len(warns) > 0 {
		fmt.Fprintf(w, "# server validation: %d warning(s) (apply proceeds):\n", len(warns))
		printFindingLines(w, "WARN", warns, capture)
	}
	if res.DryRun {
		return
	}
	switch {
	case res.Publish.Requested && res.Publish.Published:
		fmt.Fprintln(w, "# published")
	case res.Publish.Requested:
		fmt.Fprintf(w, "# publish requested but not published: %s\n", strings.Join(res.Publish.Errors, "; "))
	default:
		fmt.Fprintln(w, "# left as DRAFT (pass --publish to go live)")
	}
}

// finishServerApply renders a successful server round trip for the three
// modes: --diff (exact-identity diff against the tenant), --dry-run (plan +
// the assembled graph with resolved aliases) and a real apply (summary, entity
// re-scope, the persisted workflow on stdout).
func finishServerApply(c *client.Client, cmd *cobra.Command, spec *composeSpec, res *serverApplyResult, existing map[string]any, targetAlias string, diffFlag, dryRun, skipRescope, allowStealOwnership bool) error {
	errOut := cmd.ErrOrStderr()
	var planned map[string]any
	if len(res.Workflow) > 0 {
		if err := json.Unmarshal(res.Workflow, &planned); err != nil {
			return fmt.Errorf("server apply: unreadable workflow in response: %w", err)
		}
	}

	if diffFlag {
		printServerApplySummary(errOut, res)
		// The server resolved every ref to its real alias (existing tasks keep
		// theirs), so nodes are matched by alias.
		return diffWorkflow(c, cmd, spec, planned, existing, targetAlias)
	}

	if dryRun {
		printServerApplySummary(errOut, res)
		verdict := "nothing written"
		if !res.Validation.Valid {
			verdict = "the server would REFUSE this spec (errors above); nothing written"
		}
		fmt.Fprintf(cmd.OutOrStderr(), "# DRY RUN (server plan) -- %s path for alias=%s; %s. Body below is the graph the server would persist.\n", res.Mode, res.WorkflowAlias, verdict)
		return output.RawJSON(res.Workflow)
	}

	printServerApplySummary(errOut, res)
	if !skipRescope {
		if err := reconcileEntityScopes(c, spec, targetAlias, allowStealOwnership, errOut); err != nil {
			fmt.Fprintf(errOut, "# warning: entity-scope reconciliation hit an issue: %v (workflow itself is fine; re-scope failed entities manually with `altscore <resource> update <id> --workflow-alias %s`)\n", err, targetAlias)
		}
	}
	wfID, _ := planned["id"].(string)
	status, _ := planned["status"].(string)
	fmt.Fprintf(cmd.OutOrStderr(), "# applied workflow %s (alias=%s, status=%s)\n", wfID, targetAlias, status)
	return output.RawJSON(res.Workflow)
}
