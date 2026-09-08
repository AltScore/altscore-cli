package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Spec-local ref rewriting and dependency ordering for the assembly pass.

// reservedMappingScopes are the leading segments in a mapping value that are
// NOT spec-local refs and must not be rewritten.
var reservedMappingScopes = map[string]bool{
	"inputs":               true,
	"custom":               true,
	"system":               true,
	"task_outputs":         true,
	"task_outputs_by_type": true,
	// entity.<root>.<group>.<key>[.<subkey>] is a backend pass-through scope
	// (e.g. entity.borrower.identities.tax_id, entity.deal.deal_fields.x.amount,
	// entity.<alias>:<handle>.identities.cedula for rel/deal-contact branch
	// roots). The CLI has no data-model catalog, so like the other reserved
	// scopes it only recognises the leading segment and never validates deeper.
	"entity": true,
	// self.<field> is a node's OWN late-resolved output -- today End's pdf_url /
	// pdf_error, which do not exist when the backend substitutes endConfig (the
	// PDF is generated inside end_activity) and never reach task_outputs because
	// End is data_producing=false. BC leaves the literal in place and resolves it
	// in a second pass, so apply must not rewrite or reject it.
	"self": true,
	// documents.<key>.<attr> is the http task's OWN late-resolved input -- the
	// task's optional `documents` list names borrower documents by key and the
	// body template reads {{documents.<key>.base64}} (also .fileName,
	// .mimeType, .sizeBytes, .files). The bytes do not exist at graph time and
	// never reach task_outputs; BC resolves the literal in a second pass inside
	// http_activity, exactly like self.*, so apply must leave it alone. Only
	// meaningful in an http body; anywhere else the runtime renders it empty.
	"documents": true,
}

