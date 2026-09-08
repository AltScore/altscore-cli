package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Shared types for the workflows-v2 validation surface: the assembly capture
// `apply` sends to POST /v2/workflows/apply, and the finding shapes both that
// endpoint and POST /v2/workflows/validate (used by `lint` and `import`) answer
// with. No validation rule is re-implemented in Go: the server is the single
// oracle, and an unrecognized finding code is still rendered from its message
// and severity.

// composeCapture is what the single assembly pass records for the request that
// follows it. Assembly POSTs nothing, so a task's node id is a PLACEHOLDER --
// the spec-local ref or an explicit alias -- not a server-minted alias.
//
//   - tasks: the exact per-node task bodies, keyed by placeholder; they travel
//     inline on the flat spec's nodes.
//   - refByNodeID: placeholder -> spec-local ref, so findings read with the
//     author's own names.
type composeCapture struct {
	tasks       map[string]json.RawMessage
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

// validationResponse is the verdict shape POST /v2/workflows/apply and
// POST /v2/workflows/validate answer with (the latter returns 200 even when the
// graph is invalid; `valid` and `findings` carry the verdict). skippedNodeIds
// names the nodes whose task body the server could not resolve; apply sends
// bodies inline so nothing is skipped, `lint` reports it because there the
// bodies come from the persisted repository.
type validationResponse struct {
	Valid          bool                `json:"valid"`
	Findings       []validationFinding `json:"findings"`
	SkippedNodeIDs []string            `json:"skippedNodeIds"`
	// alias -> spec-local ref, sent by POST /v2/workflows/apply so findings
	// that name a minted alias can be shown with the author's own name.
	// Absent (nil) on /validate responses.
	Refs map[string]string `json:"refs"`
}

// preflightResponseDetail returns a short, trimmed description of a rejected
// validation response for lint's contract-mismatch note. The client folds a
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
