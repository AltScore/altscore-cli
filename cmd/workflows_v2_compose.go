package cmd

import (
	"encoding/json"
	"fmt"
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
			taskBody := map[string]any{
				"label": label,
				"type":  nodeType,
			}
			raw, err := json.Marshal(taskBody)
			if err != nil {
				return nil, fmt.Errorf("extraNodes[%d] (ref=%q): encode task: %w", i, ref, err)
			}
			if dryRun {
				fmt.Printf("# Would POST /v2/tasks (extra-node backing): %s\n", string(raw))
				taskAlias = ref
				n["taskAlias"] = taskAlias
				n["taskVersion"] = 1
			} else {
				data, _, err := c.Do("POST", "borrower_central", "/v2/tasks", json.RawMessage(raw))
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
//   1. label + type present
//   2. type is in the backend TaskType enum
//   3. http: headers must be a JSON-encoded string
//   4. data-store-write / data-store-query / webhook / comment: per-type
//      required fields
//   5. validateTaskV2Body: type-specific structural checks (conditional
//      branches, scorecard reference, mapping-table entries, rule-tree
//      enums)
func preflightTasks(spec *composeSpec) error {
	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		label, _ := task["label"].(string)
		taskType, _ := task["type"].(string)
		if label == "" || taskType == "" {
			return fmt.Errorf("tasks[%d] (ref=%q): label and type are required (validated before any POST)", i, ref)
		}
		if !validTaskTypes[taskType] {
			return fmt.Errorf(
				"tasks[%d] (ref=%q): unknown task type %q. The backend TaskType enum has %d values; "+
					"common typos: 'data-store' should be 'data-store-write' or 'data-store-query'; "+
					"'pdf-report' is now part of the 'end' task's endConfig (use type='html-template' for standalone). "+
					"Run 'altscore workflows-v2 schema-guide tasks | jq \".tasks.perType | keys\"' for the canonical list.",
				i, ref, taskType, len(validTaskTypes),
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
			if m, _ := task["errorMessage"].(string); m == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): exception task requires 'errorMessage'", i, ref)
			}
		case "child-workflow":
			eid, _ := task["executorId"].(string)
			eal, _ := task["executorAlias"].(string)
			if eid == "" && eal == "" {
				return fmt.Errorf("tasks[%d] (ref=%q): child-workflow task requires 'executorId' or 'executorAlias'", i, ref)
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
	}
	return nil
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
