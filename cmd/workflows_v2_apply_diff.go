package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/spf13/cobra"
)

// stripHashSuffix removes the trailing "-<6hex>" disambiguation suffix that
// BC appends to task aliases when slugifying labels (e.g.
// "start-553bc9" -> "start"). Two reasons we strip it for the diff:
//
//  1. The assembled body in --diff mode runs composeWorkflowBody with
//     dryRun=true, which uses the spec-local `ref` as the alias (no server
//     call). Existing tenant workflows have hash-suffixed aliases. Without
//     this stripping, every node in an existing workflow would show as
//     removed and every spec node as added on the UPDATE path -- which is
//     technically what apply does (it recreates all tasks) but useless as
//     a "what changed?" preview.
//
//  2. The slug encodes the human label, so stripped-slug comparison gives
//     a stable identity that survives the hash regeneration on every apply.
//     Trade-off: two nodes that slug to the same value collide and look
//     identical in the diff. Mitigated by also tracking the original alias
//     for tie-breaking.
var hashSuffixRegex = regexp.MustCompile(`-[0-9a-f]{6}$`)

func stripHashSuffix(s string) string {
	return hashSuffixRegex.ReplaceAllString(s, "")
}

// taskOutputsRefRegex matches `task_outputs.<alias>` segments inside a
// mapping value or template string. Used to canonicalize references so the
// spec's `task_outputs.fetch.X` compares equal to the tenant's
// `task_outputs.fetch-abc123.X`. The replacement strips the trailing
// `-<6hex>` off the alias segment only -- non-task-outputs parts of the
// string are left alone.
var taskOutputsRefRegex = regexp.MustCompile(`task_outputs\.([a-zA-Z0-9_-]+)`)

func normalizeRefValue(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	return taskOutputsRefRegex.ReplaceAllStringFunc(s, func(match string) string {
		// match is "task_outputs.<alias>"; strip just the alias's hash suffix.
		parts := strings.SplitN(match, ".", 2)
		if len(parts) != 2 {
			return match
		}
		return parts[0] + "." + stripHashSuffix(parts[1])
	})
}

// normalizeMappings returns a fresh map with every value passed through
// normalizeRefValue. Mutates nothing on the input. Used by the diff renderer
// to compare inputMappings without the hash-suffix noise.
func normalizeMappings(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = normalizeRefValue(v)
	}
	return out
}

