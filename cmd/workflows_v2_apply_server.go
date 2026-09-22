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

const serverApplyPath = "/v2/workflows/apply"

// Body keys the server assigns or derives from the node entry: sending them is either
// rejected or pointless.
var serverOwnedTaskKeys = map[string]bool{
	"alias": true, "nodeId": true, "taskId": true, "taskVersion": true,
	"specRef": true, "workflowAlias": true, "position": true,
}

type serverApplyOptions struct {
	DryRun bool
	// nil selects the server's default policy: an update over an ACTIVE version publishes,
	// a create or an adopted DRAFT stays DRAFT.
	Publish   *bool
	ForceLock bool
	ClientID  string
	// Create even when a workflow with a near-identical alias or label already exists.
	CreateNew bool
}

// action is created | bumped | unchanged | referenced.
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

// A 404/405 without an APPLY_* subcode means the backend predates the endpoint; there is
// no client-side fallback since v0.35.0.
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
	if opts.CreateNew {
		// Sent only when set so a backend that predates the near-match guard
		// never sees an unknown field.
		req["createNew"] = true
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
		return nil, fmt.Errorf("this backend has no POST /v2/workflows/apply (HTTP %d): upgrade Borrower Central, or use altscore v0.34.x, the last release with the client-side apply pipeline", status)
	case status >= 200 && status < 300:
		var res serverApplyResult
		if err := json.Unmarshal(data, &res); err != nil {
			return nil, fmt.Errorf("server apply: unreadable %d response: %w", status, err)
		}
		return &res, nil
	}
	if opts.DryRun {
		// A preview never aborts on findings: the server attaches the plan it would have
		// returned, so it is rendered together with the errors.
		if res := planFromValidationFailure(data); res != nil {
			return res, nil
		}
	}
	return nil, describeServerApplyError(cmd.ErrOrStderr(), status, data)
}

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

func isApplyErrorEnvelope(data json.RawMessage) bool {
	env, ok := parseServerErrorEnvelope(data)
	if !ok {
		return false
	}
	subCode, _ := env.Details["errorSubCode"].(string)
	return strings.HasPrefix(subCode, "APPLY_")
}

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

func captureFromRefs(refs map[string]string) *composeCapture {
	if len(refs) == 0 {
		return nil
	}
	return &composeCapture{refByNodeID: refs}
}

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

	case subCode == "APPLY_ALIAS_NEAR_MATCH":
		candidates, _ := env.Details["candidates"].([]any)
		fmt.Fprintf(w, "# no workflow has alias %q, but %d existing workflow(s) are one typo away:\n", stringOr(env.Details["alias"], "?"), len(candidates))
		for _, raw := range candidates {
			cand, _ := raw.(map[string]any)
			fmt.Fprintf(w, "#   %s  (%s v%v, %q, matched by %s)\n",
				stringOr(cand["alias"], "?"), stringOr(cand["status"], "?"), cand["version"],
				stringOr(cand["label"], ""), stringOr(cand["reason"], "alias"))
		}
		return fmt.Errorf("%s. If the spec meant one of them, set spec.alias to that alias (apply then UPDATES it); if this is really a new workflow, re-run with --create-new", env.Message)

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

func stringOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

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
