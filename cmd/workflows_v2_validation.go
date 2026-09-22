package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Assembly POSTs nothing, so a task's node id here is a PLACEHOLDER -- the spec-local
// ref or an explicit alias -- never a server-minted one.
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

type validationFinding struct {
	Code     string         `json:"code"`
	Severity string         `json:"severity"`
	NodeID   string         `json:"nodeId"`
	EdgeID   string         `json:"edgeId"`
	Params   map[string]any `json:"params"`
	Message  string         `json:"message"`
}

// /validate answers 200 even when the graph is invalid; `valid` and `findings` carry
// the verdict.
type validationResponse struct {
	Valid          bool                `json:"valid"`
	Findings       []validationFinding `json:"findings"`
	SkippedNodeIDs []string            `json:"skippedNodeIds"`
	// alias -> spec-local ref, sent by apply only; absent on /validate responses.
	Refs map[string]string `json:"refs"`
}

// The client folds a >=400 body into derr and leaves data nil, hence the fallback.
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

func dimNote(w io.Writer, msg string) {
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintf(w, "\x1b[2m# %s\x1b[0m\n", msg)
		return
	}
	fmt.Fprintf(w, "# %s\n", msg)
}

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
