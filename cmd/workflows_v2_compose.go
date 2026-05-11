package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// compose takes a single agent-friendly spec and creates the underlying
// /v2/tasks records plus the workflow with nodes referencing them. This
// is the one-shot path agents should take for greenfield workflows.
//
// Spec shape (any field omitted falls through to API defaults):
//
//	{
//	  "label":             "Scoring pipeline",
//	  "category":          "EVALUATION",
//	  "description":       "...",
//	  "inputVariables":    {"borrower_id": {"type": "string", "required": true}},
//	  "customVariables":   {},
//	  "tasks": [
//	    {
//	      "alias":          "fetch",
//	      "label":          "Fetch ECU bureau",
//	      "type":           "altdata-enrichment",
//	      "sourcesConfig":  [{"sourceId":"ECU-PUB-0002","version":"v1"}],
//	      "borrowerIdField":"borrower_id",
//	      "inputMappings":  {"borrower_id": "inputs.borrower_id"}
//	      // ... any other CreateTaskV2 field
//	    }
//	  ],
//	  "extraNodes": [
//	    {"nodeId": "start", "type": "start", "label": "Start"},
//	    {"nodeId": "end",   "type": "end",   "label": "End"}
//	  ],
//	  "edges": [
//	    {"sourceNodeId":"start","targetNodeId":"fetch"},
//	    {"sourceNodeId":"fetch","targetNodeId":"end"}
//	  ]
//	}
//
// Behavior:
//  1. POST /v2/tasks for each entry in spec.tasks, capturing the returned alias.
//  2. Build graph nodes: one node per task (nodeId = task alias, taskAlias =
//     alias, taskVersion = returned version), plus any extraNodes.
//  3. Auto-position nodes left-to-right (extraNodes first, then tasks in
//     spec order); the Hub UI re-lays them out anyway.
//  4. POST /v2/workflows with label, category, description, inputVariables,
//     customVariables, nodes, edges, status (default DRAFT).
//
// On any task-create failure, the partial state is reported and the workflow
// is not created. Use --rollback-tasks to cascade-delete created tasks on
// failure (best-effort; tasks-v2 has no DELETE today, so this is a no-op
// placeholder for future API support).

type composeSpec struct {
	Label           string                 `json:"label"`
	Category        string                 `json:"category"`
	Description     string                 `json:"description,omitempty"`
	Status          string                 `json:"status,omitempty"`
	InputVariables  map[string]any         `json:"inputVariables,omitempty"`
	CustomVariables map[string]any         `json:"customVariables,omitempty"`
	Config          map[string]any         `json:"config,omitempty"`
	Tasks           []map[string]any       `json:"tasks"`
	ExtraNodes      []map[string]any       `json:"extraNodes,omitempty"`
	Edges           []map[string]any       `json:"edges"`
	Notes           []map[string]any       `json:"notes,omitempty"`
}

