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
// fails AFTER leaking task versions -- there is no rollback (tasks-v2 has no
// DELETE; --rollback-tasks is a documented no-op).
//
// This asks BC's POST /v2/workflows/validate to check the assembled definition
// plus the inline task bodies BEFORE anything is persisted, and aborts apply on
// errors so no task rows leak. The validation rules are NOT re-implemented in
// Go: the server is the single oracle. Unknown finding codes are rendered
// generically. The endpoint is new and may be absent (built in parallel, not
// yet deployed), so a 404 / connection error / non-JSON response FAILS OPEN --
// apply proceeds exactly as it did before this check existed.

// composeCapture collects what the server-side validator needs from a dry
// assembly pass: the exact per-node task bodies apply would POST, keyed by the
// node id that backs them (the server-assigned alias -- which, in a dry
// assembly where nothing is posted, is the spec-local ref or an explicit
// alias), and a reverse map from that node id back to the spec-local ref so
// findings can be reported using the name from the author's spec.
type composeCapture struct {
	// tasks maps node id -> the marshaled task body apply would POST for it.
	tasks map[string]json.RawMessage
	// refByNodeID maps node id -> spec-local ref, for human-readable findings.
	refByNodeID map[string]string
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
type validationResponse struct {
	Valid    bool                `json:"valid"`
	Findings []validationFinding `json:"findings"`
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

	var errs, warns []validationFinding
	for _, f := range resp.Findings {
		// Severity is lowercase "error"/"warning" today; match robustly in case
		// that ever shifts case.
		if strings.EqualFold(f.Severity, "error") {
			errs = append(errs, f)
		} else {
			warns = append(warns, f)
		}
	}

	// The server's `valid` verdict is authoritative: honor it even when no
	// error-severity finding is attached. A false verdict with only warnings (or
	// none) still means "do not apply this graph".
	invalid := !resp.Valid || len(errs) > 0

	if len(warns) > 0 {
		fmt.Fprintf(errOut, "# server validation: %d warning(s):\n", len(warns))
		for _, f := range warns {
			fmt.Fprintf(errOut, "#   [WARN] %s\n", formatValidationFinding(f, capture))
		}
		// The branch-specific claim only holds when that specific finding is
		// present -- other warnings get no branch claim.
		if hasFindingCode(warns, "CONDITIONAL_BRANCH_WITHOUT_EDGE") {
			fmt.Fprintln(errOut, "# warnings do not block apply -- but a branch with no edge WILL fail at runtime when matched.")
		}
	}

	if len(errs) > 0 {
		fmt.Fprintf(errOut, "# server validation FAILED: %d error(s):\n", len(errs))
		for _, f := range errs {
			fmt.Fprintf(errOut, "#   [ERROR] %s\n", formatValidationFinding(f, capture))
		}
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

// applyAssembleValidateAndPost runs the real (non-dry, non-diff) apply
// assembly. It validates a throwaway dry assembly against the server pre-flight
// BEFORE the real posting pass, so a graph BC would reject at create/publish is
// caught before the first POST /v2/tasks -- no task versions leak (there is no
// rollback). On any error-severity finding it returns an error and posts
// nothing. Otherwise it runs the real posting pass and returns its assembled
// workflow body.
//
// compose mutates the spec in place (it deletes each node's `ref`, stamps
// specRef/workflowAlias, applies auto-defaults), so the validation pass runs
// against a deep copy; its stderr is suppressed so its guidance output doesn't
// duplicate the real pass's. If the spec can't be deep-copied, the pre-flight is
// skipped entirely (fail-open) rather than run against shared state.
func applyAssembleValidateAndPost(c *client.Client, cmd *cobra.Command, spec *composeSpec, publish, skipRescope, allowStealOwnership, noAutoDefaults bool) (map[string]any, error) {
	// Single definition of the compose call so the dry (validation) pass and the
	// real (posting) pass can never diverge in their shared arguments -- a future
	// arg change lands in one place, not two positional call sites. `dry` also
	// selects the spec: the validation pass runs against a throwaway deep copy
	// (compose mutates in place), the posting pass against the live spec.
	var specCopy *composeSpec
	compose := func(dry bool, capture *composeCapture) (map[string]any, error) {
		s := spec
		if dry {
			s = specCopy
		}
		return composeWorkflowBody(c, s, dry, publish, !skipRescope, allowStealOwnership, !noAutoDefaults, capture)
	}

	dup, copyErr := deepCopyComposeSpec(spec)
	if copyErr != nil {
		// The dry validation pass depends on an isolated copy; without one we must
		// not run it against shared state (a mutating dry pass would corrupt the
		// spec the real pass posts). Skip pre-flight and post directly -- same
		// fail-open contract as an unavailable oracle.
		dimNote(cmd.ErrOrStderr(), fmt.Sprintf("server pre-flight skipped: spec not copyable (%v); proceeding without it", copyErr))
		return compose(false, nil)
	}
	specCopy = dup

	capture := newComposeCapture()
	var dryWf map[string]any
	var dryErr error
	withSuppressedStderr(func() {
		dryWf, dryErr = compose(true, capture)
	})
	if dryErr != nil {
		// An assembly-level error (not a server finding): it would fail the real
		// pass too, so surface it now -- before any /v2/tasks POST.
		return nil, dryErr
	}
	if abortErr := serverPreflightValidate(c, cmd, dryWf, capture, true); abortErr != nil {
		return nil, abortErr
	}
	// Real posting pass -- unchanged behavior.
	return compose(false, nil)
}

// withSuppressedStderr runs fn with os.Stderr redirected to the null device,
// restoring it afterward. Used to silence the throwaway dry-assembly pass that
// feeds the server pre-flight validator on the real apply path -- that pass's
// guidance output (alias notes, "Would POST /v2/tasks ...", advisories) would
// otherwise duplicate the real posting pass's. The CLI is single-threaded per
// command, so the global swap is safe here. If the null device can't be opened,
// fn still runs (unsuppressed) rather than being skipped.
func withSuppressedStderr(fn func()) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		fn()
		return
	}
	saved := os.Stderr
	os.Stderr = devnull
	defer func() {
		os.Stderr = saved
		_ = devnull.Close()
	}()
	fn()
}

// deepCopyComposeSpec returns an independent copy of the spec so the throwaway
// dry assembly (used to feed the server pre-flight validator) can't disturb the
// spec the real posting pass consumes. A copy error is propagated -- NOT
// swallowed by returning shared state -- so the caller can skip the dry pass
// rather than silently run it against the original.
func deepCopyComposeSpec(s *composeSpec) (*composeSpec, error) {
	cp := *s
	if s.Description != nil {
		d := *s.Description
		cp.Description = &d
	}
	var err error
	if cp.Nodes, err = deepCopyMapSlice(s.Nodes); err != nil {
		return nil, err
	}
	if cp.Tasks, err = deepCopyMapSlice(s.Tasks); err != nil {
		return nil, err
	}
	if cp.ExtraNodes, err = deepCopyMapSlice(s.ExtraNodes); err != nil {
		return nil, err
	}
	if cp.Edges, err = deepCopyMapSlice(s.Edges); err != nil {
		return nil, err
	}
	if cp.Notes, err = deepCopyMapSlice(s.Notes); err != nil {
		return nil, err
	}
	if cp.InputVariables, err = deepCopyMap(s.InputVariables); err != nil {
		return nil, err
	}
	if cp.CustomVariables, err = deepCopyMap(s.CustomVariables); err != nil {
		return nil, err
	}
	if cp.Config, err = deepCopyMap(s.Config); err != nil {
		return nil, err
	}
	return &cp, nil
}

func deepCopyMapSlice(in []map[string]any) ([]map[string]any, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]map[string]any, len(in))
	for i, m := range in {
		cp, err := deepCopyMap(m)
		if err != nil {
			return nil, err
		}
		out[i] = cp
	}
	return out, nil
}

func deepCopyMap(in map[string]any) (map[string]any, error) {
	if in == nil {
		return nil, nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
