package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/spf13/cobra"
)

func diffWorkflow(c *client.Client, cmd *cobra.Command, spec *composeSpec, assembled map[string]any, existing map[string]any, targetAlias string) error {
	out := cmd.OutOrStdout()
	keyFn := nodeAliasKey

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

	shortID := existingID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	var buf strings.Builder
	changes := 0

	// A spec without an explicit status is not a request to drop ACTIVE back to DRAFT:
	// the assembled body injects DRAFT, so the original spec is what decides.
	specHasStatus := spec.Status != ""
	for _, field := range []string{"label", "description", "category", "status", "alias"} {
		a, _ := assembled[field].(string)
		b, _ := current[field].(string)
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

	specNodes := indexNodesBy(toMapSlice(assembled["nodes"]), keyFn)
	currNodes := indexNodesBy(toMapSlice(current["nodes"]), keyFn)
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
		if contains(ch.fields, "inputMappings") {
			diffMappings(&buf, "      ", "inputMappings",
				readNodeMappings(specNodes[key]), readNodeMappings(currNodes[key]))
		}
		changes++
	}

	specNodeKeys := buildNodeKeyByID(toMapSlice(assembled["nodes"]), keyFn)
	currNodeKeys := buildNodeKeyByID(toMapSlice(current["nodes"]), keyFn)
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

	changes += diffVarSection(&buf, "inputVariables",
		toMap(assembled["inputVariables"]),
		toMap(current["inputVariables"]))

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

	printScopeConflicts(out, c, spec, targetAlias)
	return nil
}

type nodeChange struct {
	key    string
	fields []string
}

func nodeAliasKey(n map[string]any) string {
	if alias, _ := n["taskAlias"].(string); alias != "" {
		return alias
	}
	id, _ := n["nodeId"].(string)
	return id
}

func indexNodesBy(nodes []map[string]any, keyFn func(map[string]any) string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, n := range nodes {
		key := keyFn(n)
		if key == "" {
			continue
		}
		out[key] = n
	}
	return out
}

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
		// taskVersion bumps on every apply, so comparing it would report every task changed.
		for _, f := range []string{"type", "label", "config"} {
			if !reflect.DeepEqual(sn[f], cn[f]) {
				fields = append(fields, f)
			}
		}
		if !reflect.DeepEqual(readNodeMappings(sn), readNodeMappings(cn)) {
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

type edgeKey struct {
	source string
	handle string
	target string
}

// Endpoints are mapped through nodeKeyByID, so an edge compares by node identity rather
// than by raw graph id.
func indexEdges(edges []map[string]any, nodeKeyByID map[string]string) map[edgeKey]map[string]any {
	out := map[edgeKey]map[string]any{}
	resolve := func(s string) string {
		if k, ok := nodeKeyByID[s]; ok {
			return k
		}
		return s
	}
	for _, e := range edges {
		src, _ := e["sourceNodeId"].(string)
		tgt, _ := e["targetNodeId"].(string)
		handle, _ := e["sourceHandle"].(string)
		out[edgeKey{resolve(src), handle, resolve(tgt)}] = e
	}
	return out
}

func buildNodeKeyByID(nodes []map[string]any, keyFn func(map[string]any) string) map[string]string {
	out := map[string]string{}
	for _, n := range nodes {
		nodeID, _ := n["nodeId"].(string)
		if nodeID == "" {
			nodeID, _ = n["taskAlias"].(string)
		}
		if nodeID == "" {
			continue
		}
		if key := keyFn(n); key != "" {
			out[nodeID] = key
		}
	}
	return out
}

func formatEdgeKey(k edgeKey) string {
	if k.handle != "" {
		return fmt.Sprintf("%s [%s] -> %s", k.source, k.handle, k.target)
	}
	return fmt.Sprintf("%s -> %s", k.source, k.target)
}

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

// Advisory: a lookup failure is silent (normalize already warned) and an entity already
// scoped to the target is the common case, so it is not printed.
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
			return
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

func quoteName(s string) string {
	return "`" + s + "`"
}

func plural(n int, singular, multiple string) string {
	if n == 1 {
		return singular
	}
	return multiple
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

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

func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// The assembled body puts inputMappings at node.inputMappings; the GET response wraps it
// under node.data.inputMappings.
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