func makeWfv2ComposeCmd() *cobra.Command {
	var bodyFlag string
	var dryRun bool
	var publish bool
	var skipLintOnPublish bool

	cmd := &cobra.Command{
		Use:   "compose",
		Short: "Create tasks + workflow from a single spec (greenfield one-shot)",
		Long: `Take a single JSON spec describing tasks and graph topology, create the
underlying /v2/tasks records, then create the workflow with nodes
referencing those tasks by alias.

The created workflow is in DRAFT status by default -- mirrors how the Hub's
visual editor saves drafts. DRAFT workflows can be executed but the engine
silently skips every node, so 'workflows-v2 execute' will return an empty
output and look successful while doing nothing useful. Pass --publish to
publish immediately after create (POST /v2/workflows/{id}/publish), which is
typically what you want when composing from the CLI.

Use --dry-run to print what would be sent without making any API calls.

Spec format (see file header for full reference):
  - label, category, description, status (DRAFT default)
  - inputVariables, customVariables
  - tasks: list of CreateTaskV2 objects (alias, label, type, type-specific fields)
  - extraNodes: list of pure graph nodes (start, end, etc. -- no task backing)
  - edges: list of {sourceNodeId, targetNodeId, sourceHandle?, label?}`,
		Example: `  altscore workflows-v2 compose --body @scoring-pipeline.json
  altscore workflows-v2 compose --body @spec.json --publish        # ready to execute
  altscore workflows-v2 compose --body @spec.json --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			var spec composeSpec
			if err := json.Unmarshal(body, &spec); err != nil {
				return fmt.Errorf("invalid spec JSON: %w", err)
			}
			if spec.Label == "" {
				return fmt.Errorf("spec.label is required")
			}
			if len(spec.Tasks) == 0 && len(spec.ExtraNodes) == 0 {
				return fmt.Errorf("spec must include at least one of: tasks, extraNodes")
			}

			c, err := loadClient()
			if err != nil {
				return err
			}

			workflow, err := composeWorkflowBody(c, &spec, dryRun, publish)
			if err != nil {
				return err
			}

			wfBody, err := json.Marshal(workflow)
			if err != nil {
				return err
			}

			if dryRun {
				fmt.Fprintln(cmd.OutOrStderr(), "# DRY RUN -- the above POSTs were skipped; final POST /v2/workflows body:")
				return output.RawJSON(json.RawMessage(wfBody))
			}

			data, _, err := c.Do("POST", "borrower_central", "/v2/workflows", json.RawMessage(wfBody))
			if err != nil {
				return fmt.Errorf("create workflow: %w", err)
			}

			// Summary on stderr: list every server-assigned task alias keyed by
			// its node type. Saves a follow-up `workflows-v2 get | jq` to learn
			// what compose just created. Skipped on dry-run since nothing was
			// actually created.
			printComposeSummary(cmd.ErrOrStderr(), workflow)

			if publish {
				var created map[string]any
				if err := json.Unmarshal(data, &created); err != nil {
					return fmt.Errorf("parse created workflow response: %w", err)
				}
				wfID, _ := created["id"].(string)
				if wfID == "" {
					return fmt.Errorf("compose succeeded but response had no 'id' field; cannot publish")
				}
				// Pre-publish lint: refuse to publish a workflow with topology
				// errors (orphan nodes, dangling edges, missing start/end). The
				// engine would silently no-op on most of those; better to surface
				// before the workflow goes ACTIVE. Always run lint even with
				// --skip-lint-on-publish so we can WARN about the override
				// rather than ship a broken workflow silently.
				lintData, _, lerr := c.Do("GET", "borrower_central", "/v2/workflows/"+wfID, nil)
				if lerr == nil {
					var wfFull map[string]any
					if err := json.Unmarshal(lintData, &wfFull); err == nil {
						report := lintWorkflowV2(wfFull)
						errs := []string{}
						for _, issue := range report.Issues {
							if issue.Severity == "error" {
								errs = append(errs, "  - "+issue.Message)
							}
						}
						if len(errs) > 0 {
							if skipLintOnPublish {
								fmt.Fprintf(cmd.OutOrStderr(),
									"# WARNING: pre-publish lint found %d topology error(s) but --skip-lint-on-publish was set; publishing anyway:\n%s\n",
									len(errs), strings.Join(errs, "\n"))
							} else {
								return fmt.Errorf(
									"workflow %s created but pre-publish lint found %d topology error(s); refusing to publish:\n%s\n"+
										"Fix the spec, run 'altscore workflows-v2 publish %s' manually after editing, or pass --skip-lint-on-publish.",
									wfID, len(errs), strings.Join(errs, "\n"), wfID,
								)
							}
						}
					}
				}
				if _, _, err := c.Do("POST", "borrower_central", "/v2/workflows/"+wfID+"/publish", nil); err != nil {
					return fmt.Errorf("workflow %s created but publish failed: %w", wfID, err)
				}
				fmt.Fprintf(cmd.OutOrStderr(), "# published workflow %s\n", wfID)
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON spec (or pipe via stdin)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the assembled workflow body without making API calls")
	cmd.Flags().BoolVar(&publish, "publish", false, "publish the workflow after creation (DRAFT workflows execute but skip every node)")
	cmd.Flags().BoolVar(&skipLintOnPublish, "skip-lint-on-publish", false, "skip the pre-publish topology lint that refuses to publish on errors")
	return cmd
}


// localRef returns the spec-local reference for a task or extraNode entry,
// in priority order: explicit `ref`, then `alias` (for tasks) or `nodeId`
// (for nodes), falling back to the supplied default.
func localRef(entry map[string]any, fallback string) string {
	if v, _ := entry["ref"].(string); v != "" {
		return v
	}
	if v, _ := entry["alias"].(string); v != "" {
		return v
	}
	if v, _ := entry["nodeId"].(string); v != "" {
		return v
	}
	return fallback
}

// slugifyWorkflowLabel mirrors borrower-central's
// app/utils/workflow_alias.py:slugify_workflow_label so compose can predict
// the alias the server will assign before any task POST happens. Knowing the
// alias up-front matters because credit-decisioning entities
// (evaluation-rules, rule-trees, mapping-tables, scorecards) only show up in
// a workflow's builder pickers when their workflowAlias matches the
// workflow's alias -- and the alias is server-derived from the label, not
// settable from the body. When a label like "All 5 types" silently slugs to
// "all-5-types", entities stamped with "all-types" become invisible.
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

// edgeEndpoints reads the source/target ref or nodeId of an edge entry,
// preferring the spec-side `from`/`to` shortcuts.
func edgeEndpoints(e map[string]any) (from, to string) {
	from, _ = e["from"].(string)
	if from == "" {
		from, _ = e["sourceNodeId"].(string)
	}
	to, _ = e["to"].(string)
	if to == "" {
		to, _ = e["targetNodeId"].(string)
	}
	return from, to
}

// endNodeBilliableIDSchema is the canonical billable_id slot the Hub end-task
// plugin adds to every end node. We mirror it byte-for-byte so compose's end
// task matches a Hub-recreated one.
var endNodeBilliableIDSchema = map[string]any{
	"description": "Optional billable identifier, default Customer ID",
	"title":       "Billable ID",
	"type":        "string",
}

// pdfTitleByType is the section title the PDF report editor uses for each
// supported ancestor type. Verified against a Hub-recreated end task: titles
// are the type's display name, NOT the upstream task's label (with one
// exception: altdata uses the literal string "AltData").
var pdfTitleByType = map[string]string{
	"altdata-enrichment": "AltData",
	"scorecard":          "Scorecard",
	"mapping-table":      "Mapping Table",
	"rule-tree":          "Rule Tree",
	"evaluate-rules":     "Evaluate Rules",
}

// buildEndAutoWiring returns the four end-node task fields the Hub auto-wires
// when an end node is dropped on a canvas with credit-decisioning predecessors:
//
//	inputSchema   -- one entry per ancestor type, plus the canonical
//	                 billable_id slot. Verified byte-for-byte against a
//	                 Hub-recreated end task in production.
//	inputMappings -- matching entries pointing at task_outputs.<alias>(.<deep>)
//	pdfSections   -- the endConfig.pdfConfig.sourcesConfig array. WITHOUT
//	                 this, the end task ships with endConfig:null and the
//	                 PDF generator sees no enabled config -- the report
//	                 silently doesn't render. This was the root cause of the
//	                 user's "PDF didn't come out" report on the first
//	                 compose-generated workflow.
//	hasAltdata    -- whether at least one ancestor is altdata-enrichment.
//	                 Returned for the call site's awareness; no longer
//	                 needed to switch on a two-phase create (CreateTaskV2
//	                 accepts multi-dot inputMappings since the validator
//	                 was relaxed -- see borrower-central
//	                 app/model/workflows_v2/task_schemas.py).
//
// Mirrors `buildSectionsFromPredecessors` in
// altscore-ai-chat/lib/stores/workflow-builder-v2/actions/edge/pdf-data-source-auto-mapping.ts.
// Walks ALL transitive ancestors via BFS over incoming edges (matches the
// Hub's getPredecessorsSorted), not just direct parents.
//
// Five upstream task types are recognised: altdata-enrichment, scorecard,
// mapping-table, rule-tree, evaluate-rules. Other predecessor types are
// skipped silently (matches Hub). All return slices/maps are non-nil so
// callers can use len() unconditionally.
func buildEndAutoWiring(endRef string, edges []map[string]any, taskByRef map[string]map[string]any, refMap map[string]string) (inputSchema, inputMappings map[string]any, pdfSections []map[string]any, hasAltdata bool) {
	inputSchema = map[string]any{
		"billable_id": endNodeBilliableIDSchema,
	}
	inputMappings = map[string]any{}
	pdfSections = []map[string]any{}

	// BFS over incoming edges starting from endRef. Direct parents come first
	// (they're at depth 1) but we accumulate every ancestor in `visited` so
	// the end task sees outputs from every upstream credit-decisioning task.
	parents := map[string][]string{}
	for _, e := range edges {
		from, to := edgeEndpoints(e)
		if from == "" || to == "" {
			continue
		}
		parents[to] = append(parents[to], from)
	}
	visited := map[string]bool{endRef: true}
	queue := append([]string{}, parents[endRef]...)
	ancestors := []string{}
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		if visited[ref] {
			continue
		}
		visited[ref] = true
		ancestors = append(ancestors, ref)
		queue = append(queue, parents[ref]...)
	}

	for _, ancestorRef := range ancestors {
		predTask, hasTask := taskByRef[ancestorRef]
		predAlias := refMap[ancestorRef]
		if !hasTask || predAlias == "" {
			continue
		}
		predType, _ := predTask["type"].(string)
		predLabel, _ := predTask["label"].(string)
		if predLabel == "" {
			predLabel = predAlias
		}

		// section is the per-ancestor entry that lands in
		// endConfig.pdfConfig.sourcesConfig. Title comes from pdfTitleByType
		// (matches Hub's persisted shape -- generic display names, not the
		// upstream task's label). Subtitle uses the upstream label so the
		// rendered PDF is still self-describing.
		title, ok := pdfTitleByType[predType]
		if !ok {
			continue // unrecognised type; skip silently like the Hub
		}
		var sourceInputSchema string
		var mappingValue string
		var components []map[string]any

		switch predType {
		case "altdata-enrichment":
			sourceInputSchema = "sources_output_packages_" + predAlias
			mappingValue = "task_outputs." + predAlias + ".sources_output_packages"
			hasAltdata = true
			// One component per source in the upstream's sourcesConfig.
			for _, raw := range asSlice(predTask["sourcesConfig"]) {
				s, _ := raw.(map[string]any)
				if s == nil {
					continue
				}
				sid, _ := s["sourceId"].(string)
				ver, _ := s["version"].(string)
				if ver == "" {
					ver = "v1"
				}
				components = append(components, map[string]any{
					"id":             newUUIDv4(),
					"name":           sid + "_" + ver,
					"altdataPackage": sid,
				})
			}

		case "scorecard":
			sourceInputSchema = "scorecard_result_" + predAlias
			mappingValue = "task_outputs." + predAlias
			cfg, _ := predTask["scorecardConfig"].(map[string]any)
			totalVar := "total_score"
			breakdownVar := "score_breakdown"
			if cfg != nil {
				if v, _ := cfg["totalScoreVariable"].(string); v != "" {
					totalVar = v
				}
				if v, _ := cfg["breakdownVariable"].(string); v != "" {
					breakdownVar = v
				}
			}
			components = append(components, map[string]any{
				"id":                 newUUIDv4(),
				"name":               "scorecardResult",
				"totalScoreVariable": totalVar,
				"breakdownVariable":  breakdownVar,
			})

		case "mapping-table":
			sourceInputSchema = "mapping_table_result_" + predAlias
			mappingValue = "task_outputs." + predAlias
			cfg, _ := predTask["mappingTableConfig"].(map[string]any)
			outputVariables := []map[string]any{}
			if cfg != nil {
				for _, raw := range asSlice(cfg["entries"]) {
					em, _ := raw.(map[string]any)
					v, _ := em["outputVariable"].(string)
					if v == "" {
						continue
					}
					outputVariables = append(outputVariables, map[string]any{
						"variable": v,
						"label":    v,
					})
				}
			}
			components = append(components, map[string]any{
				"id":              newUUIDv4(),
				"name":            "mappingTableResult",
				"outputVariables": outputVariables,
			})

		case "rule-tree":
			sourceInputSchema = "rule_tree_result_" + predAlias
			mappingValue = "task_outputs." + predAlias
			cfg, _ := predTask["ruleTreeConfig"].(map[string]any)
			outVar := "decision"
			if cfg != nil {
				if v, _ := cfg["outputVariable"].(string); v != "" {
					outVar = v
				}
			}
			components = append(components, map[string]any{
				"id":             newUUIDv4(),
				"name":           "ruleTreeResult",
				"outputVariable": outVar,
			})

		case "evaluate-rules":
			sourceInputSchema = "evaluate_rules_result_" + predAlias
			mappingValue = "task_outputs." + predAlias
			components = append(components, map[string]any{
				"id":   newUUIDv4(),
				"name": "evaluateRulesResult",
			})
		}

		inputSchema[sourceInputSchema] = map[string]any{"type": "object", "title": predLabel}
		inputMappings[sourceInputSchema] = mappingValue

		// PDF section. type=altdata-enrichment maps to PDF type='altdata' (the
		// renderer's discriminator drops the suffix). All other types pass
		// through verbatim.
		pdfType := predType
		if pdfType == "altdata-enrichment" {
			pdfType = "altdata"
		}
		pdfSections = append(pdfSections, map[string]any{
			"id":                newUUIDv4(),
			"type":              pdfType,
			"title":             title,
			"subtitle":          "From " + predLabel,
			"enabled":           true,
			"page_break":        true,
			"sourceInputSchema": sourceInputSchema,
			"taskAlias":         predAlias,
			"components":        components,
		})
	}

	return inputSchema, inputMappings, pdfSections, hasAltdata
}

// readSpecHTMLSections extracts the spec-only `htmlSections` field from an
// extraNode. Returns nil when the field is absent or shaped wrong (compose
// silently ignores malformed entries; the spec validator would have caught
// truly broken JSON before we got here).
func readSpecHTMLSections(node map[string]any) []map[string]any {
	raw, ok := node["htmlSections"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// htmlSectionVarRegex matches {<word>} placeholders in HTML content -- same
// shape that runtime safe_format() resolves. Compose uses it to discover
// which workflow input/custom variables a section references so it can wire
// them into the end task's inputMappings (otherwise safe_format leaves
// {var} as a literal because the runtime context never resolves it).
var htmlSectionVarRegex = regexp.MustCompile(`\{(\w+)\}`)

// buildHTMLSections turns spec htmlSections into pdfConfig.sourcesConfig
// entries with components shaped as {name:"htmlBlock", content, context}.
// Returns:
//   - sections: ready-to-append entries for endConfig.pdfConfig.sourcesConfig
//   - inputSchema additions: a slot per input/custom variable referenced in
//     any section's content
//   - inputMappings additions: matching wiring (inputs.<name> or
//     custom.<name>) so safe_format resolves at runtime
//
// Each section accepts:
//   - title: PDF heading (default "")
//   - subtitle: PDF subheading (default "")
//   - content: the raw HTML string with {var} interpolation tokens
//   - context: visual treatment "none" | "info" | "warning" | "success" |
//     "danger" (default "none")
//   - pageBreak: whether to start a new PDF page (default true)
func buildHTMLSections(sections []map[string]any, inputVars, customVars map[string]any) (
	built []map[string]any,
	schema map[string]any,
	mappings map[string]any,
) {
	built = make([]map[string]any, 0, len(sections))
	schema = map[string]any{}
	mappings = map[string]any{}

	for _, s := range sections {
		content, _ := s["content"].(string)
		title, _ := s["title"].(string)
		subtitle, _ := s["subtitle"].(string)
		ctx, _ := s["context"].(string)
		if ctx == "" {
			ctx = "none"
		}
		pageBreak := true
		if v, has := s["pageBreak"].(bool); has {
			pageBreak = v
		}

		built = append(built, map[string]any{
			"id":         newUUIDv4(),
			"type":       "htmlBlock",
			"title":      title,
			"subtitle":   subtitle,
			"enabled":    true,
			"page_break": pageBreak,
			"components": []map[string]any{{
				"id":      newUUIDv4(),
				"name":    "htmlBlock",
				"content": content,
				"context": ctx,
			}},
		})

		// Auto-wire workflow-scope variables referenced in the content.
		// Task-output references resolve via end_activity's enriched_context
		// promotion (no wiring needed); only inputs/custom vars need to be
		// pulled into the end task's resolved-context root.
		for _, m := range htmlSectionVarRegex.FindAllStringSubmatch(content, -1) {
			name := m[1]
			if _, has := mappings[name]; has {
				continue
			}
			if _, isInput := inputVars[name]; isInput {
				mappings[name] = "inputs." + name
				schema[name] = inferSchemaForVar(inputVars[name], name)
				continue
			}
			if _, isCustom := customVars[name]; isCustom {
				mappings[name] = "custom." + name
				schema[name] = inferSchemaForVar(customVars[name], name)
				continue
			}
			// Else: assume the name is a task output (alerts, credit_score,
			// decision, ...). end_activity promotes those to root, so no
			// inputMappings entry is needed -- safe_format will find them.
		}
	}
	return built, schema, mappings
}

// inferSchemaForVar derives an inputSchema entry for a workflow-scope
// variable, falling back to type:"string" when the spec doesn't define one
// (or when the spec value isn't shaped as a JSON-Schema-style object).
func inferSchemaForVar(specValue any, name string) map[string]any {
	out := map[string]any{
		"type":  "string",
		"title": humanizeKey(name),
	}
	v, ok := specValue.(map[string]any)
	if !ok {
		return out
	}
	if t, _ := v["type"].(string); t != "" {
		out["type"] = t
	}
	if title, _ := v["title"].(string); title != "" {
		out["title"] = title
	} else if label, _ := v["label"].(string); label != "" {
		out["title"] = label
	}
	return out
}

// printComposeSummary writes a one-line-per-task summary to stderr after
// compose finishes its real (non-dry-run) POSTs. Without this, the only thing
// compose returns to stdout is `{"id": "..."}` of the workflow -- the user
// then has to GET the workflow + each task to discover server-assigned
// aliases. Format mirrors `workflows-v2 lint`'s short report style.
func printComposeSummary(w io.Writer, workflow map[string]any) {
	if w == nil {
		return
	}
	rawNodes, ok := workflow["nodes"].([]map[string]any)
	if !ok {
		// Try the slice-of-any form some assemblies produce.
		alt, _ := workflow["nodes"].([]any)
		for _, item := range alt {
			if m, ok := item.(map[string]any); ok {
				rawNodes = append(rawNodes, m)
			}
		}
	}
	if len(rawNodes) == 0 {
		return
	}
	fmt.Fprintf(w, "# created %d task(s) backing the workflow nodes:\n", len(rawNodes))
	for _, n := range rawNodes {
		typ, _ := n["type"].(string)
		alias, _ := n["taskAlias"].(string)
		label, _ := n["label"].(string)
		if alias == "" {
			continue
		}
		fmt.Fprintf(w, "#   %-20s  alias=%-40s  label=%q\n", typ, alias, label)
	}
}

// postTask creates a task via POST /v2/tasks and returns the server-assigned
// alias and version. Used by the tasks loop and the extraNodes loop. The
// previous two-phase create (postTaskWithMultiDotFallback) is gone:
// CreateTaskV2's strict-vs-lenient distinction was relaxed in the backend --
// the input_mappings validator now just returns its argument unchanged
// (see borrower-central/app/model/workflows_v2/task_schemas.py). Multi-dot
// inputMappings now land at version 1 in a single POST.
//
// In dryRun mode the caller-supplied `ref` is used as the alias placeholder
// (so downstream extraNode positioning matches what real-run produces) and
// the version is always 1.
func postTask(c *client.Client, body map[string]any, ref string, dryRun bool, label string) (alias string, version int, err error) {
	bytes, err := json.Marshal(body)
	if err != nil {
		return "", 0, fmt.Errorf("encode %s: %w", label, err)
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "# Would POST /v2/tasks (%s): %s\n", label, string(bytes))
		if a, _ := body["alias"].(string); a != "" {
			alias = a
		} else {
			alias = ref
		}
		return alias, 1, nil
	}

	data, _, err := c.Do("POST", "borrower_central", "/v2/tasks", json.RawMessage(bytes))
	if err != nil {
		return "", 0, err
	}
	var created map[string]any
	if err := json.Unmarshal(data, &created); err != nil {
		return "", 0, fmt.Errorf("parse %s response: %w", label, err)
	}
	if a, _ := created["alias"].(string); a != "" {
		alias = a
	} else {
		return "", 0, fmt.Errorf("%s: server returned no alias", label)
	}
	if v, ok := created["version"].(float64); ok {
		version = int(v)
	} else {
		version = 1
	}
	return alias, version, nil
}

// reservedMappingScopes are the leading segments in a mapping value that are
// NOT spec-local refs and must not be rewritten.
var reservedMappingScopes = map[string]bool{
	"inputs":               true,
	"custom":               true,
	"system":               true,
	"task_outputs":         true,
	"task_outputs_by_type": true,
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
	}
	if len(fields) == 0 {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, s := range fields {
		for _, m := range templatePlaceholderRegex.FindAllStringSubmatch(s, -1) {
			inner := strings.TrimSpace(m[1])
			ref := mappingDependencyRef(inner)
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}

// rewriteRefsInMappings replaces leading spec-local refs in mapping values
// with the server-assigned alias from refMap. Handles both forms:
//   - canonical "<taskRef>.<outputName>" (rewritten to
//     "task_outputs.<server-alias>.<outputName>" so the runtime resolver's
//     namespace check passes -- a bare ref head fails at execute time with
//     "Unknown variable namespace")
//   - long "task_outputs.<taskRef>.<field>" -> "task_outputs.<server-alias>.<field>"
//
// Reserved scopes (inputs, custom, system, task_outputs, task_outputs_by_type)
// are never treated as refs.
//
// Errors when a mapping value has a path-like shape whose head is neither a
// reserved scope nor a known ref. Previously this was a silent pass-through
// that produced runtime "Unknown variable namespace" failures on execute --
// for example, a task whose inputMappings referenced a downstream task that
// hadn't been created yet (typical when the spec listed tasks in non-
// topological order). composeWorkflowBody now sorts tasks topologically
// before this runs, so a remaining unknown ref is always either a typo or a
// reference to a task that simply isn't in spec.tasks.
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
							"Known refs: %s. (Reserved scopes: inputs, custom, system, task_outputs, task_outputs_by_type.)",
						k, v, ref, ref, sortedRefMapKeys(refMap))
				}
			}
		} else if dot := strings.Index(s, "."); dot > 0 {
			// Bare <ref>.<rest> -- rewrite to task_outputs.<alias>.<rest>.
			head := s[:dot]
			if !reservedMappingScopes[head] {
				if alias, found := refMap[head]; found {
					s = "task_outputs." + alias + s[dot:]
				} else if isServerAlias(head) {
					// User supplied a server-style alias directly (e.g. wired
					// into an externally-created entity). Wrap with the
					// task_outputs. namespace so the runtime resolves it.
					s = "task_outputs." + s
				} else {
					return nil, fmt.Errorf(
						"inputMappings[%q]=%q has head %q which is neither a reserved namespace "+
							"nor a known spec ref nor a server alias (slug-NNNNNN). At execute time the "+
							"runtime resolver fails with 'Unknown variable namespace: %s'. "+
							"Known refs: %s. (Reserved scopes: inputs, custom, system, task_outputs, task_outputs_by_type.)",
						k, v, head, head, sortedRefMapKeys(refMap))
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
		s = strings.ReplaceAll(s, "task_outputs."+ref+".", "task_outputs."+alias+".")
	}
	return s
}

// rewriteRefsInTemplate rewrites every {{...}} placeholder in s whose inner
// expression looks like a spec-local ref reference. The rewrite mirrors
// rewriteRefsInMappings:
//   - {{task_outputs.<ref>.<deep>}} -> {{task_outputs.<server-alias>.<deep>}}
//   - {{<ref>.<rest>}} (bare)       -> {{task_outputs.<server-alias>.<rest>}}
//   - {{<reserved-scope>...}}        -> unchanged (inputs/custom/system/...)
//   - {{<server-alias>...}}          -> wrapped with task_outputs. prefix
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
// task body has the canonical form.
func rewriteRefsInTemplate(s string, refMap map[string]string) (string, error) {
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
					"template references task_outputs.%s.* but %q is not a known spec ref. Known refs: %s. (Reserved scopes: inputs, custom, system, task_outputs, task_outputs_by_type.)",
					ref, ref, sortedRefMapKeys(refMap))
				return match
			}
			return match
		}
		// Reserved scope at head -> leave as-is.
		head := inner[:dot]
		if reservedMappingScopes[head] {
			return match
		}
		// Bare <ref>.<rest> -> rewrite.
		if alias, found := refMap[head]; found {
			return "{{task_outputs." + alias + inner[dot:] + "}}"
		}
		if isServerAlias(head) {
			return "{{task_outputs." + inner + "}}"
		}
		// Unknown head -- error so the user sees the typo before runtime
		// silently substitutes nothing.
		firstErr = fmt.Errorf(
			"template uses {{%s.<...>}} but %q is neither a reserved namespace nor a known spec ref nor a server alias. Known refs: %s. (Reserved scopes: inputs, custom, system, task_outputs, task_outputs_by_type.)",
			inner, head, sortedRefMapKeys(refMap))
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
//   - http: url, body, headers
//   - end: endConfig.outputJson
//
// The function mutates `task` in place and returns an error from the first
// failed rewrite, with the field name in the error context.
//
// New task types that introduce template fields should be added here AND in
// mappingDependencyRef -- otherwise topologicalTaskOrder won't see the
// inputMappings-style dep and a forward reference inside a template will
// fail at rewrite time instead of being ordered correctly.
func rewriteRefsInTaskTemplates(task map[string]any, refMap map[string]string) error {
	taskType, _ := task["type"].(string)
	rewriteField := func(fieldPath string, value string) (string, error) {
		out, err := rewriteRefsInTemplate(value, refMap)
		if err != nil {
			return "", fmt.Errorf("%s: %w", fieldPath, err)
		}
		return out, nil
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
		for _, nested := range []string{"scorecardConfig", "ruleTreeConfig"} {
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
		// Template-string dependencies (http url/body/headers, end
		// outputJson). A task whose http url uses {{<other-task>.field}}
		// must be ordered AFTER that task so rewriteRefsInTaskTemplates
		// resolves the ref to a server alias.
		for _, ref := range templateDependencyRefs(t) {
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

// humanizeKey turns "borrower_id" into "Borrower Id", "minScore" into "Min Score",
// etc. Used to auto-generate display titles for input variables when the spec
// doesn't supply one.
func humanizeKey(key string) string {
	if key == "" {
		return ""
	}
	// Split on _ and -
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '_' || r == '-' })
	// Also split camelCase within each part.
	var words []string
	for _, p := range parts {
		buf := []rune{}
		for i, r := range p {
			if i > 0 && r >= 'A' && r <= 'Z' {
				words = append(words, string(buf))
				buf = []rune{r}
			} else {
				buf = append(buf, r)
			}
		}
		if len(buf) > 0 {
			words = append(words, string(buf))
		}
	}
	for i, w := range words {
		if w == "" {
			continue
		}
		runes := []rune(w)
		if runes[0] >= 'a' && runes[0] <= 'z' {
			runes[0] = runes[0] - 32
		}
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

// composeWorkflowBody resolves task creates and assembles the final workflow body.
// In dryRun mode, prints what would be POSTed to /v2/tasks but does not call.
//
// Reference resolution: each task/extraNode has a spec-local "ref" (taken from
// the explicit `ref` field, falling back to `alias`/`nodeId`, falling back to a
// generated `t<idx>`). Edges and inputMappings reference tasks by ref. After
// creating a task, the server returns an alias which becomes the canonical
// nodeId + taskAlias and is used to rewrite all references downstream. If the
// caller provided `alias` in a task body, that alias is sent to the API; if
// absent, the server picks one and we use whatever it returns.
func composeWorkflowBody(c *client.Client, spec *composeSpec, dryRun bool, publish bool) (map[string]any, error) {
	if err := validateEntityTypeVsTaskTypes(spec); err != nil {
		return nil, err
	}

	// Surface the predicted workflow alias up-front. The server slugifies
	// `label` and ignores any `alias` key in the body, so callers can't
	// influence it after the fact. Credit-decisioning entities scoped via
	// --workflow-alias on create only show up in pickers when their
	// workflowAlias matches THIS slug, so tell the caller exactly what to
	// stamp before they create entities -- not after, when the workflow's
	// pickers come up empty and they have to re-stamp.
	predictedAlias := slugifyWorkflowLabel(spec.Label)
	fmt.Fprintf(os.Stderr, "# Workflow alias will be: %q (server-derived from label %q).\n", predictedAlias, spec.Label)
	fmt.Fprintf(os.Stderr, "# Stamp entities with this alias on create:\n")
	fmt.Fprintf(os.Stderr, "#   altscore evaluation-rules create --workflow-alias %s ...\n", predictedAlias)
	fmt.Fprintf(os.Stderr, "#   altscore rule-trees       create --workflow-alias %s ...\n", predictedAlias)
	fmt.Fprintf(os.Stderr, "#   altscore mapping-tables   create --workflow-alias %s ...\n", predictedAlias)
	fmt.Fprintf(os.Stderr, "#   altscore scorecards       create --workflow-alias %s ...\n", predictedAlias)

	// Pre-flight: validate every task's REQUIRED-field shape locally before
	// posting anything. The previous loop would create tasks 0..N-1 on the
	// server, then fail on N because a label/type was missing or an enum was
	// wrong. Without a tasks-v2 DELETE endpoint there's no cleanup, so we
	// surface those errors before the first HTTP call. Server-side errors
	// (e.g. "headers must be JSON-encoded string") still happen mid-loop, but
	// the cheap-and-obvious mistakes are now blocked client-side.
	if err := preflightTasks(spec); err != nil {
		return nil, err
	}

	// Auto-add `persona` to the workflow's inputVariables when any
	// customer/deal/asset task uses operation=write but the spec didn't
	// declare it. CreateBorrower's strict Literal["individual","business"]
	// validator fires at runtime on the new-borrower path; without this
	// auto-add, a workflow first-time-running for a not-yet-existing
	// borrower crashes with a Pydantic ValidationError that surfaces as
	// statusCode=500 and an opaque error message. The default value is
	// "individual" -- conservative, matches what the Hub UI fills in.
	// Caller-supplied inputVariables.persona always wins (we never override).
	for _, t := range spec.Tasks {
		tt, _ := t["type"].(string)
		if tt != "customer" && tt != "deal" && tt != "asset" {
			continue
		}
		if op, _ := t["operation"].(string); op != "write" {
			continue
		}
		if spec.InputVariables == nil {
			spec.InputVariables = map[string]any{}
		}
		if _, has := spec.InputVariables["persona"]; !has {
			spec.InputVariables["persona"] = map[string]any{
				"type":        "string",
				"default":     "individual",
				"required":    false,
				"title":       "Type of customer",
				"description": "Borrower persona at create time. Defaults to 'individual'; pass 'business' when triggering the workflow for a corporate borrower.",
			}
		}
		break // one auto-add covers all entity-write tasks in the spec
	}

	createdAliases := []string{}
	taskNodes := []map[string]any{}

	// refMap: spec-local reference -> server-assigned alias.
	refMap := map[string]string{}

	// taskByRef: spec-local reference -> task body. Captured BEFORE the task
	// loop strips `ref` so the end-node auto-wiring (run later from the
	// extraNodes loop) can look up each predecessor's task body to read its
	// type, label, and config (scorecardConfig.totalScoreVariable, etc.).
	taskByRef := map[string]map[string]any{}
	for i, t := range spec.Tasks {
		taskByRef[localRef(t, fmt.Sprintf("t%d", i))] = t
	}

	// Topologically sort task creation order so cross-task inputMappings
	// always resolve at the time we POST each task. Without this, a task
	// listed in spec.tasks BEFORE the task it references would have a bare
	// "<ref>.<output>" value persist verbatim -- the runtime resolver then
	// fails with "Unknown variable namespace". rewriteRefsInMappings now
	// errors on unresolved refs as a safety net, so this ordering is the
	// difference between "compose works regardless of spec ordering" and
	// "compose silently produces broken workflows when authors list tasks
	// in flow order rather than dependency order".
	order, err := topologicalTaskOrder(spec.Tasks, spec.Edges)
	if err != nil {
		return nil, err
	}

	for _, specIdx := range order {
		i := specIdx
		task := spec.Tasks[specIdx]
		ref := localRef(task, fmt.Sprintf("t%d", i))
		label, _ := task["label"].(string)
		taskType, _ := task["type"].(string)
		if label == "" || taskType == "" {
			return nil, fmt.Errorf("tasks[%d] (ref=%q): label and type are required", i, ref)
		}

		// Strip the spec-only `ref` field before posting; it's not part of the API.
		delete(task, "ref")

		// Rewrite inputMappings using refs resolved so far. Topological
		// ordering above guarantees every dependency is in refMap by the
		// time we get here, so an "unknown ref" error here always means a
		// typo or a reference to a task that simply isn't in spec.tasks.
		if mappings, ok := task["inputMappings"].(map[string]any); ok {
			rewritten, rerr := rewriteRefsInMappings(mappings, refMap)
			if rerr != nil {
				return nil, fmt.Errorf("tasks[%d] (ref=%q): %w", i, ref, rerr)
			}
			task["inputMappings"] = rewritten
		}
		// scorecard and rule-tree activities resolve their per-rule field
		// references from a NESTED inputMappings map (scorecardConfig and
		// ruleTreeConfig respectively -- see graph_workflow's dedicated
		// per-task-type branches in _resolve_task_variables). Compose's
		// rewrite was only walking the top-level map, leaving nested refs
		// like "task_outputs.<spec-ref>.field" untouched at the server
		// after spec-ref-to-alias translation -- so the runtime resolver
		// looked up the spec ref (which doesn't exist in task_outputs)
		// and every rule field came back None. Symptom: scorecard total=0
		// for every input, rule-tree falls through to its default branch
		// regardless of upstream values. Rewrite the nested maps too.
		for _, nested := range []string{"scorecardConfig", "ruleTreeConfig"} {
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
				return nil, fmt.Errorf("tasks[%d] (ref=%q) %s.inputMappings: %w", i, ref, nested, rerr)
			}
			cfg["inputMappings"] = rewritten
			task[nested] = cfg
		}
		// Mapping-table tasks store their per-entry input wiring as
		// mappingTableConfig.entries[].inputVariable -- NOT in inputMappings.
		// The runtime mapping_table_activity resolves each entry's input
		// against the workflow context using whatever string is in
		// inputVariable verbatim, so a spec-local ref left as
		// `task_outputs.compute.X` after compose doesn't resolve (the
		// _task_outputs key is the server alias, not the spec ref). The same
		// is true for the top-level inputMappings that
		// normalizeMappingTableTask later mirrors from the entries -- if we
		// rewrite the entries here, the mirror will pick up canonical
		// (server-aliased) values too.
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

		// Rewrite {{...}} template placeholders in known per-type fields
		// (http url/body/headers, end outputJson). Same rules as
		// inputMappings rewrite, just operating on substrings inside
		// template strings. Without this, a `{{<other-task>.field}}` on
		// an http body silently survives compose and fails at execute
		// time -- the runtime template engine substitutes nothing because
		// the bare ref isn't a valid runtime namespace.
		if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
			return nil, fmt.Errorf("tasks[%d] (ref=%q): %w", i, ref, err)
		}

		// Type-specific normalization: enrich altdata-enrichment with inputKeys
		// from source inputFields, validate conditional branches, etc.
		if err := normalizeTaskBody(c, task, &composeNormalizeOpts{
			PredictedAlias:  predictedAlias,
			CustomVariables: spec.CustomVariables,
			InputVariables:  spec.InputVariables,
			Publish:         publish,
		}, dryRun); err != nil {
			return nil, fmt.Errorf("tasks[%d] (ref=%q): %w", i, ref, err)
		}

		serverAlias, version, err := postTask(
			c, task, ref, dryRun,
			fmt.Sprintf("tasks[%d] ref=%q", i, ref),
		)
		if err != nil {
			return nil, fmt.Errorf("%w (created so far: %v)", err, createdAliases)
		}
		if !dryRun {
			createdAliases = append(createdAliases, serverAlias)
		}

		refMap[ref] = serverAlias

		// Build the corresponding graph node using the server-assigned alias.
		node := map[string]any{
			"nodeId":      serverAlias,
			"type":        taskType,
			"label":       label,
			"taskAlias":   serverAlias,
			"taskVersion": version,
			"position":    map[string]float64{"x": float64(200 * (i + 1)), "y": 0},
			"data":        map[string]any{},
		}
		if mappings, ok := task["inputMappings"]; ok {
			data := node["data"].(map[string]any)
			data["inputMappings"] = mappings
		}
		taskNodes = append(taskNodes, node)
	}

	// Compose nodes: extraNodes first (typically start/end), then task nodes.
	// EVERY node needs a backing /v2/tasks record -- the Hub creates trivial
	// tasks for start/end too. So we create one task per extraNode unless the
	// caller already supplied taskAlias.
	allNodes := []map[string]any{}
	for i, n := range spec.ExtraNodes {
		ref := localRef(n, fmt.Sprintf("n%d", i))
		nodeType, _ := n["type"].(string)
		label, _ := n["label"].(string)
		if nodeType == "" || label == "" {
			return nil, fmt.Errorf("extraNodes[%d] (ref=%q): type and label are required", i, ref)
		}

		// Strip the spec-only `ref` field; nodes use `nodeId` (canonical).
		delete(n, "ref")

		taskAlias, _ := n["taskAlias"].(string)
		taskID, _ := n["taskId"].(string)
		if taskAlias == "" && taskID == "" {
			// Auto-create a trivial backing task: just type + label.
			// For `end` nodes we also auto-wire inputSchema / inputMappings
			// from upstream tasks so the PDF report editor (and end_activity's
			// context.get() calls) have the data they expect. Without this,
			// the end task ships with empty schemas and the PDF picker shows
			// nothing -- matching the bug the user hit before manually
			// recreating the end node in the Hub UI.
			taskBody := map[string]any{
				"label": label,
				"type":  nodeType,
			}
			if strings.ToLower(nodeType) == "end" {
				inSchema, inMappings, pdfSections, _ := buildEndAutoWiring(ref, spec.Edges, taskByRef, refMap)

				// Spec extension: per-end-node htmlSections render as
				// additional PDF sections at the top of the report (before
				// the auto-wired data-source sections). Each section
				// interpolates {var} tokens against the end task's resolved
				// context at runtime; compose auto-wires the inputMappings
				// for any input or custom variable referenced in the
				// content, so safe_format finds them. Task-output references
				// (e.g. {credit_score}) need no wiring -- end_activity
				// promotes upstream task outputs to root in enriched_context.
				if rawSections := readSpecHTMLSections(n); len(rawSections) > 0 {
					built, htmlSchema, htmlMappings := buildHTMLSections(rawSections, spec.InputVariables, spec.CustomVariables)
					pdfSections = append(built, pdfSections...)
					for k, v := range htmlSchema {
						if _, exists := inSchema[k]; !exists {
							inSchema[k] = v
						}
					}
					for k, v := range htmlMappings {
						if _, exists := inMappings[k]; !exists {
							inMappings[k] = v
						}
					}
				}
				// htmlSections is spec sugar, not part of the API node shape.
				delete(n, "htmlSections")

				if len(inSchema) > 0 {
					taskBody["inputSchema"] = inSchema
				}
				if len(inMappings) > 0 {
					taskBody["inputMappings"] = inMappings
				}
				// Build endConfig from per-end-node spec input + auto-wired
				// PDF sections. Caller-supplied fields under
				// `extraNodes[].endConfig` (decisionConfig, outputJson,
				// pdfConfig title/subtitle/brandLogo, etc.) are preserved
				// verbatim; we only auto-fill the pieces compose has
				// canonical knowledge of (pdfConfig.enabled +
				// pdfConfig.sourcesConfig from upstream PDF data sources).
				//
				// pdfConfig.enabled=true is what actually flips the PDF
				// generator on. Without it, the runtime end_activity sees
				// endConfig=null and skips report rendering entirely -- the
				// failure mode the user hit on the first compose-generated
				// workflow ("PDF didn't come out"). Same for decisionConfig:
				// if the spec sets `enabled: true, decisionType: "final"`,
				// the runtime records the rule-tree decision via
				// /v1/executions/{id}/decisions; if compose silently
				// stripped that to null, decisions never persist.
				userEndCfg, _ := n["endConfig"].(map[string]any)
				if userEndCfg == nil {
					userEndCfg = map[string]any{}
				}
				userPdfCfg, _ := userEndCfg["pdfConfig"].(map[string]any)
				if userPdfCfg == nil {
					userPdfCfg = map[string]any{}
				}
				// Compose-canonical PDF defaults (overridden by user when
				// supplied via spec).
				pdfDefaults := map[string]any{
					"brandLogo":             nil,
					"enabled":               true,
					"filePrefix":            "",
					"pdfGenerationRequired": false,
					"sourcesConfig":         pdfSections,
					"title":                 "Report",
					"subtitle":              "",
				}
				for k, v := range userPdfCfg {
					pdfDefaults[k] = v
				}
				// User-supplied sourcesConfig is APPENDED after compose's
				// auto-wired sections; spec authors who want to swap order
				// should override sourcesConfig wholesale (their value
				// takes precedence above already).
				endCfgOut := map[string]any{
					"decisionConfig": userEndCfg["decisionConfig"], // may be nil; spec passes through
					"outputJson":     "",
					"pdfConfig":      pdfDefaults,
				}
				if oj, ok := userEndCfg["outputJson"].(string); ok {
					endCfgOut["outputJson"] = oj
				}
				// Carry through any extra fields the user supplied that
				// compose doesn't know about (forward-compat with future
				// endConfig additions).
				for k, v := range userEndCfg {
					if _, handled := endCfgOut[k]; handled {
						continue
					}
					if k == "pdfConfig" {
						continue
					}
					endCfgOut[k] = v
				}
				taskBody["endConfig"] = endCfgOut
				// Strip the spec-only `endConfig` key from the graph node
				// so it doesn't double-write.
				delete(n, "endConfig")

				// Rewrite spec-local refs in endConfig.outputJson so they
				// land canonical (task_outputs.<server-alias>.<deep>). The
				// rewrite for spec.tasks happens at line ~1348; extraNode
				// end tasks bypass that path because they're built here, so
				// without this call an outputJson template like
				// `{{score.total_score}}` survives unchanged and the runtime
				// resolver rejects `score` as an unknown scope.
				if err := rewriteRefsInTaskTemplates(taskBody, refMap); err != nil {
					return nil, fmt.Errorf("extraNodes[%d] (ref=%q): %w", i, ref, err)
				}
			}

			alias, version, err := postTask(
				c, taskBody, ref, dryRun,
				fmt.Sprintf("extraNodes[%d] ref=%q (extra-node backing)", i, ref),
			)
			if err != nil {
				return nil, err
			}
			taskAlias = alias
			n["taskAlias"] = alias
			n["taskVersion"] = version
			if !dryRun {
				createdAliases = append(createdAliases, alias)
			}
		}

		// nodeId follows the server-assigned task alias (1:1) so edges and
		// downstream references resolve cleanly.
		if taskAlias != "" {
			n["nodeId"] = taskAlias
			refMap[ref] = taskAlias
		} else {
			// taskId-only path (rare): keep whatever nodeId was passed.
			if id, _ := n["nodeId"].(string); id == "" {
				n["nodeId"] = ref
			}
			refMap[ref] = fmt.Sprint(n["nodeId"])
		}

		// Auto-position if missing: spread along the same row
		if _, hasPos := n["position"]; !hasPos {
			x := float64(0)
			if strings.ToLower(nodeType) == "end" {
				x = float64(200 * (len(spec.Tasks) + 1))
			}
			n["position"] = map[string]float64{"x": x, "y": 0}
		}
		if _, hasData := n["data"]; !hasData {
			n["data"] = map[string]any{}
		}
		// Mirror Hub: end nodes carry isEndNode + inputMappings on node.data
		// in addition to their backing task body. Task body powers the runtime
		// (end_activity.py reads from there); node.data powers the canvas
		// renderer and the PDF editor's section picker.
		if strings.ToLower(nodeType) == "end" {
			data, _ := n["data"].(map[string]any)
			if data == nil {
				data = map[string]any{}
			}
			data["isEndNode"] = true
			if _, hasMappings := data["inputMappings"]; !hasMappings {
				if _, inMappings, _, _ := buildEndAutoWiring(ref, spec.Edges, taskByRef, refMap); len(inMappings) > 0 {
					data["inputMappings"] = inMappings
				}
			}
			n["data"] = data
		}
		allNodes = append(allNodes, n)
	}
	for _, n := range taskNodes {
		allNodes = append(allNodes, n)
	}

	// Edges: support `from`/`to` shortcuts and resolve refs via refMap.
	allEdges := []map[string]any{}
	for i, e := range spec.Edges {
		src, _ := e["sourceNodeId"].(string)
		if src == "" {
			if v, _ := e["from"].(string); v != "" {
				src = v
			}
		}
		tgt, _ := e["targetNodeId"].(string)
		if tgt == "" {
			if v, _ := e["to"].(string); v != "" {
				tgt = v
			}
		}
		if src == "" || tgt == "" {
			return nil, fmt.Errorf("edges[%d]: source/target required (use sourceNodeId+targetNodeId or from+to)", i)
		}
		if alias, ok := refMap[src]; ok {
			src = alias
		}
		if alias, ok := refMap[tgt]; ok {
			tgt = alias
		}
		delete(e, "from")
		delete(e, "to")
		e["sourceNodeId"] = src
		e["targetNodeId"] = tgt
		if _, hasID := e["id"]; !hasID {
			e["id"] = fmt.Sprintf("%s->%s", src, tgt)
		}
		allEdges = append(allEdges, e)
	}

	status := spec.Status
	if status == "" {
		status = "DRAFT"
	}
	inputVars := spec.InputVariables
	if inputVars == nil {
		inputVars = map[string]any{}
	}
	// Hub-created input variables include name + title + description so the
	// builder can render labeled form fields. Auto-fill these from the dict
	// key if the caller didn't supply them or supplied empty strings.
	// `label` (intuitive) maps to `title` (canonical) for caller convenience.
	for key, raw := range inputVars {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := v["name"].(string); name == "" {
			v["name"] = key
		}
		// Allow callers to provide either `title` or `label`; either an absent
		// or empty value triggers the auto-humanized fallback.
		title, _ := v["title"].(string)
		if title == "" {
			if l, _ := v["label"].(string); l != "" {
				title = l
			}
		}
		delete(v, "label")
		if title == "" {
			title = humanizeKey(key)
		}
		v["title"] = title
		if _, has := v["description"]; !has {
			v["description"] = ""
		}
		inputVars[key] = v
	}
	customVars := spec.CustomVariables
	if customVars == nil {
		customVars = map[string]any{}
	}
	// Normalize customVariable expression shape so the runtime
	// compute-variables wrapper produces a non-None value for the common
	// authoring mistake of `expression: "640"` (bare literal). The wrapper
	// at borrower-central/app/temporal/activities/compute_variables_activity.py
	// is `def execute(...): inputs = input; <expression>; return <returnValue
	// or "None">`. With a bare literal expression and no returnValue, the
	// statement is evaluated and discarded, then None is returned.
	//
	// Three resolution paths:
	//   1. expression contains `result =` AND returnValue=="result"  -> shape OK
	//   2. expression has no assignment AND no returnValue           -> set returnValue
	//      to the expression itself so the wrapper's `return <returnValue>`
	//      evaluates the literal/expression directly. Idempotent under
	//      re-compose because the second pass sees a returnValue and skips.
	//   3. variable defined with just `{type, default}` (no expression)
	//      -> nothing to do; BC's graph_workflow seeds custom_variables[<name>]
	//      from `default` at workflow init.
	for name, raw := range customVars {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		expression, _ := v["expression"].(string)
		returnValue, _ := v["returnValue"].(string)
		if expression == "" || returnValue != "" {
			// nothing to mirror, fall through to the ref-rewrite step below
		} else if !strings.Contains(expression, "=") {
			// Bare-literal / single-expression detection: no top-level
			// assignment statement. Match the simplest case (no `=` at all).
			// If the expression has multi-statement bodies the author should
			// declare returnValue explicitly.
			v["returnValue"] = expression
		}

		// Rewrite spec-ref prefixes in customVariable strings so the runtime
		// compute_variables_activity can resolve them against _task_outputs.
		// The dependency list is the lookup key against _task_outputs (see
		// borrower-central/app/temporal/activities/compute_variables_activity.py
		// :_collect_dependencies), AND the same string is used as the dict
		// key into `inputs` inside the expression -- so both must agree on
		// the alias. Symptom when this rewrite is missing: every dependency
		// resolves to None, the compute function falls back to whatever
		// default it has (often 0 or a sentinel like -999999), and every
		// downstream score / decision collapses. Tracked through the v10
		// stress run (Argentine SMB workflow with refs like
		// "task_outputs.enrich.ARG-PUB-0001..." that compose persisted
		// without rewriting `enrich` to the server alias).
		expression, _ = v["expression"].(string)
		returnValue, _ = v["returnValue"].(string)
		if expression != "" {
			v["expression"] = rewriteTaskOutputsRefsInString(expression, refMap)
		}
		if returnValue != "" {
			v["returnValue"] = rewriteTaskOutputsRefsInString(returnValue, refMap)
		}
		if deps, ok := v["dependencies"].([]any); ok {
			for i, d := range deps {
				if ds, ok := d.(string); ok {
					deps[i] = rewriteTaskOutputsRefsInString(ds, refMap)
				}
			}
			v["dependencies"] = deps
		}

		customVars[name] = v
	}
	wf := map[string]any{
		"label":           spec.Label,
		"category":        spec.Category,
		"status":          status,
		"inputVariables":  inputVars,
		"customVariables": customVars,
		"nodes":           allNodes,
		"edges":           allEdges,
	}
	if spec.Description != "" {
		wf["description"] = spec.Description
	}
	if spec.Config != nil {
		wf["config"] = spec.Config
	}
	if spec.Notes != nil {
		wf["notes"] = spec.Notes
	}
	return wf, nil
}

// customerHiddenTypes / dealHiddenTypes mirror the Hub's palette filter at
// altscore-ai-chat/components/workflow-builder-v2/canvas/ComponentsMenu.tsx
// (CUSTOMER_HIDDEN_TYPES / DEAL_HIDDEN_TYPES). A workflow whose
// config.entityType is "customer" cannot include these task types in the
// Hub editor, so a CLI-composed workflow that uses them would render as a
// non-editable graph -- catch it at compose time instead.
var customerHiddenTypes = map[string]bool{
	"deal":         true,
	"asset":        true,
	"array-router": true,
}
var dealHiddenTypes = map[string]bool{
	"customer":         true,
	"list-of-similars": true,
}

// validTaskTypes mirrors the backend TaskType enum at
// borrower-central/app/model/workflows_v2/task.py. Sourced once and kept in
// sync as the enum evolves. Used by preflight to reject typos like
// "data-store" (correct: data-store-write/data-store-query) BEFORE creating
// any /v2/tasks rows. Without this check, a typo would orphan all earlier
// tasks in the compose loop (no rollback path exists today).
var validTaskTypes = map[string]bool{
	"http": true, "conditional": true, "comment": true,
	"start": true, "end": true, "wait": true, "webhook": true,
	"create-borrower": true, "update-borrower": true, "update-borrower-name": true,
	"evaluate-rules": true, "altdata-enrichment": true, "create-identity": true,
	"fetch-entity": true, "html-template": true, "fetch-borrower-entities": true,
	"child-workflow": true, "exception": true, "soap": true,
	"mapping-table": true, "scorecard": true, "rule-tree": true,
	"compute-variables": true, "data-store-write": true, "data-store-query": true,
	"customer": true, "deal": true, "credit-line": true,
	"list-of-similars": true, "array-router": true, "asset": true,
}

// preflightTasks runs cheap, local-only validation across every task in the
// spec before composeWorkflowBody starts POSTing. Catches the mistakes that
// would otherwise create orphan /v2/tasks rows mid-loop. Without a tasks-v2
// DELETE endpoint, partial-failure cleanup is impossible; everything we can
// catch locally we MUST.
//
// Checks (in order, fail-fast):
//   1. duplicate spec-local refs / explicit aliases
//   2. label + type present
//   3. type is in the backend TaskType enum (with closest-match suggestion)
//   4. http: headers must be a JSON-encoded string
//   5. data-store-write / data-store-query / webhook / comment / exception /
//      child-workflow: per-type required fields
//   6. validateTaskV2Body: type-specific structural checks (conditional
//      branches, scorecard reference, mapping-table entries, rule-tree
//      enums)
//   7. inputMappings values: leading segment must be a runtime namespace OR
//      a known spec-local ref; task_outputs.<X>.<rest> validates <X> too.
//      {{...}} template syntax is skipped (handled by template engine).
//   8. edge endpoints (from/to) must reference a known ref.
//   9. duplicate edges and self-loops are rejected.
func preflightTasks(spec *composeSpec) error {
	// Spec-level checks: workflow category + inputVariables shape. These
	// fail with opaque backend errors otherwise; surface here.
	if spec.Category != "" && !validWorkflowCategories[strings.ToUpper(spec.Category)] {
		return fmt.Errorf(
			"workflow.category=%q is not a valid value. Valid: ACTION, EVALUATION, CONTACT, OTHER. "+
				"Note: CUSTOMER and DEAL are workflow ENTITY TYPES (config.entityType), not categories.",
			spec.Category,
		)
	}
	for name, def := range spec.InputVariables {
		dm, _ := def.(map[string]any)
		if dm == nil {
			continue
		}
		t, _ := dm["type"].(string)
		if t == "" {
			continue
		}
		if !validInputSchemaTypes[t] {
			return fmt.Errorf(
				"workflow.inputVariables.%s.type=%q is not a valid type. "+
					"Valid: string, integer, number, boolean, object, array.",
				name, t,
			)
		}
	}

	// Collect every spec-local ref upfront so we can validate forward
	// references in inputMappings AND detect duplicates that would
	// otherwise silently orphan tasks (the rewriter only records the
	// last ref-to-alias mapping).
	knownRefs := map[string]bool{}
	knownAliases := map[string]bool{}
	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		// 'ref' becomes the server-assigned alias prefix (and thus the
		// nodeId) when no explicit 'alias' is set, so the same URL-safety
		// constraint applies. Reject upper-case / spaces / punctuation
		// upfront -- otherwise it propagates to nodeIds that misbehave in
		// downstream cmds (set-mapping --node-id, lock acquire by alias).
		if !validAliasPattern.MatchString(ref) {
			return fmt.Errorf(
				"tasks[%d]: ref %q has invalid characters. Refs become server-assigned aliases (and nodeIds), "+
					"so must be lowercase alphanumeric with internal dashes only "+
					"(regex: ^[a-z0-9][a-z0-9-]*$). Don't use spaces, slashes, uppercase, or punctuation.",
				i, ref,
			)
		}
		if knownRefs[ref] {
			return fmt.Errorf(
				"tasks[%d]: duplicate ref %q -- two tasks share the same spec-local key. "+
					"Compose's edge rewriter only records the LAST ref-to-alias mapping, so the earlier "+
					"task ends up with no incident edges (silent orphan). Give each task a unique 'ref'.",
				i, ref,
			)
		}
		knownRefs[ref] = true
		if alias, _ := task["alias"].(string); alias != "" {
			if !validAliasPattern.MatchString(alias) {
				return fmt.Errorf(
					"tasks[%d] (ref=%q): alias %q has invalid characters. Aliases end up in URL paths "+
						"so must be lowercase alphanumeric with internal dashes only "+
						"(regex: ^[a-z0-9][a-z0-9-]*$). "+
						"Don't use spaces, slashes, uppercase, or punctuation.",
					i, ref, alias,
				)
			}
			if knownAliases[alias] {
				return fmt.Errorf(
					"tasks[%d] (ref=%q): duplicate explicit alias %q -- two tasks declare the same alias. "+
						"The second create either version-bumps the first or 409s. "+
						"Either drop the alias on one (compose will pick a unique one) or pick distinct aliases.",
					i, ref, alias,
				)
			}
			knownAliases[alias] = true
		}
	}
	startCount, endCount := 0, 0
	for i, node := range spec.ExtraNodes {
		ref := localRef(node, fmt.Sprintf("n%d", i))
		if knownRefs[ref] {
			return fmt.Errorf(
				"extraNodes[%d]: duplicate ref %q -- collides with a task or another extraNode. "+
					"Give each node a unique 'ref'.",
				i, ref,
			)
		}
		knownRefs[ref] = true
		// extraNodes is for trivial type-only graph nodes (start/end). Other
		// types belong in 'tasks' where they get full validation; putting
		// them in extraNodes 400s at the backing-task POST with an opaque
		// "field required" because compose synthesizes a minimal body.
		nodeType, _ := node["type"].(string)
		switch nodeType {
		case "start":
			startCount++
		case "end":
			endCount++
		default:
			return fmt.Errorf(
				"extraNodes[%d] (ref=%q): type=%q is not allowed in extraNodes. "+
					"Only 'start' and 'end' belong here -- those are trivial type-only nodes that compose "+
					"synthesizes a backing task for. Move %q to 'tasks' instead so its required config "+
					"fields can be supplied (compose will reject the task body if anything's missing).",
				i, ref, nodeType, nodeType,
			)
		}
	}
	if startCount == 0 {
		return fmt.Errorf(
			"compose spec has no 'start' node in extraNodes. Every workflow needs exactly one " +
				"start node; the engine doesn't know where to begin without it. Add: " +
				`{"ref": "start", "type": "start", "label": "Start"} to extraNodes.`,
		)
	}
	if startCount > 1 {
		return fmt.Errorf(
			"compose spec has %d 'start' nodes in extraNodes. Every workflow needs exactly ONE "+
				"start; multiple starts make the engine's traversal non-deterministic. Drop the extras.",
			startCount,
		)
	}
	if endCount == 0 {
		// 'end' is conventional but not strictly required; warn-only via
		// stderr, never block. Keep this open for niche use cases (e.g.
		// workflows that terminate via 'exception' branches).
		fmt.Fprintln(os.Stderr,
			"# warning: compose spec has no 'end' node in extraNodes. Most workflows need one for the engine to know where to terminate cleanly.")
	}

	// Soft advisory: routing tasks (conditional, array-router) with branch
	// edges targeting exception tasks usually indicate the agent is treating
	// 'rejected'/'declined'/'manual review' as a failure when they're really
	// valid workflow outcomes that belong on end nodes (one per branch,
	// each with its own endConfig.decisionConfig). See the
	// 'terminationPatterns' schema-guide section. We don't block -- some
	// workflows legitimately fail-fast on a bad-input conditional branch --
	// but we want the agent to see this advisory at compose time, not
	// discover it after deploying a workflow whose executions all show up
	// as failures in metrics.
	refType := map[string]string{}
	for _, t := range spec.Tasks {
		if r := localRef(t, ""); r != "" {
			if tt, _ := t["type"].(string); tt != "" {
				refType[r] = tt
			}
		}
	}
	for _, n := range spec.ExtraNodes {
		if r := localRef(n, ""); r != "" {
			if tt, _ := n["type"].(string); tt != "" {
				refType[r] = tt
			}
		}
	}
	type advise struct{ srcRef, tgtRef, srcType string }
	advisories := []advise{}
	for _, e := range spec.Edges {
		// Mirror the canonical edge normalizer (assembleWorkflowBody at the
		// bottom of this file): specs may use `from`/`to` as shortcuts for
		// `sourceNodeId`/`targetNodeId`. The advisory runs in the preflight
		// pass BEFORE normalization, so without this fallback every edge
		// authored with the documented shortcut form is invisible and the
		// advisory finds nothing.
		src, _ := e["sourceNodeId"].(string)
		if src == "" {
			src, _ = e["from"].(string)
		}
		tgt, _ := e["targetNodeId"].(string)
		if tgt == "" {
			tgt, _ = e["to"].(string)
		}
		if src == "" || tgt == "" {
			continue
		}
		st := refType[src]
		tt := refType[tgt]
		if (st == "conditional" || st == "array-router") && tt == "exception" {
			advisories = append(advisories, advise{src, tgt, st})
		}
	}
	if len(advisories) > 0 {
		fmt.Fprintf(os.Stderr,
			"# advice: spec has %d branch edge(s) from a conditional/array-router targeting an exception task.\n"+
				"# advice: exception tasks fail the workflow (isSuccess=false). For VALID decision outcomes\n"+
				"# advice: like 'reject', 'manual_review', 'declined' -- which are expected business results,\n"+
				"# advice: not errors -- prefer a separate end node per branch, each with its own\n"+
				"# advice: endConfig.decisionConfig.enabled=true so the decision is recorded via\n"+
				"# advice: /v1/executions/{id}/decisions. See 'workflows-v2 schema-guide terminationPatterns'.\n"+
				"# advice: Reserve exception tasks for genuine error paths (missing required input, upstream\n"+
				"# advice: HTTP 5xx, unrecoverable state) where the workflow truly could not complete.\n",
			len(advisories))
		for _, a := range advisories {
			fmt.Fprintf(os.Stderr, "# advice:   %s (%s) -> %s (exception)\n", a.srcRef, a.srcType, a.tgtRef)
		}
	}

	// Edge topology: every from/to must reference a known ref; reject
	// duplicate edges and self-loops (almost always bugs). Also reject
	// unknown edge keys -- the most common is 'branchName' (a natural-
	// feeling but unsupported alias for 'sourceHandle' that disappears
	// silently and leaves conditional outgoing edges with sourceHandle:
	// null, breaking the conditional at runtime).
	seenEdges := map[string]bool{}
	for i, edge := range spec.Edges {
		for k := range edge {
			if !validEdgeKeys[k] {
				hint := ""
				if k == "branchName" || k == "branch_name" || k == "branch" {
					hint = " (use 'sourceHandle' instead -- conditional branches are wired by branch_<idx> or 'branch-else')"
				} else if k == "fromHandle" {
					hint = " (did you mean 'sourceHandle'?)"
				} else if k == "toHandle" {
					hint = " (did you mean 'targetHandle'?)"
				}
				return fmt.Errorf("edges[%d]: unknown key %q%s. Valid keys: from, to, sourceNodeId, targetNodeId, sourceHandle, targetHandle, label, id.", i, k, hint)
			}
		}
		from, _ := edge["from"].(string)
		to, _ := edge["to"].(string)
		// Some specs use sourceNodeId/targetNodeId directly with explicit
		// aliases; if those are present and from/to are absent, fall back.
		if from == "" {
			from, _ = edge["sourceNodeId"].(string)
		}
		if to == "" {
			to, _ = edge["targetNodeId"].(string)
		}
		if from == "" || to == "" {
			return fmt.Errorf("edges[%d]: missing 'from'/'to' (or sourceNodeId/targetNodeId)", i)
		}
		// Refs are validated only when they look spec-local (no '-NNNNNN' suffix);
		// explicit-alias edges may target server-style aliases not in knownRefs.
		if !isServerAlias(from) && !knownRefs[from] {
			return fmt.Errorf(
				"edges[%d]: 'from'=%q is not a known ref. Known refs: %s.",
				i, from, strings.Join(sortedKeys(knownRefs), ", "),
			)
		}
		if !isServerAlias(to) && !knownRefs[to] {
			return fmt.Errorf(
				"edges[%d]: 'to'=%q is not a known ref. Known refs: %s.",
				i, to, strings.Join(sortedKeys(knownRefs), ", "),
			)
		}
		if from == to {
			return fmt.Errorf(
				"edges[%d]: self-loop on %q -- a node can't be its own source AND target. "+
					"Almost always a copy-paste bug; if it's intentional, build the cycle through an intermediate node.",
				i, from,
			)
		}
		handle, _ := edge["sourceHandle"].(string)
		key := from + "|" + handle + "->" + to
		if seenEdges[key] {
			return fmt.Errorf(
				"edges[%d]: duplicate edge %s->%s (same sourceHandle %q). "+
					"Drop the duplicate; the workflow graph already has it.",
				i, from, to, handle,
			)
		}
		seenEdges[key] = true
	}

	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		label, _ := task["label"].(string)
		taskType, _ := task["type"].(string)
		if label == "" || taskType == "" {
			return fmt.Errorf("tasks[%d] (ref=%q): label and type are required (validated before any POST)", i, ref)
		}
		if !validTaskTypes[taskType] {
			suggestion := closestTaskType(taskType)
			suggestionLine := ""
			if suggestion != "" {
				suggestionLine = fmt.Sprintf("Did you mean %q? ", suggestion)
			}
			return fmt.Errorf(
				"tasks[%d] (ref=%q): unknown task type %q. %s"+
					"The backend TaskType enum accepts %d values (active + deprecated combined). "+
					"For new workflows use the active palette (20 types) -- run "+
					"'altscore workflows-v2 schema-guide tasks | jq \".tasks.perType | keys\"' to list them, "+
					"and '.tasks.deprecatedTypes | keys' for the legacy ones. "+
					"Common deprecations: 'data-store' is split into 'data-store-write'/'data-store-query'; "+
					"'pdf-report' is now part of the 'end' task's endConfig.",
				i, ref, taskType, suggestionLine, len(validTaskTypes),
			)
		}

		// Per-type required-field checks. These cover the orphan-task class
		// of bug seen in iter-3 smoke tests.
		switch taskType {
		case "http":
			if h, present := task["headers"]; present {
				if _, ok := h.(string); !ok {
					return fmt.Errorf(
						"tasks[%d] (ref=%q): http task 'headers' must be a JSON-encoded string, "+
							"not an inline object. Wrap it: \"headers\": \"{\\\"Content-Type\\\":\\\"application/json\\\"}\". "+
							"The runtime fails with an opaque 'str type expected' error otherwise.",
						i, ref,
					)
				}
			}
			if u, _ := task["url"].(string); u == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): http task requires 'url'", i, ref)
			}
		case "webhook":
			if u, _ := task["url"].(string); u == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): webhook task requires 'url'", i, ref)
			}
			if s, _ := task["secret"].(string); s == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): webhook task requires 'secret'", i, ref)
			}
		case "comment":
			if c, _ := task["comment"].(string); c == "" {
				return fmt.Errorf(
					"tasks[%d] (ref=%q): comment task requires a non-empty 'comment' field "+
						"(the canvas annotation body, distinct from 'label' which is the node header).",
					i, ref,
				)
			}
		case "data-store-write":
			cfg := asMap(task["dataStoreWriteConfig"])
			if t, _ := cfg["tableName"].(string); t == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): data-store-write task requires dataStoreWriteConfig.tableName", i, ref)
			}
		case "data-store-query":
			cfg := asMap(task["dataStoreQueryConfig"])
			if t, _ := cfg["tableName"].(string); t == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): data-store-query task requires dataStoreQueryConfig.tableName", i, ref)
			}
		case "exception":
			// Canonical wire name is 'errorMessage'. Both the BC API
			// schema (CreateTaskV2 / CreateTaskVersionV2 in
			// app/model/workflows_v2/task_schemas.py) and the runtime
			// activity (exception_activity.py) read it. Old specs that
			// ship 'message' are promoted to 'errorMessage' server-side
			// by _promote_legacy_exception_message, so the CLI normalizes
			// outgoing bodies to errorMessage-only without losing legacy
			// specs. Strip the legacy key so the body is unambiguous.
			em, _ := task["errorMessage"].(string)
			m, _ := task["message"].(string)
			if em == "" && m == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): exception task requires 'errorMessage' -- the failure message surfaced when this branch fires", i, ref)
			}
			if em == "" {
				task["errorMessage"] = m
			}
			delete(task, "message")
		case "child-workflow":
			eid, _ := task["executorId"].(string)
			eal, _ := task["executorAlias"].(string)
			if eid == "" && eal == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): child-workflow task requires 'executorId' or 'executorAlias'", i, ref)
			}
			// child-workflow auto-detects single vs batch from the resolved
			// type of inputExpression (list -> fan-out, dict -> single). The
			// legacy runInBatch flag and the hardcoded `input_items` context
			// key are no longer read by the runtime. Warn so old specs that
			// relied on the flag get migrated to inputExpression instead of
			// silently downgrading to a single execution.
			if rib, _ := task["runInBatch"].(bool); rib {
				if _, hasExpr := task["inputExpression"].(string); !hasExpr {
					fmt.Fprintf(os.Stderr,
						"# warning: tasks[%d] (ref=%q): child-workflow has runInBatch=true but no inputExpression. "+
							"The runtime ignores runInBatch and dispatches by the type of inputExpression "+
							"(list -> batch, dict -> single). Without inputExpression this task runs once with the full parent context. "+
							"To batch, set inputExpression to an expression that resolves to a list "+
							"(e.g. \"task_outputs.fetch.rows\").\n",
						i, ref)
				}
			}
			if fp, _ := task["failurePolicy"].(string); fp != "" && fp != "fail-fast" && fp != "best-effort" {
				return fmt.Errorf(
					"tasks[%d] (ref=%q): child-workflow failurePolicy=%q is invalid. Must be \"fail-fast\" or \"best-effort\" (default: \"best-effort\")",
					i, ref, fp)
			}
		case "compute-variables":
			// compute-variables expressions live in the customVariables DSL
			// scope; the runtime evaluates them against the workflow's
			// customVariables dict, NOT the task's inputMappings. Wiring
			// inputs via inputMappings on this task type is a no-op:
			// inputMappings populates task_outputs.<this-task>.<field>
			// AFTER the expression runs (which is itself silently
			// useless since the DSL doesn't see them).
			if im, _ := task["inputMappings"].(map[string]any); len(im) > 0 {
				fmt.Fprintf(os.Stderr,
					"# warning: tasks[%d] (ref=%q): compute-variables has inputMappings but those are NOT visible to the expression DSL. "+
						"The DSL evaluates against the workflow's customVariables dict; reference values via the customVariable names directly. "+
						"Wire upstream values into customVariables via the workflow body, not into this task's inputMappings.\n",
					i, ref)
			}
		case "array-router":
			// The runtime activity reads the source array via
			// inputMappings.source_array, NOT the top-level
			// sourceArrayPath field documented on the task body. Mirror
			// the value into inputMappings (and inputSchema) at
			// serialize time so the schema-guide contract still works
			// without forcing every spec to declare the wiring twice.
			if sap, _ := task["sourceArrayPath"].(string); sap != "" {
				im := asMap(task["inputMappings"])
				if _, exists := im["source_array"]; !exists {
					im["source_array"] = sap
					task["inputMappings"] = im
				}
				is := asMap(task["inputSchema"])
				if _, exists := is["source_array"]; !exists {
					is["source_array"] = map[string]any{"type": "array"}
					task["inputSchema"] = is
				}
			}
		case "customer", "deal", "asset":
			// sourcesConfig entries control which fields are written/read.
			// Each entry needs at minimum a 'key' AND a 'type' (the
			// data-model type, not the schema type) -- the runtime
			// activity 'Fetch Customer fail 'type'' on missing fields.
			sources := asSlice(task["sourcesConfig"])
			for sci, sc := range sources {
				sm, ok := sc.(map[string]any)
				if !ok {
					return fmt.Errorf("tasks[%d] (ref=%q): %s sourcesConfig[%d] must be an object", i, ref, taskType, sci)
				}
				if k, _ := sm["key"].(string); k == "" {
					return fmt.Errorf("tasks[%d] (ref=%q): %s sourcesConfig[%d] missing 'key'", i, ref, taskType, sci)
				}
				if t, _ := sm["type"].(string); t == "" {
					return fmt.Errorf(
						"tasks[%d] (ref=%q): %s sourcesConfig[%d] missing 'type'. "+
							"The runtime activity needs the data-model type per entry "+
							"(e.g. 'identity_key', 'borrower_field') -- a missing 'type' "+
							"surfaces at runtime as an opaque KeyError.",
						i, ref, taskType, sci,
					)
				}
			}
		}

		// Reuse the type-specific structural validator (conditional
		// branches, scorecard reference model, mapping-table entries,
		// rule-tree enums).
		body, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("tasks[%d] (ref=%q): cannot encode for preflight: %w", i, ref, err)
		}
		// Structural-only: the altdata-enrichment empty-inputKeys check
		// belongs in validateTaskV2Body (used by manual tasks-v2 create),
		// not here. Compose's normalize step fills inputKeys from each
		// source's inputFields automatically; rejecting the spec at preflight
		// would block work that compose can fix on its own.
		if err := validateTaskV2BodyStructural(json.RawMessage(body)); err != nil {
			return fmt.Errorf("tasks[%d] (ref=%q): %w", i, ref, err)
		}

		// inputSchema.<field>.type must be in the JSON-Schema-style enum
		// the runtime accepts. Backend rejects unknown values with a
		// misleading "permitted: 'array'" message; surface the full
		// enum here so the agent picks the right value.
		if is, ok := task["inputSchema"].(map[string]any); ok {
			for fname, fdef := range is {
				fm, _ := fdef.(map[string]any)
				if fm == nil {
					continue
				}
				t, _ := fm["type"].(string)
				if t == "" {
					continue
				}
				if !validInputSchemaTypes[t] {
					return fmt.Errorf(
						"tasks[%d] (ref=%q): inputSchema.%s.type=%q is not a valid type. "+
							"Valid: string, integer, number, boolean, object, array.",
						i, ref, fname, t,
					)
				}
			}
		}

		// Conditional task: every branch's condition.field must exist in
		// inputSchema, otherwise the branch silently never matches at
		// runtime. Also: inputMappings keys must match inputSchema keys --
		// stray keys are wired to nothing.
		if taskType == "conditional" {
			schemaFields := map[string]bool{}
			if is, ok := task["inputSchema"].(map[string]any); ok {
				for k := range is {
					schemaFields[k] = true
				}
			}
			if im, ok := task["inputMappings"].(map[string]any); ok {
				strays := []string{}
				for k := range im {
					if !schemaFields[k] {
						strays = append(strays, k)
					}
				}
				if len(strays) > 0 {
					sort.Strings(strays)
					return fmt.Errorf(
						"tasks[%d] (ref=%q): conditional inputMappings has key(s) not in inputSchema: %s. "+
							"Every inputMappings key must match an inputSchema key (the schema declares the type, the mapping wires the value). "+
							"Add to inputSchema or remove from inputMappings.",
						i, ref, strings.Join(strays, ", "),
					)
				}
			}
			branches := asSlice(task["branches"])
			for bi, b := range branches {
				bm, _ := b.(map[string]any)
				if bm == nil {
					continue
				}
				if missing := unknownConditionFields(asMap(bm["conditions"]), schemaFields); len(missing) > 0 {
					return fmt.Errorf(
						"tasks[%d] (ref=%q): conditional branch[%d] references field(s) not declared in inputSchema: %s. "+
							"Add them to inputSchema (with type + inputMappings) or fix the typo -- otherwise the branch silently never matches at runtime.",
						i, ref, bi, strings.Join(missing, ", "),
					)
				}
			}
		}

		// Mapping namespace check: every inputMappings value with a
		// dotted path must lead with a valid runtime namespace OR a
		// known spec-local ref. {{...}} template syntax bypasses the
		// dotted-path resolver, so skip it. For 'task_outputs.<X>.<rest>'
		// also validate <X> is known -- typos like 'task_outputs.producre.v'
		// pass the leading-segment check but break at runtime.
		if im, ok := task["inputMappings"].(map[string]any); ok {
			for k, v := range im {
				s, _ := v.(string)
				if s == "" {
					continue
				}
				if strings.HasPrefix(strings.TrimSpace(s), "{{") {
					continue // template-engine syntax, not a dotted path
				}
				dot := strings.Index(s, ".")
				if dot <= 0 {
					continue
				}
				head := s[:dot]
				if !reservedMappingScopes[head] && !knownRefs[head] {
					return fmt.Errorf(
						"tasks[%d] (ref=%q): inputMappings[%q]=%q has unknown leading segment %q. "+
							"Valid namespaces: inputs, custom, system, task_outputs, task_outputs_by_type. "+
							"Or use a spec-local ref (one of: %s) which compose rewrites to task_outputs.<alias>. "+
							"Without one of these the runtime resolver fails with 'Unknown variable namespace' at execution.",
						i, ref, k, s, head, strings.Join(sortedKeys(knownRefs), ", "),
					)
				}
				if head == "task_outputs" {
					rest := s[dot+1:]
					dot2 := strings.Index(rest, ".")
					if dot2 <= 0 {
						continue
					}
					middle := rest[:dot2]
					if knownRefs[middle] || isServerAlias(middle) {
						continue
					}
					return fmt.Errorf(
						"tasks[%d] (ref=%q): inputMappings[%q]=%q references task_outputs.%s.* but %q "+
							"is not a known spec-local ref and doesn't look like a server-assigned alias "+
							"(slug-NNNNNN). Known refs: %s. Likely a typo of one of those.",
						i, ref, k, s, middle, middle,
						strings.Join(sortedKeys(knownRefs), ", "),
					)
				}
			}
		}
	}

	// Orphan task detection: every task ref must appear as an edge
	// endpoint at least once. Tasks unwired from the graph never run
	// (lint flags them post-publish, but compose should catch earlier
	// so the user doesn't waste a publish round-trip).
	connected := map[string]bool{}
	for _, edge := range spec.Edges {
		if from, _ := edge["from"].(string); from != "" {
			connected[from] = true
		} else if from, _ := edge["sourceNodeId"].(string); from != "" {
			connected[from] = true
		}
		if to, _ := edge["to"].(string); to != "" {
			connected[to] = true
		} else if to, _ := edge["targetNodeId"].(string); to != "" {
			connected[to] = true
		}
	}
	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		taskType, _ := task["type"].(string)
		// Comments are decorative -- they're allowed to be orphaned.
		if taskType == "comment" {
			continue
		}
		if !connected[ref] {
			return fmt.Errorf(
				"tasks[%d] (ref=%q, type=%q) has no incident edges -- it's unreachable. "+
					"Add at least one 'edges' entry connecting %q to the rest of the graph, "+
					"or remove the task. (Comment tasks are allowed to be orphaned; everything else isn't.)",
				i, ref, taskType, ref,
			)
		}
	}

	// DAG topology check: each task's inputMappings can only read from
	// task_outputs.<X> where X is a transitive ancestor in the edge graph.
	// Otherwise the value isn't produced when the consumer runs and
	// surfaces as a runtime KeyError. Built once across the whole spec.
	ancestors := buildAncestors(spec)
	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		im, _ := task["inputMappings"].(map[string]any)
		if im == nil {
			continue
		}
		for k, v := range im {
			s, _ := v.(string)
			if s == "" || strings.HasPrefix(strings.TrimSpace(s), "{{") {
				continue
			}
			head, middle := mappingHeadAndMiddle(s)
			if head != "task_outputs" || middle == "" {
				continue
			}
			// Only validate when the middle segment is a known spec-local
			// ref (server-style aliases reference workflows we don't have
			// the topology for).
			if !knownRefs[middle] || isServerAlias(middle) {
				continue
			}
			if middle == ref {
				return fmt.Errorf(
					"tasks[%d] (ref=%q): inputMappings[%q]=%q references its own output -- "+
						"a task cannot consume its own task_outputs.<self>. Did you mean a different ref?",
					i, ref, k, s,
				)
			}
			if !ancestors[ref][middle] {
				return fmt.Errorf(
					"tasks[%d] (ref=%q): inputMappings[%q]=%q references task_outputs.%s.* but "+
						"%q is not an ancestor of %q in the edge graph (it doesn't run before this task). "+
						"At runtime task_outputs.%s won't exist yet -- add an edge from %q to %q (directly or transitively) or remove the mapping.",
					i, ref, k, s, middle, middle, ref, middle, middle, ref,
				)
			}
		}
	}
	return nil
}

