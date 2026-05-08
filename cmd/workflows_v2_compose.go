package cmd

import (
	"encoding/json"
	"fmt"
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

			workflow, err := composeWorkflowBody(c, &spec, dryRun)
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

// taskHasMultiDotMapping reports whether any inputMapping value contains more
// than one dot (i.e. uses the canonical task_outputs.<alias>.<...> form). Such
// values are rejected by CreateTaskV2's strict Pydantic validator but accepted
// by CreateTaskVersionV2's lenient one. The compose loop uses this to switch
// to a two-phase create when needed.
func taskHasMultiDotMapping(task map[string]any) bool {
	im, ok := task["inputMappings"].(map[string]any)
	if !ok {
		return false
	}
	for _, v := range im {
		s, _ := v.(string)
		if strings.Count(s, ".") >= 2 {
			return true
		}
	}
	return false
}

// shallowCopy returns a top-level copy of a map. Inner values are shared with
// the original (good enough for the strip-fields use case in compose).
func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
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

// buildEndAutoWiring returns inputSchema + inputMappings for an `end` task by
// walking ALL transitive ancestors (not just direct predecessors) and
// emitting the per-task-type entries the Hub auto-wires. Mirrors
// `buildSectionsFromPredecessors` in altscore-ai-chat/lib/stores/workflow-builder-v2/actions/edge/pdf-data-source-auto-mapping.ts,
// which calls getPredecessorsSorted -- a BFS over incoming edges that yields
// every upstream task, not just the immediate parent.
//
// Without this wiring, the end task's PDF report editor sees no available
// sections (each section is keyed off `task_type_underscored_result_<alias>`
// in inputSchema). The runtime end_activity.py also reads task outputs via
// these mapping keys with context.get(); missing keys silently produce empty
// values in the PDF.
//
// Five upstream task types are recognised (those the Hub knows how to
// auto-wire): altdata-enrichment, scorecard, mapping-table, rule-tree,
// evaluate-rules. Other predecessor types are skipped silently (matches Hub).
//
// The function falls through gracefully when:
//   - no predecessors found (e.g. end is detached from the graph in the spec)
//   - a predecessor's ref isn't in refMap (e.g. dry-run partial map)
//   - a predecessor type isn't in the recognised set
// Callers can rely on the maps always being non-nil.
func buildEndAutoWiring(endRef string, edges []map[string]any, taskByRef map[string]map[string]any, refMap map[string]string) (inputSchema, inputMappings map[string]any) {
	inputSchema = map[string]any{}
	inputMappings = map[string]any{}

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

		// One-section-per-ancestor: emit the <task-type-underscored>_result_<alias>
		// pair (schema + mapping) the Hub also persists. The Hub source code
		// emits more keys (alerts/alerts_count for evaluate-rules,
		// <outputVar>_rule_code etc. for rule-tree), but those were never seen
		// in the persisted-and-working end task observed in production -- and
		// the strict CreateTaskV2 validator rejects any inputMappings key that
		// isn't in inputSchema. Sticking to the minimal observed set keeps the
		// validator happy AND matches what the Hub actually ships.
		switch predType {
		case "altdata-enrichment":
			key := "sources_output_packages_" + predAlias
			inputSchema[key] = map[string]any{"type": "object", "title": predLabel}
			inputMappings[key] = "task_outputs." + predAlias + ".sources_output_packages"

		case "scorecard":
			key := "scorecard_result_" + predAlias
			inputSchema[key] = map[string]any{"type": "object", "title": predLabel}
			inputMappings[key] = "task_outputs." + predAlias

		case "mapping-table":
			key := "mapping_table_result_" + predAlias
			inputSchema[key] = map[string]any{"type": "object", "title": predLabel}
			inputMappings[key] = "task_outputs." + predAlias

		case "rule-tree":
			key := "rule_tree_result_" + predAlias
			inputSchema[key] = map[string]any{"type": "object", "title": predLabel}
			inputMappings[key] = "task_outputs." + predAlias

		case "evaluate-rules":
			key := "evaluate_rules_result_" + predAlias
			inputSchema[key] = map[string]any{"type": "object", "title": predLabel}
			inputMappings[key] = "task_outputs." + predAlias
		}
	}

	return inputSchema, inputMappings
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

// rewriteRefsInMappings replaces leading spec-local refs in mapping values
// with the server-assigned alias from refMap. Handles both forms:
//   - canonical "<taskRef>.<outputName>" (what the API validator accepts)
//   - long "task_outputs.<taskRef>.<field>" (template-style; agents sometimes
//     write this even though the inputMappings validator rejects it)
//
// Reserved scopes (inputs, custom, system, task_outputs, task_outputs_by_type)
// are never treated as refs.
func rewriteRefsInMappings(mappings map[string]any, refMap map[string]string) map[string]any {
	if len(refMap) == 0 {
		return mappings
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
				}
			}
		} else if dot := strings.Index(s, "."); dot > 0 {
			// Bare <ref>.<rest> -- rewrite to task_outputs.<alias>.<rest>.
			// The runtime resolver requires the leading segment to be a valid
			// namespace (inputs/task_outputs/task_outputs_by_type/custom/system);
			// a bare task alias is not a namespace and triggers
			// "Unknown variable namespace" at execution time.
			head := s[:dot]
			if !reservedMappingScopes[head] {
				if alias, found := refMap[head]; found {
					s = "task_outputs." + alias + s[dot:]
				}
			}
		}
		out[k] = s
	}
	return out
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
func composeWorkflowBody(c *client.Client, spec *composeSpec, dryRun bool) (map[string]any, error) {
	if err := validateEntityTypeVsTaskTypes(spec); err != nil {
		return nil, err
	}

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

	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		label, _ := task["label"].(string)
		taskType, _ := task["type"].(string)
		if label == "" || taskType == "" {
			return nil, fmt.Errorf("tasks[%d] (ref=%q): label and type are required", i, ref)
		}

		// Strip the spec-only `ref` field before posting; it's not part of the API.
		delete(task, "ref")

		// Rewrite inputMappings using refs resolved so far. Tasks must be listed in
		// dependency order (consumer after producer) for cross-task refs to resolve.
		if mappings, ok := task["inputMappings"].(map[string]any); ok {
			task["inputMappings"] = rewriteRefsInMappings(mappings, refMap)
		}

		// Type-specific normalization: enrich altdata-enrichment with inputKeys
		// from source inputFields, validate conditional branches, etc.
		if err := normalizeTaskBody(c, task, spec.CustomVariables, dryRun); err != nil {
			return nil, fmt.Errorf("tasks[%d] (ref=%q): %w", i, ref, err)
		}

		// Detect cross-task mappings (task_outputs.<alias>.<deep>). The strict
		// CreateTaskV2 Pydantic validator rejects multi-dot mapping values, but
		// the runtime resolver requires them. CreateTaskVersionV2 has a lenient
		// validator. So when we have multi-dot mappings, do a two-phase create:
		//   Phase 1: POST /v2/tasks with inputMappings + inputSchema stripped
		//            so the strict validator skips. Server creates v=1 stub.
		//   Phase 2: POST /v2/tasks/{alias} with the full body, server bumps to
		//            v=2 with the canonical mappings persisted.
		multiDot := taskHasMultiDotMapping(task)

		var firstPhaseBody []byte
		var encErr error
		if multiDot {
			stub := shallowCopy(task)
			delete(stub, "inputMappings")
			delete(stub, "inputSchema")
			firstPhaseBody, encErr = json.Marshal(stub)
		} else {
			firstPhaseBody, encErr = json.Marshal(task)
		}
		if encErr != nil {
			return nil, fmt.Errorf("tasks[%d]: encode: %w", i, encErr)
		}
		fullBody, encErr := json.Marshal(task)
		if encErr != nil {
			return nil, fmt.Errorf("tasks[%d]: encode full body: %w", i, encErr)
		}

		var (
			version     = 1
			serverAlias string
		)
		if dryRun {
			fmt.Printf("# Would POST /v2/tasks: %s\n", string(firstPhaseBody))
			if multiDot {
				fmt.Printf("# Would POST /v2/tasks/{alias} (phase 2 -- multi-dot mappings): %s\n", string(fullBody))
			}
			if a, _ := task["alias"].(string); a != "" {
				serverAlias = a
			} else {
				serverAlias = ref
			}
			if multiDot {
				version = 2
			}
		} else {
			data, _, err := c.Do("POST", "borrower_central", "/v2/tasks", json.RawMessage(firstPhaseBody))
			if err != nil {
				return nil, fmt.Errorf("tasks[%d] (ref=%q): %w (created so far: %v)", i, ref, err, createdAliases)
			}
			var created map[string]any
			if err := json.Unmarshal(data, &created); err != nil {
				return nil, fmt.Errorf("tasks[%d] (ref=%q): parse response: %w", i, ref, err)
			}
			if v, ok := created["version"].(float64); ok {
				version = int(v)
			}
			if a, _ := created["alias"].(string); a != "" {
				serverAlias = a
			} else {
				return nil, fmt.Errorf("tasks[%d] (ref=%q): server returned no alias", i, ref)
			}
			createdAliases = append(createdAliases, serverAlias)

			// Phase 2: re-post with the full body (multi-dot mappings, full
			// inputSchema). The lenient CreateTaskVersionV2 validator accepts
			// the canonical task_outputs.<alias>.<deep.path> form.
			if multiDot {
				path := "/v2/tasks/" + serverAlias
				v2Data, _, err := c.Do("POST", "borrower_central", path, json.RawMessage(fullBody))
				if err != nil {
					return nil, fmt.Errorf("tasks[%d] (ref=%q) phase 2: %w", i, ref, err)
				}
				var v2created map[string]any
				if err := json.Unmarshal(v2Data, &v2created); err != nil {
					return nil, fmt.Errorf("tasks[%d] (ref=%q) phase 2: parse response: %w", i, ref, err)
				}
				if v, ok := v2created["version"].(float64); ok {
					version = int(v)
				}
			}
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
				inSchema, inMappings := buildEndAutoWiring(ref, spec.Edges, taskByRef, refMap)
				if len(inSchema) > 0 {
					taskBody["inputSchema"] = inSchema
				}
				if len(inMappings) > 0 {
					taskBody["inputMappings"] = inMappings
				}
			}

			// Two-phase create when the body has multi-dot mapping values
			// (currently only altdata-enrichment ancestors trigger this:
			// task_outputs.<alias>.sources_output_packages). The strict
			// CreateTaskV2 validator rejects multi-dot values, so phase 1
			// posts a stub without inputMappings/inputSchema, then phase 2
			// re-posts the full body via /v2/tasks/{alias} which uses the
			// lenient CreateTaskVersionV2 validator. Same pattern as the
			// tasks loop above; without it, an `end` node downstream of an
			// altdata-enrichment task fails at create with HTTP 400.
			multiDot := taskHasMultiDotMapping(taskBody)
			var firstPhaseBody []byte
			var encErr error
			if multiDot {
				stub := shallowCopy(taskBody)
				delete(stub, "inputMappings")
				delete(stub, "inputSchema")
				firstPhaseBody, encErr = json.Marshal(stub)
			} else {
				firstPhaseBody, encErr = json.Marshal(taskBody)
			}
			if encErr != nil {
				return nil, fmt.Errorf("extraNodes[%d] (ref=%q): encode task: %w", i, ref, encErr)
			}
			fullBody, encErr := json.Marshal(taskBody)
			if encErr != nil {
				return nil, fmt.Errorf("extraNodes[%d] (ref=%q): encode full body: %w", i, ref, encErr)
			}
			if dryRun {
				fmt.Printf("# Would POST /v2/tasks (extra-node backing): %s\n", string(firstPhaseBody))
				if multiDot {
					fmt.Printf("# Would POST /v2/tasks/{alias} (extra-node phase 2 -- multi-dot mappings): %s\n", string(fullBody))
				}
				taskAlias = ref
				n["taskAlias"] = taskAlias
				n["taskVersion"] = 1
				if multiDot {
					n["taskVersion"] = 2
				}
			} else {
				data, _, err := c.Do("POST", "borrower_central", "/v2/tasks", json.RawMessage(firstPhaseBody))
				if err != nil {
					return nil, fmt.Errorf("extraNodes[%d] (ref=%q): create backing task: %w", i, ref, err)
				}
				var created map[string]any
				if err := json.Unmarshal(data, &created); err != nil {
					return nil, fmt.Errorf("extraNodes[%d] (ref=%q): parse task response: %w", i, ref, err)
				}
				if a, ok := created["alias"].(string); ok && a != "" {
					taskAlias = a
					n["taskAlias"] = a
				}
				if v, ok := created["version"].(float64); ok {
					n["taskVersion"] = int(v)
				}
				createdAliases = append(createdAliases, fmt.Sprint(n["taskAlias"]))

				if multiDot {
					path := "/v2/tasks/" + taskAlias
					v2Data, _, err := c.Do("POST", "borrower_central", path, json.RawMessage(fullBody))
					if err != nil {
						return nil, fmt.Errorf("extraNodes[%d] (ref=%q) phase 2: %w", i, ref, err)
					}
					var v2created map[string]any
					if err := json.Unmarshal(v2Data, &v2created); err != nil {
						return nil, fmt.Errorf("extraNodes[%d] (ref=%q) phase 2: parse response: %w", i, ref, err)
					}
					if v, ok := v2created["version"].(float64); ok {
						n["taskVersion"] = int(v)
					}
				}
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
				if _, inMappings := buildEndAutoWiring(ref, spec.Edges, taskByRef, refMap); len(inMappings) > 0 {
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
			// BC unified the exception task on 'errorMessage' as the
			// canonical wire name and defaults statusCode to 400 server-
			// side, so the CLI no longer mirrors. We still tolerate legacy
			// specs that carry the old 'message' key by promoting it to
			// 'errorMessage' (BC also accepts the legacy form via its
			// pre-validator; this just normalizes our outgoing body).
			em, _ := task["errorMessage"].(string)
			m, _ := task["message"].(string)
			if em == "" && m == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): exception task requires 'errorMessage' -- the failure message surfaced when this branch fires", i, ref)
			}
			if em == "" && m != "" {
				task["errorMessage"] = m
				delete(task, "message")
			}
		case "child-workflow":
			eid, _ := task["executorId"].(string)
			eal, _ := task["executorAlias"].(string)
			if eid == "" && eal == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): child-workflow task requires 'executorId' or 'executorAlias'", i, ref)
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
		if err := validateTaskV2Body(json.RawMessage(body)); err != nil {
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
