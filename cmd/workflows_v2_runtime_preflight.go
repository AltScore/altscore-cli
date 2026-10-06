package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// Checks for spec mistakes that save, pass the server's dry-run and fail only when a
// workflow executes the node. Each one reproduces what the Borrower Central runtime does.

// extractionSchemaProblem describes why an extractionSchema is not the object schema the
// extraction runtime asks the provider to fill, or returns "" when it is. A bare map such
// as {"name": "string"} fails at run time with INVALID_EXTRACTION_SCHEMA, and a property
// whose value is not an object is never asked for, so it always comes back null.
func extractionSchemaProblem(schema map[string]any) string {
	if t, _ := schema["type"].(string); t != "object" {
		return "documentExtractionConfig.extractionSchema must be an object schema, " +
			`{"type": "object", "properties": {"<name>": {"type": "string", "description": "..."}}}; ` +
			"a bare map of names to types fails at run time (INVALID_EXTRACTION_SCHEMA)"
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return "documentExtractionConfig.extractionSchema needs at least one entry in 'properties'"
	}
	var flat []string
	for name, v := range props {
		if _, isObject := v.(map[string]any); !isObject {
			flat = append(flat, name)
		}
	}
	if len(flat) > 0 {
		sort.Strings(flat)
		return fmt.Sprintf("documentExtractionConfig.extractionSchema properties %v are not objects, so the "+
			`provider is never asked for them and they always come back null; write each as {"type": "...", "description": "..."}`, flat)
	}
	return ""
}

var bareTemplateTokenRe = regexp.MustCompile(`^\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}$`)

// A dotless {{token}} is not a resolver path, so unless an inputMappings key of that name
// supplies it, the runtime receives the braces as literal text.
func bareUnmappedToken(value string, mappings map[string]any) (string, bool) {
	m := bareTemplateTokenRe.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return "", false
	}
	if _, mapped := mappings[m[1]]; mapped {
		return "", false
	}
	return m[1], true
}

// The runtime resolves executorId by id, then as an alias, and runs only the ACTIVE
// version; a missing child fails the node only when it executes. A transport error
// leaves the decision to the runtime rather than refusing a spec on a network blip.
// The caller prefixes errors with the node ref; warnings name the executor.
func checkChildWorkflowExecutor(c *client.Client, executor string) error {
	if c == nil || executor == "" {
		return nil
	}
	_, status, err := findWorkflowByAlias(c, executor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "# warning: child-workflow executorId %q could not be checked (%v); the runtime will\n", executor, err)
		return nil
	}
	switch status {
	case "ACTIVE":
		return nil
	case "DRAFT":
		fmt.Fprintf(os.Stderr, "# warning: child-workflow executorId %q has only a DRAFT; the runtime runs the child's ACTIVE version, so publish it before this workflow runs\n", executor)
		return nil
	}
	data, code, err := c.Do("GET", "borrower_central", "/v2/workflows/"+url.PathEscape(executor), nil)
	if err != nil {
		if code == http.StatusNotFound || code == http.StatusBadRequest || code == http.StatusUnprocessableEntity {
			return fmt.Errorf("child-workflow executorId %q is neither a workflow id nor the alias of a workflow "+
				"in this tenant, so the node fails when it runs. Check the alias with 'altscore workflows-v2 list --filter alias=%s'",
				executor, executor)
		}
		fmt.Fprintf(os.Stderr, "# warning: child-workflow executorId %q could not be checked (%v); the runtime will\n", executor, err)
		return nil
	}
	var wf map[string]any
	if json.Unmarshal(data, &wf) == nil {
		if s, _ := wf["status"].(string); s != "" && s != "ACTIVE" {
			fmt.Fprintf(os.Stderr, "# warning: child-workflow executorId %q is %s; the runtime runs the child's ACTIVE version, so publish it before this workflow runs\n", executor, s)
		}
	}
	return nil
}