// mappingHeadAndMiddle parses 'task_outputs.<middle>.<rest>' or
// '<head>.<rest>' and returns the leading two dotted segments. Empty
// strings indicate not-applicable.
func mappingHeadAndMiddle(s string) (head, middle string) {
	dot := strings.Index(s, ".")
	if dot <= 0 {
		return "", ""
	}
	head = s[:dot]
	rest := s[dot+1:]
	dot2 := strings.Index(rest, ".")
	if dot2 <= 0 {
		return head, rest
	}
	return head, rest[:dot2]
}

// buildAncestors does one BFS per task ref over the edge graph and
// returns ancestors[ref] = {set of refs that can reach ref through
// edges}. Edges in the spec use ref/from-to so we don't need to wait
// for server-assigned aliases. Used by the DAG mapping check.
func buildAncestors(spec *composeSpec) map[string]map[string]bool {
	// parents[X] = direct predecessors of X
	parents := map[string]map[string]bool{}
	for _, edge := range spec.Edges {
		from, _ := edge["from"].(string)
		to, _ := edge["to"].(string)
		if from == "" {
			from, _ = edge["sourceNodeId"].(string)
		}
		if to == "" {
			to, _ = edge["targetNodeId"].(string)
		}
		if from == "" || to == "" {
			continue
		}
		if parents[to] == nil {
			parents[to] = map[string]bool{}
		}
		parents[to][from] = true
	}
	ancestors := map[string]map[string]bool{}
	var visit func(node string, set map[string]bool)
	visit = func(node string, set map[string]bool) {
		for p := range parents[node] {
			if set[p] {
				continue
			}
			set[p] = true
			visit(p, set)
		}
	}
	for _, task := range spec.Tasks {
		ref := localRef(task, "")
		if ref == "" {
			continue
		}
		set := map[string]bool{}
		visit(ref, set)
		ancestors[ref] = set
	}
	return ancestors
}

