package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Leading segments of a mapping value that are NOT spec-local refs and must not be rewritten.
var reservedMappingScopes = map[string]bool{
	"inputs":               true,
	"custom":               true,
	"system":               true,
	"task_outputs":         true,
	"task_outputs_by_type": true,
	// entity.* is a backend pass-through scope; the CLI has no data-model catalog,
	// so like the other reserved scopes it only recognises the leading segment.
	"entity": true,
	// self.* is a node's OWN late-resolved output (End's pdf_url): BC leaves the
	// literal in place and resolves it in a second pass, so apply must not touch it.
	"self": true,
	// documents.* is the http task's own late-resolved input, likewise resolved in a
	// second pass inside http_activity -- the bytes do not exist at graph time.
	"documents": true,
}

// Rendered into error messages so the hint cannot drift from the map the validators consult.
func reservedScopesList() string {
	keys := make([]string, 0, len(reservedMappingScopes))
	for k := range reservedMappingScopes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

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

// Keep in step with rewriteRefsInTaskTemplates: a template field the rewriter
// walks but this one misses is ordered wrong, and its ref silently survives.
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
			// Bare task_outputs.<ref>.<path>, the only shape resolve_context_field accepts.
			// A ref naming no task is ignored downstream, so a plain SQL string costs nothing.
			add(mappingDependencyRef(s))
			continue
		}
		for _, m := range templatePlaceholderRegex.FindAllStringSubmatch(s, -1) {
			add(mappingDependencyRef(strings.TrimSpace(m[1])))
		}
	}
	return out
}

// Both forms are accepted by BC -- bare "<ref>.<field>" is the implicit
// task_outputs shortcut, expanded server-side -- so only the head is substituted.
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
			head := s[:dot]
			if !reservedMappingScopes[head] {
				if alias, found := refMap[head]; found {
					s = alias + s[dot:]
				} else if isServerAlias(head) {
					// A server-style alias is already final; BC's resolver matches it at runtime.
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

var templatePlaceholderRegex = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

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

// The ref must not be followed by a word char or a hyphen, so `tabla` cannot match
// inside `tablas`. RE2 has no lookahead, hence the manual peek at the next byte.
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
			// A longer ref starts here (tabla vs tablas): copy past it and keep scanning.
			b.WriteString(s[:end])
			s = s[end:]
			continue
		}
		b.WriteString(s[:i])
		b.WriteString("task_outputs." + alias)
		s = s[end:]
	}
}

// Read-only twin of replaceTaskOutputsRef and must agree with it: a boundary the
// detector does not recognise is a residual ref that ships silently.
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