// validateEvaluationRuleBody checks the operators of an evaluation rule's conditions the way
// apply checks a conditional's: the evaluator returns False for an operator it does not
// know, so a rule written with one never fires and never errors. A camelCase spelling the
// vocabulary holds only in snake_case is rewritten to it.
func validateEvaluationRuleBody(body *json.RawMessage) error {
	if body == nil || len(*body) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(*body, &m); err != nil {
		return nil
	}
	conditions, ok := m["conditions"]
	if !ok || conditions == nil {
		return nil
	}
	changed := false
	var walk func(node any, path string) error
	walk = func(node any, path string) error {
		g, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		if items, has := g["items"]; has {
			for i, it := range asSlice(items) {
				if err := walk(it, fmt.Sprintf("%s.items[%d]", path, i)); err != nil {
					return err
				}
			}
			return nil
		}
		op, _ := g["operator"].(string)
		if op == "" {
			return nil
		}
		if snake := camelToSnake(op); !conditionOperators[op] && snake != op && conditionOperators[snake] {
			fmt.Fprintf(os.Stderr, "# %s.operator %q rewritten to %q: the evaluator knows only that spelling and evaluates an unknown operator to False without an error\n", path, op, snake)
			g["operator"] = snake
			changed = true
			return nil
		}
		return checkConditionOperator(op, path)
	}
	if err := walk(conditions, "conditions"); err != nil {
		return err
	}
	if changed {
		out, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("encode evaluation rule: %w", err)
		}
		*body = out
	}
	return nil
}

// borrowerIdFieldProblem says why an altdata-enrichment borrowerIdField never resolves,
// or returns "". The runtime looks the field up as a key of the node's context, where the
// workflow inputs sit at the top level, so an `inputs.` path finds nothing and the node
// silently falls back to the workflow's primary borrower.
func borrowerIdFieldProblem(field string) string {
	f := strings.TrimSpace(field)
	if !strings.HasPrefix(f, "inputs.") && !strings.Contains(f, "{{") {
		return ""
	}
	return fmt.Sprintf("borrowerIdField %q never resolves: it names a key of the node's own inputs, not a path, "+
		"and the runtime then falls back to the workflow's primary borrower without a warning. Map the id instead, "+
		`inputMappings {"borrower_id": "inputs.<name>"}, and drop borrowerIdField (it defaults to borrower_id)`, field)
}

var objectInputFieldRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])inputs\.([A-Za-z0-9_]+)\.([A-Za-z0-9_]+)`)

// undeclaredObjectInputFields lists every inputs.<object>.<field> the nodes read whose object
// input declares no such property. A caller (the Hub's run form, an API client) knows only the
// declared properties, so it never sends the field and the read comes back null. An object
// marked "additionalProperties": true is open on purpose and is not checked.
func undeclaredObjectInputFields(inputs map[string]any, nodes ...[]map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			for _, m := range objectInputFieldRe.FindAllStringSubmatch(t, -1) {
				decl, _ := inputs[m[1]].(map[string]any)
				if typ, _ := decl["type"].(string); typ != "object" || decl["additionalProperties"] == true {
					continue
				}
				if _, declared := asMap(decl["properties"])[m[2]]; declared {
					continue
				}
				if key := m[1] + "." + m[2]; !seen[key] {
					seen[key] = true
					out = append(out, key)
				}
			}
		case map[string]any:
			for _, x := range t {
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	for _, list := range nodes {
		for _, n := range list {
			walk(n)
		}
	}
	sort.Strings(out)
	return out
}

func undeclaredObjectInputFieldsError(fields []string) error {
	if len(fields) == 0 {
		return nil
	}
	obj, field, _ := strings.Cut(fields[0], ".")
	return fmt.Errorf("inputVariables: the nodes read %s, which the object inputs do not declare. A caller knows only "+
		"the declared properties, so it never sends these and each read comes back null. Declare each under its "+
		`object, e.g. "%s": {"type": "object", "properties": {"%s": {"type": "number"}}}, `+
		`or mark the object "additionalProperties": true when its fields are open on purpose`,
		strings.Join(fields, ", "), obj, field)
}

// The Python sandbox (python-eval-service, app/usecase/simple_eval.py) validates a custom
// variable's code statically before running any of it, and in a compute node's shared mode
// one refusal fails every variable of that node with a 500. These mirror its rules: imports
// are allowed except the blocked modules, and a bare call to one of the blocked builtins is
// refused wherever it appears. A method call such as re.compile(...) is fine.
var sandboxBlockedModules = []string{
	"subprocess", "os", "sys", "builtins", "importlib", "ctypes", "multiprocessing", "threading",
	"socket", "requests", "urllib", "http", "ftp", "telnetlib", "smtplib", "ssl",
}

var sandboxBlockedCalls = map[string]string{
	"__import__": "write the import as its own statement instead",
	"eval":       "compute the value directly",
	"exec":       "compute the value directly",
	"compile":    "compute the value directly",
	"open":       "files are not reachable from a variable",
	"print":      "use logger.info(...) instead",
}

var (
	pyImportStmtRe = regexp.MustCompile(`(?m)^[ \t]*import[ \t]+([^\n]+)`)
	pyFromStmtRe   = regexp.MustCompile(`(?m)^[ \t]*from[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)
	pyBareCallRe   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])(__import__|eval|exec|compile|open|print)[ \t]*\(`)
	pyNameAttrRe   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])([A-Za-z_][A-Za-z0-9_]*)[ \t]*\.`)
	pyDunderArgRe  = regexp.MustCompile(`^__import__[ \t]*\([ \t]*['"]([A-Za-z_][A-Za-z0-9_.]*)['"]`)
)