// reservedScopesList renders reservedMappingScopes for error messages, so the
// hint can never drift from the map the validators actually consult.
func reservedScopesList() string {
	keys := make([]string, 0, len(reservedMappingScopes))
	for k := range reservedMappingScopes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// mappingDependencyRef returns the spec-local task ref that an inputMappings
// VALUE depends on, or "" when the value is a literal / refers to a reserved
// namespace / doesn't have a path-like shape. Used by both the topological
// sort (to know which task each mapping depends on) and rewriteRefsInMappings
// (to know whether the head needs rewriting).
//
// Recognised forms:
//
//	"task_outputs.<ref>.<deep>" -> "<ref>"
//	"<ref>.<rest>"              -> "<ref>" (when head is not a reserved scope)
//	"inputs.<name>"             -> ""     (reserved scope, no task dep)
//	"custom.<name>"             -> ""     (reserved scope)
//	"<literal>" (no dot)        -> ""
func mappingDependencyRef(s string) string {
	dot := strings.Index(s, ".")
	if dot <= 0 {
		return ""
	}
	if strings.HasPrefix(s, "task_outputs.") {
		rest := s[len("task_outputs."):]
		innerDot := strings.Index(rest, ".")
		if innerDot <= 0 {
			return ""
		}
		return rest[:innerDot]
	}
	head := s[:dot]
	if reservedMappingScopes[head] {
		return ""
	}
	return head
}

// templateDependencyRefs returns the spec-local refs a task body's template
// strings reference, for use by topologicalTaskOrder. Mirrors
// mappingDependencyRef but scans {{...}} placeholders across known template
// fields per task type. Empty when the task type has no template fields or
// all references are reserved scopes / unknown heads.
//
// Keep this in step with rewriteRefsInTaskTemplates. A template field the
// rewriter walks but this function does not is ordered wrong: the consumer can
// be POSTed before the task it references, refMap has no alias for that ref
// yet, and the rewrite silently leaves the spec ref in place -- the exact
// failure the rewriter was added to prevent.
func templateDependencyRefs(task map[string]any) []string {
	taskType, _ := task["type"].(string)
	var fields []string
	switch taskType {
	case "http", "webhook":
		for _, f := range []string{"url", "body", "headers"} {
			if s, ok := task[f].(string); ok && s != "" {
				fields = append(fields, s)
			}
		}
	case "end":
		if endCfg, ok := task["endConfig"].(map[string]any); ok {
			if s, ok := endCfg["outputJson"].(string); ok && s != "" {
				fields = append(fields, s)
			}
		}
	case "exception":
		if s, ok := task["errorMessage"].(string); ok && s != "" {
			fields = append(fields, s)
		}
	case "data-store-write":
		if cfg, _ := task["dataStoreWriteConfig"].(map[string]any); cfg != nil {
			for _, m := range asMapSlice(cfg["columnMappings"]) {
				if s, ok := m["valueTemplate"].(string); ok && s != "" {
					fields = append(fields, s)
				}
			}
			if s, ok := cfg["batchSource"].(string); ok && s != "" {
				fields = append(fields, s)
			}
		}
	case "data-store-query":
		if cfg, _ := task["dataStoreQueryConfig"].(map[string]any); cfg != nil {
			if s, ok := cfg["sql"].(string); ok && s != "" {
				fields = append(fields, s)
			}
			if params, ok := cfg["sqlParameters"].(map[string]any); ok {
				for _, v := range params {
					if s, ok := v.(string); ok && s != "" {
						fields = append(fields, s)
					}
				}
			}
			for _, m := range asMapSlice(cfg["filters"]) {
				if s, ok := m["valueTemplate"].(string); ok && s != "" {
					fields = append(fields, s)
				}
			}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	add := func(ref string) {
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, ref)
	}
	for _, s := range fields {
		if !strings.Contains(s, "{{") {
			// Bare `task_outputs.<ref>.<path>` -- the shape data-store
			// batchSource / sqlParameters are usually authored in, and the only
			// shape resolve_context_field accepts. mappingDependencyRef on a
			// non-path literal returns "" or an unknown head, and
			// topologicalTaskOrder ignores a ref that names no task, so a plain
			// SQL string or an item key costs nothing here.
			add(mappingDependencyRef(s))
			continue
		}
		for _, m := range templatePlaceholderRegex.FindAllStringSubmatch(s, -1) {
			add(mappingDependencyRef(strings.TrimSpace(m[1])))
		}
	}
	return out
}

// rewriteRefsInMappings resolves the spec-local ref at the head of each mapping
// value through refMap. Spec refs are the human-friendly names the spec assigns
// to each task (`ref` / `alias` / `nodeId` fallback); the server substitutes
// the slug-NNNNNN alias it assigns to the task.
//
// Two shapes are recognised:
//   - long  "task_outputs.<taskRef>.<field>" -> "task_outputs.<server-alias>.<field>"
//   - bare  "<taskRef>.<field>"              -> "<server-alias>.<field>"
//
// Both forms are accepted by the BC runtime resolver: the bare form is the
// implicit task_outputs.<alias> shortcut, expanded server-side.
//
// Reserved scopes (inputs, custom, system, task_outputs, task_outputs_by_type,
// entity) are never treated as refs.
//
// Errors when a mapping value has a path-like shape whose head is neither a
// reserved scope nor a known ref. composeWorkflowBody sorts tasks
// topologically before this runs, so a remaining unknown ref is always either
// a typo or a reference to a task that simply isn't in spec.tasks.
func rewriteRefsInMappings(mappings map[string]any, refMap map[string]string) (map[string]any, error) {
	if len(mappings) == 0 {
		return mappings, nil
	}
	out := map[string]any{}
	for k, v := range mappings {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		// Long form: task_outputs.<ref>.<field>
		if strings.HasPrefix(s, "task_outputs.") {
			rest := s[len("task_outputs."):]
			if dot := strings.Index(rest, "."); dot > 0 {
				ref := rest[:dot]
				if alias, found := refMap[ref]; found {
					s = "task_outputs." + alias + rest[dot:]
				} else if !isServerAlias(ref) {
					return nil, fmt.Errorf(
						"inputMappings[%q]=%q references task_outputs.%s.* but %q is not a known spec ref. "+
							"Known refs: %s. (Reserved scopes: %s.)",
						k, v, ref, ref, sortedRefMapKeys(refMap), reservedScopesList())
				}
			}
		} else if dot := strings.Index(s, "."); dot > 0 {
			// Bare <ref>.<rest> -- substitute the head with the server-assigned
			// alias and leave the rest in bare form. BC's resolver treats bare
			// alias.<field> as implicit task_outputs.<alias>.<field>.
			head := s[:dot]
			if !reservedMappingScopes[head] {
				if alias, found := refMap[head]; found {
					s = alias + s[dot:]
				} else if isServerAlias(head) {
					// User supplied a server-style alias directly -- leave it.
					// BC's resolver matches it against task_outputs at runtime.
				} else {
					return nil, fmt.Errorf(
						"inputMappings[%q]=%q has head %q which is neither a reserved namespace "+
							"nor a known spec ref nor a server alias (slug-NNNNNN). "+
							"Known refs: %s. (Reserved scopes: %s.)",
						k, v, head, sortedRefMapKeys(refMap), reservedScopesList())
				}
			}
		}
		out[k] = s
	}
	return out, nil
}

// templatePlaceholderRegex matches `{{...}}` substitutions used by the BC
// runtime template engine in http body/headers/url and end outputJson.
// Whitespace around the inner expression is tolerated. The captured group
// is the inner expression (e.g. "task_outputs.fetch.tax_id" or "borrower_id").
var templatePlaceholderRegex = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// rewriteTaskOutputsRefsInString rewrites `task_outputs.<spec-ref>.` substrings
// in arbitrary strings. Used by composeWorkflowBody to rewrite customVariable
// expressions / returnValues / dependencies arrays after task aliases have
// been assigned. Unknown refs (not in refMap) are left alone so server-style
// aliases and other scopes (inputs, custom, system) pass through unchanged.
//
// The trailing dot in the match prefix is a boundary marker: ref "a" won't
// accidentally match "task_outputs.a-extended." because the char after "a"
// is "-", not ".". Maps are iterated in non-deterministic order; the trailing
// dot also prevents one ref's rewrite from feeding into another (the alias
// "a-server" doesn't contain "task_outputs.a." after rewriting).
func rewriteTaskOutputsRefsInString(s string, refMap map[string]string) string {
	if s == "" || refMap == nil {
		return s
	}
	for ref, alias := range refMap {
		if ref == alias {
			continue
		}
		s = replaceTaskOutputsRef(s, ref, alias)
	}
	return s
}

// replaceTaskOutputsRef swaps `task_outputs.<ref>` for `task_outputs.<alias>`
// wherever the ref ends on a real boundary: a dot, a bracket (`[0]`, `[].f`,
// legal in the Hub's formula syntax) or the end of the string.
//
// The ref must not be followed by a word char or a hyphen, so a ref `tabla`
// cannot match inside `task_outputs.tablas`. Go's RE2 has no lookahead, so the
// check is a manual peek at the next byte, which is also cheaper than a regex
// per ref per string.
func replaceTaskOutputsRef(s, ref, alias string) string {
	if !containsTaskOutputsRef(s, ref) {
		return s
	}
	needle := "task_outputs." + ref
	var b strings.Builder
	for {
		i := strings.Index(s, needle)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(needle)
		if end < len(s) && isRefNameByte(s[end]) {
			// A longer ref starts here (tabla vs tablas): copy past this
			// candidate and keep scanning.
			b.WriteString(s[:end])
			s = s[end:]
			continue
		}
		b.WriteString(s[:i])
		b.WriteString("task_outputs." + alias)
		s = s[end:]
	}
}

// containsTaskOutputsRef reports whether s references ref on a real boundary.
// It is the read-only twin of replaceTaskOutputsRef and must agree with it:
// validateNoResidualSpecRefs exists to catch a field the rewriter forgot to
// walk, so a boundary the detector does not recognise is a residual ref that
// ships silently -- the guard would be blind in exactly the shapes the rewriter
// is blind to.
func containsTaskOutputsRef(s, ref string) bool {
	needle := "task_outputs." + ref
	for i := 0; ; {
		j := strings.Index(s[i:], needle)
		if j < 0 {
			return false
		}
		end := i + j + len(needle)
		if end >= len(s) || !isRefNameByte(s[end]) {
			return true
		}
		i = end
	}
}

// isRefNameByte reports whether c can continue a spec-local ref name. Refs are
// lowercase alphanumeric with internal dashes, so anything else -- a dot, a
// bracket, a quote, whitespace, end of string -- terminates one.
func isRefNameByte(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// rewriteTaskOutputsRefsDeep rewrites every `task_outputs.<ref>` reference
// reachable from o, in place: string values at any depth AND map keys. It is
// the one generic pass behind rewriteCustomVariableRefs and the task-body
// rewrite in rewriteTaskRefs, so a new field carrying the long form is covered
// without anyone remembering to name it. Six fixes (#18, #20, #21, #106, #111
// and the pdf sourcesConfig one before them) were each "rewrite refs in one
// more field"; the long form is unambiguous, so it can be rewritten wherever it
// appears.
//
// skipFields names the map keys whose string values are prose and are left
// alone (label, description, ...). The nearest enclosing key applies through
// arrays, which is the rule validateNoResidualSpecRefs uses to skip the same
// strings, so the pass and the guard always agree on the surface. Keys are
// always rewritten: the only key-borne refs today are
// customVariables[].dependencyTypes, keyed BY the dependency string, and a
// key-shaped ref is never prose.
//
// A key already present under the destination name wins: it names a
// dependency the author declared directly, and overwriting it with a
// carried-over entry would replace a correct declaration with a stale one.
// Moves run in sorted source order so the outcome does not depend on Go's map
// iteration order.
func rewriteTaskOutputsRefsDeep(o any, refMap map[string]string, skipFields map[string]bool) any {
	return rewriteDeepUnderField(o, "", refMap, skipFields)
}

func rewriteDeepUnderField(o any, field string, refMap map[string]string, skipFields map[string]bool) any {
	switch t := o.(type) {
	case string:
		if skipFields[field] {
			return t
		}
		return rewriteTaskOutputsRefsInString(t, refMap)
	case []any:
		for i, e := range t {
			t[i] = rewriteDeepUnderField(e, field, refMap, skipFields)
		}
		return t
	case []map[string]any:
		for _, m := range t {
			rewriteDeepUnderField(m, field, refMap, skipFields)
		}
		return t
	case map[string]any:
		for k, e := range t {
			t[k] = rewriteDeepUnderField(e, k, refMap, skipFields)
		}
		var moves []string
		for k := range t {
			if rewriteTaskOutputsRefsInString(k, refMap) != k {
				moves = append(moves, k)
			}
		}
		sort.Strings(moves)
		for _, k := range moves {
			v := t[k]
			delete(t, k)
			nk := rewriteTaskOutputsRefsInString(k, refMap)
			if _, taken := t[nk]; !taken {
				t[nk] = v
			}
		}
		return t
	}
	return o
}

// deepTaskOutputsRefs returns, in first-seen order, every distinct <head> of a
// `task_outputs.<head>` reference reachable from body -- values and keys,
// skipping the same prose fields rewriteTaskOutputsRefsDeep skips. Heads come
// back as written; topologicalTaskOrder ignores one that names no task, so the
// scan can afford to be generous. Maps are walked in sorted key order so the
// result is deterministic.
func deepTaskOutputsRefs(body any, skipFields map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		for _, h := range taskOutputsHeads(s) {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	var walk func(o any, field string)
	walk = func(o any, field string) {
		switch t := o.(type) {
		case string:
			if !skipFields[field] {
				add(t)
			}
		case []any:
			for _, e := range t {
				walk(e, field)
			}
		case []map[string]any:
			for _, m := range t {
				walk(m, field)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				add(k)
				walk(t[k], k)
			}
		}
	}
	walk(body, "")
	return out
}

// taskOutputsHeads extracts the <head> of every `task_outputs.<head>` in s,
// where a head is a maximal run of ref-name bytes (isRefNameByte). The dot in
// the prefix keeps `task_outputs_by_type.` from matching.
func taskOutputsHeads(s string) []string {
	const prefix = "task_outputs."
	var heads []string
	for {
		i := strings.Index(s, prefix)
		if i < 0 {
			return heads
		}
		s = s[i+len(prefix):]
		j := 0
		for j < len(s) && isRefNameByte(s[j]) {
			j++
		}
		if j > 0 {
			heads = append(heads, s[:j])
		}
		s = s[j:]
	}
}

// rewriteCustomVariableRefs rewrites every spec-local ref inside ONE compute
// variable definition, in place: every string value at any depth and every map
// key. `dependencyTypes` is keyed BY the dependency string, so the ref lives in
// a map KEY there; a stale key makes the runtime's dependency coercion miss and
// the builder's reference scan report a phantom dangling reference. An entry
// already under the destination key wins: it names a dependency the author
// declared directly.
func rewriteCustomVariableRefs(v map[string]any, refMap map[string]string) {
	if v == nil {
		return
	}
	// Every string VALUE at any depth and every map KEY; a variable definition
	// has no free-text field the runtime ignores.
	rewriteTaskOutputsRefsDeep(v, refMap, nil)
}

// rewriteRefsInTemplate rewrites every {{...}} placeholder in s whose inner
// expression looks like a spec-local ref reference. The rewrite mirrors
// rewriteRefsInMappings:
//   - {{task_outputs.<ref>.<deep>}} -> {{task_outputs.<server-alias>.<deep>}}
//   - {{<ref>.<rest>}} (bare)       -> {{<server-alias>.<rest>}} (head substitution only)
//   - {{<reserved-scope>...}}        -> unchanged (inputs/custom/system/...)
//   - {{<server-alias>...}}          -> unchanged (BC resolver accepts bare form)
//
// Returns an error if a placeholder's head is neither a reserved scope nor a
// known spec ref nor a server alias -- a typo or stale reference would
// otherwise silently fail at runtime when the template engine resolves
// against an empty context.
//
// This is the template-string analog of rewriteRefsInMappings. Both share
// mappingDependencyRef + topologicalTaskOrder for ordering, so a task whose
// http url uses {{<downstream-task>.<field>}} gets ordered correctly, the
// rewrite runs after refMap has the downstream alias, and the persisted
// task body has the (now bare-or-canonical) form.
//
// Bare-form output is supported by BC's variable resolver (the resolver
// expands `{{<alias>.<field>}}` to the implicit
// `{{task_outputs.<alias>.<field>}}` at execute time), so we no longer
// re-wrap bare heads with the task_outputs. prefix.
func rewriteRefsInTemplate(s string, refMap map[string]string, localMappings map[string]any) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	var firstErr error
	out := templatePlaceholderRegex.ReplaceAllStringFunc(s, func(match string) string {
		if firstErr != nil {
			return match
		}
		m := templatePlaceholderRegex.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		inner := strings.TrimSpace(m[1])
		// Only rewrite path-like inner expressions; leave plain literals alone.
		dot := strings.Index(inner, ".")
		if dot <= 0 {
			// Bare single-token placeholder, e.g. {{borrower_id}}. By itself
			// the runtime template engine has no scope to resolve it against,
			// so it renders literally and corrupts the JSON. If the task's
			// inputMappings maps this token to a resolvable expression (e.g.
			// "inputs.borrower_id" or "task_outputs.fetch.tax_id"), rewrite to
			// that long form. When the token isn't a known mapping, leave it
			// untouched -- we only rewrite when we can confidently resolve it.
			if resolved, ok := localMappings[inner].(string); ok && resolved != "" {
				return "{{" + resolved + "}}"
			}
			return match
		}
		// task_outputs.<ref>.<deep>
		if strings.HasPrefix(inner, "task_outputs.") {
			rest := inner[len("task_outputs."):]
			innerDot := strings.Index(rest, ".")
			if innerDot <= 0 {
				return match
			}
			ref := rest[:innerDot]
			if alias, found := refMap[ref]; found {
				return "{{task_outputs." + alias + rest[innerDot:] + "}}"
			}
			if !isServerAlias(ref) {
				firstErr = fmt.Errorf(
					"template references task_outputs.%s.* but %q is not a known spec ref. Known refs: %s. (Reserved scopes: %s.)",
					ref, ref, sortedRefMapKeys(refMap), reservedScopesList())
				return match
			}
			return match
		}
		// Reserved scope at head -> leave as-is.
		head := inner[:dot]
		if reservedMappingScopes[head] {
			return match
		}
		// Bare <ref>.<rest> -> substitute head, keep bare form. The BC
		// runtime resolver expands `<alias>.<field>` to the implicit
		// `task_outputs.<alias>.<field>`.
		if alias, found := refMap[head]; found {
			return "{{" + alias + inner[dot:] + "}}"
		}
		if isServerAlias(head) {
			// Already a server alias in bare form -- BC handles it directly.
			return match
		}
		// Unknown head -- error so the user sees the typo before runtime
		// silently substitutes nothing.
		firstErr = fmt.Errorf(
			"template uses {{%s.<...>}} but %q is neither a reserved namespace nor a known spec ref nor a server alias. Known refs: %s. (Reserved scopes: %s.)",
			inner, head, sortedRefMapKeys(refMap), reservedScopesList())
		return match
	})
	if firstErr != nil {
		return s, firstErr
	}
	return out, nil
}

// rewriteRefsInTaskTemplates applies the template rewrite to every string
// field on a task body that the runtime treats as a {{...}}-substituting
// template. Today that's:
//   - http / webhook: url, body, headers
//   - end: endConfig.outputJson, endConfig.standardOutput.*,
//     endConfig.pdfConfig.sourcesConfig[].taskAlias
//   - exception: errorMessage
//   - conditional: variable-typed condition values
//   - child-workflow: inputExpression
//   - data-store-write: dataStoreWriteConfig.columnMappings[].valueTemplate,
//     dataStoreWriteConfig.batchSource
//   - data-store-query: dataStoreQueryConfig.sql,
//     dataStoreQueryConfig.sqlParameters[*],
//     dataStoreQueryConfig.filters[].valueTemplate
//
// The function mutates `task` in place and returns an error from the first
// failed rewrite, with the field name in the error context.
//
// New task types that introduce template fields should be added here AND in
// templateDependencyRefs -- otherwise topologicalTaskOrder won't see the
// inputMappings-style dep and a forward reference inside a template will
// fail at rewrite time instead of being ordered correctly.
//
// This switch is an ALLOWLIST for the shapes only a typed case can handle:
// substituting a bare `<ref>.<field>` head, expanding a bare {{token}} through
// inputMappings, and rejecting an unknown head as a typo. The unambiguous long
// form (`task_outputs.<ref>`) is NOT its job any more: rewriteTaskRefs runs
// rewriteTaskOutputsRefsDeep over the whole body after this switch, and
// topologicalTaskOrder scans the same surface, so a new field that carries the
// long form is ordered and rewritten without an entry here. A field that uses
// the BARE form still needs a case here and in templateDependencyRefs.
func rewriteRefsInTaskTemplates(task map[string]any, refMap map[string]string) error {
	taskType, _ := task["type"].(string)
	// The task's inputMappings map short tokens (e.g. "borrower_id") to
	// resolvable expressions (e.g. "inputs.borrower_id"). Bare single-token
	// placeholders in a template are rewritten to their mapped long form when
	// a mapping exists. By the time this runs the inputMappings values are
	// already in resolved/server-alias form (rewriteRefsInMappings ran first).
	localMappings, _ := task["inputMappings"].(map[string]any)
	rewriteField := func(fieldPath string, value string) (string, error) {
		out, err := rewriteRefsInTemplate(value, refMap, localMappings)
		if err != nil {
			return "", fmt.Errorf("%s: %w", fieldPath, err)
		}
		return out, nil
	}
	// rewriteTemplateOrPath handles a field the runtime accepts in EITHER form:
	// a {{...}} template (resolved by graph_workflow's generic
	// _resolve_dict_variables walk) or a BARE `task_outputs.<alias>.<path>`
	// (resolved by the activity itself via resolve_context_field, which requires
	// that literal prefix and does NOT accept the bare `<alias>.<field>`
	// shortcut). rewriteRefsInTemplate early-returns on a string with no "{{",
	// so the bare form needs the substring rewriter (data-store batchSource
	// and sqlParameters are commonly authored bare).
	rewriteTemplateOrPath := func(fieldPath string, value string) (string, error) {
		if strings.Contains(value, "{{") {
			return rewriteField(fieldPath, value)
		}
		return rewriteTaskOutputsRefsInString(value, refMap), nil
	}
	switch taskType {
	case "http", "webhook":
		for _, field := range []string{"url", "body", "headers"} {
			if s, ok := task[field].(string); ok && s != "" {
				out, err := rewriteField(field, s)
				if err != nil {
					return err
				}
				task[field] = out
			}
		}
	case "end":
		endCfg, _ := task["endConfig"].(map[string]any)
		if endCfg != nil {
			if s, ok := endCfg["outputJson"].(string); ok && s != "" {
				out, err := rewriteField("endConfig.outputJson", s)
				if err != nil {
					return err
				}
				endCfg["outputJson"] = out
			}
			// pdfConfig.sourcesConfig[].taskAlias carries a spec-local ref to
			// the data-producing upstream task whose output gets rendered as
			// a PDF section (scorecard, rule-tree, altdata-enrichment, etc.).
			// The runtime end_activity looks the alias up directly against
			// the workflow's task list -- so a spec-local ref like "score"
			// that survives compose persists on the server, the renderer
			// can't find a task with that alias, and the section falls
			// through to an empty render or hits the auto-resolver fallback.
			// Symptom matches the la-fabril / kyc-pf-mx spike: apply needs a
			// manual post-publish fix-up (fetch end task, rewrite section
			// aliases, bump task version, re-publish) for every PDF report
			// the spec defines section entries for.
			if pdfCfg, _ := endCfg["pdfConfig"].(map[string]any); pdfCfg != nil {
				if sources, ok := pdfCfg["sourcesConfig"].([]any); ok {
					for idx, src := range sources {
						sm, _ := src.(map[string]any)
						if sm == nil {
							continue
						}
						alias, _ := sm["taskAlias"].(string)
						if alias == "" {
							continue
						}
						if server, found := refMap[alias]; found {
							sm["taskAlias"] = server
							sources[idx] = sm
						}
					}
					pdfCfg["sourcesConfig"] = sources
				}
			}
			// standardOutput carries {{task_outputs.<ref>...}} templates on each
			// section (data/scorecard/metrics/rules/alerts/decision), the score
			// sub-object's fields, and each author-added `fields` entry. Like
			// outputJson these are runtime templates whose spec-local refs must be
			// rewritten to server aliases -- and validated (unknown ref -> error).
			// Without this walk a section ships with a stale spec ref and the
			// runtime resolves it to an empty section (the safety net below only
			// catches BARE refs, not {{...}}-wrapped ones). An explicit null is
			// how the server stores "no standard output" and how an export
			// emits it, so it reads as absent, not as the wrong type.
			if raw, present := endCfg["standardOutput"]; present && raw != nil {
				stdOut, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("endConfig.standardOutput must be an object, got %T", raw)
				}
				if err := validateStandardOutputShape(stdOut); err != nil {
					return err
				}
				for _, key := range []string{"data", "scorecard", "metrics", "rules", "alerts", "decision"} {
					if s, ok := stdOut[key].(string); ok && s != "" {
						out, err := rewriteField("endConfig.standardOutput."+key, s)
						if err != nil {
							return err
						}
						stdOut[key] = out
					}
				}
				if score, _ := stdOut["score"].(map[string]any); score != nil {
					for _, key := range []string{"value", "key", "label", "maxValue"} {
						if s, ok := score[key].(string); ok && s != "" {
							out, err := rewriteField("endConfig.standardOutput.score."+key, s)
							if err != nil {
								return err
							}
							score[key] = out
						}
					}
				}
				if fields, _ := stdOut["fields"].(map[string]any); fields != nil {
					for k, v := range fields {
						if s, ok := v.(string); ok && s != "" {
							out, err := rewriteField("endConfig.standardOutput.fields."+k, s)
							if err != nil {
								return err
							}
							fields[k] = out
						}
					}
				}
			}
		}
	case "exception":
		// errorMessage flows through graph_workflow's _resolve_dict_variables
		// at execute time, so {{task_outputs.<alias>.<deep>}} placeholders
		// resolve from ScopedWorkflowContext just like an http body would.
		// Without this rewrite, a spec-local ref like {{score.total_score}}
		// survives compose unchanged, the runtime resolver rejects `score`
		// (not a known scope), and the persisted exception ships with the
		// raw template literal instead of the resolved value.
		if s, ok := task["errorMessage"].(string); ok && s != "" {
			out, err := rewriteField("errorMessage", s)
			if err != nil {
				return err
			}
			task["errorMessage"] = out
		}
	case "conditional":
		// Leaf ConditionItems with valueType=="variable" carry a deep ref in
		// `value` (e.g. task_outputs.<spec-ref>.<deep>). Without this rewrite
		// the spec-local ref survives compose, the conditional activity tries
		// to resolve <spec-ref> as a scope, and the comparator falls through
		// to its zero-value default -- silently mis-routing every branch.
		branches, _ := task["branches"].([]any)
		for _, b := range branches {
			bm, _ := b.(map[string]any)
			if bm == nil {
				continue
			}
			if err := rewriteRefsInConditionGroup(bm["conditions"], refMap); err != nil {
				return err
			}
		}
	case "child-workflow":
		// inputExpression carries the dispatch expression for batch mode
		// (BC #1247) -- the runtime resolves it against the parent's
		// task_outputs scope at execute time. A spec-local ref like
		// `task_outputs.fetch.cuit_list` survives compose unchanged
		// otherwise, the runtime can't find `fetch` in the scope, and the
		// expression collapses to a single execution with the full parent
		// context. Walk the same rewriter we use on other deep-ref fields.
		if s, ok := task["inputExpression"].(string); ok && s != "" {
			task["inputExpression"] = rewriteTaskOutputsRefsInString(s, refMap)
		}
	case "data-store-write":
		// FIVE ref-bearing template surfaces live under the two data-store
		// configs and NONE of them was walked, so a spec-local ref shipped
		// verbatim to the database. Three of the five fail SILENTLY, which is
		// why this is data corruption rather than a broken run:
		//
		//   columnMappings[].valueTemplate  SILENT -- graph_workflow's generic
		//     _resolve_dict_variables walk leaves an unresolvable {{...}}
		//     as its LITERAL (variable_resolver.resolve_variables, json_mode
		//     off), and _execute_single_write binds value_template straight
		//     into named_args. The row is written, the node reports success,
		//     and the cell holds the template text.
		//   batchSource  LOUD -- _execute_batch_write resolve_field()s it and
		//     raises "Batch source must resolve to a list, got str".
		//
		// In BATCH mode valueTemplate is a KEY into each item dict rather than
		// a template (`item.get(key, key)`), which is why the substring
		// rewriter is the right tool: it only ever touches
		// "task_outputs.<ref>." and leaves a plain item key alone.
		if cfg, _ := task["dataStoreWriteConfig"].(map[string]any); cfg != nil {
			if cols, ok := cfg["columnMappings"].([]any); ok {
				for idx, cm := range cols {
					m, _ := cm.(map[string]any)
					if m == nil {
						continue
					}
					s, _ := m["valueTemplate"].(string)
					if s == "" {
						continue
					}
					out, err := rewriteTemplateOrPath(
						fmt.Sprintf("dataStoreWriteConfig.columnMappings[%d].valueTemplate", idx), s)
					if err != nil {
						return err
					}
					m["valueTemplate"] = out
					cols[idx] = m
				}
				cfg["columnMappings"] = cols
			}
			if s, ok := cfg["batchSource"].(string); ok && s != "" {
				out, err := rewriteTemplateOrPath("dataStoreWriteConfig.batchSource", s)
				if err != nil {
					return err
				}
				cfg["batchSource"] = out
			}
			task["dataStoreWriteConfig"] = cfg
		}
	case "data-store-query":
		// The other three of the five (see data-store-write above):
		//
		//   sql  LOUD -- _execute_sql_query passes it verbatim, so the literal
		//     {{...}} is interpolated into the SQL text and Turso rejects the
		//     statement with a syntax error.
		//   sqlParameters[*]  SILENT -- bound as a named arg, so the query
		//     runs with the template text as the parameter value.
		//   filters[].valueTemplate  SILENT -- _execute_simple_query binds it
		//     into the WHERE clause, the filter matches nothing, and the node
		//     succeeds with an empty result set.
		if cfg, _ := task["dataStoreQueryConfig"].(map[string]any); cfg != nil {
			if s, ok := cfg["sql"].(string); ok && s != "" {
				out, err := rewriteTemplateOrPath("dataStoreQueryConfig.sql", s)
				if err != nil {
					return err
				}
				cfg["sql"] = out
			}
			if params, ok := cfg["sqlParameters"].(map[string]any); ok {
				for k, v := range params {
					s, _ := v.(string)
					if s == "" {
						continue
					}
					out, err := rewriteTemplateOrPath(
						fmt.Sprintf("dataStoreQueryConfig.sqlParameters[%q]", k), s)
					if err != nil {
						return err
					}
					params[k] = out
				}
				cfg["sqlParameters"] = params
			}
			if filters, ok := cfg["filters"].([]any); ok {
				for idx, f := range filters {
					m, _ := f.(map[string]any)
					if m == nil {
						continue
					}
					s, _ := m["valueTemplate"].(string)
					if s == "" {
						continue
					}
					out, err := rewriteTemplateOrPath(
						fmt.Sprintf("dataStoreQueryConfig.filters[%d].valueTemplate", idx), s)
					if err != nil {
						return err
					}
					m["valueTemplate"] = out
					filters[idx] = m
				}
				cfg["filters"] = filters
			}
			task["dataStoreQueryConfig"] = cfg
		}
	}
	return nil
}

// residualSpecRefExcludedFields is the set of task-body field names where a
// string value is allowed to coincidentally equal a spec-local ref without
// it being a missed rewrite. These are user-facing text fields (labels,
// descriptions) and authored literal slots (mapping-table outputs, condition
// literal values, input-variable defaults) -- a word like "score" landing
// there is a normal user choice, not a compose bug.
//
// The list is intentionally conservative: it shouldn't grow much. The point
// of validateNoResidualSpecRefs is to surface unknown ref-bearing paths;
// excluding too much defeats that. New entries here should be cross-checked
// against whether the field is also walked by rewriteRefsInTaskTemplates.
var residualSpecRefExcludedFields = map[string]bool{
	"label":        true,
	"description":  true,
	"title":        true,
	"subtitle":     true,
	"name":         true,
	"code":         true,
	"comment":      true,
	"errorMessage": true, // template, rewritten elsewhere; value may match a ref legitimately
	"outputJson":   true, // template, rewritten elsewhere
	"filePrefix":   true, // PDF metadata
	"brandLogo":    true, // PDF metadata
	"value":        true, // condition literals, mapping entry literals
	"outputValue":  true, // mapping table literals
	"defaultValue": true, // mapping table fallback literals
	"default":      true, // input-variable defaults
	"placeholder":  true,
	"helpText":     true,
	"hint":         true,
	"tooltip":      true,
	// mappingTableConfig.entries[].inputVariable is dual-mode at runtime, and
	// only ONE of the two modes is a node reference. BC's resolve_context_field
	// branches on the literal `task_outputs.` prefix: with it, the second dotted
	// segment is a task alias; WITHOUT it the whole string is a flat key looked
	// up in {workflow inputVariables + customVariables + this task's
	// resolvedInputs}. rewriteTaskRefs already rewrites the dotted form via
	// rewriteTaskOutputsRefsInString, and a dotted string can never be an exact
	// match for a bare ref anyway -- so this validator only ever fires on the
	// BARE form, which is provably a variable name and never a node ref.
	// Flagging it is a false positive by construction, and it aborted a real
	// migration mid-POST. checkRefVariableCollisions is what keeps the bare form
	// honest, by refusing a spec where a ref and a variable share a name.
	"inputVariable": true,
	// categoryConfig.categoryKey names one of the TENANT's categories and
	// categoryConfig.valueFields names the keys of the object being assigned.
	// Both are user-authored literals resolved server-side at run time, never
	// node references -- the category node deliberately takes no id of any kind.
	// A spec is perfectly entitled to have a node ref that reads the same as a
	// category key ("segmentation" is a natural name for both), and flagging
	// that is a false positive by construction.
	"categoryKey": true,
	"valueFields": true,
}

// validateNoResidualSpecRefs walks a composed task body and returns an error
// if any string value at a non-excluded path exactly equals a key in refMap
// whose server-assigned alias is different. A surviving spec-local ref means
// some ref-bearing field on this task type isn't covered by
// rewriteRefsInTaskTemplates -- the existing rewriter is a hardcoded
// per-type switch, so adding a new ref-bearing field (e.g.
// endConfig.pdfConfig.sourcesConfig[].taskAlias, which silently shipped as
// a bug for some time) requires touching the switch. This validator is the
// safety net: when the next ref-bearing field is added to the API but the
// rewriter isn't updated, compose fails loudly with the offending JSON path
// instead of letting the bad task body ship to the server.
//
// TWO shapes are checked, and the second is the one that matters:
//
//  1. The BARE ref: the whole string equals a refMap key (e.g. a
//     `taskAlias: "score"`). Exact-match keeps this high-signal -- "score"
//     inside a label "Final score breakdown" doesn't match the key "score".
//  2. The EMBEDDED ref: the string CONTAINS "task_outputs.<ref>." anywhere
//     (e.g. `{{task_outputs.score.total}}`, or a bare
//     `task_outputs.score.rows` in a field the runtime resolves with
//     resolve_context_field). This is the only form authors actually write,
//     and for a long time the validator could not see it: shape 1 alone
//     passed a body whose data-store `valueTemplate` still carried
//     `{{task_outputs.q1.rows[0].uno}}`, which the runtime leaves as a
//     LITERAL and the node then writes to Turso as text, reporting success.
//     Checking the substring is what converts every FUTURE missed template
//     surface from silent-in-production into loud-at-apply.
//
// A string that only contains "task_outputs.<server-alias>." is not a match:
// the refMap key is the spec ref, the trailing dot pins the boundary, and
// identity entries (refMap[ref] == ref, which is what the assembly phase
// passes) are skipped -- so an already-rewritten field never trips this.
//
// When a legitimate user literal collides with a ref name, the agent can
// either rename the ref or add the field to residualSpecRefExcludedFields --
// the error message names the field, so the remediation is obvious.
func validateNoResidualSpecRefs(body map[string]any, refMap map[string]string, ctx string) error {
	if len(refMap) == 0 {
		return nil
	}
	// Sorted so a body carrying several residual refs always names the same
	// one: map iteration order would otherwise make the error non-deterministic
	// and the failure look flaky across runs.
	embedded := make([]string, 0, len(refMap))
	for ref, server := range refMap {
		if ref == server {
			continue
		}
		embedded = append(embedded, ref)
	}
	sort.Strings(embedded)
	var walk func(node any, path string) error
	walk = func(node any, path string) error {
		switch v := node.(type) {
		case map[string]any:
			for k, sub := range v {
				childPath := k
				if path != "" {
					childPath = path + "." + k
				}
				if err := walk(sub, childPath); err != nil {
					return err
				}
			}
		case []any:
			for i, sub := range v {
				if err := walk(sub, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		case string:
			if v == "" {
				return nil
			}
			// Last segment of the dotted path is the field name.
			last := path
			if idx := strings.LastIndexByte(path, '.'); idx >= 0 {
				last = path[idx+1:]
			}
			// Strip a trailing array index "[N]" so the field-name check
			// matches whether the value is at parent.field or
			// parent.field[3].
			if bracket := strings.IndexByte(last, '['); bracket >= 0 {
				last = last[:bracket]
			}
			if residualSpecRefExcludedFields[last] {
				return nil
			}
			if server, found := refMap[v]; found && server != v {
				return fmt.Errorf(
					"%s: residual spec-local ref %q at path %q (expected server-assigned alias %q). "+
						"This means rewriteRefsInTaskTemplates doesn't yet walk this field for the task type. "+
						"Add the field to the rewriter (or add %q to residualSpecRefExcludedFields if the "+
						"literal is genuinely user-authored text).",
					ctx, v, path, server, last)
			}
			// Embedded form -- see the shape-2 note on this function. A
			// template surface the rewriter doesn't walk keeps the spec ref
			// inside the string, which no exact match can ever see.
			for _, ref := range embedded {
				if !containsTaskOutputsRef(v, ref) {
					continue
				}
				return fmt.Errorf(
					"%s: residual spec-local ref %q embedded in a template at path %q: %q still contains "+
						"%q (expected server-assigned alias %q). rewriteTaskOutputsRefsDeep walks every "+
						"non-prose string in the body, so this is a bug in that pass -- at runtime the "+
						"reference resolves to nothing and the literal is persisted/used as-is. Report it "+
						"(or add %q to residualSpecRefExcludedFields if the text is genuinely "+
						"user-authored prose).",
					ctx, ref, path, truncateForError(v), "task_outputs."+ref+".", refMap[ref], last)
			}
		}
		return nil
	}
	return walk(body, "")
}

// standardOutputSectionKeys are the whole-section bindings on
// endConfig.standardOutput -- each holds a single {{task_outputs.<ref>.<field>}}
// template string bound to a variable that produces that section's shape.
var standardOutputSectionKeys = []string{"data", "scorecard", "metrics", "rules", "alerts", "decision"}

// standardOutputAllowedKeys is the frozen v1 white-box shape. It is an allowlist
// (not passthrough) on purpose: the schema is a stable legacy contract, so an
// unknown top-level key is almost always a typo (scoreCard, metric, rule) worth
// catching locally instead of shipping a silently-ignored field.
var standardOutputAllowedKeys = map[string]bool{
	"enabled": true, "fields": true, "score": true,
	"data": true, "scorecard": true, "metrics": true,
	"rules": true, "alerts": true, "decision": true,
}

var standardOutputScoreAllowedKeys = map[string]bool{
	"key": true, "label": true, "value": true, "maxValue": true,
}

// validateStandardOutputShape checks the structural shape of
// endConfig.standardOutput before it ships, surfacing a clear local error
// instead of a backend 400. Variable refs are NOT checked here --
// rewriteRefsInTaskTemplates rewrites and validates those.
func validateStandardOutputShape(stdOut map[string]any) error {
	for k := range stdOut {
		if !standardOutputAllowedKeys[k] {
			return fmt.Errorf(
				"endConfig.standardOutput has unknown field %q (allowed: enabled, fields, score, "+
					"data, scorecard, metrics, rules, alerts, decision)", k)
		}
	}
	if v, ok := stdOut["enabled"]; ok {
		if _, isBool := v.(bool); !isBool {
			return fmt.Errorf("endConfig.standardOutput.enabled must be a boolean, got %T", v)
		}
	}
	// Whole-section bindings must be a single template string (or absent). A list
	// or object here is the classic "I pasted the data instead of a variable"
	// mistake -- bind the section to ONE variable, e.g. "{{task_outputs.sc.rows}}".
	for _, key := range standardOutputSectionKeys {
		if v, ok := stdOut[key]; ok && v != nil {
			if _, isStr := v.(string); !isStr {
				return fmt.Errorf(
					"endConfig.standardOutput.%s must be a variable template string like "+
						"\"{{task_outputs.<ref>.<field>}}\", got %T", key, v)
			}
		}
	}
	if v, ok := stdOut["score"]; ok && v != nil {
		score, isMap := v.(map[string]any)
		if !isMap {
			return fmt.Errorf(
				"endConfig.standardOutput.score must be an object {key,label,value,maxValue}, got %T", v)
		}
		for sk := range score {
			if !standardOutputScoreAllowedKeys[sk] {
				return fmt.Errorf(
					"endConfig.standardOutput.score has unknown field %q (allowed: key, label, value, maxValue)", sk)
			}
		}
	}
	if v, ok := stdOut["fields"]; ok && v != nil {
		if _, isMap := v.(map[string]any); !isMap {
			return fmt.Errorf(
				"endConfig.standardOutput.fields must be an object of key -> value, got %T", v)
		}
	}
	return nil
}

// nestedInputMappingConfigKeys lists the task-config objects that carry their
// OWN inputMappings map, which the runtime resolves instead of (scorecard,
// rule-tree) or in addition to (category) the top-level one. Both the rewriter
// and the dependency scanner walk this list; keeping it in one place is what
// stops a new nested-mapping node type from being wired into one and missed by
// the other -- the failure there is silent (refs resolve to None at runtime).
var nestedInputMappingConfigKeys = []string{"scorecardConfig", "ruleTreeConfig", "categoryConfig"}

// rewriteTaskRefs applies every ref->alias rewrite a task-node body needs, in
// the order the runtime resolver expects: top-level inputMappings, the nested
// scorecardConfig/ruleTreeConfig inputMappings (resolved from their own map),
// mappingTableConfig entry inputVariables, then {{...}} templates + conditional
// condition values, and finally the residual-ref safety net.
//
// It runs once, during assembly, with an identity ref->placeholder map: that
// validates the graph (an unknown head is a typo) and leaves every reference in
// the canonical `task_outputs.<ref>` form the server substitutes for the real
// alias. It is designed to be a no-op on identifiers it already resolved, so
// running it again with a real alias map is safe.
func rewriteTaskRefs(task map[string]any, refMap map[string]string, ctx string) error {
	if mappings, ok := task["inputMappings"].(map[string]any); ok {
		rewritten, rerr := rewriteRefsInMappings(mappings, refMap)
		if rerr != nil {
			return fmt.Errorf("%s: %w", ctx, rerr)
		}
		task["inputMappings"] = rewritten
	}
	// scorecard and rule-tree activities resolve their per-rule field
	// references from a NESTED inputMappings map (scorecardConfig /
	// ruleTreeConfig) -- see graph_workflow's per-task-type branches in
	// _resolve_task_variables. Rewrite those too, or every rule field resolves
	// to None at runtime (scorecard total=0, rule-tree falls to default).
	// categoryConfig joins them because normalize mirrors the top-level map
	// into it, so an unrewritten ref there assigns an empty category value.
	for _, nested := range nestedInputMappingConfigKeys {
		cfg, _ := task[nested].(map[string]any)
		if cfg == nil {
			continue
		}
		nestedMappings, ok := cfg["inputMappings"].(map[string]any)
		if !ok || len(nestedMappings) == 0 {
			continue
		}
		rewritten, rerr := rewriteRefsInMappings(nestedMappings, refMap)
		if rerr != nil {
			return fmt.Errorf("%s %s.inputMappings: %w", ctx, nested, rerr)
		}
		cfg["inputMappings"] = rewritten
		task[nested] = cfg
	}
	// Mapping-table tasks store their per-entry input wiring as
	// mappingTableConfig.entries[].inputVariable (NOT inputMappings). The
	// runtime resolves that string verbatim against _task_outputs (keyed by
	// server alias), so a spec-local ref left here doesn't resolve.
	if cfg, _ := task["mappingTableConfig"].(map[string]any); cfg != nil {
		if entries, ok := cfg["entries"].([]any); ok {
			for idx, e := range entries {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				if s, ok := em["inputVariable"].(string); ok && s != "" {
					em["inputVariable"] = rewriteTaskOutputsRefsInString(s, refMap)
				}
				entries[idx] = em
			}
			cfg["entries"] = entries
			task["mappingTableConfig"] = cfg
		}
	}
	// {{...}} template placeholders (http url/body/headers, end outputJson) and
	// conditional condition values.
	if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
		return fmt.Errorf("%s: %w", ctx, err)
	}
	// Generic pass: every remaining `task_outputs.<ref>` reference anywhere in
	// the body, values and keys, minus the prose fields the guard below also
	// skips. The typed rewriters above own the shapes only they can handle: a
	// bare `<ref>.<field>` head, a bare {{token}} expanded through
	// inputMappings, an unknown head rejected as a typo. The long form is
	// unambiguous, so it is rewritten wherever it appears, and
	// topologicalTaskOrder scans the same surface (deepTaskOutputsRefs) so the
	// alias is already minted by the time this runs.
	rewriteTaskOutputsRefsDeep(task, refMap, residualSpecRefExcludedFields)
	// Safety net: after all rewriters run, fail loud on any string that still
	// exactly equals a ref/placeholder the map would rename (a bare identifier
	// in a field no typed rewriter walks) or still embeds one (which, after the
	// generic pass, can only mean a bug in that pass). Run with the map that
	// excludes THIS task's own identifier (the caller has not added it yet), so
	// a task type/ref collision (e.g. an "end" node) is never mistaken for a
	// residue.
	if err := validateNoResidualSpecRefs(task, refMap, ctx); err != nil {
		return err
	}
	return nil
}

// rewriteRefsInConditionGroup walks a ConditionGroup tree (operator + items
// where each item is either a leaf {field, operator, value, valueType} or
// another nested group) and rewrites the `value` of every variable-typed
// leaf via the spec-ref -> server-alias map. Literal-typed values are left
// alone.
func rewriteRefsInConditionGroup(node any, refMap map[string]string) error {
	group, _ := node.(map[string]any)
	if group == nil {
		return nil
	}
	items, _ := group["items"].([]any)
	for _, it := range items {
		im, _ := it.(map[string]any)
		if im == nil {
			continue
		}
		if _, isGroup := im["items"]; isGroup {
			if err := rewriteRefsInConditionGroup(im, refMap); err != nil {
				return err
			}
			continue
		}
		vt, _ := im["valueType"].(string)
		if vt != "variable" {
			continue
		}
		s, _ := im["value"].(string)
		if s == "" {
			continue
		}
		im["value"] = rewriteTaskOutputsRefsInString(s, refMap)
	}
	return nil
}

// sortedRefMapKeys returns refMap's keys sorted -- used in error messages so
// the agent can scan for a near-typo without re-running 'workflows-v2 list'.
func sortedRefMapKeys(refMap map[string]string) string {
	keys := make([]string, 0, len(refMap))
	for k := range refMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// topologicalTaskOrder returns the indices of spec.Tasks sorted so that each
// task's dependencies (incoming edges + tasks referenced in its inputMappings)
// come BEFORE it. Without this, a task whose inputMappings reads from a
// downstream task in the spec produces a bare ref that survives
// rewriteRefsInMappings (refMap doesn't yet have the downstream alias) and
// then fails at runtime.
//
// Stability: independent tasks retain their original spec order.
//
// Returns an error when a cycle prevents a complete ordering. Cycles are
// also rejected by the runtime engine but surfacing here gives a faster,
// clearer message naming the unresolved tasks.
func topologicalTaskOrder(tasks []map[string]any, edges []map[string]any) ([]int, error) {
	n := len(tasks)
	if n == 0 {
		return nil, nil
	}
	refToIdx := map[string]int{}
	refs := make([]string, n)
	for i, t := range tasks {
		ref := localRef(t, fmt.Sprintf("t%d", i))
		refs[i] = ref
		refToIdx[ref] = i
	}

	// deps[i] = task indices that task i depends on.
	deps := make([]map[int]bool, n)
	for i := range deps {
		deps[i] = map[int]bool{}
	}
	addDep := func(consumer int, depRef string) {
		if depRef == "" {
			return
		}
		depIdx, ok := refToIdx[depRef]
		if !ok || depIdx == consumer {
			return
		}
		deps[consumer][depIdx] = true
	}

	// Edge dependencies: A -> B means A is a dep of B.
	for _, e := range edges {
		from, to := edgeEndpoints(e)
		toIdx, hasTo := refToIdx[to]
		if !hasTo {
			continue // edge endpoint isn't a task (likely an extraNode like start/end)
		}
		addDep(toIdx, from)
	}

	// inputMappings dependencies: a value pointing at task_outputs.X or X.Y
	// makes the consumer depend on X. We walk both the top-level map AND
	// the nested scorecardConfig.inputMappings / ruleTreeConfig.inputMappings
	// maps -- the scorecard/rule-tree activities resolve from the nested
	// form, so unrewritten refs there cause the runtime score=0 collapse
	// even when the top-level looks clean.
	collectMappingDeps := func(consumer int, m map[string]any) {
		for _, v := range m {
			s, _ := v.(string)
			addDep(consumer, mappingDependencyRef(s))
		}
	}
	for i, t := range tasks {
		mappings, _ := t["inputMappings"].(map[string]any)
		collectMappingDeps(i, mappings)
		for _, nested := range nestedInputMappingConfigKeys {
			if cfg, _ := t[nested].(map[string]any); cfg != nil {
				if nm, _ := cfg["inputMappings"].(map[string]any); nm != nil {
					collectMappingDeps(i, nm)
				}
			}
		}
		// mapping-table entries store inputVariable per-entry (not under
		// inputMappings). Mirror the topological-order coverage so a
		// mapping-table that depends on a compute-variables task gets
		// ordered after it.
		if cfg, _ := t["mappingTableConfig"].(map[string]any); cfg != nil {
			if entries, ok := cfg["entries"].([]any); ok {
				for _, e := range entries {
					if em, ok := e.(map[string]any); ok {
						if s, _ := em["inputVariable"].(string); s != "" {
							addDep(i, mappingDependencyRef(s))
						}
					}
				}
			}
		}
		// child-workflow's inputExpression is a bare path (e.g.
		// `task_outputs.fetch.cuit_list`) the runtime resolves at execute
		// time -- without a topo-dep entry the rewrite pass would fire
		// before refMap knew about <fetch>, leaving the spec-local ref
		// to leak into the persisted task.
		if s, _ := t["inputExpression"].(string); s != "" {
			addDep(i, mappingDependencyRef(s))
		}
		// Template-string dependencies (http url/body/headers, end
		// outputJson). A task whose http url uses {{<other-task>.field}}
		// must be ordered AFTER that task so rewriteRefsInTaskTemplates
		// resolves the ref to a server alias.
		for _, ref := range templateDependencyRefs(t) {
			addDep(i, ref)
		}
		// Long-form `task_outputs.<ref>` references anywhere else in the body,
		// i.e. a field no typed scanner names yet. rewriteTaskRefs rewrites the
		// same surface, so ordering and rewriting cannot disagree on what counts
		// as a reference: a forward reference in a brand-new field is ordered
		// here and resolved there.
		for _, ref := range deepTaskOutputsRefs(t, residualSpecRefExcludedFields) {
			addDep(i, ref)
		}
	}

	// Stable topological sort: scan original order each iteration, emitting
	// any task whose deps are all visited. O(n²) but n is typically <50.
	out := make([]int, 0, n)
	visited := make([]bool, n)
	for {
		progress := false
		for i := 0; i < n; i++ {
			if visited[i] {
				continue
			}
			ready := true
			for d := range deps[i] {
				if !visited[d] {
					ready = false
					break
				}
			}
			if ready {
				out = append(out, i)
				visited[i] = true
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	if len(out) != n {
		unresolved := []string{}
		for i := 0; i < n; i++ {
			if !visited[i] {
				unresolved = append(unresolved, refs[i])
			}
		}
		return nil, fmt.Errorf(
			"cyclic task dependency detected; could not topologically order: %s. "+
				"Each task in inputMappings can only reference upstream tasks (or those that don't depend on it).",
			strings.Join(unresolved, ", "))
	}
	return out, nil
}
