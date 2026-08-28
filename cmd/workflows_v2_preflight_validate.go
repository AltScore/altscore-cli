package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Server-side pre-flight validation for `workflows-v2 apply`.
//
// apply's CREATE and UPDATE paths both POST every node's task version
// (POST /v2/tasks) BEFORE the workflow itself is created / published. Borrower
// Central rejects graphs with conditional branch-handle problems at create /
// publish (errorSubCodes CONDITIONAL_EDGE_MISSING_BRANCH_HANDLE since #1463,
// CONDITIONAL_EDGE_BRANCH_MISMATCH since #1571). A rejected graph therefore
// fails AFTER leaking task versions, and there is no undo: DELETE
// /v2/tasks/{alias} removes EVERY version of an alias, not the one this run
// added, so apply exposes no rollback flag (see the note at the top of
// workflows_v2_apply.go). Pre-flight is the only protection there is.
//
// This asks BC's POST /v2/workflows/validate to check the assembled definition
// plus the inline task bodies BEFORE anything is persisted, and aborts apply on
// errors so no task rows leak. The validation rules are NOT re-implemented in
// Go: the server is the single oracle. Unknown finding codes are rendered
// generically. The endpoint is new and may be absent (built in parallel, not
// yet deployed), so a 404 / connection error / non-JSON response FAILS OPEN --
// apply proceeds exactly as it did before this check existed.

// composeCapture is what the single assembly pass hands to the two phases that
// follow it: the server pre-flight validator and the task-posting phase.
// Assembly POSTs nothing, so a task's node id is a PLACEHOLDER -- the spec-local
// ref or an explicit alias -- not a server-minted alias.
//
//   - tasks: the exact per-node task bodies apply will POST, keyed by placeholder
//     -- the validator's `tasks` payload.
//   - refByNodeID: placeholder -> spec-local ref, so findings read with the
//     author's own names.
//   - postPlan: the task bodies in POST order (topologically-ordered task nodes,
//     then extra nodes), each with a substitution closure that re-applies the
//     assembly rewriters with the real alias map. postCapturedTasks consumes it.
type composeCapture struct {
	tasks       map[string]json.RawMessage
	refByNodeID map[string]string
	postPlan    []*capturedTask
}

// capturedTask is one entry of the ordered post-plan: an assembled task body,
// its placeholder identifier in the graph, and the closure that rewrites its
// ref placeholders to the server aliases minted by earlier POSTs. substitute is
// nil for trivial bodies (e.g. a start node's backing task) with no refs.
type capturedTask struct {
	placeholder string
	body        map[string]any
	label       string
	substitute  func(refMap map[string]string) error
}

func newComposeCapture() *composeCapture {
	return &composeCapture{
		tasks:       map[string]json.RawMessage{},
		refByNodeID: map[string]string{},
	}
}

// validationFinding mirrors one entry of the /v2/workflows/validate response.
// No code enum is compiled in -- the server owns the rules, so an unrecognized
// code is still rendered from its message + severity.
type validationFinding struct {
	Code     string         `json:"code"`
	Severity string         `json:"severity"`
	NodeID   string         `json:"nodeId"`
	EdgeID   string         `json:"edgeId"`
	Params   map[string]any `json:"params"`
	Message  string         `json:"message"`
}

// validationResponse is the /v2/workflows/validate 200 body (returned 200 even
// when the graph is invalid; `valid` and `findings` carry the verdict).
// skippedNodeIds names the nodes whose task body the server could not resolve,
// so a clean verdict can be told apart from one whose interesting parts were
// never checked. apply's pre-flight has them inline and ignores it; `lint`
// reports it, because there the bodies come from the persisted repository.
type validationResponse struct {
	Valid          bool                `json:"valid"`
	Findings       []validationFinding `json:"findings"`
	SkippedNodeIDs []string            `json:"skippedNodeIds"`
}

