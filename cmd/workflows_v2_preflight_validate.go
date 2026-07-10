package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

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
// Fail-open: a 404 / connection error / non-2xx / non-JSON response means an
// older backend without the endpoint -- it prints a dim one-line note and
// returns nil so apply proceeds unchanged.
//
// When abortOnError is true (the real apply path) and the server reports any
// error-severity finding, it returns a non-nil error so the caller aborts
// before the first POST. Warnings never abort: they print prominently (they
// include "branch has no edge", which now fails at runtime when the branch is
// matched) and apply continues. When abortOnError is false (--dry-run) it never
// returns an error -- it only prints results.
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
	if derr != nil || status < 200 || status >= 300 || len(data) == 0 {
		// 404 / connection error / non-2xx / empty body -> older backend that
		// doesn't have the endpoint yet. Fail open.
		dimNote(errOut, "server pre-flight skipped: POST /v2/workflows/validate unavailable (older backend); proceeding without it")
		return nil
	}

	var resp validationResponse
	if jerr := json.Unmarshal(data, &resp); jerr != nil {
		// Non-JSON / unexpected shape -> treat as an older backend, fail open.
		dimNote(errOut, "server pre-flight skipped: unrecognized /v2/workflows/validate response; proceeding without it")
		return nil
	}

	var errs, warns []validationFinding
	for _, f := range resp.Findings {
		if f.Severity == "error" {
			errs = append(errs, f)
		} else {
			warns = append(warns, f)
		}
	}

	if len(warns) > 0 {
		fmt.Fprintf(errOut, "# server validation: %d warning(s):\n", len(warns))
		for _, f := range warns {
			fmt.Fprintf(errOut, "#   [WARN] %s\n", formatValidationFinding(f, capture))
		}
		fmt.Fprintln(errOut, "# warnings do not block apply -- but a branch with no edge WILL fail at runtime when matched.")
	}

	if len(errs) > 0 {
		fmt.Fprintf(errOut, "# server validation FAILED: %d error(s):\n", len(errs))
		for _, f := range errs {
			fmt.Fprintf(errOut, "#   [ERROR] %s\n", formatValidationFinding(f, capture))
		}
		if abortOnError {
			fmt.Fprintln(errOut, "# aborting before any task was created -- no /v2/tasks rows were leaked. Fix the spec and re-apply.")
			return fmt.Errorf("server validation failed with %d error(s); no tasks were created", len(errs))
		}
		return nil
	}

	if len(warns) == 0 && !abortOnError {
		// --dry-run always surfaces a result, even a clean one.
		fmt.Fprintln(errOut, "# server validation: no issues found.")
	}
	return nil
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
// duplicate the real pass's.
func applyAssembleValidateAndPost(c *client.Client, cmd *cobra.Command, spec *composeSpec, publish, skipRescope, allowStealOwnership, noAutoDefaults bool) (map[string]any, error) {
	specCopy := deepCopyComposeSpec(spec)
	capture := newComposeCapture()
	var dryWf map[string]any
	var dryErr error
	withSuppressedStderr(func() {
		dryWf, dryErr = composeWorkflowBody(c, specCopy, true, publish, !skipRescope, allowStealOwnership, !noAutoDefaults, capture)
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
	return composeWorkflowBody(c, spec, false, publish, !skipRescope, allowStealOwnership, !noAutoDefaults, nil)
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
// spec the real posting pass consumes.
func deepCopyComposeSpec(s *composeSpec) *composeSpec {
	cp := *s
	if s.Description != nil {
		d := *s.Description
		cp.Description = &d
	}
	cp.Nodes = deepCopyMapSlice(s.Nodes)
	cp.Tasks = deepCopyMapSlice(s.Tasks)
	cp.ExtraNodes = deepCopyMapSlice(s.ExtraNodes)
	cp.Edges = deepCopyMapSlice(s.Edges)
	cp.Notes = deepCopyMapSlice(s.Notes)
	cp.InputVariables = deepCopyMap(s.InputVariables)
	cp.CustomVariables = deepCopyMap(s.CustomVariables)
	cp.Config = deepCopyMap(s.Config)
	return &cp
}

func deepCopyMapSlice(in []map[string]any) []map[string]any {
	if in == nil {
		return nil
	}
	out := make([]map[string]any, len(in))
	for i, m := range in {
		out[i] = deepCopyMap(m)
	}
	return out
}

func deepCopyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return in // best-effort; spec maps are always JSON-round-trippable
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return in
	}
	return out
}
