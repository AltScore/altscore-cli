package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// Advisory by construction: a finding never moves the exit code, and each practice prints
// ONE aggregated line -- a workflow with 218 untitled variables must not print 218.
type readabilityFinding struct {
	Practice string
	Count    int
	Total    int
	Sample   []string
	Advice   string
}

const readabilitySampleCap = 5

func (f readabilityFinding) line() string {
	sample := ""
	if len(f.Sample) > 0 {
		shown := f.Sample
		more := ""
		if len(shown) > readabilitySampleCap {
			more = fmt.Sprintf(" (+%d more)", len(shown)-readabilitySampleCap)
			shown = shown[:readabilitySampleCap]
		}
		sample = fmt.Sprintf(" e.g. %s%s", strings.Join(shown, ", "), more)
	}
	return fmt.Sprintf("#   [%s] %d of %d -- %s%s\n", f.Practice, f.Count, f.Total, f.Advice, sample)
}

func adviseHandoffReadability(wf map[string]any, endTasks []map[string]any, rules []any, w io.Writer) {
	if w == nil {
		return
	}
	var findings []readabilityFinding
	if f, ok := adviseVariableTitles(asMap(wf["customVariables"])); ok {
		findings = append(findings, f)
	}
	if f, ok := adviseOrdinalCodeVars(asMap(wf["customVariables"])); ok {
		findings = append(findings, f)
	}
	findings = append(findings, advisePDFSections(asSlice(wf["nodes"]), endTasks)...)
	if f, ok := adviseRuleDescriptions(rules); ok {
		findings = append(findings, f)
	}
	if f, ok := adviseDiacritics(humanStringsFromWorkflow(wf, endTasks, rules)); ok {
		findings = append(findings, f)
	}
	printReadabilityFindings(w, findings)
}

func printReadabilityFindings(w io.Writer, findings []readabilityFinding) {
	if w == nil || len(findings) == 0 {
		return
	}
	fmt.Fprintf(w, "# readability advisory (client handoff): %d finding(s). "+
		"Advisory only -- never fails lint or blocks apply.\n", len(findings))
	for _, f := range findings {
		fmt.Fprint(w, f.line())
	}
	fmt.Fprintf(w, "#   see `altscore workflows-v2 schema-guide handoffReadability`\n")
}

func adviseVariableTitles(customVariables map[string]any) (readabilityFinding, bool) {
	total := len(customVariables)
	if total == 0 {
		return readabilityFinding{}, false
	}
	var missing []string
	for name, raw := range customVariables {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := v["title"].(string); strings.TrimSpace(t) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return readabilityFinding{}, false
	}
	sort.Strings(missing)
	return readabilityFinding{
		Practice: "variable-titles",
		Count:    len(missing),
		Total:    total,
		Sample:   missing,
		Advice: "custom variable(s) have no `title`. The Hub falls back to the raw name, " +
			"so the reader sees the author's shorthand in every picker, node overlay and PDF. " +
			"Set `title` to the business term the client already uses",
	}, true
}

// An htmlBlock body that is nothing but one {placeholder}: markup assembled in Python.
var barePlaceholder = regexp.MustCompile(`^\s*\{[A-Za-z0-9_-]+\}\s*$`)