// pythonCodeOnly blanks comments and string literals, keeping every newline, so the scans
// below see code only and their offsets still map to the author's line numbers.
func pythonCodeOnly(src string) string {
	b := []byte(src)
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '#':
			for i < len(b) && b[i] != '\n' {
				b[i] = ' '
				i++
			}
		case c == '\'' || c == '"':
			delim := []byte{c}
			if i+2 < len(b) && b[i+1] == c && b[i+2] == c {
				delim = []byte{c, c, c}
			}
			i += len(delim)
			for i < len(b) && !bytes.HasPrefix(b[i:], delim) {
				if b[i] == '\\' && i+1 < len(b) && b[i+1] != '\n' {
					b[i] = ' '
					i++
				}
				if b[i] != '\n' {
					b[i] = ' '
				}
				i++
			}
			i += len(delim)
		default:
			i++
		}
	}
	return string(b)
}

// sandboxRefusals lists, in source order, what the sandbox would refuse in one expression.
func sandboxRefusals(expression string) []string {
	code := pythonCodeOnly(expression)
	lineOf := func(offset int) int { return strings.Count(code[:offset], "\n") + 1 }
	blocked := map[string]bool{}
	for _, m := range sandboxBlockedModules {
		blocked[m] = true
	}
	type refusal struct {
		offset int
		text   string
	}
	var found []refusal
	for _, m := range pyImportStmtRe.FindAllStringSubmatchIndex(code, -1) {
		for _, part := range strings.Split(code[m[2]:m[3]], ",") {
			fields := strings.Fields(part)
			if len(fields) == 0 {
				continue
			}
			if module, _, _ := strings.Cut(fields[0], "."); blocked[module] {
				found = append(found, refusal{m[2], fmt.Sprintf("line %d: import of '%s' is blocked", lineOf(m[2]), module)})
			}
		}
	}
	for _, m := range pyFromStmtRe.FindAllStringSubmatchIndex(code, -1) {
		if module := code[m[2]:m[3]]; blocked[module] {
			found = append(found, refusal{m[2], fmt.Sprintf("line %d: import from '%s' is blocked", lineOf(m[2]), module)})
		}
	}
	for _, m := range pyBareCallRe.FindAllStringSubmatchIndex(code, -1) {
		name := code[m[2]:m[3]]
		fix := sandboxBlockedCalls[name]
		if arg := pyDunderArgRe.FindStringSubmatch(expression[m[2]:]); name == "__import__" && arg != nil {
			fix = fmt.Sprintf("write `import %s` on its own line instead", arg[1])
		}
		found = append(found, refusal{m[2], fmt.Sprintf("line %d: %s(...) is never allowed; %s", lineOf(m[2]), name, fix)})
	}
	for _, m := range pyNameAttrRe.FindAllStringSubmatchIndex(code, -1) {
		if name := code[m[2]:m[3]]; blocked[name] {
			found = append(found, refusal{m[2], fmt.Sprintf("line %d: access to the '%s' module is blocked", lineOf(m[2]), name)})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].offset < found[j].offset })
	var texts []string
	seen := map[string]bool{}
	for _, f := range found {
		if !seen[f.text] {
			seen[f.text] = true
			texts = append(texts, f.text)
		}
	}
	return texts
}

func sandboxRefusalProblems(customVars map[string]any) []error {
	names := make([]string, 0, len(customVars))
	for name := range customVars {
		names = append(names, name)
	}
	sort.Strings(names)
	var problems []error
	for _, name := range names {
		v, _ := customVars[name].(map[string]any)
		expression, _ := v["expression"].(string)
		if refusals := sandboxRefusals(expression); len(refusals) > 0 {
			problems = append(problems, fmt.Errorf(
				"customVariables[%q]: the Python sandbox refuses this code before running it, and in a compute node "+
					"one refusal fails every variable of the node: %s. Imports are allowed except %s; datetime and math "+
					"are not pre-imported, so import them",
				name, strings.Join(refusals, "; "), strings.Join(sandboxBlockedModules, ", ")))
		}
	}
	return problems
}