// Refs are alphanumeric with internal dashes, so anything else terminates one.
func isRefNameByte(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// The one generic pass: the long form is unambiguous, so it is rewritten wherever
// it appears. skipFields names the prose keys validateNoResidualSpecRefs also skips.
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

// Generous on purpose: topologicalTaskOrder ignores a head that names no task.
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

// The dot in the prefix keeps `task_outputs_by_type.` from matching.
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

// dependencyTypes is keyed BY the dependency string, so the ref lives in a map KEY
// there; a stale key makes the runtime's dependency coercion miss.
func rewriteCustomVariableRefs(v map[string]any, refMap map[string]string) {
	if v == nil {
		return
	}
	// nil skipFields: a variable definition has no free-text field to protect.
	rewriteTaskOutputsRefsDeep(v, refMap, nil)
}

// The template-string analog of rewriteRefsInMappings. Bare-form output is fine:
// BC's resolver expands {{<alias>.<field>}} to the implicit task_outputs form.
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
		dot := strings.Index(inner, ".")
		if dot <= 0 {
			// A bare {{token}} has no scope to resolve against at runtime: it renders
			// literally and corrupts the JSON. Resolve it through inputMappings when we can.
			if resolved, ok := localMappings[inner].(string); ok && resolved != "" {
				return "{{" + resolved + "}}"
			}
			return match
		}
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
		head := inner[:dot]
		if reservedMappingScopes[head] {
			return match
		}
		if alias, found := refMap[head]; found {
			return "{{" + alias + inner[dot:] + "}}"
		}
		if isServerAlias(head) {
			return match
		}
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

// An ALLOWLIST for the shapes only a typed case can handle: a bare <ref>.<field>
// head, a bare {{token}} expanded through inputMappings, an unknown head as a typo.
func rewriteRefsInTaskTemplates(task map[string]any, refMap map[string]string) error {
	taskType, _ := task["type"].(string)
	// By the time this runs the inputMappings values are already in resolved form.
	localMappings, _ := task["inputMappings"].(map[string]any)
	rewriteField := func(fieldPath string, value string) (string, error) {
		out, err := rewriteRefsInTemplate(value, refMap, localMappings)
		if err != nil {
			return "", fmt.Errorf("%s: %w", fieldPath, err)
		}
		return out, nil
	}
	// A field the runtime accepts in EITHER form. rewriteRefsInTemplate early-returns
	// on a string with no "{{", so the bare path needs the substring rewriter.
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
			// taskAlias is looked up directly against the workflow's task list, so a spec-local
			// ref that survives here renders an empty PDF section.
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
			// The sections carry {{task_outputs.<ref>}} templates, and a stale ref resolves to
			// an empty section. An explicit null is how the server stores "no standard output".
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
		// errorMessage flows through graph_workflow's _resolve_dict_variables at execute time.
		if s, ok := task["errorMessage"].(string); ok && s != "" {
			out, err := rewriteField("errorMessage", s)
			if err != nil {
				return err
			}
			task["errorMessage"] = out
		}
	case "conditional":
		// A leaf carrying a deep ref in `value` mis-routes every branch when unrewritten:
		// the activity cannot resolve the scope and the comparator falls to its zero value.
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
		// inputExpression is resolved against the parent's task_outputs at dispatch time.
		if s, ok := task["inputExpression"].(string); ok && s != "" {
			task["inputExpression"] = rewriteTaskOutputsRefsInString(s, refMap)
		}
	case "data-store-write":
		// Three of these five surfaces fail SILENTLY: an unresolvable {{...}} is left as
		// its LITERAL and written to the database as text, with the node reporting success.
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

// Field names where a string may coincidentally equal a spec-local ref without
// being a missed rewrite. Conservative on purpose: excluding too much blinds the guard.
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
	// Dual-mode at runtime: without the literal `task_outputs.` prefix the whole string
	// is a flat variable name, never a node ref, so flagging it is a false positive.
	"inputVariable": true,
	// Tenant-authored literals resolved server-side; a node ref is entitled to read
	// the same ("segmentation" is a natural name for both).
	"categoryKey": true,
	"valueFields": true,
	// Deal-role literals resolved server-side, never node references. The role
	// vocabulary collides head-on with the ref one (ref and role_key both "customer").
	"role_key": true,
	"roleKey":  true,
}

// The same exclusion qualified by the field's PARENT. artifactConfig.artifactAlias
// is a tenant literal, but inputMappings.artifactAlias is a ref that must rewrite.
var residualSpecRefExcludedPaths = map[string]bool{
	"artifactConfig.artifactAlias":  true,
	"artifactConfig.artifact_alias": true,
	"artifactConfig.columns":        true,
}

func parentSegment(path string) string {
	idx := strings.LastIndexByte(path, '.')
	if idx < 0 {
		return ""
	}
	parent := path[:idx]
	if pidx := strings.LastIndexByte(parent, '.'); pidx >= 0 {
		parent = parent[pidx+1:]
	}
	if bracket := strings.IndexByte(parent, '['); bracket >= 0 {
		parent = parent[:bracket]
	}
	return parent
}

// The safety net: a surviving spec-local ref means some ref-bearing field on this
// task type is not walked yet, so fail loud here rather than silently at runtime.
func validateNoResidualSpecRefs(body map[string]any, refMap map[string]string, ctx string) error {
	if len(refMap) == 0 {
		return nil
	}
	// Sorted so a body carrying several residual refs always names the same one.
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
			last := path
			if idx := strings.LastIndexByte(path, '.'); idx >= 0 {
				last = path[idx+1:]
			}
			// Strip a trailing "[N]" so the check matches parent.field and parent.field[3] alike.
			if bracket := strings.IndexByte(last, '['); bracket >= 0 {
				last = last[:bracket]
			}
			if residualSpecRefExcludedFields[last] {
				return nil
			}
			if parent := parentSegment(path); parent != "" &&
				residualSpecRefExcludedPaths[parent+"."+last] {
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
			// Embedded form: a ref living inside a string, which no exact match can see.
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

var standardOutputSectionKeys = []string{"data", "scorecard", "metrics", "rules", "alerts", "decision"}

// An allowlist on purpose: the shape is a frozen legacy contract, so an unknown
// key is almost always a typo that would ship silently ignored.
var standardOutputAllowedKeys = map[string]bool{
	"enabled": true, "fields": true, "score": true,
	"data": true, "scorecard": true, "metrics": true,
	"rules": true, "alerts": true, "decision": true,
}

var standardOutputScoreAllowedKeys = map[string]bool{
	"key": true, "label": true, "value": true, "maxValue": true,
}

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

// The configs that carry their OWN inputMappings. Both the rewriter and the
// dependency scanner walk this list; wiring only one fails silently (refs -> None).
var nestedInputMappingConfigKeys = []string{"scorecardConfig", "ruleTreeConfig", "categoryConfig"}

// Runs once during assembly with an identity ref map: that validates the graph and
// leaves references in the canonical form the server substitutes. Re-running is safe.
func rewriteTaskRefs(task map[string]any, refMap map[string]string, ctx string) error {
	if mappings, ok := task["inputMappings"].(map[string]any); ok {
		rewritten, rerr := rewriteRefsInMappings(mappings, refMap)
		if rerr != nil {
			return fmt.Errorf("%s: %w", ctx, rerr)
		}
		task["inputMappings"] = rewritten
	}
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
	if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
		return fmt.Errorf("%s: %w", ctx, err)
	}
	rewriteTaskOutputsRefsDeep(task, refMap, residualSpecRefExcludedFields)
	// Run with the map that excludes THIS task's own identifier, so a ref that reads
	// like a task type (an "end" node) is never mistaken for a residue.
	if err := validateNoResidualSpecRefs(task, refMap, ctx); err != nil {
		return err
	}
	return nil
}

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

func sortedRefMapKeys(refMap map[string]string) string {
	keys := make([]string, 0, len(refMap))
	for k := range refMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// Orders every task after its dependencies: otherwise one whose inputMappings reads
// a downstream task keeps a bare ref that refMap cannot resolve yet.
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
		if s, _ := t["inputExpression"].(string); s != "" {
			addDep(i, mappingDependencyRef(s))
		}
		for _, ref := range templateDependencyRefs(t) {
			addDep(i, ref)
		}
		// rewriteTaskRefs rewrites this same surface, so ordering and rewriting cannot
		// disagree on what counts as a reference.
		for _, ref := range deepTaskOutputsRefs(t, residualSpecRefExcludedFields) {
			addDep(i, ref)
		}
	}

	// O(n^2), but n is typically <50; scanning in original order keeps it stable.
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