// validEdgeKeys is the whitelist for edge object keys in the compose spec.
// 'from'/'to' are spec-local conveniences; 'sourceNodeId'/'targetNodeId' are
// the canonical API names. 'sourceHandle' wires conditional + array-router
// branches by handle id; 'branchName' was a common typo that silently
// dropped and broke conditionals at runtime, so we reject it explicitly.
var validEdgeKeys = map[string]bool{
	"from":         true,
	"to":           true,
	"sourceNodeId": true,
	"targetNodeId": true,
	"sourceHandle": true,
	"targetHandle": true,
	"label":        true,
	"id":           true,
}

// validWorkflowCategories mirrors the backend's WorkflowCategory enum.
// Common confusion: CUSTOMER and DEAL are entity TYPES (config.entityType),
// not categories. Keep these distinct in error messages.
var validWorkflowCategories = map[string]bool{
	"ACTION":     true,
	"EVALUATION": true,
	"CONTACT":    true,
	"OTHER":      true,
}

// validAliasPattern matches the alias regex the backend treats as URL-safe.
// Lowercase alphanumeric with internal dashes; backend does additional
// length/uniqueness checks but at minimum aliases must match this shape so
// they round-trip through path parameters.
var validAliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validInputSchemaTypes mirrors the SchemaTypes Pydantic discriminated union
// in borrower-central. The backend's error message lies ("permitted:
// 'array'"); the real enum is below. Used by preflight to reject typos in
// inputSchema.<field>.type before the API round-trip.
var validInputSchemaTypes = map[string]bool{
	"string":  true,
	"integer": true,
	"number":  true,
	"boolean": true,
	"object":  true,
	"array":   true,
}