// serverPreflightValidate POSTs the assembled workflow body plus the per-node
// task bodies to BC's /v2/workflows/validate and reports the findings.
//
// Fail-open, split by outcome so a contract breakage is never misattributed to
// an old backend: a 404 / 5xx / transport error / empty or non-JSON body prints
// a dim "unavailable (older backend)" note; a non-404 4xx (the endpoint IS there
// and rejected the request -- a likely contract mismatch) prints a LOUD warning
// with the status and a trimmed body. Either way it returns nil so apply
// proceeds unchanged -- oracle trouble never blocks apply.
//
// When abortOnError is true (the real apply path) it returns a non-nil error --
// so the caller aborts before the first POST -- when the server reports any
// error-severity finding OR judges the graph invalid (valid=false), even with no
// error-severity finding to pin it to. Warnings never abort: they print
// prominently and apply continues. When abortOnError is false (--dry-run) it
// never returns an error -- it only prints results.
func serverPreflightValidate(c *client.Client, cmd *cobra.Command, workflow map[string]any, capture *composeCapture, abortOnError bool) error {
	if c == nil || workflow == nil || capture == nil {
		return nil
	}
	errOut := cmd.ErrOrStderr()

	tasks := capture.tasks
	if tasks == nil {
		tasks = map[string]json.RawMessage{}
	}
	payload := map[string]any{
		"workflow": workflow,
		"tasks":    tasks,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// Best-effort: if we can't even encode the payload, fail open.
		dimNote(errOut, fmt.Sprintf("server pre-flight skipped: could not encode validation payload (%v); proceeding without it", err))
		return nil
	}

	data, status, derr := c.Do("POST", "borrower_central", "/v2/workflows/validate", json.RawMessage(body))
	switch {
	case status == http.StatusNotFound:
		// Endpoint absent -> older backend that doesn't have it yet. Fail open.
		dimNote(errOut, preflightUnavailableNote)
		return nil
	case status >= 500:
		// Server-side trouble -- never block apply on the oracle's health.
		dimNote(errOut, preflightUnavailableNote)
		return nil
	case status >= 400:
		// A non-404 4xx (400 / 401 / 403 / 422 / ...): the endpoint IS there and
		// rejected the request -- a likely request/response contract mismatch.
		// Flag it LOUDLY (not the dim "older backend" note) but never block apply
		// on it. The client folds a >=400 body into derr (data is nil).
		fmt.Fprintf(errOut, "# warning: server rejected the validation request (HTTP %d) -- possible contract mismatch; proceeding without pre-flight.\n", status)
		if detail := preflightResponseDetail(data, derr, status); detail != "" {
			fmt.Fprintf(errOut, "#   response: %s\n", detail)
		}
		return nil
	case derr != nil, status < 200 || status >= 300, len(data) == 0:
		// Transport error (no HTTP status), an unexpected non-2xx (e.g. a
		// surfaced 3xx), or an empty body -> nothing usable. Fail open.
		dimNote(errOut, preflightUnavailableNote)
		return nil
	}

	var resp validationResponse
	if jerr := json.Unmarshal(data, &resp); jerr != nil {
		// Non-JSON / unexpected shape -> treat as an older/foreign backend, fail open.
		dimNote(errOut, "server pre-flight skipped: unrecognized /v2/workflows/validate response; proceeding without it")
		return nil
	}

	errs, warns := partitionFindings(resp.Findings)

	// The server's `valid` verdict is authoritative: honor it even when no
	// error-severity finding is attached. A false verdict with only warnings (or
	// none) still means "do not apply this graph".
	invalid := !resp.Valid || len(errs) > 0

	if len(warns) > 0 {
		fmt.Fprintf(errOut, "# server validation: %d warning(s):\n", len(warns))
		printFindingLines(errOut, "WARN", warns, capture)
		// The branch-specific claim only holds when that specific finding is
		// present -- other warnings get no branch claim.
		if hasFindingCode(warns, "CONDITIONAL_BRANCH_WITHOUT_EDGE") {
			fmt.Fprintln(errOut, "# warnings do not block apply -- but a branch with no edge WILL fail at runtime when matched.")
		}
	}

	if len(errs) > 0 {
		fmt.Fprintf(errOut, "# server validation FAILED: %d error(s):\n", len(errs))
		printFindingLines(errOut, "ERROR", errs, capture)
	}

	if invalid {
		if len(errs) == 0 {
			// valid=false with no error-severity finding to point at: state the
			// server's verdict plainly so it is never silent.
			fmt.Fprintln(errOut, "# server validation FAILED: the server judged the graph invalid (valid=false) with no error-severity finding.")
		}
		if abortOnError {
			fmt.Fprintln(errOut, "# aborting before any task was created -- no /v2/tasks rows were leaked. Fix the spec and re-apply.")
			if len(errs) > 0 {
				return fmt.Errorf("server validation failed with %d error(s); no tasks were created", len(errs))
			}
			return fmt.Errorf("server validation failed (valid=false); no tasks were created")
		}
		return nil
	}

	if len(warns) == 0 && !abortOnError {
		// --dry-run always surfaces a result, even a clean one.
		fmt.Fprintln(errOut, "# server validation: no issues found.")
	}
	return nil
}

// preflightUnavailableNote is the dim fail-open note shared by every "the oracle
// isn't usable" branch (404 / 5xx / transport error / non-2xx / empty body).
const preflightUnavailableNote = "server pre-flight skipped: POST /v2/workflows/validate unavailable (older backend); proceeding without it"

