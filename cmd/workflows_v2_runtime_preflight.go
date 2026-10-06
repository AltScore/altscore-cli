package cmd

import (
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
