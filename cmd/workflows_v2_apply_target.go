package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// Target resolution: which workflow, if any, the spec reconciles against.

func findWorkflowByAlias(c *client.Client, alias string) (map[string]any, string, error) {
	active, err := queryLatestWorkflowByAliasStatus(c, alias, "ACTIVE")
	if err != nil {
		return nil, "", err
	}
	if active != nil {
		return active, "ACTIVE", nil
	}
	draft, err := queryLatestWorkflowByAliasStatus(c, alias, "DRAFT")
	if err != nil {
		return nil, "", err
	}
	if draft != nil {
		return draft, "DRAFT", nil
	}
	return nil, "", nil
}

// workflowNodeTypes collects the `type` of every node in a live workflow
// payload -- the set the deprecation gate diffs the incoming spec against, so a
// retired type the target already holds is recognised as a carry-forward rather
// than as new authoring (see deprecatedTaskTypeRefused).
//
// No request of its own: findWorkflowByAlias already returns the full node
// list. GET /v2/workflows?alias=... serves the same DTO as the single-workflow
// GET and drops only config.taskSnapshots (borrower-central
// app/model/workflows_v2/workflows.py::to_api_dto), so nodes[].type is present
// on the listing payload apply has in hand before assembly.
//
// nil in, nil out: a create has no target, so it carries nothing forward and
// every deprecated type stays refused.
func workflowNodeTypes(workflow map[string]any) map[string]bool {
	if workflow == nil {
		return nil
	}
	out := map[string]bool{}
	for _, node := range toMapSlice(workflow["nodes"]) {
		if t, _ := node["type"].(string); t != "" {
			out[t] = true
		}
	}
	return out
}

// queryLatestWorkflowByAliasStatus returns the highest-version workflow matching
// alias+status, or nil when none match. BC handlers have historically ignored
// the query filters, so we also filter client-side after parsing.
func queryLatestWorkflowByAliasStatus(c *client.Client, alias, status string) (map[string]any, error) {
	if alias == "" {
		return nil, nil
	}
	q := url.Values{}
	q.Set("alias", alias)
	q.Set("status", status)
	q.Set("is-latest", "true")
	q.Set("per-page", "10")
	path := "/v2/workflows?" + q.Encode()
	data, _, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, err
	}
	// BC list endpoints return a JSON array at top level.
	var arr []map[string]any
	if jerr := json.Unmarshal(data, &arr); jerr != nil {
		// Some endpoints wrap in {items: [...], total: ...}; tolerate both.
		var wrapped struct {
			Items []map[string]any `json:"items"`
		}
		if werr := json.Unmarshal(data, &wrapped); werr != nil {
			return nil, fmt.Errorf("parse list response: %w", jerr)
		}
		arr = wrapped.Items
	}
	matches := []map[string]any{}
	for _, w := range arr {
		wa, _ := w["alias"].(string)
		if wa == "" {
			wa, _ = w["workflowAlias"].(string)
		}
		st, _ := w["status"].(string)
		if wa == alias && st == status {
			matches = append(matches, w)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > 1 {
		// Pick the highest version.
		best := matches[0]
		bestV := 0
		if v, ok := best["version"].(float64); ok {
			bestV = int(v)
		}
		for _, m := range matches[1:] {
			if v, ok := m["version"].(float64); ok && int(v) > bestV {
				best = m
				bestV = int(v)
			}
		}
		return best, nil
	}
	return matches[0], nil
}

// slugifyWorkflowLabel mirrors borrower-central's
// app/utils/workflow_alias.py:slugify_workflow_label so compose can predict
// the alias the server derives for a spec without an explicit `alias`. Knowing
// it up-front matters because credit-decisioning entities (evaluation-rules,
// rule-trees, mapping-tables, scorecards) only show up in a workflow's builder
// pickers when their workflowAlias matches the workflow's alias: a label like
// "All 5 types" silently slugs to "all-5-types", and entities stamped with
// "all-types" become invisible.
//
// Rules (must match BC byte-for-byte):
//   - lowercase + strip
//   - replace any run of non-[a-z0-9] with "-"
//   - collapse repeated "-"
//   - trim leading/trailing "-"
//   - cap at 100 chars
//   - default to "workflow" when empty
func slugifyWorkflowLabel(label string) string {
	s := strings.ToLower(strings.TrimSpace(label))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 100 {
		out = out[:100]
	}
	if out == "" {
		out = "workflow"
	}
	return out
}