// unknownConditionFields walks a ConditionGroup tree and returns every leaf
// 'field' value that isn't in the schemaFields set. Used to validate that
// conditional branches only reference fields the task declares -- otherwise
// the branch silently never matches at runtime.
func unknownConditionFields(group map[string]any, schemaFields map[string]bool) []string {
	if len(group) == 0 || len(schemaFields) == 0 {
		return nil
	}
	var missing []string
	items := asSlice(group["items"])
	for _, it := range items {
		im, _ := it.(map[string]any)
		if im == nil {
			continue
		}
		// Nested ConditionGroup
		if _, isGroup := im["operator"]; isGroup {
			if _, hasItems := im["items"]; hasItems {
				missing = append(missing, unknownConditionFields(im, schemaFields)...)
				continue
			}
		}
		// Leaf ConditionItem -- skip when valueType=variable (those
		// reference RHS fields that may live on a different scope).
		field, _ := im["field"].(string)
		if field == "" {
			continue
		}
		if !schemaFields[field] {
			missing = append(missing, field)
		}
	}
	return missing
}

// isServerAlias reports whether s looks like a server-assigned task alias --
// the trailing 6-hex-after-dash pattern produced by
// borrower-central/app/utils/alias_generator.py::generate_task_alias.
func isServerAlias(s string) bool {
	if len(s) < 8 {
		return false
	}
	dash := strings.LastIndex(s, "-")
	if dash < 1 || len(s)-dash-1 != 6 {
		return false
	}
	for i := dash + 1; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// sortedKeys returns the keys of m in deterministic order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// closestTaskType returns the canonical TaskType nearest to a given typo by
// Levenshtein distance, or "" if nothing is meaningfully close.
func closestTaskType(input string) string {
	best := ""
	bestDist := -1
	cutoff := len(input) / 2
	if cutoff < 2 {
		cutoff = 2
	}
	for t := range validTaskTypes {
		d := levenshtein(input, t)
		if d <= cutoff && (bestDist == -1 || d < bestDist) {
			best, bestDist = t, d
		}
	}
	return best
}

// levenshtein computes edit distance between two strings.
func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del, ins, sub := prev[j]+1, curr[j-1]+1, prev[j-1]+cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// validateEntityTypeVsTaskTypes rejects compose specs whose task-type set
// would render as a broken palette in the Hub for the declared entityType.
// No-op if entityType is unset (the workflow gets the generic palette).
func validateEntityTypeVsTaskTypes(spec *composeSpec) error {
	cfg := spec.Config
	if cfg == nil {
		return nil
	}
	entityType, _ := cfg["entityType"].(string)
	if entityType == "" {
		return nil
	}
	var hidden map[string]bool
	switch strings.ToLower(entityType) {
	case "customer":
		hidden = customerHiddenTypes
	case "deal":
		hidden = dealHiddenTypes
	default:
		return nil
	}
	violations := []string{}
	for i, task := range spec.Tasks {
		t, _ := task["type"].(string)
		if hidden[t] {
			ref := localRef(task, fmt.Sprintf("t%d", i))
			violations = append(violations, fmt.Sprintf("tasks[%d] (ref=%q) type=%q", i, ref, t))
		}
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf(
		"config.entityType=%q hides these task types from the Hub palette, but the spec uses them: %s. "+
			"The workflow would compose successfully but the Hub editor couldn't render it. "+
			"Either change config.entityType, drop the task type, or remove config.entityType to get the generic palette.",
		entityType, strings.Join(violations, ", "),
	)
}