// Says nothing about an ABSENT data-source section: includeAllSources wires those at render
// time. endTasks is passed in because GET /v2/workflows/{id} does not embed task bodies.
func advisePDFSections(nodes []any, endTasks []map[string]any) []readabilityFinding {
	pdfConfigs := make([]map[string]any, 0, 1)
	for _, n := range nodes {
		nm := asMap(n)
		if strings.ToLower(fmt.Sprint(nm["type"])) != "end" {
			continue
		}
		if pdf := asMap(asMap(nm["endConfig"])["pdfConfig"]); pdf != nil {
			pdfConfigs = append(pdfConfigs, pdf)
			continue
		}
		if pdf := asMap(asMap(asMap(nm["data"])["endConfig"])["pdfConfig"]); pdf != nil {
			pdfConfigs = append(pdfConfigs, pdf)
		}
	}
	for _, t := range endTasks {
		if pdf := asMap(asMap(t["endConfig"])["pdfConfig"]); pdf != nil {
			pdfConfigs = append(pdfConfigs, pdf)
		}
	}

	var untitled, pythonHTML []string
	sections := 0
	for _, pdf := range pdfConfigs {
		if enabled, ok := pdf["enabled"].(bool); !ok || !enabled {
			continue
		}
		for i, s := range asSlice(pdf["sourcesConfig"]) {
			sm := asMap(s)
			if on, ok := sm["enabled"].(bool); ok && !on {
				continue
			}
			sections++
			kind := fmt.Sprint(sm["type"])
			ref := fmt.Sprint(sm["taskAlias"])
			if ref == "" || ref == "<nil>" {
				ref = fmt.Sprintf("%s[%d]", kind, i)
			}
			if t, _ := sm["title"].(string); strings.TrimSpace(t) == "" {
				untitled = append(untitled, ref)
			}
			if kind != "htmlBlock" {
				continue
			}
			for _, c := range asSlice(sm["components"]) {
				body, _ := asMap(c)["content"].(string)
				if body != "" && barePlaceholder.MatchString(body) {
					pythonHTML = append(pythonHTML, strings.TrimSpace(body))
				}
			}
		}
	}
	if sections == 0 {
		return nil
	}
	var out []readabilityFinding
	if len(untitled) > 0 {
		sort.Strings(untitled)
		out = append(out, readabilityFinding{
			Practice: "pdf-section-titles",
			Count:    len(untitled),
			Total:    sections,
			Sample:   untitled,
			Advice: "PDF section(s) render with no title. With includeAllSources the runtime " +
				"wires the section for you, but it cannot name it -- add {taskAlias, title} to " +
				"sourcesConfig to retitle an auto section in the client's language",
		})
	}
	if len(pythonHTML) > 0 {
		sort.Strings(pythonHTML)
		out = append(out, readabilityFinding{
			Practice: "pdf-python-html",
			Count:    len(pythonHTML),
			Total:    sections,
			Sample:   pythonHTML,
			Advice: "htmlBlock section(s) whose entire body is one placeholder, so the markup is " +
				"being assembled in a Python expression. The client cannot change a heading " +
				"without editing Python. Author the HTML in `content` and let safe_format fill " +
				"{placeholders} from upstream outputs",
		})
	}
	return out
}

// A description that records where the rule CAME FROM instead of what it checks.
var provenanceDescription = regexp.MustCompile(`(?i)\b(ported|migrated|copied|imported|carried over)\s+(from|out of)\b|\bfrom\s+v[0-9]+\b|\blegacy\s+(rule|engine|workflow)\b`)

func adviseRuleDescriptions(rules []any) (readabilityFinding, bool) {
	if len(rules) == 0 {
		return readabilityFinding{}, false
	}
	var bad []string
	for _, r := range rules {
		rm := asMap(r)
		code, _ := rm["code"].(string)
		if code == "" {
			code, _ = rm["label"].(string)
		}
		desc, _ := rm["description"].(string)
		if strings.TrimSpace(desc) == "" || provenanceDescription.MatchString(desc) {
			bad = append(bad, code)
		}
	}
	if len(bad) == 0 {
		return readabilityFinding{}, false
	}
	sort.Strings(bad)
	return readabilityFinding{
		Practice: "rule-descriptions",
		Count:    len(bad),
		Total:    len(rules),
		Sample:   bad,
		Advice: "evaluation rule(s) have a blank description or one that records the rule's " +
			"provenance rather than what it checks. The client reads this field; state the " +
			"business rule and keep migration notes in the workflow description",
	}, true
}

// GET /v2/workflows/{id} returns nodes WITHOUT their task bodies, and pdfConfig lives on
// the task. Fail-open per task.
func fetchEndTaskBodies(c *client.Client, nodes []any) []map[string]any {
	if c == nil {
		return nil
	}
	var out []map[string]any
	for _, n := range nodes {
		nm := asMap(n)
		if strings.ToLower(fmt.Sprint(nm["type"])) != "end" {
			continue
		}
		alias, _ := nm["taskAlias"].(string)
		if alias == "" {
			continue
		}
		task, err := fetchPersistedTask(c, alias)
		if err != nil || task == nil {
			continue
		}
		out = append(out, task)
	}
	return out
}

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

// The filter key is `workflow-alias`. `workflowAlias` and `workflow_alias` are accepted and
// SILENTLY IGNORED, returning the tenant's unfiltered first page. Do not tidy to camelCase.
func fetchWorkflowRules(c *client.Client, alias string) []any {
	if c == nil || alias == "" {
		return nil
	}
	data, _, err := c.Do("GET", "borrower_central",
		"/v1/evaluation-rules?workflow-alias="+url.QueryEscape(alias)+"&per-page=200", nil)
	if err != nil {
		return nil
	}
	var rules []any
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil
	}
	return rules
}
