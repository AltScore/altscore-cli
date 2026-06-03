package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// verifyAppliedTasks reads back the tasks that apply just persisted and warns
// (to stderr, best-effort, non-fatal) about any field the SPEC explicitly set
// that came back missing or null on the persisted task.
//
// This catches silent server-side field drops -- the exact class of bug where
// the spec sent `contacts: [...]` on a task, apply's --diff happily previewed
// it, the POST succeeded, and the backend quietly discarded the field because
// its task schema didn't model it. Nothing downstream noticed until runtime.
//
// Scope is deliberately narrow to stay high-signal:
//   - Only top-level fields the spec explicitly set on a task node are checked
//     (so we never warn about server-added defaults the spec didn't ask for).
//   - Bookkeeping / graph-only keys (ref, type, label, position, data, ...) are
//     excluded -- they aren't task-body fields.
//   - A field is "dropped" when it is absent on the persisted task, or present
//     but null, while the spec set it to a non-null value.
//
// Correlation: the assembled `workflow` body carries one node per spec node,
// each with the server-assigned `taskAlias` plus the original `type`/`label`.
// We match spec nodes to assembled nodes by (type, label) -- labels are
// effectively unique within a workflow -- then GET /v2/tasks/{alias} for the
// persisted body. Any correlation or fetch failure is logged and skipped; the
// workflow itself is already applied, so verification never blocks success.
func verifyAppliedTasks(c *client.Client, spec *composeSpec, workflow map[string]any, errOut io.Writer) {
	if c == nil || spec == nil || workflow == nil {
		return
	}

	// Build (type|label) -> server taskAlias from the assembled workflow nodes.
	aliasByKey := map[string]string{}
	for _, raw := range coerceNodeSlice(workflow["nodes"]) {
		typ, _ := raw["type"].(string)
		label, _ := raw["label"].(string)
		alias, _ := raw["taskAlias"].(string)
		if alias == "" {
			continue
		}
		aliasByKey[typ+"|"+label] = alias
	}

	// Every spec node that backs a task (Tasks + ExtraNodes; start nodes still
	// get a trivial backing task, so they're worth a glance too).
	specNodes := append(append([]map[string]any{}, spec.Tasks...), spec.ExtraNodes...)

	type drop struct {
		alias  string
		typ    string
		label  string
		fields []string
	}
	var drops []drop
	checked := 0

	for _, node := range specNodes {
		typ, _ := node["type"].(string)
		label, _ := node["label"].(string)
		alias := aliasByKey[typ+"|"+label]
		if alias == "" {
			continue
		}

		persisted, err := fetchPersistedTask(c, alias)
		if err != nil {
			fmt.Fprintf(errOut, "# verify: could not read back task %q (%v); skipping\n", alias, err)
			continue
		}
		checked++

		missing := droppedSpecFields(node, persisted)
		if len(missing) > 0 {
			drops = append(drops, drop{alias: alias, typ: typ, label: label, fields: missing})
		}
	}

	if len(drops) == 0 {
		if checked > 0 {
			fmt.Fprintf(errOut, "# verify: read back %d task(s); all spec-set fields persisted\n", checked)
		}
		return
	}

	fmt.Fprintf(errOut, "# WARNING: read-back verification found spec fields the backend did not persist.\n")
	fmt.Fprintf(errOut, "# These were present in the spec but missing/null on the saved task -- the server\n")
	fmt.Fprintf(errOut, "# likely dropped them silently (its task schema may not model the field).\n")
	for _, d := range drops {
		fmt.Fprintf(errOut, "#   task %s (type=%s, label=%q): dropped %s\n",
			d.alias, d.typ, d.label, strings.Join(d.fields, ", "))
	}
}

// droppedSpecFields returns the sorted list of top-level fields the spec node
// meaningfully set (non-empty, non-bookkeeping) that are absent or null on the
// persisted task. This is the pure comparison core of the read-back verifier.
func droppedSpecFields(specNode, persisted map[string]any) []string {
	var missing []string
	for k, v := range specNode {
		if verifyExcludedTaskFields[k] {
			continue
		}
		if isEmptyVerifyValue(v) {
			// Spec didn't meaningfully set it -- nothing to verify.
			continue
		}
		pv, present := persisted[k]
		if !present || pv == nil {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

// verifyExcludedTaskFields are keys on a spec node that are NOT persisted task
// body fields (graph wiring, spec-local bookkeeping, or sugar apply expands).
// Comparing them against the task read-back would produce false positives.
var verifyExcludedTaskFields = map[string]bool{
	"ref":           true, // spec-local, stripped before POST
	"type":          true, // present on task too, but graph-authoritative; skip
	"label":         true, // ditto
	"nodeId":        true,
	"taskAlias":     true,
	"taskId":        true,
	"taskVersion":   true,
	"position":      true,
	"data":          true, // canvas mirror, not a task field
	"specRef":       true, // injected by compose
	"workflowAlias": true, // injected by compose
	"htmlSections":  true, // spec sugar -> endConfig.pdfConfig, not a task field
}

// isEmptyVerifyValue reports whether a spec value is "unset" for verification
// purposes -- nil, empty string, empty slice, or empty map. We only verify
// fields the spec meaningfully populated.
func isEmptyVerifyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// fetchPersistedTask GETs the latest version of a task by alias and returns its
// body as a generic map.
func fetchPersistedTask(c *client.Client, alias string) (map[string]any, error) {
	data, _, err := c.Do("GET", "borrower_central", "/v2/tasks/"+alias, nil)
	if err != nil {
		return nil, err
	}
	var task map[string]any
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("parse task: %w", err)
	}
	return task, nil
}

// coerceNodeSlice normalizes workflow["nodes"] into []map[string]any. The
// assembled body stores it as []map[string]any, but a re-parsed body (e.g.
// from an autosave response) may carry []any of map[string]any.
func coerceNodeSlice(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}