// diffWorkflow renders a human-readable structural diff between the spec
// (already assembled into the workflow body apply would POST/autosave) and
// the current state of the target workflow on the tenant. Read-only: no
// /v2/workflows mutations, no /v2/tasks POSTs, no entity-scope PATCHes.
//
//   - existing == nil  -> CREATE preview: print a summary of what apply would
//     create (counts of tasks, edges, variables).
//   - existing != nil  -> UPDATE preview: GET /v2/workflows/{id}, compare each
//     section (label/description/category/status, nodes-by-taskAlias, edges,
//     inputVariables, customVariables), and print a per-section diff using
//     ASCII markers (+ added, - removed, ~ changed).
//
// Entity-scope conflicts that apply's preflight would catch are also surfaced
// here -- when the spec references credit-decisioning entities whose current
// workflowAlias points at a different workflow, we list the entities (best-
// effort; the preflight inside composeWorkflowBody catches them as hard
// errors before this diff renderer runs, so this branch only fires when
// AllowStealOwnership is true and the entities would be re-stamped).
//
// Exit code 0 on success regardless of whether diffs exist. The caller (apply
// RunE) returns nil and Cobra prints nothing extra; the diff itself goes to
// stdout so it composes with `| less` / `> /tmp/foo`.
func diffWorkflow(c *client.Client, cmd *cobra.Command, spec *composeSpec, assembled map[string]any, existing map[string]any, targetAlias string) error {
	out := cmd.OutOrStdout()

	// === CREATE preview ===
	if existing == nil {
		nodes := toMapSlice(assembled["nodes"])
		edges := toMapSlice(assembled["edges"])
		inputVars := toMap(assembled["inputVariables"])
		customVars := toMap(assembled["customVariables"])
		fmt.Fprintf(out, "+ would CREATE workflow %s\n", quoteName(targetAlias))
		if lbl, _ := assembled["label"].(string); lbl != "" {
			fmt.Fprintf(out, "    label: %q\n", lbl)
		}
		if cat, _ := assembled["category"].(string); cat != "" {
			fmt.Fprintf(out, "    category: %s\n", cat)
		}
		if desc, _ := assembled["description"].(string); desc != "" {
			fmt.Fprintf(out, "    description: %q\n", desc)
		}
		if status, _ := assembled["status"].(string); status != "" {
			fmt.Fprintf(out, "    status: %s\n", status)
		}
		fmt.Fprintf(out, "    nodes: %d\n", len(nodes))
		fmt.Fprintf(out, "    edges: %d\n", len(edges))
		fmt.Fprintf(out, "    inputVariables: %d\n", len(inputVars))
		fmt.Fprintf(out, "    customVariables: %d\n", len(customVars))
		printScopeConflicts(out, c, spec, targetAlias)
		return nil
	}

	// === UPDATE preview ===
	existingID, _ := existing["id"].(string)
	if existingID == "" {
		return fmt.Errorf("diff: existing workflow %q has no id", targetAlias)
	}
	currentRaw, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+existingID, nil)
	if err != nil {
		return fmt.Errorf("diff: fetch current workflow %s: %w", existingID, err)
	}
	var current map[string]any
	if err := json.Unmarshal(currentRaw, &current); err != nil {
		return fmt.Errorf("diff: parse current workflow %s: %w", existingID, err)
	}

	// Header. Slice the id to 8 chars to keep the header compact -- the full
	// UUID is already in the GET URL above so the agent can copy it.
	shortID := existingID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	// Buffer the body of the diff. We only emit the "no changes" footer if
	// nothing landed in the buffer.
	var buf strings.Builder
	changes := 0

	// Metadata diff: only the fields apply actually rewrites on autosave.
	// The spec's status defaults to "DRAFT" if not set, but autosave then
	// publishes the workflow to ACTIVE on the update path -- so a spec
	// without an explicit status is NOT a request to drop ACTIVE back to
	// DRAFT. Skip the status compare when the spec didn't set it (we read
	// the original spec, not the assembled body that injected DRAFT).
	specHasStatus := spec.Status != ""
	for _, field := range []string{"label", "description", "category", "status", "alias"} {
		a, _ := assembled[field].(string)
		b, _ := current[field].(string)
		// Spec's alias may be absent (server-derived); compare only when the
		// assembled body actually carries the field.
		if field == "alias" {
			if _, has := assembled[field]; !has {
				continue
			}
		}
		if field == "status" && !specHasStatus {
			continue
		}
		if a != b {
			fmt.Fprintf(&buf, "  ~ %s: %q -> %q\n", field, b, a)
			changes++
		}
	}

	// Nodes diff: keyed by taskAlias (start nodes have no taskAlias, so we
	// fall back to nodeId for graph-only entries).
	specNodes := indexNodesByKey(toMapSlice(assembled["nodes"]))
	currNodes := indexNodesByKey(toMapSlice(current["nodes"]))
	added, removed, changed := diffNodeIndex(specNodes, currNodes)
	for _, key := range added {
		n := specNodes[key]
		typ, _ := n["type"].(string)
		fmt.Fprintf(&buf, "  + nodes[]: %s (%s)\n", quoteName(key), typ)
		changes++
	}
	for _, key := range removed {
		n := currNodes[key]
		typ, _ := n["type"].(string)
		fmt.Fprintf(&buf, "  - nodes[]: %s (%s)\n", quoteName(key), typ)
		changes++
	}
	for _, ch := range changed {
		key := ch.key
		typ, _ := specNodes[key]["type"].(string)
		fmt.Fprintf(&buf, "  ~ nodes[]: %s (%s) -- %s\n", quoteName(key), typ, strings.Join(ch.fields, ", "))
		// Per-field detail for inputMappings.
		if contains(ch.fields, "inputMappings") {
			diffMappings(&buf, "      ", "inputMappings",
				normalizeMappings(readNodeMappings(specNodes[key])),
				normalizeMappings(readNodeMappings(currNodes[key])))
		}
		changes++
	}

	// Edges diff: keyed by (diff-source, sourceHandle, diff-target) where
	// diff-source/target use the same identity rule as indexNodesByKey
	// (slugified label, falling back to stripped alias).
	specNodeKeys := buildNodeKeyByID(toMapSlice(assembled["nodes"]))
	currNodeKeys := buildNodeKeyByID(toMapSlice(current["nodes"]))
	specEdges := indexEdges(toMapSlice(assembled["edges"]), specNodeKeys)
	currEdges := indexEdges(toMapSlice(current["edges"]), currNodeKeys)
	for k := range specEdges {
		if _, has := currEdges[k]; !has {
			fmt.Fprintf(&buf, "  + edges[]: %s\n", formatEdgeKey(k))
			changes++
		}
	}
	for k := range currEdges {
		if _, has := specEdges[k]; !has {
			fmt.Fprintf(&buf, "  - edges[]: %s\n", formatEdgeKey(k))
			changes++
		}
	}

	// inputVariables diff.
	changes += diffVarSection(&buf, "inputVariables",
		toMap(assembled["inputVariables"]),
		toMap(current["inputVariables"]))

	// customVariables diff.
	changes += diffVarSection(&buf, "customVariables",
		toMap(assembled["customVariables"]),
		toMap(current["customVariables"]))

	if changes == 0 {
		fmt.Fprintf(out, "= workflow %s (id=%s)\n", quoteName(targetAlias), shortID)
		fmt.Fprintf(out, "  no changes -- spec matches current state\n")
	} else {
		fmt.Fprintf(out, "~ workflow %s (id=%s)\n", quoteName(targetAlias), shortID)
		out.Write([]byte(buf.String()))
	}

	// Entity scope conflicts (advisory; preflight has already aborted with
	// a hard error if any cross-ownership exists without --allow-steal-
	// ownership, but we still surface the touched entities so the human
	// reviewer can sanity-check).
	printScopeConflicts(out, c, spec, targetAlias)
	return nil
}