// hasFindingCode reports whether any finding carries the given code.
func hasFindingCode(findings []validationFinding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// preflightResponseDetail returns a short, trimmed description of a rejected
// validation response for the loud contract-mismatch note. The client folds a
// >=400 response body into derr (data is nil), so it falls back to derr; a bare
// "HTTP <status>" (empty body) is dropped since the caller already prints the
// status.
func preflightResponseDetail(data json.RawMessage, derr error, status int) string {
	detail := strings.TrimSpace(string(data))
	if detail == "" && derr != nil {
		detail = strings.TrimSpace(derr.Error())
	}
	if detail == fmt.Sprintf("HTTP %d", status) {
		return ""
	}
	const maxLen = 400
	if len(detail) > maxLen {
		detail = detail[:maxLen] + "..."
	}
	return detail
}

// formatValidationFinding renders one finding for stderr. The caller supplies
// the [ERROR]/[WARN] prefix; this returns `CODE (node "ref") (edge "id"):
// message`, mapping the server's nodeId back to the spec-local ref via the
// capture's reverse map so the reader sees the name from their own spec rather
// than an internal id.
func formatValidationFinding(f validationFinding, capture *composeCapture) string {
	loc := ""
	if f.NodeID != "" {
		ref := f.NodeID
		if capture != nil {
			if mapped, ok := capture.refByNodeID[f.NodeID]; ok && mapped != "" {
				ref = mapped
			}
		}
		loc += fmt.Sprintf(" (node %q)", ref)
	}
	if f.EdgeID != "" {
		loc += fmt.Sprintf(" (edge %q)", f.EdgeID)
	}
	code := f.Code
	if code == "" {
		code = "UNSPECIFIED"
	}
	msg := f.Message
	if msg == "" {
		msg = "(no message provided)"
	}
	return fmt.Sprintf("%s%s: %s", code, loc, msg)
}

// dimNote prints a single informational line to w, prefixed with "# " to match
// the CLI's stderr note convention. It is dimmed with ANSI faint only when
// stderr is an interactive terminal, so piped output and test buffers stay
// plain.
func dimNote(w io.Writer, msg string) {
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintf(w, "\x1b[2m# %s\x1b[0m\n", msg)
		return
	}
	fmt.Fprintf(w, "# %s\n", msg)
}

// applyAssembleValidateAndPost runs the real (non-dry, non-diff) apply path in
// four phases so nothing is persisted until the graph has cleared the server
// pre-flight:
//
//  1. ASSEMBLE once (strict normalization, no POSTs): composeWorkflowBody builds
//     the workflow body with ref placeholders and records the ordered task
//     post-plan on `capture`. Because it posts nothing, mutating the spec in
//     place is harmless -- no deep copy, no stderr suppression, and the meta
//     lookups (task types, operators, ...) run once, not twice.
//  2. VALIDATE those exact artifacts against POST /v2/workflows/validate. On any
//     error-severity finding (or valid=false) it returns an error and posts
//     nothing -- no /v2/tasks rows leak (there is no rollback). Fail-open on
//     oracle trouble (see serverPreflightValidate).
//  3. POST the task bodies in order, collecting placeholder -> (alias, version).
//  4. SUBSTITUTE those aliases + versions into the assembled workflow body.
//
// The posted workflow body is the validated body mutated in place by phase 4
// alone, so it differs from what the validator saw ONLY by the ref -> server
// alias identifier substitution.
func applyAssembleValidateAndPost(c *client.Client, cmd *cobra.Command, spec *composeSpec, publish, skipRescope, allowStealOwnership, noAutoDefaults, noLayout bool) (map[string]any, error) {
	// Phase 1: assemble once. dryRun=false selects strict normalization -- the
	// real apply must hard-fail on a bad source/entity, not stub it as a preview
	// would. An assembly-level error surfaces here, before any /v2/tasks POST.
	capture := newComposeCapture()
	wf, err := composeWorkflowBody(c, spec, false, publish, !skipRescope, allowStealOwnership, !noAutoDefaults, !noLayout, capture)
	if err != nil {
		return nil, err
	}

	// Phase 2: validate the exact artifacts we are about to post.
	if abortErr := serverPreflightValidate(c, cmd, wf, capture, true); abortErr != nil {
		return nil, abortErr
	}

	// Phase 3: post the task bodies, minting the real aliases + versions.
	refMap, versionMap, err := postCapturedTasks(c, capture)
	if err != nil {
		return nil, err
	}

	// Phase 4: the only change between the validated body and the posted body.
	if err := substituteWorkflowAliases(wf, refMap, versionMap); err != nil {
		return nil, err
	}
	return wf, nil
}

