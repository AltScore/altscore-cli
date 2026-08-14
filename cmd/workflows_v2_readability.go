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

// Handoff readability advisories.
//
// These check the three things that decide whether the analyst who INHERITS a
// workflow can read it. They are all invisible to the runtime -- a workflow that
// trips every one of them executes identically -- which is exactly why nothing
// else catches them and why they rot silently until the client opens the builder
// and finds `pjex_cat_final_limit_amount` where a label should be.
//
// Advisory by construction: written to a writer, contributing nothing to
// lintReport.Issues, so the exit code never moves. Findings are AGGREGATED --
// one line per practice with a capped sample, never one line per variable. A
// workflow with 218 untitled variables must not print 218 lines.

// readabilityFinding is one practice's verdict: how many objects tripped it, a
// short sample to make it actionable, and the advice.
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

// adviseHandoffReadability emits ONE aggregated advisory block covering every
// practice that has something to say. rules may be nil when the caller could not
// fetch them; that practice is then simply not reported (never guessed at).
func adviseHandoffReadability(wf map[string]any, endTasks []map[string]any, rules []any, w io.Writer) {
	if w == nil {
		return
	}
	var findings []readabilityFinding
	if f, ok := adviseVariableTitles(asMap(wf["customVariables"])); ok {
		findings = append(findings, f)
	}
	findings = append(findings, advisePDFSections(asSlice(wf["nodes"]), endTasks)...)
	if f, ok := adviseRuleDescriptions(rules); ok {
		findings = append(findings, f)
	}
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(w, "# readability advisory (client handoff): %d finding(s). "+
		"Advisory only -- never fails lint or blocks apply.\n", len(findings))
	for _, f := range findings {
		fmt.Fprint(w, f.line())
	}
	fmt.Fprintf(w, "#   see `altscore workflows-v2 schema-guide handoffReadability`\n")
}

// adviseVariableTitles flags custom variables with no `title`.
//
// The Hub renders `variable.title || variable.name` in the elements panel, the
// compute-variables node overlay, every variable picker and the PDF. Without a
// title the reader gets the raw snake_case name, which is the author's shorthand
// and not the business term the client uses.
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

// barePlaceholder matches an htmlBlock body that is nothing but a single
// {placeholder} -- the signature of HTML assembled in Python and piped through
// one variable, rather than authored in the section.
var barePlaceholder = regexp.MustCompile(`^\s*\{[A-Za-z0-9_-]+\}\s*$`)

// advisePDFSections flags the two ways a PDF stops being editable without Python:
// a section with no title, and an htmlBlock whose whole body is one placeholder.
//
// It deliberately says NOTHING about a data-source section being absent:
// includeAllSources auto-wires those at render time, so "missing" is the normal,
// recommended shape and warning on it would mislead (this is the same trap the
// removed silent-PDF lint fell into).
//
// endConfig lives in two places depending on the caller. An apply spec carries it
// INLINE on the node; GET /v2/workflows/{id} does NOT embed task bodies at all
// (an end node there has only data.inputMappings), so lint has to fetch the end
// task and pass it in via endTasks. Reading only the node would make this check a
// silent no-op on every saved workflow.
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

// provenanceDescription matches a description that records where the rule CAME
// FROM instead of what it checks. Migration notes are for the engineer doing the
// port; the client reads this field in the builder and in the report.
var provenanceDescription = regexp.MustCompile(`(?i)\b(ported|migrated|copied|imported|carried over)\s+(from|out of)\b|\bfrom\s+v[0-9]+\b|\blegacy\s+(rule|engine|workflow)\b`)

// adviseRuleDescriptions flags evaluation rules whose description is empty or is
// pure migration provenance.
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

// fetchWorkflowRules pulls the evaluation rules scoped to a workflow alias so
// lint can judge their descriptions. Fail-open: any error yields nil and the
// rule-descriptions practice is simply not reported.
//
// The filter key is `workflow-alias`. `workflowAlias` and `workflow_alias` are
// accepted by the endpoint and SILENTLY IGNORED -- they return the tenant's
// unfiltered first page, which would make this advisory judge other workflows'
// rules. Do not "tidy" this into camelCase.
// fetchEndTaskBodies resolves the task body behind every end node, because
// GET /v2/workflows/{id} returns nodes WITHOUT their task bodies -- pdfConfig
// lives on the task, not the node. Fail-open per task: one unreadable end task
// costs that task's findings, not the whole advisory.
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