// nodeChange records a per-node field-level diff result.
type nodeChange struct {
	key    string
	fields []string
}

// indexNodesByKey returns a map keyed by a stable diff identity. We strip
// the "-<6hex>" suffix BC appends on task create so re-applies with the
// same spec compare identical (apply recreates all tasks on every update,
// so the raw alias changes every version-bump even when the slug encoding
// the human label doesn't). We also re-slugify the label so that the
// assembled body (which in dry-run uses the spec ref as the alias
// placeholder, NOT the server-derived slug) matches the GET response
// (which uses the server slug of the label).
//
// Priority:
//  1. slugifyWorkflowLabel(label) -- the server's identity rule. Survives
//     re-apply because the label is human-stable.
//  2. stripped taskAlias -- for nodes without labels (rare).
//  3. raw nodeId -- last resort.
func indexNodesByKey(nodes []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, n := range nodes {
		key := ""
		if label, _ := n["label"].(string); label != "" {
			key = slugifyWorkflowLabel(label)
		}
		if key == "" {
			alias, _ := n["taskAlias"].(string)
			if alias == "" {
				alias, _ = n["nodeId"].(string)
			}
			key = stripHashSuffix(alias)
		}
		if key == "" {
			continue
		}
		out[key] = n
	}
	return out
}