// postCapturedTasks POSTs every task body the assembly pass recorded, in the
// order they were assembled (topologically-ordered task nodes, then extra
// nodes). Before each POST it re-runs that body's substitution closure with the
// aliases minted by earlier POSTs -- the same rewriters assembly ran with
// identity placeholders, now resolving cross-task references to the stored
// server alias. It returns placeholder -> alias and placeholder -> version for
// substituting the workflow body. A mid-loop failure leaves earlier tasks
// created (there is no tasks-v2 DELETE); the phase-2 pre-flight makes that rare.
func postCapturedTasks(c *client.Client, capture *composeCapture) (map[string]string, map[string]int, error) {
	refMap := map[string]string{}
	versionMap := map[string]int{}
	posted := []string{}
	for _, ct := range capture.postPlan {
		// Resolve this body's cross-task references to the aliases minted so far.
		// The closure runs with refMap BEFORE this task's own placeholder is
		// added, so a task type/ref collision can't be mistaken for a residue.
		if ct.substitute != nil {
			if err := ct.substitute(refMap); err != nil {
				return nil, nil, fmt.Errorf("%w (created so far: %v)", err, posted)
			}
		}
		alias, version, err := postTask(c, ct.body, ct.label)
		if err != nil {
			return nil, nil, fmt.Errorf("%w (created so far: %v)", err, posted)
		}
		refMap[ct.placeholder] = alias
		versionMap[ct.placeholder] = version
		posted = append(posted, alias)
	}
	return refMap, versionMap, nil
}

// substituteWorkflowAliases rewrites the assembled workflow body in place,
// replacing the ref placeholders used during assembly with the server aliases +
// task versions from postCapturedTasks. It touches only identifier-bearing
// fields -- node id / taskAlias / taskVersion + node.data.inputMappings, edge
// endpoints + auto-generated ids, and customVariable expressions / returnValues
// / dependencies -- reusing the same rewriters assembly used. refMap is keyed by
// placeholder (the identifier the graph carries).
//
// This is the sole transformation between validation and the workflow POST, so
// the posted body differs from the validated one only by this substitution.
func substituteWorkflowAliases(wf map[string]any, refMap map[string]string, versionMap map[string]int) error {
	for _, n := range asMapSlice(wf["nodes"]) {
		id, _ := n["nodeId"].(string)
		if alias, ok := refMap[id]; ok {
			n["nodeId"] = alias
			if ta, _ := n["taskAlias"].(string); ta == id {
				n["taskAlias"] = alias
			}
			if v, ok := versionMap[id]; ok {
				n["taskVersion"] = v
			}
		}
		if data, _ := n["data"].(map[string]any); data != nil {
			if im, _ := data["inputMappings"].(map[string]any); len(im) > 0 {
				rewritten, err := rewriteRefsInMappings(im, refMap)
				if err != nil {
					return fmt.Errorf("node %q data.inputMappings: %w", id, err)
				}
				data["inputMappings"] = rewritten
			}
		}
	}
	for _, e := range asMapSlice(wf["edges"]) {
		oldSrc, _ := e["sourceNodeId"].(string)
		oldTgt, _ := e["targetNodeId"].(string)
		newSrc, newTgt := oldSrc, oldTgt
		if a, ok := refMap[oldSrc]; ok {
			newSrc = a
		}
		if a, ok := refMap[oldTgt]; ok {
			newTgt = a
		}
		// An auto-generated edge id tracks its endpoints (src->tgt); an explicit
		// id is left alone.
		if id, _ := e["id"].(string); id == oldSrc+"->"+oldTgt {
			e["id"] = newSrc + "->" + newTgt
		}
		e["sourceNodeId"] = newSrc
		e["targetNodeId"] = newTgt
	}
	if cvs, _ := wf["customVariables"].(map[string]any); cvs != nil {
		for name, raw := range cvs {
			v, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// This is the EFFECTIVE site: refMap holds real server aliases
			// here, unlike the assembly pass in apply where it is the identity.
			rewriteCustomVariableRefs(v, refMap)
			cvs[name] = v
		}
	}
	return nil
}

// asMapSlice coerces an assembled node/edge list to []map[string]any. compose
// builds them as []map[string]any; the []any branch guards a marshaled
// round-trip.
func asMapSlice(v any) []map[string]any {
	switch s := v.(type) {
	case []map[string]any:
		return s
	case []any:
		out := make([]map[string]any, 0, len(s))
		for _, e := range s {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}