// diffNodeIndex returns added / removed / changed-with-field-list lists.
// Sort everything so output is deterministic across re-runs.
func diffNodeIndex(spec, current map[string]map[string]any) (added, removed []string, changed []nodeChange) {
	for k := range spec {
		if _, has := current[k]; !has {
			added = append(added, k)
		}
	}
	for k := range current {
		if _, has := spec[k]; !has {
			removed = append(removed, k)
		}
	}
	for k, sn := range spec {
		cn, has := current[k]
		if !has {
			continue
		}
		fields := []string{}
		// Per-field shallow compare on the fields apply actually mutates.
		// taskVersion bumps every apply (fresh tasks), so we skip it to
		// avoid spamming "all 12 tasks changed taskVersion".
		for _, f := range []string{"type", "label", "config"} {
			if !reflect.DeepEqual(sn[f], cn[f]) {
				fields = append(fields, f)
			}
		}
		// inputMappings is stored at node.inputMappings in the assembled
		// body but at node.data.inputMappings in the GET response. Read
		// from both and normalize task_outputs.<alias> refs before
		// comparing.
		specMappings := normalizeMappings(readNodeMappings(sn))
		currMappings := normalizeMappings(readNodeMappings(cn))
		if !reflect.DeepEqual(specMappings, currMappings) {
			fields = append(fields, "inputMappings")
		}
		if len(fields) > 0 {
			sort.Strings(fields)
			changed = append(changed, nodeChange{key: k, fields: fields})
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Slice(changed, func(i, j int) bool { return changed[i].key < changed[j].key })
	return
}

// edgeKey identifies an edge by its triple (source, handle, target). Empty
// sourceHandle is normalized to "" so legacy edges and explicit "" match.
type edgeKey struct {
	source string
	handle string
	target string
}

// indexEdges keys edges by their (source, handle, target) triple. Endpoints
// are mapped through nodeKeyByID -- the same slugified-label keys
// indexNodesByKey produces -- so that an edge `start-abc123 -> fetch-def456`
// in the current tenant compares identical to `start -> fetch` in the
// assembled spec body. nodeKeyByID is the {nodeId -> diff-key} reverse map
// built by buildNodeKeyByID.
func indexEdges(edges []map[string]any, nodeKeyByID map[string]string) map[edgeKey]map[string]any {
	out := map[edgeKey]map[string]any{}
	resolve := func(s string) string {
		if k, ok := nodeKeyByID[s]; ok {
			return k
		}
		return stripHashSuffix(s)
	}
	for _, e := range edges {
		src, _ := e["sourceNodeId"].(string)
		tgt, _ := e["targetNodeId"].(string)
		handle, _ := e["sourceHandle"].(string)
		out[edgeKey{resolve(src), handle, resolve(tgt)}] = e
	}
	return out
}

// buildNodeKeyByID returns {nodeId -> diff-key} for a node slice. Used to
// translate edge endpoints into the same key space indexNodesByKey uses.
func buildNodeKeyByID(nodes []map[string]any) map[string]string {
	out := map[string]string{}
	for _, n := range nodes {
		nodeID, _ := n["nodeId"].(string)
		if nodeID == "" {
			nodeID, _ = n["taskAlias"].(string)
		}
		if nodeID == "" {
			continue
		}
		key := ""
		if label, _ := n["label"].(string); label != "" {
			key = slugifyWorkflowLabel(label)
		}
		if key == "" {
			alias, _ := n["taskAlias"].(string)
			if alias == "" {
				alias = nodeID
			}
			key = stripHashSuffix(alias)
		}
		out[nodeID] = key
	}
	return out
}

func formatEdgeKey(k edgeKey) string {
	if k.handle != "" {
		return fmt.Sprintf("%s [%s] -> %s", k.source, k.handle, k.target)
	}
	return fmt.Sprintf("%s -> %s", k.source, k.target)
}

// diffVarSection emits a +/-/~ block for inputVariables or customVariables.
// Returns the number of lines emitted so the caller knows whether to print
// the "no changes" footer.
func diffVarSection(buf *strings.Builder, sectionName string, spec, current map[string]any) int {
	added := []string{}
	removed := []string{}
	changed := []string{}
	for k := range spec {
		if _, has := current[k]; !has {
			added = append(added, k)
		}
	}
	for k := range current {
		if _, has := spec[k]; !has {
			removed = append(removed, k)
		}
	}
	for k, sv := range spec {
		cv, has := current[k]
		if !has {
			continue
		}
		if !reflect.DeepEqual(sv, cv) {
			changed = append(changed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	for _, k := range added {
		fmt.Fprintf(buf, "  + %s[]: %s %s\n", sectionName, quoteName(k), varBrief(spec[k]))
	}
	for _, k := range removed {
		fmt.Fprintf(buf, "  - %s[]: %s %s\n", sectionName, quoteName(k), varBrief(current[k]))
	}
	for _, k := range changed {
		fmt.Fprintf(buf, "  ~ %s[]: %s\n", sectionName, quoteName(k))
		printVarChange(buf, "      ", toMap(spec[k]), toMap(current[k]))
	}
	return len(added) + len(removed) + len(changed)
}

// varBrief renders a one-line summary "(type, default=X)" for an inputVariable
// or customVariable definition. Used in + / - lines where the value is
// dropped wholesale and a compact summary is more useful than a JSON dump.
func varBrief(raw any) string {
	m, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	typ, _ := m["type"].(string)
	parts := []string{}
	if typ != "" {
		parts = append(parts, typ)
	}
	if def, has := m["default"]; has {
		parts = append(parts, fmt.Sprintf("default=%v", def))
	}
	if required, _ := m["required"].(bool); required {
		parts = append(parts, "required")
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// printVarChange walks the keys of both maps and prints per-field changes.
// Stays at one level deep -- nested map changes get dumped via JSON.
func printVarChange(buf *strings.Builder, indent string, spec, current map[string]any) {
	keys := map[string]bool{}
	for k := range spec {
		keys[k] = true
	}
	for k := range current {
		keys[k] = true
	}
	sortedKeys := make([]string, 0, len(keys))
	for k := range keys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	for _, k := range sortedKeys {
		sv, sHas := spec[k]
		cv, cHas := current[k]
		if !reflect.DeepEqual(sv, cv) {
			if !cHas {
				fmt.Fprintf(buf, "%s+ %s: %s\n", indent, k, briefVal(sv))
			} else if !sHas {
				fmt.Fprintf(buf, "%s- %s: %s\n", indent, k, briefVal(cv))
			} else {
				fmt.Fprintf(buf, "%s~ %s: %s -> %s\n", indent, k, briefVal(cv), briefVal(sv))
			}
		}
	}
}

// diffMappings emits per-key changes for an inputMappings (or any flat
// string-keyed) map. Both values are stringified for the diff line.
func diffMappings(buf *strings.Builder, indent, label string, spec, current map[string]any) {
	keys := map[string]bool{}
	for k := range spec {
		keys[k] = true
	}
	for k := range current {
		keys[k] = true
	}
	sortedKeys := make([]string, 0, len(keys))
	for k := range keys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	for _, k := range sortedKeys {
		sv, sHas := spec[k]
		cv, cHas := current[k]
		if !reflect.DeepEqual(sv, cv) {
			if !cHas {
				fmt.Fprintf(buf, "%s+ %s.%s: %s\n", indent, label, k, briefVal(sv))
			} else if !sHas {
				fmt.Fprintf(buf, "%s- %s.%s: %s\n", indent, label, k, briefVal(cv))
			} else {
				fmt.Fprintf(buf, "%s~ %s.%s: %s -> %s\n", indent, label, k, briefVal(cv), briefVal(sv))
			}
		}
	}
}

// briefVal returns a one-line representation of any JSON-shaped value.
// Strings are quoted, scalars dumped as-is, nested maps/slices JSON-encoded.
func briefVal(v any) string {
	if v == nil {
		return "null"
	}
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("%q", t)
	case bool, float64, int, int64:
		return fmt.Sprintf("%v", t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// printScopeConflicts walks the spec's credit-decisioning entities and lists
// the ones the apply would touch via reconcileEntityScopes. Each entity is
// classified:
//   - "would CLAIM":     entity has no workflowAlias yet (apply will stamp)
//   - "ok (already)":    entity already scoped to the target alias (no-op)
//   - "would RE-STAMP":  entity is currently owned by another workflow
//     (apply errors unless --allow-steal-ownership is passed; we still list
//     it so the human reviewer sees the intent)
//
// Best-effort: lookup failures are silent (the missing entity was already
// warned by composeWorkflowBody's normalize step). Only prints if at least
// one entity is "would CLAIM" or "would RE-STAMP" -- the no-op case is the
// common one and noise.
func printScopeConflicts(out io.Writer, c *client.Client, spec *composeSpec, targetAlias string) {
	if c == nil || targetAlias == "" {
		return
	}
	type touch struct {
		resource string
		ref      string
		actual   string
	}
	touches := []touch{}
	seen := map[string]bool{}

	check := func(resource, ref string) {
		if ref == "" {
			return
		}
		key := resource + "|" + ref
		if seen[key] {
			return
		}
		seen[key] = true
		entity, _ := lookupEntity(c, resource, ref, false)
		if entity == nil {
			return
		}
		actual, _ := entity["workflowAlias"].(string)
		if actual == targetAlias {
			return // no-op
		}
		touches = append(touches, touch{resource, ref, actual})
	}

	walk := func(tasks []map[string]any) {
		for _, t := range tasks {
			tt, _ := t["type"].(string)
			switch tt {
			case "scorecard":
				cfg, _ := t["scorecardConfig"].(map[string]any)
				if cfg != nil {
					code, _ := cfg["scorecardCode"].(string)
					if code == "" {
						code, _ = cfg["scorecardId"].(string)
					}
					check("scorecards", code)
				}
			case "rule-tree":
				cfg, _ := t["ruleTreeConfig"].(map[string]any)
				if cfg != nil {
					code, _ := cfg["ruleTreeCode"].(string)
					if code == "" {
						code, _ = cfg["ruleTreeId"].(string)
					}
					check("rule-trees", code)
				}
			case "evaluate-rules":
				rules, _ := t["rulesConfig"].([]any)
				for _, rraw := range rules {
					rm, ok := rraw.(map[string]any)
					if !ok {
						continue
					}
					rc, _ := rm["ruleCode"].(string)
					if rc == "" {
						rc, _ = rm["ruleId"].(string)
					}
					check("evaluation-rules", rc)
				}
			case "mapping-table":
				cfg, _ := t["mappingTableConfig"].(map[string]any)
				if cfg == nil {
					continue
				}
				entries, _ := cfg["entries"].([]any)
				for _, eraw := range entries {
					em, ok := eraw.(map[string]any)
					if !ok {
						continue
					}
					mt, _ := em["mappingTableCode"].(string)
					if mt == "" {
						mt, _ = em["mappingTableId"].(string)
					}
					check("mapping-tables", mt)
				}
			}
		}
	}
	walk(spec.Tasks)
	walk(spec.ExtraNodes)

	if len(touches) == 0 {
		return
	}
	// Stable order.
	sort.Slice(touches, func(i, j int) bool {
		if touches[i].resource != touches[j].resource {
			return touches[i].resource < touches[j].resource
		}
		return touches[i].ref < touches[j].ref
	})
	claim := []touch{}
	steal := []touch{}
	for _, t := range touches {
		if t.actual == "" {
			claim = append(claim, t)
		} else {
			steal = append(steal, t)
		}
	}
	if len(claim) > 0 {
		fmt.Fprintf(out, "~ Would CLAIM %d unscoped entit%s for workflow %q:\n",
			len(claim), plural(len(claim), "y", "ies"), targetAlias)
		for _, t := range claim {
			fmt.Fprintf(out, "    + %s/%s (currently unscoped)\n", t.resource, t.ref)
		}
	}
	if len(steal) > 0 {
		fmt.Fprintf(out, "~ Would RE-STAMP %d cross-owned entit%s (requires --allow-steal-ownership):\n",
			len(steal), plural(len(steal), "y", "ies"))
		for _, t := range steal {
			fmt.Fprintf(out, "    ! %s/%s currently owned by %q -> would become %q\n",
				t.resource, t.ref, t.actual, targetAlias)
		}
	}
}

// quoteName wraps a name in backticks for the diff output. Backticks read
// well in stderr against terminals that interpret single/double quotes as
// shell delimiters when copy-pasting.
func quoteName(s string) string {
	return "`" + s + "`"
}

// plural is a 1-arg ternary -- "1 entity" vs "N entities".
func plural(n int, singular, multiple string) string {
	if n == 1 {
		return singular
	}
	return multiple
}

// contains returns true if needle is in haystack.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// toMapSlice coerces an "any" value into []map[string]any. The assembled
// body uses []map[string]any directly but the GET response (parsed from
// JSON) uses []any with map[string]any items. Both shapes show up here.
func toMapSlice(v any) []map[string]any {
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

// toMap coerces an "any" value into map[string]any. Returns an empty (non-
// nil) map so iteration is safe.
func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// readNodeMappings extracts inputMappings from a node, supporting both
// shapes: the assembled body puts it at node.inputMappings, while the GET
// response wraps it under node.data.inputMappings. Returns nil when neither
// is present.
func readNodeMappings(n map[string]any) map[string]any {
	if m, ok := n["inputMappings"].(map[string]any); ok && len(m) > 0 {
		return m
	}
	data, _ := n["data"].(map[string]any)
	if data == nil {
		return nil
	}
	if m, ok := data["inputMappings"].(map[string]any); ok {
		return m
	}
	return nil
}
