package cmd

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// newUUIDv4 returns a random RFC 4122 v4 UUID. Used to mint stable IDs for
// task-config sub-entities (e.g. mappingTableConfig.entries[].id) that the
// runtime models require but agents typically forget to include.
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// conditionOperators is the canonical operator vocabulary used by conditional
// task branches and evaluation rules. Mirrors CONDITION_OPERATORS in
// altscore-ai-chat/lib/types/borrower_central/evaluation-rules.ts -- keep
// in sync.
var conditionOperators = map[string]bool{
	"eq": true, "neq": true,
	"gt": true, "gte": true, "lt": true, "lte": true,
	"contains": true, "startsWith": true, "endsWith": true,
	"in": true, "notIn": true, "between": true,
	"isNull": true, "isNotNull": true,
	"isAltdataEmpty": true, "isAltdataNotCalculated": true,
	"isAltdataError": true, "isAltdataNull": true, "isNotAltdataNull": true,
	"arrayContainsAny": true, "arrayContainsAll": true,
}

// validateTaskV2Body is a non-mutating, network-free check used by the
// tasks-v2 create / create-version commands. It runs the structural pass
// (validateTaskV2BodyStructural) plus a sourcable-only pass that catches
// the altdata-enrichment-with-empty-inputKeys mistake. The sourcable pass
// is split out because compose's preflight runs BEFORE its normalize step
// (which auto-fills inputKeys from the source's inputFields metadata) --
// surfacing the empty-inputKeys error there would block specs that compose
// can fix on its own. Manual `tasks-v2 create` callers don't run normalize,
// so for them the error remains useful.
//
// validateTaskV2Body = structural + sourcable. Compose calls structural only.
func validateTaskV2Body(body json.RawMessage) error {
	if err := validateTaskV2BodyStructural(body); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	var task map[string]any
	if err := json.Unmarshal(body, &task); err != nil {
		return nil
	}
	if t, _ := task["type"].(string); t == "altdata-enrichment" {
		sources := asSlice(task["sourcesConfig"])
		if len(sources) == 0 {
			return nil
		}
		inputKeys := asMap(task["inputKeys"])
		if len(inputKeys) == 0 {
			return fmt.Errorf(
				"altdata-enrichment task with non-empty sourcesConfig but empty inputKeys -- " +
					"the Hub UI will show an unwired source. Run 'altscore workflows-v2 sources-status --filter id=<SOURCE_ID>' " +
					"to see the source's required inputFields, then add an inputKeys entry per field, " +
					`e.g. inputKeys: {"personId": "{{personId}}"}. Compose does this automatically.`)
		}
	}
	return nil
}

// validateTaskV2BodyStructural runs the network-free structural checks that
// are safe to invoke regardless of whether normalize will run afterward.
// Catches:
//   - conditional task with 'expression' instead of structured 'conditions'
//   - conditional task using 'is_else' (snake_case) instead of 'isElse'
//   - conditional task with no isElse:true default branch
//   - conditional branch with neither 'conditions' nor 'isElse:true'
//   - missing required fields on credit-decisioning tasks (rulesConfig
//     non-empty, mappingTableConfig.entries non-empty, scorecardCode set,
//     ruleTreeCode + outputVariable set)
//
// Does NOT catch the altdata-enrichment empty-inputKeys case (see
// validateTaskV2Body). Compose's preflight uses this so a compose spec
// missing inputKeys is fixed by normalize, not rejected by preflight.
func validateTaskV2BodyStructural(body json.RawMessage) error {
	if len(body) == 0 {
		return nil
	}
	var task map[string]any
	if err := json.Unmarshal(body, &task); err != nil {
		return nil
	}
	taskType, _ := task["type"].(string)
	switch taskType {
	case "conditional":
		// Run normalize on a deep copy so we don't mutate the caller's body.
		copyJSON, _ := json.Marshal(task)
		var copyTask map[string]any
		_ = json.Unmarshal(copyJSON, &copyTask)
		return normalizeConditionalTask(copyTask)
	case "evaluate-rules":
		if rules := asSlice(task["rulesConfig"]); len(rules) == 0 {
			return fmt.Errorf("evaluate-rules task requires rulesConfig: a non-empty array of {ruleCode: \"<code>\"} references")
		}
	case "mapping-table":
		cfg := asMap(task["mappingTableConfig"])
		if entries := asSlice(cfg["entries"]); len(entries) == 0 {
			return fmt.Errorf("mapping-table task requires mappingTableConfig.entries: a non-empty array of {mappingTableId|mappingTableCode, inputVariable, outputVariable}")
		}
	case "scorecard":
		cfg := asMap(task["scorecardConfig"])
		id, _ := cfg["scorecardId"].(string)
		code, _ := cfg["scorecardCode"].(string)
		if id == "" && code == "" {
			return fmt.Errorf("scorecard task requires scorecardConfig.scorecardCode (preferred) or scorecardId -- " +
				"the task references a /v1/scorecards entity. The scorecard's rules (each linked to a /v1/mapping-tables entity) " +
				"live on the entity, not the task. Create the scorecard first via 'altscore scorecards create'.")
		}
	case "rule-tree":
		cfg := asMap(task["ruleTreeConfig"])
		id, _ := cfg["ruleTreeId"].(string)
		code, _ := cfg["ruleTreeCode"].(string)
		if id == "" && code == "" {
			return fmt.Errorf("rule-tree task requires ruleTreeConfig.ruleTreeId or ruleTreeCode")
		}
		if outVar, _ := cfg["outputVariable"].(string); outVar == "" {
			return fmt.Errorf("rule-tree task requires ruleTreeConfig.outputVariable")
		}
	case "child-workflow":
		// Batch mode supplies per-element inputs by resolving inputExpression
		// (e.g. "inputs.cuit_list") to a list. Without it the BC runtime has
		// nothing to fan out over and the parent fails on first dispatch.
		// Single mode uses inputMappings instead and is fine with no
		// inputExpression. Mirrors the Hub workflow-selector plugin validator.
		if runInBatch, _ := task["runInBatch"].(bool); runInBatch {
			if expr, _ := task["inputExpression"].(string); strings.TrimSpace(expr) == "" {
				return fmt.Errorf("child-workflow task with runInBatch=true requires inputExpression " +
					"(an expression resolving to a list, e.g. \"inputs.cuit_list\"); without it BC's batch dispatcher has nothing to fan out over")
			}
		}
	}
	return nil
}

// composeNormalizeOpts threads the cross-cutting context that several
// normalizers need without bloating the normalizeTaskBody signature.
//   - PredictedAlias: workflow's eventual server-derived alias. Used by the
//     entity-lookup normalizers to warn when a referenced credit-decisioning
//     entity is scoped to a different workflow alias (or to none) -- the
//     entity exists, but the picker won't show it.
//   - CustomVariables: workflow-level customVariables (from the spec). Needed
//     by compute-variables tasks to derive outputSchema. Pass nil when the
//     normalizer runs outside compose (e.g. tasks-v2 create).
type composeNormalizeOpts struct {
	PredictedAlias  string
	CustomVariables map[string]any
	// InputVariables -- workflow-level inputVariables. Needed by the
	// mapping-table normalizer to wrap bare inputVariable names (e.g.
	// "bureau_score") into the Hub-canonical "inputs.bureau_score" form.
	// Without it the bare form persists, which the runtime tolerates but
	// the Hub picker doesn't surface as an obvious source.
	InputVariables map[string]any
	// Publish -- whether the caller passed --publish to compose. When true,
	// "referenced entity missing on tenant" is promoted from a stderr
	// warning to a hard error. A published workflow that references a
	// missing scorecard / rule-tree / mapping-table / evaluation-rule
	// will fail 100% of its executions at runtime ("Scorecard not found:
	// X"), and the failure surfaces only when someone tries to actually
	// run the workflow -- often hours or days after compose. Catching it
	// at publish time turns a runtime mystery into a clear compose error.
	Publish bool
}

// missingEntityHandler returns an error when --publish is set, otherwise
// prints a stderr warning. Used by the four credit-decisioning task
// normalizers when a referenced entity isn't on the tenant. The dryRun
// guard means dry-runs never error or warn -- agents iterating on a spec
// in --dry-run mode aren't expected to have created entities yet.
func missingEntityHandler(opts *composeNormalizeOpts, dryRun bool, resourceKind, ref string) error {
	msg := fmt.Sprintf("%s task references %q which was not found on the tenant", resourceKind, ref)
	if opts != nil && opts.Publish {
		// Hard error even in dry-run: agents testing the publish flow via
		// --dry-run --publish need to see the same outcome they'd see at
		// real-run publish time. Skipping the check in dry-run would let
		// agents "validate" a spec that's guaranteed to fail on actual
		// publish.
		predicted := ""
		if opts.PredictedAlias != "" {
			predicted = " --workflow-alias " + opts.PredictedAlias
		}
		return fmt.Errorf(
			"%s -- refusing to publish a workflow that references a missing entity. "+
				"Create the entity first (altscore %s create%s ...) or remove the reference. "+
				"Without --publish compose would have warned and proceeded; --publish was set so this is a hard error.",
			msg, resourceKind, predicted)
	}
	// Warning path: stderr only, and only on real-run (dry-run agents
	// iterating on spec shape haven't created entities yet, so silence the
	// noise for them).
	if dryRun {
		return nil
	}
	fmt.Fprintf(os.Stderr, "# warning: %s\n", msg)
	return nil
}

// validateEntityWorkflowAliasMatch returns an error when a credit-
// decisioning entity's workflowAlias doesn't match the workflow being
// composed. The Hub UI's elements panel filters entities by exact
// workflow_alias match -- an entity scoped to a DIFFERENT workflow is
// referenceable (compose still wires it correctly and the runtime
// resolves it), but invisible to the editor, so a human reviewing the
// workflow in the Hub sees an unconfigured-looking task that secretly
// pulls from another workflow's entity. That cross-workflow drift is
// hard to undo once it spreads.
//
// Allowed: entity has no workflowAlias (global / shared by intent) OR
// the lookup didn't find anything (lookupEntity already warned, no
// scope to check) OR we don't know the predicted alias (compose context
// missing -- non-compose callers).
//
// Disallowed: any other mismatch. The error message tells the caller
// exactly which CLI command fixes it.
func validateEntityWorkflowAliasMatch(entity map[string]any, predictedAlias, resourceKind, ref string) error {
	if entity == nil || predictedAlias == "" {
		return nil
	}
	actual, _ := entity["workflowAlias"].(string)
	if actual == "" {
		return nil
	}
	if actual == predictedAlias {
		return nil
	}
	id, _ := entity["id"].(string)
	if id == "" {
		id = "<id>"
	}
	return fmt.Errorf(
		"%s %q is scoped to workflowAlias=%q, but this workflow's alias will be %q -- "+
			"the entity won't appear in the Hub's elements panel for this workflow, so any human "+
			"reviewing the task in the editor sees an unconfigured-looking reference. "+
			"Pick ONE: (a) re-scope the entity to this workflow: "+
			"`altscore %s update %s --workflow-alias %s` -- recommended when the entity is "+
			"specific to this workflow; or (b) clear the entity's workflowAlias to make it "+
			"globally shared across workflows: `altscore %s update %s --body '{\"workflowAlias\": null}'` "+
			"-- pick this only if multiple workflows really should share the same entity definition; "+
			"or (c) create a workflow-specific copy of the entity with a fresh code",
		resourceKind, ref, actual, predictedAlias, resourceKind, id, predictedAlias,
		resourceKind, id)
}

// normalizeTaskBody mutates the task spec in place to match the canonical
// shape the Hub UI expects. Returns an error if the body is fundamentally
// broken (missing isElse branch, expression instead of conditions, etc.).
//
// opts threads the compose-time context (workflow's predicted alias for
// cross-checking entity scopes, customVariables for compute-variables
// outputSchema derivation). Pass nil when running outside compose -- the
// normalizers degrade gracefully (e.g. mismatch warnings are skipped).
func normalizeTaskBody(c *client.Client, task map[string]any, opts *composeNormalizeOpts, dryRun bool) error {
	if opts == nil {
		opts = &composeNormalizeOpts{}
	}
	taskType, _ := task["type"].(string)
	switch taskType {
	case "altdata-enrichment":
		return normalizeAltdataTask(c, task, dryRun)
	case "compute-variables":
		return normalizeComputeVariablesTask(task, opts.CustomVariables)
	case "conditional":
		return normalizeConditionalTask(task)
	case "customer", "deal", "asset":
		return normalizeEntityWriteTask(task)
	case "evaluate-rules":
		return normalizeEvaluateRulesTask(c, task, opts, dryRun)
	case "mapping-table":
		return normalizeMappingTableTask(c, task, opts, dryRun)
	case "scorecard":
		return normalizeScorecardTask(c, task, opts, dryRun)
	case "rule-tree":
		return normalizeRuleTreeTask(c, task, opts, dryRun)
	case "child-workflow":
		return normalizeChildWorkflowTask(c, task, dryRun)
	}
	return nil
}

// childWorkflowInfo carries what a child-workflow caller needs to fill the
// parent task body: the child's typed input variables (used as the parent
// task's inputSchema -- mirrors what the Hub picker writes via
// applyAutoUpdate) and the end task's inputSchema (used as the parent
// task's outputSchema, because the end task receives what the child
// workflow emits, so its input shape is the child's output shape).
type childWorkflowInfo struct {
	inputVariables map[string]any
	outputSchema   map[string]any
}

// childWorkflowLookupCache memoizes /v2/workflows/<alias>/latest +
// /v2/tasks/<endTaskAlias> within a single compose run so multiple
// child-workflow tasks pointing at the same executor don't refetch.
var childWorkflowLookupCache = map[string]*childWorkflowInfo{}

func lookupChildWorkflow(c *client.Client, alias string, dryRun bool) (*childWorkflowInfo, error) {
	if cached, ok := childWorkflowLookupCache[alias]; ok {
		return cached, nil
	}
	if c == nil {
		if dryRun {
			fmt.Fprintf(os.Stderr, "# (dry-run) skipping live child-workflow lookup for %q; inputSchema/outputSchema fill will be empty\n", alias)
			return &childWorkflowInfo{
				inputVariables: map[string]any{},
				outputSchema:   map[string]any{},
			}, nil
		}
		return nil, fmt.Errorf("no client available for child-workflow lookup")
	}
	wfPath := "/v2/workflows/" + alias + "/latest"
	data, _, err := c.Do("GET", "borrower_central", wfPath, nil)
	if err != nil {
		if dryRun {
			fmt.Fprintf(os.Stderr, "# (dry-run) child-workflow lookup failed for %q (%v); skipping inputSchema/outputSchema fill\n", alias, err)
			return &childWorkflowInfo{
				inputVariables: map[string]any{},
				outputSchema:   map[string]any{},
			}, nil
		}
		return nil, fmt.Errorf("look up child workflow %q: %w", alias, err)
	}
	var wf map[string]any
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse child workflow %q: %w", alias, err)
	}
	info := &childWorkflowInfo{
		inputVariables: asMap(wf["inputVariables"]),
		outputSchema:   map[string]any{},
	}

	// Find the end node -> fetch its task -> use its inputSchema as the
	// parent's outputSchema. Matches the Hub picker behavior:
	// selectedWorkflowDetails -> find node.type=="end" -> getTaskByAlias ->
	// applyAutoUpdate({ outputSchema: endTask.inputSchema }).
	nodes := asSlice(wf["nodes"])
	var endTaskAlias string
	for _, n := range nodes {
		nm, _ := n.(map[string]any)
		if nm == nil {
			continue
		}
		if t, _ := nm["type"].(string); t == "end" {
			if a, _ := nm["taskAlias"].(string); a != "" {
				endTaskAlias = a
				break
			}
		}
	}
	if endTaskAlias != "" {
		taskPath := "/v2/tasks/" + endTaskAlias
		td, _, terr := c.Do("GET", "borrower_central", taskPath, nil)
		if terr != nil {
			if !dryRun {
				fmt.Fprintf(os.Stderr, "# warning: end-task lookup failed for child workflow %q end=%q (%v); skipping outputSchema fill\n", alias, endTaskAlias, terr)
			}
		} else {
			var endTask map[string]any
			if jerr := json.Unmarshal(td, &endTask); jerr == nil {
				info.outputSchema = asMap(endTask["inputSchema"])
			}
		}
	}

	childWorkflowLookupCache[alias] = info
	return info, nil
}

// normalizeChildWorkflowTask fills the parent task body's inputSchema and
// outputSchema from the referenced child workflow (matches what the Hub
// picker does via applyAutoUpdate) and validates batch-mode fields.
// preflightTasks already renamed executorAlias -> executorId; by the time
// this runs the alias lives on `executorId`.
func normalizeChildWorkflowTask(c *client.Client, task map[string]any, dryRun bool) error {
	executor, _ := task["executorId"].(string)
	if executor == "" {
		return nil
	}
	info, err := lookupChildWorkflow(c, executor, dryRun)
	if err != nil {
		// In dry-run mode lookupChildWorkflow already absorbed the error
		// and returned a stub. For real runs we surface it; the user can
		// fix the alias and recompose.
		return err
	}

	runInBatch, _ := task["runInBatch"].(bool)

	// Caller-supplied keys win on key-PRESENCE (not just non-empty).
	// `len == 0` collapses missing and explicit `{}` to the same case,
	// which means an agent that wants to opt out of the auto-fill by
	// writing `"inputSchema": {}` would still get the auto-fill. Key
	// presence respects the explicit empty opt-out.
	_, inputSchemaProvided := task["inputSchema"]
	_, outputSchemaProvided := task["outputSchema"]

	// Skip the inputSchema fill for runInBatch tasks: BC's batch
	// dispatcher reads `inputSchema` and treats it as a per-element
	// validator. When the auto-filled shape inherits `required: true`
	// fields from the child's typed variables, every dispatch fails with
	// `Missing required input: <name>` before any child execution
	// spawns -- even when the array elements DO contain those fields.
	// (Surfaced by stress test v17.) Keep the fill for single-mode
	// tasks where the parent's inputMappings populates the per-key
	// inputs and the runtime validates against them happily.
	if !inputSchemaProvided && !runInBatch && len(info.inputVariables) > 0 {
		task["inputSchema"] = info.inputVariables
	}
	// outputSchema fill is safe in both modes -- the parent's downstream
	// tasks consume task_outputs.<this-task>.<field> regardless of how
	// the child was dispatched, so knowing the shape is purely useful.
	if !outputSchemaProvided && len(info.outputSchema) > 0 {
		task["outputSchema"] = info.outputSchema
	}

	// Coverage warning for single-mode specs: any required child input
	// without a corresponding inputMappings entry will go unset at runtime.
	// Skip when runInBatch is true -- batch dispatch reads inputs from the
	// resolved inputExpression, not from inputMappings.
	if !runInBatch {
		mappings := asMap(task["inputMappings"])
		missing := make([]string, 0)
		for name, raw := range info.inputVariables {
			vm, _ := raw.(map[string]any)
			if vm == nil {
				continue
			}
			required, _ := vm["required"].(bool)
			if !required {
				continue
			}
			if _, present := mappings[name]; !present {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			ref := localRef(task, "")
			fmt.Fprintf(os.Stderr,
				"# warning: child-workflow ref=%q executor=%q has required inputs without inputMappings: %v. "+
					"The child execution will see these as missing at runtime.\n",
				ref, executor, missing)
		}
	}

	return nil
}

func normalizeAltdataTask(c *client.Client, task map[string]any, dryRun bool) error {
	sources := asSlice(task["sourcesConfig"])
	if len(sources) == 0 {
		return nil
	}

	inputKeys := asMap(task["inputKeys"])
	inputSchema := asMap(task["inputSchema"])
	outputSchema := asMap(task["outputSchema"])

	// Default packageAlias / dataAge on each sourcesConfig entry.
	for i, s := range sources {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if _, has := sm["dataAge"]; !has {
			sm["dataAge"] = 30
		}
		if _, has := sm["packageAlias"]; !has {
			if sid, _ := sm["sourceId"].(string); sid != "" {
				sm["packageAlias"] = strings.ToLower(strings.ReplaceAll(sid, "-", "_"))
			}
		}
		sources[i] = sm
	}
	task["sourcesConfig"] = sources

	// Look up each source once and use the metadata for both inputKeys
	// (from inputFields) and outputSchema (from outputSchema). Mirrors the
	// Hub editor at AltDataEnrichmentEditor.tsx which merges both into the
	// task body whenever sourcesConfig changes.
	seenInput := map[string]bool{}
	derivedInputKeys := len(inputKeys) == 0
	for _, s := range sources {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		sid, _ := sm["sourceId"].(string)
		ver, _ := sm["version"].(string)
		if sid == "" {
			continue
		}
		src, err := lookupAltdataSource(c, sid, ver, dryRun)
		if err != nil {
			return fmt.Errorf("could not look up source %s %s: %w", sid, ver, err)
		}
		if derivedInputKeys {
			for _, f := range src.inputFields {
				if seenInput[f] {
					continue
				}
				seenInput[f] = true
				inputKeys[f] = "{{" + f + "}}"
				if _, has := inputSchema[f]; !has {
					inputSchema[f] = map[string]any{"type": "string", "required": false}
				}
			}
		}
		// Merge each source's outputSchema entries into the task outputSchema.
		// Source outputSchema is keyed by sourceId, e.g. {ECU-PUB-0002: {...}}.
		// Existing user-supplied keys win; we only fill in missing ones.
		for k, v := range src.outputSchema {
			if _, exists := outputSchema[k]; !exists {
				outputSchema[k] = v
			}
		}
	}
	if derivedInputKeys && len(inputKeys) > 0 {
		task["inputKeys"] = inputKeys
		task["inputSchema"] = inputSchema
	}

	if _, has := task["mode"]; !has {
		task["mode"] = "single"
	}
	if _, has := task["savePackages"]; !has {
		task["savePackages"] = true
	}
	if _, has := task["timeout"]; !has {
		task["timeout"] = 60
	}
	// Always include the canonical sources_output_packages entry so the Hub's
	// calculateAvailableOutputs surfaces a top-level output for downstream
	// mapping dropdowns even if the per-source outputSchema is empty.
	if _, has := outputSchema["sources_output_packages"]; !has {
		outputSchema["sources_output_packages"] = map[string]any{
			"type":        "object",
			"title":       "Output Packages",
			"description": "Enrichment results from configured sources",
		}
	}
	task["outputSchema"] = outputSchema
	return nil
}

// normalizeComputeVariablesTask derives outputSchema from selectedVariables +
// the workflow's customVariables, mirroring the Hub plugin's buildOutputSchema.
// Without this, downstream tasks see no available outputs from this task.
func normalizeComputeVariablesTask(task map[string]any, customVariables map[string]any) error {
	rawSelected := asSlice(task["selectedVariables"])
	if len(rawSelected) == 0 {
		return nil
	}
	out := asMap(task["outputSchema"])
	for _, raw := range rawSelected {
		name, _ := raw.(string)
		if name == "" {
			continue
		}
		if _, has := out[name]; has {
			continue
		}
		entry := map[string]any{
			"type":        "string",
			"title":       name,
			"description": "",
		}
		if cv, _ := customVariables[name].(map[string]any); cv != nil {
			if t, _ := cv["type"].(string); t != "" {
				entry["type"] = t
			}
			if title, _ := cv["title"].(string); title != "" {
				entry["title"] = title
			}
			if desc, _ := cv["description"].(string); desc != "" {
				entry["description"] = desc
			}
		}
		out[name] = entry
	}
	task["outputSchema"] = out
	return nil
}

// wrapAsObjectSchemaForPydantic recursively rewrites a nested schema dict so
// every non-leaf level matches the SchemaTypes ObjectSchema variant in
// borrower-central/app/model/workflows_v2/schemas.py:
//
//   {type: "object", properties: {<k>: <recursive>}, title?, description?}
//
// Source outputSchemas come back from /v2/workflows/sources-status in a
// flatter shape (no `type: "object"` markers, child keys like `data` are
// direct properties). Pydantic's discriminated union picks StringSchema as
// the first matching variant for that flatter shape and drops every nested
// field on the floor. Wrapping as canonical JSON Schema makes Pydantic match
// ObjectSchema all the way down.
//
// Leaves (anything with an explicit `type` field) are passed through; only
// recurse into their `properties` if present.
func wrapAsObjectSchemaForPydantic(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	// Leaf: explicit type already set (string, integer, number, boolean, ...).
	if _, hasType := m["type"]; hasType {
		if props, ok := m["properties"].(map[string]any); ok {
			rewritten := map[string]any{}
			for k, val := range props {
				rewritten[k] = wrapAsObjectSchemaForPydantic(val)
			}
			m["properties"] = rewritten
		}
		return m
	}
	// No type. Has explicit `properties` -> object whose children we recurse into.
	if props, ok := m["properties"].(map[string]any); ok {
		rewritten := map[string]any{}
		for k, val := range props {
			rewritten[k] = wrapAsObjectSchemaForPydantic(val)
		}
		out := map[string]any{
			"type":       "object",
			"properties": rewritten,
		}
		for _, k := range []string{"title", "description"} {
			if val, ok := m[k]; ok {
				out[k] = val
			}
		}
		return out
	}
	// No type, no properties -> treat the dict's children as implicit
	// properties of an object schema. (This handles the source's bare
	// `{ECU-PUB-0002: {data: {properties: {...}}}}` shape.)
	implicitProps := map[string]any{}
	out := map[string]any{"type": "object"}
	for k, val := range m {
		switch k {
		case "title", "description":
			out[k] = val
		default:
			implicitProps[k] = wrapAsObjectSchemaForPydantic(val)
		}
	}
	if len(implicitProps) > 0 {
		out["properties"] = implicitProps
	}
	return out
}

// altdataSourceInfo captures the parts of an AltData source's metadata that
// the compose normalizer needs: required input field names (for inputKeys)
// and the source's outputSchema (merged into the task's outputSchema).
type altdataSourceInfo struct {
	inputFields  []string
	outputSchema map[string]any
}

// sourceLookupCache memoizes /v2/workflows/sources-status results within a
// single compose run so a multi-source task only fetches once.
var sourceLookupCache = map[string]*altdataSourceInfo{}

// lookupAltdataSource fetches an AltData source from /v2/workflows/sources-status
// and returns its inputField names and outputSchema. Used by normalizeAltdataTask
// to auto-fill inputKeys/inputSchema (input side) and outputSchema (output side)
// to match what the Hub's editor produces when the user adds the source.
func lookupAltdataSource(c *client.Client, sourceID, version string, dryRun bool) (*altdataSourceInfo, error) {
	cacheKey := sourceID + "|" + version
	if cached, ok := sourceLookupCache[cacheKey]; ok {
		return cached, nil
	}
	if c == nil {
		if dryRun {
			fmt.Fprintf(os.Stderr, "# (dry-run) skipping live source lookup for %s %s; assuming personId/taxId\n", sourceID, version)
			return &altdataSourceInfo{
				inputFields:  []string{"personId", "taxId"},
				outputSchema: map[string]any{},
			}, nil
		}
		return nil, fmt.Errorf("no client available for source lookup")
	}
	q := url.Values{}
	q.Set("per-page", "200")
	path := "/v2/workflows/sources-status?" + q.Encode()
	data, _, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		if dryRun {
			fmt.Fprintf(os.Stderr, "# (dry-run) source lookup failed for %s %s (%v); using stub\n", sourceID, version, err)
			return &altdataSourceInfo{
				inputFields:  []string{"personId", "taxId"},
				outputSchema: map[string]any{},
			}, nil
		}
		return nil, err
	}
	var sources []map[string]any
	if err := json.Unmarshal(data, &sources); err != nil {
		return nil, fmt.Errorf("parse sources-status: %w", err)
	}
	for _, s := range sources {
		sid, _ := s["sourceId"].(string)
		sver, _ := s["sourceVersion"].(string)
		if sid != sourceID {
			continue
		}
		if version != "" && sver != version {
			continue
		}
		fields := asSlice(s["inputFields"])
		fieldNames := make([]string, 0, len(fields))
		for _, f := range fields {
			fm, _ := f.(map[string]any)
			if name, _ := fm["field"].(string); name != "" {
				fieldNames = append(fieldNames, name)
			}
		}
		// Source's outputSchema, plus title/description on the source-keyed
		// entry to match how the Hub annotates it.
		outSchema := asMap(s["outputSchema"])
		if entry, ok := outSchema[sourceID].(map[string]any); ok {
			if name, _ := s["name"].(string); name != "" {
				if _, has := entry["title"]; !has {
					entry["title"] = name
				}
			}
			if desc, ok := s["description"].(map[string]any); ok {
				if en, _ := desc["en"].(string); en != "" {
					if _, has := entry["description"]; !has {
						entry["description"] = en
					}
				}
			}
			outSchema[sourceID] = entry
		}
		// Convert the source's raw outputSchema (which uses bare nested
		// objects without `type: "object"` wrappers) into Pydantic-friendly
		// JSON-Schema shape so the SchemaTypes discriminated union matches
		// ObjectSchema. Without this, Pydantic falls back to StringSchema and
		// silently drops the nested fields.
		canonical := map[string]any{}
		for k, v := range outSchema {
			canonical[k] = wrapAsObjectSchemaForPydantic(v)
		}
		// Annotate the source-keyed entry with title/description from the
		// source metadata so downstream picker UIs render a friendly label.
		if entry, ok := canonical[sourceID].(map[string]any); ok {
			if name, _ := s["name"].(string); name != "" {
				if _, has := entry["title"]; !has {
					entry["title"] = name
				}
			}
			if desc, ok := s["description"].(map[string]any); ok {
				if en, _ := desc["en"].(string); en != "" {
					if _, has := entry["description"]; !has {
						entry["description"] = en
					}
				}
			}
			canonical[sourceID] = entry
		}
		info := &altdataSourceInfo{
			inputFields:  fieldNames,
			outputSchema: canonical,
		}
		sourceLookupCache[cacheKey] = info
		return info, nil
	}
	return nil, fmt.Errorf("source %s %s not found in sources-status", sourceID, version)
}

func normalizeConditionalTask(task map[string]any) error {
	branches := asSlice(task["branches"])
	if len(branches) == 0 {
		return fmt.Errorf("conditional task must have at least one branch (incl. an isElse:true default)")
	}

	hasElse := false
	elseIdx := -1
	for i, b := range branches {
		bm, ok := b.(map[string]any)
		if !ok {
			return fmt.Errorf("branches[%d]: not an object", i)
		}

		if expr, has := bm["expression"]; has && expr != nil && expr != "" {
			return fmt.Errorf(
				"branches[%d]: 'expression' is not a real branch field -- the API silently drops it. "+
					"Use structured 'conditions' instead, e.g.\n  "+
					`{"id": "branch_X", "label": "Approve", "isElse": false, "order": 0,`+"\n   "+
					`"conditions": {"operator": "AND", "items": [{"field": "score", "operator": "gte", "value": "700", "valueType": "value"}]}}`,
				i)
		}
		delete(bm, "expression")

		if v, has := bm["is_else"]; has {
			if _, hasCamel := bm["isElse"]; !hasCamel {
				bm["isElse"] = v
			}
			delete(bm, "is_else")
		}

		isElse, _ := bm["isElse"].(bool)
		if isElse {
			if hasElse {
				return fmt.Errorf("branches[%d]: a conditional may only have one isElse branch", i)
			}
			hasElse = true
			elseIdx = i
			bm["conditions"] = nil
			if id, _ := bm["id"].(string); id == "" {
				bm["id"] = "branch-else"
			}
		} else {
			cond := bm["conditions"]
			if err := validateConditionGroup(cond, fmt.Sprintf("branches[%d].conditions", i)); err != nil {
				return err
			}
			if id, _ := bm["id"].(string); id == "" {
				bm["id"] = fmt.Sprintf("branch_%d", i)
			}
		}

		if _, has := bm["order"]; !has {
			bm["order"] = i
		}
		if _, has := bm["label"]; !has {
			bm["label"] = fmt.Sprintf("Branch %d", i)
		}

		branches[i] = bm
	}
	if !hasElse {
		return fmt.Errorf("conditional task must include an isElse:true default branch")
	}

	if elseIdx != len(branches)-1 {
		elseB := branches[elseIdx]
		branches = append(branches[:elseIdx], branches[elseIdx+1:]...)
		branches = append(branches, elseB)
	}
	for i, b := range branches {
		bm := b.(map[string]any)
		bm["order"] = i
		branches[i] = bm
	}
	task["branches"] = branches

	return nil
}

func validateConditionGroup(v any, path string) error {
	if v == nil {
		return fmt.Errorf("%s: required (use {operator: AND|OR, items: [...]})", path)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: must be an object", path)
	}

	if itemsRaw, hasItems := m["items"]; hasItems {
		op, _ := m["operator"].(string)
		if op != "AND" && op != "OR" {
			return fmt.Errorf("%s.operator must be 'AND' or 'OR', got %q", path, op)
		}
		items := asSlice(itemsRaw)
		if len(items) == 0 {
			return fmt.Errorf("%s.items must be non-empty", path)
		}
		for i, it := range items {
			if err := validateConditionGroup(it, fmt.Sprintf("%s.items[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	}

	field, _ := m["field"].(string)
	op, _ := m["operator"].(string)
	if field == "" {
		return fmt.Errorf("%s.field is required", path)
	}
	if op == "" {
		return fmt.Errorf("%s.operator is required", path)
	}
	if !conditionOperators[op] {
		known := []string{}
		for k := range conditionOperators {
			known = append(known, k)
		}
		return fmt.Errorf("%s.operator %q is not a known condition operator. valid: %v", path, op, known)
	}
	if _, has := m["valueType"]; !has {
		m["valueType"] = "value"
	}
	return nil
}

// ===================== Credit-decisioning task normalizers =====================
//
// The four credit-decisioning task types (evaluate-rules, mapping-table,
// scorecard, rule-tree) reference entities at /v1/{evaluation-rules,
// mapping-tables, scorecards, rule-trees}. The Hub plugins fill canonical
// outputSchema entries when these tasks are placed on a canvas; compose
// mirrors that here so downstream tasks see the right available outputs.

// entityCache memoizes /v1/{resource} lookups within a compose run.
// Same idea as sourceLookupCache; one entry per (resource, codeOrId).
var entityCache = map[string]map[string]any{}

// lookupEntity fetches /v1/{resource}?code=<codeOrId> (best-effort) and
// returns the first matching record. Used for non-blocking warnings when
// a task references something that doesn't exist on the tenant, and for
// outputSchema refinement (e.g. mapping-tables outputType=number).
//
// The lookup runs in dry-run too -- skipping it makes dry-run preview a
// different shape than real compose, defeating the purpose of dry-run.
// Matches the lookupAltdataSource contract above. The dryRun flag is kept
// in the signature for symmetry with sibling normalizers and so callers
// can opt in to dry-run-only printf behavior.
func lookupEntity(c *client.Client, resource, codeOrID string, dryRun bool) (map[string]any, error) {
	_ = dryRun
	if codeOrID == "" {
		return nil, nil
	}
	cacheKey := resource + "|" + codeOrID
	if cached, ok := entityCache[cacheKey]; ok {
		return cached, nil
	}
	if c == nil {
		return nil, nil
	}
	q := url.Values{}
	q.Set("code", codeOrID)
	q.Set("per-page", "5")
	path := "/v1/" + resource + "?" + q.Encode()
	data, _, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, nil // best-effort, suppress
	}
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, nil
	}
	for _, e := range arr {
		if code, _ := e["code"].(string); code == codeOrID {
			entityCache[cacheKey] = e
			return e, nil
		}
		if id, _ := e["id"].(string); id == codeOrID {
			entityCache[cacheKey] = e
			return e, nil
		}
	}
	// Cache the miss so we don't re-fetch.
	entityCache[cacheKey] = nil
	return nil, nil
}

// normalizeEvaluateRulesTask validates that rulesConfig is a non-empty array
// of {ruleCode, ruleId} references and pre-fills outputSchema with the
// canonical alerts/alerts_count fields produced by the activity at
// borrower-central/app/temporal/activities/evaluate_rules_activity.py.
func normalizeEvaluateRulesTask(c *client.Client, task map[string]any, opts *composeNormalizeOpts, dryRun bool) error {
	predictedAlias := ""
	if opts != nil {
		predictedAlias = opts.PredictedAlias
	}
	rules := asSlice(task["rulesConfig"])
	if len(rules) == 0 {
		return fmt.Errorf("evaluate-rules task requires rulesConfig: a non-empty array of {ruleCode: \"<code>\"} references")
	}
	for i, r := range rules {
		rm, ok := r.(map[string]any)
		if !ok {
			return fmt.Errorf("rulesConfig[%d]: must be an object", i)
		}
		code, _ := rm["ruleCode"].(string)
		id, _ := rm["ruleId"].(string)
		if code == "" && id == "" {
			return fmt.Errorf("rulesConfig[%d]: must include ruleCode (preferred) or ruleId", i)
		}
		// Best-effort tenant existence check + workflow-alias scope check.
		ref := code
		if ref == "" {
			ref = id
		}
		entity, _ := lookupEntity(c, "evaluation-rules", ref, dryRun)
		if entity == nil && c != nil {
			if err := missingEntityHandler(opts, dryRun, "evaluation-rules", ref); err != nil {
				return fmt.Errorf("rulesConfig[%d]: %w", i, err)
			}
		}
		if err := validateEntityWorkflowAliasMatch(entity, predictedAlias, "evaluation-rules", ref); err != nil {
			return fmt.Errorf("rulesConfig[%d]: %w", i, err)
		}
	}
	out := asMap(task["outputSchema"])
	if _, has := out["alerts"]; !has {
		out["alerts"] = map[string]any{
			"type":        "array",
			"title":       "Alerts",
			"description": "Alerts triggered by matching rules",
		}
	}
	if _, has := out["alerts_count"]; !has {
		out["alerts_count"] = map[string]any{
			"type":        "integer",
			"title":       "Alerts Count",
			"description": "Number of alerts triggered",
		}
	}
	task["outputSchema"] = out
	return nil
}

// normalizeMappingTableTask validates mappingTableConfig.entries[] and
// pre-fills outputSchema with each entry's outputVariable so downstream
// tasks see them as top-level outputs. Each entry needs a stable id (the
// runtime MappingTableEntry pydantic model requires it); compose mints a
// UUID v4 when the agent omits it, matching what the Hub editor does.
func normalizeMappingTableTask(c *client.Client, task map[string]any, opts *composeNormalizeOpts, dryRun bool) error {
	predictedAlias := ""
	var inputVars map[string]any
	if opts != nil {
		predictedAlias = opts.PredictedAlias
		inputVars = opts.InputVariables
	}
	cfg := asMap(task["mappingTableConfig"])
	entries := asSlice(cfg["entries"])
	if len(entries) == 0 {
		return fmt.Errorf("mapping-table task requires mappingTableConfig.entries: a non-empty array of {mappingTableId|mappingTableCode, inputVariable, outputVariable}")
	}
	// Detect duplicate or missing `order` across entries. The Hub UI sorts
	// entries by `order` and collapses duplicates (or renders only the first
	// entry per order value), so a spec with two entries both at order=0
	// shows up as a single-entry panel even though the persisted task body
	// has both. Auto-assign sequential orders matching array position when
	// we detect this. Preserve caller-supplied distinct orders.
	seenOrders := map[float64]int{}
	hasDupOrder := false
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		ord, hasOrd := em["order"]
		if !hasOrd {
			hasDupOrder = true // missing order -> renumber everyone
			break
		}
		var f float64
		switch v := ord.(type) {
		case float64:
			f = v
		case int:
			f = float64(v)
		default:
			hasDupOrder = true
			break
		}
		if _, dup := seenOrders[f]; dup {
			hasDupOrder = true
			break
		}
		seenOrders[f] = 1
	}
	if hasDupOrder {
		for i, e := range entries {
			if em, ok := e.(map[string]any); ok {
				em["order"] = i
				entries[i] = em
			}
		}
	}

	out := asMap(task["outputSchema"])
	for i, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			return fmt.Errorf("mappingTableConfig.entries[%d]: must be an object", i)
		}
		if entryID, _ := em["id"].(string); entryID == "" {
			em["id"] = newUUIDv4()
		}
		id, _ := em["mappingTableId"].(string)
		code, _ := em["mappingTableCode"].(string)
		if id == "" && code == "" {
			return fmt.Errorf("mappingTableConfig.entries[%d]: missing mappingTableId/mappingTableCode", i)
		}
		inVar, _ := em["inputVariable"].(string)
		outVar, _ := em["outputVariable"].(string)
		if inVar == "" || outVar == "" {
			return fmt.Errorf("mappingTableConfig.entries[%d]: missing inputVariable or outputVariable", i)
		}
		// Wrap bare inputVariable names into the Hub-canonical scope form
		// when we can recognize them. Currently we only know about workflow
		// inputs (via spec.inputVariables); a bare name matching an input
		// gets `inputs.<name>`. Bare names not matching anything are left
		// alone -- they may legitimately reference a runtime-context key
		// (e.g. an upstream task output already promoted to root) that
		// compose can't see from this side.
		if !strings.Contains(inVar, ".") {
			if _, isInput := inputVars[inVar]; isInput {
				em["inputVariable"] = "inputs." + inVar
			}
		}
		ref := code
		if ref == "" {
			ref = id
		}
		// Best-effort lookup so we can refine outputSchema type from the table's outputType.
		entity, _ := lookupEntity(c, "mapping-tables", ref, dryRun)
		outType := "string"
		if entity != nil {
			if t, _ := entity["outputType"].(string); t == "number" {
				outType = "number"
			}
		}
		if entity == nil && c != nil {
			if err := missingEntityHandler(opts, dryRun, "mapping-tables", ref); err != nil {
				return fmt.Errorf("entries[%d]: %w", i, err)
			}
		}
		if err := validateEntityWorkflowAliasMatch(entity, predictedAlias, "mapping-tables", ref); err != nil {
			return fmt.Errorf("entries[%d]: %w", i, err)
		}
		// Pre-fill the three canonical outputs mapping_table_activity emits
		// per entry (see app/temporal/activities/mapping_table_activity.py):
		//   <outputVariable>        -- the mapped value (typed per entity outputType)
		//   <outputVariable>_bucket -- label of the matched bucket
		//   <outputVariable>_input  -- the original input value, for debugging
		// Mirrors the Hub editor (which adds all three when wiring a new
		// entry). Without the companion fields, downstream Hub mapping
		// pickers only show the bare value -- agents lose the audit trail
		// the runtime is already emitting.
		if _, has := out[outVar]; !has {
			out[outVar] = map[string]any{
				"type":        outType,
				"title":       outVar,
				"description": "Mapping table output: " + outVar,
			}
		}
		if _, has := out[outVar+"_bucket"]; !has {
			out[outVar+"_bucket"] = map[string]any{
				"type":        "string",
				"title":       outVar + " Bucket",
				"description": "Label of the matched bucket",
			}
		}
		if _, has := out[outVar+"_input"]; !has {
			// Best-guess input type. If we couldn't load the entity it
			// defaults to "string"; the Hub editor uses "number" for
			// numerical tables but tolerates either.
			inType := "string"
			if entity != nil {
				if mt, _ := entity["mappingType"].(string); mt == "numerical" {
					inType = "number"
				}
			}
			out[outVar+"_input"] = map[string]any{
				"type":        inType,
				"title":       outVar + " Input",
				"description": "Original input value",
			}
		}
		entries[i] = em
	}
	cfg["entries"] = entries
	task["mappingTableConfig"] = cfg
	task["outputSchema"] = out

	// Mirror per-entry inputVariables into the task's top-level inputMappings
	// + inputSchema. The Hub UI's mapping-table properties panel renders from
	// the top-level maps -- a task whose entries[].inputVariable resolves at
	// runtime correctly but whose top-level inputSchema is empty shows up as
	// "N entries" on the node card but expands to a blank panel. Mirroring
	// is per-field-name extracted from inputVariable's last dotted segment.
	mirrorEntryInputsToTopLevel(task, entries)
	return nil
}

// mirrorEntryInputsToTopLevel ensures the task's top-level inputMappings +
// inputSchema reflect every entry's inputVariable. The Hub UI renders the
// mapping-table properties panel from the top-level maps (not from
// entries[].inputVariable); without this, the panel appears empty even
// though the runtime resolves entries correctly. Caller-supplied entries
// in inputMappings/inputSchema are preserved.
func mirrorEntryInputsToTopLevel(task map[string]any, entries []any) {
	mappings := asMap(task["inputMappings"])
	schema := asMap(task["inputSchema"])
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		inVar, _ := em["inputVariable"].(string)
		if inVar == "" {
			continue
		}
		field := inVar
		if idx := strings.LastIndex(inVar, "."); idx >= 0 {
			field = inVar[idx+1:]
		}
		if _, has := mappings[field]; !has {
			mappings[field] = inVar
		}
		if _, has := schema[field]; !has {
			schema[field] = map[string]any{
				"type":  "number",
				"title": field,
			}
		}
	}
	task["inputMappings"] = mappings
	task["inputSchema"] = schema
}

// mirrorMappingsToInputSchema derives top-level inputSchema entries from a
// task's top-level inputMappings keys. Used by scorecard + rule-tree
// normalizers because the Hub properties panel reads inputSchema to display
// each task's required inputs. Caller-supplied inputSchema entries win.
func mirrorMappingsToInputSchema(task map[string]any) {
	mappings := asMap(task["inputMappings"])
	if len(mappings) == 0 {
		return
	}
	schema := asMap(task["inputSchema"])
	for k := range mappings {
		if _, has := schema[k]; !has {
			schema[k] = map[string]any{
				"type":  "number",
				"title": k,
			}
		}
	}
	task["inputSchema"] = schema
}

// normalizeScorecardTask validates that scorecardConfig references an existing
// /v1/scorecards entity and pre-fills outputSchema with totalScoreVariable +
// breakdownVariable. The runtime activity loads the scorecard by code/id and
// uses the entity's rules; any inline 'rules' on the task body are ignored.
// Compose preserves user-supplied inline rules (for legacy bodies) but never
// requires them.
func normalizeScorecardTask(c *client.Client, task map[string]any, opts *composeNormalizeOpts, dryRun bool) error {
	predictedAlias := ""
	if opts != nil {
		predictedAlias = opts.PredictedAlias
	}
	cfg := asMap(task["scorecardConfig"])
	id, _ := cfg["scorecardId"].(string)
	code, _ := cfg["scorecardCode"].(string)
	if id == "" && code == "" {
		return fmt.Errorf("scorecard task requires scorecardConfig.scorecardCode (preferred) or scorecardId -- " +
			"the task references a /v1/scorecards entity. Each rule on the scorecard must link to a /v1/mapping-tables " +
			"entity (mappingTableCode); inline 'rules' on the task body are ignored at runtime. " +
			"Create the scorecard first via 'altscore scorecards create'.")
	}
	ref := code
	if ref == "" {
		ref = id
	}
	entity, _ := lookupEntity(c, "scorecards", ref, dryRun)
	if entity == nil && c != nil {
		if err := missingEntityHandler(opts, dryRun, "scorecards", ref); err != nil {
			return err
		}
	}
	if err := validateEntityWorkflowAliasMatch(entity, predictedAlias, "scorecards", ref); err != nil {
		return err
	}
	out := asMap(task["outputSchema"])
	totalVar, _ := cfg["totalScoreVariable"].(string)
	if totalVar == "" {
		totalVar = "total_score"
		cfg["totalScoreVariable"] = totalVar
	}
	if _, has := out[totalVar]; !has {
		out[totalVar] = map[string]any{
			"type":        "number",
			"title":       humanizeKey(totalVar),
			"description": "Total score from the scorecard",
		}
	}
	// Runtime defaults breakdownVariable to 'score_breakdown'; mirror that so
	// downstream Hub mapping pickers see the breakdown output even when the
	// agent didn't set the field explicitly.
	breakdown, _ := cfg["breakdownVariable"].(string)
	if breakdown == "" {
		breakdown = "score_breakdown"
		cfg["breakdownVariable"] = breakdown
	}
	if _, has := out[breakdown]; !has {
		out[breakdown] = map[string]any{
			"type":        "object",
			"title":       humanizeKey(breakdown),
			"description": "Per-field score breakdown",
		}
	}
	task["scorecardConfig"] = cfg
	task["outputSchema"] = out

	// Mirror top-level inputMappings into scorecardConfig.inputMappings when
	// the nested map is empty -- the scorecard activity reads its inputs
	// from the nested map specifically (see graph_workflow.py
	// _resolve_task_variables, the "if scorecardConfig in task_logic" branch).
	// Without this every scorecard rule reads a None field and the total
	// score is 0.
	mirrorNestedInputMappings(task, cfg, "scorecardConfig")
	// Derive top-level inputSchema from inputMappings keys so the Hub UI
	// scorecard properties panel renders correctly. Without this the panel
	// expands to nothing even though inputMappings has the right wiring.
	mirrorMappingsToInputSchema(task)
	return nil
}

// normalizeRuleTreeTask validates ruleTreeConfig and pre-fills outputSchema
// with the configured outputVariable using the configured outputType.
func normalizeRuleTreeTask(c *client.Client, task map[string]any, opts *composeNormalizeOpts, dryRun bool) error {
	predictedAlias := ""
	if opts != nil {
		predictedAlias = opts.PredictedAlias
	}
	cfg := asMap(task["ruleTreeConfig"])
	id, _ := cfg["ruleTreeId"].(string)
	code, _ := cfg["ruleTreeCode"].(string)
	if id == "" && code == "" {
		return fmt.Errorf("rule-tree task requires ruleTreeConfig with ruleTreeId or ruleTreeCode")
	}
	outVar, _ := cfg["outputVariable"].(string)
	if outVar == "" {
		return fmt.Errorf("rule-tree task requires ruleTreeConfig.outputVariable")
	}
	outType, _ := cfg["outputType"].(string)
	switch outType {
	case "string", "number", "boolean":
	case "":
		outType = "string"
		cfg["outputType"] = outType
		task["ruleTreeConfig"] = cfg
	default:
		return fmt.Errorf("rule-tree task ruleTreeConfig.outputType must be one of: string, number, boolean")
	}
	ref := code
	if ref == "" {
		ref = id
	}
	entity, _ := lookupEntity(c, "rule-trees", ref, dryRun)
	if entity == nil && c != nil {
		if err := missingEntityHandler(opts, dryRun, "rule-trees", ref); err != nil {
			return err
		}
	}
	if err := validateEntityWorkflowAliasMatch(entity, predictedAlias, "rule-trees", ref); err != nil {
		return err
	}
	// Pre-fill outputSchema with EVERY canonical output rule_tree_activity
	// emits (see app/temporal/activities/rule_tree_activity.py:172-194):
	//   <outputVariable>                 -> the matched rule's decision_key
	//   <outputVariable>_rule_code       -> code of the matched rule
	//   <outputVariable>_rule_label      -> label of the matched rule
	//   <outputVariable>_rules           -> full results array (one entry per
	//                                       rule with hit/value/alert metadata)
	//   <outputVariable>_alert_created   -> whether any alert fired
	//
	// Without these, downstream conditional/end tasks see only the bare
	// outputVariable in the Hub mapping picker and agents end up guessing
	// names like `decision_key` (the field name on each rule entry, NOT a
	// task output) -- which then doesn't resolve at runtime because
	// task_outputs.<alias>.decision_key isn't a key the activity ever wrote.
	out := asMap(task["outputSchema"])
	canonical := map[string]map[string]any{
		outVar: {
			"type":        outType,
			"title":       humanizeKey(outVar),
			"description": "Decision key of the matched rule",
		},
		outVar + "_rule_code": {
			"type":        "string",
			"title":       humanizeKey(outVar) + " Rule Code",
			"description": "Code of the matched rule (or 'default' if none matched)",
		},
		outVar + "_rule_label": {
			"type":        "string",
			"title":       humanizeKey(outVar) + " Rule Label",
			"description": "Label of the matched rule",
		},
		outVar + "_rules": {
			"type":        "array",
			"title":       humanizeKey(outVar) + " Rules",
			"description": "Per-rule evaluation results (hit, value, alert metadata)",
		},
		outVar + "_alert_created": {
			"type":        "boolean",
			"title":       humanizeKey(outVar) + " Alert Created",
			"description": "Whether any alert was emitted by a hitting rule",
		},
	}
	for k, v := range canonical {
		if _, has := out[k]; !has {
			out[k] = v
		}
	}
	task["outputSchema"] = out

	// Mirror the top-level inputMappings into ruleTreeConfig.inputMappings
	// when the nested map is empty. The rule-tree activity reads its inputs
	// from ruleTreeConfig.inputMappings specifically (see graph_workflow.py
	// _resolve_task_variables -- it has dedicated handling for the nested
	// scorecardConfig.inputMappings and ruleTreeConfig.inputMappings); a
	// top-level entry of `{total_score: "task_outputs.scorecard.total_score"}`
	// is invisible to the rule-tree at execute time unless mirrored. Without
	// this, agents have to remember to fill BOTH maps, and the rule-tree's
	// conditions read nothing -- silently making every rule a no-op.
	mirrorNestedInputMappings(task, cfg, "ruleTreeConfig")
	// Derive top-level inputSchema from inputMappings keys for Hub UI
	// rendering (see mirrorMappingsToInputSchema docstring).
	mirrorMappingsToInputSchema(task)
	return nil
}

// mirrorNestedInputMappings keeps the task's top-level inputMappings and
// the nested config's inputMappings (scorecardConfig / ruleTreeConfig) in
// sync. The runtime requires BOTH:
//   - top-level inputMappings -> the Hub UI properties panel renders from
//     this; an empty top-level shows the task as a blank panel
//   - nested config.inputMappings -> the scorecard/rule-tree activities
//     resolve their per-rule field references from this specifically
//     (graph_workflow._resolve_task_variables has dedicated branches that
//     skip the top-level for these task types)
//
// Mirroring is symmetric and missing-only: each side fills from the other
// if it's empty, preserving caller-supplied values on either side. Agents
// writing inputMappings under either spelling get a working task without
// having to remember to mirror by hand.
func mirrorNestedInputMappings(task map[string]any, cfg map[string]any, cfgKey string) {
	topLevel := asMap(task["inputMappings"])
	nested := asMap(cfg["inputMappings"])
	if len(topLevel) == 0 && len(nested) == 0 {
		return
	}
	// Nested -> top-level when top-level is empty (the agent wrote
	// inputMappings only under the nested config -- common when copying
	// from schema-guide examples that document the nested form).
	if len(topLevel) == 0 && len(nested) > 0 {
		mirror := map[string]any{}
		for k, v := range nested {
			mirror[k] = v
		}
		task["inputMappings"] = mirror
	}
	// Top-level -> nested when nested is empty (the agent wrote at the
	// top level only -- common when treating these like any other task).
	if len(nested) == 0 && len(topLevel) > 0 {
		mirror := map[string]any{}
		for k, v := range topLevel {
			mirror[k] = v
		}
		cfg["inputMappings"] = mirror
		task[cfgKey] = cfg
	}
}

// normalizeEntityWriteTask covers customer / deal / asset task types. Fills
// gaps between what the canonical Hub-shaped body looks like and what an
// agent typically writes from the schema-guide:
//   - sourcesConfig[].type = "identity_key" -> "identity" (runtime checks
//     `source["type"] == "identity"` literally; "identity_key" is silently
//     dropped at runtime because the canonical entity_activity branch
//     filter doesn't match it, so the borrower never gets the identity
//     stamped on it)
//   - For operation=write, pre-fill inputSchema.persona with the default
//     ("individual") + required=true so CreateBorrower's strict Literal
//     validator passes without the agent having to thread persona through
//     every spec
//   - Pre-fill outputSchema with the lookup key + borrower_id so downstream
//     tasks see them in mapping pickers
//
// Symmetric for customer/deal/asset because all three share CustomerTaskData /
// DealTaskData / AssetTaskData schemas and the same operation/lookupBy/key
// shape.
func normalizeEntityWriteTask(task map[string]any) error {
	taskType, _ := task["type"].(string)

	for i, s := range asSlice(task["sourcesConfig"]) {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := sm["type"].(string); t == "identity_key" {
			sm["type"] = "identity"
			// The runtime resolves the identity value from
			// context[<key>] (see entity_activity.handle_config_sources_key_value).
			// A literal `value: "{{inputs.tax_id}}"` from the spec is
			// redundant -- worse, it persists in the body and shows up in
			// the Hub source picker as a stale template. Strip it so the
			// persisted shape matches what the UI editor produces.
			delete(sm, "value")
			task["sourcesConfig"].([]any)[i] = sm
		}
	}

	operation, _ := task["operation"].(string)
	if operation == "write" {
		inSchema := asMap(task["inputSchema"])
		persona := asMap(inSchema["persona"])
		if _, has := persona["type"]; !has {
			persona["type"] = "string"
		}
		if _, has := persona["default"]; !has {
			persona["default"] = "individual"
		}
		if _, has := persona["title"]; !has {
			persona["title"] = "Type of customer"
		}
		if _, has := persona["required"]; !has {
			persona["required"] = true
		}
		inSchema["persona"] = persona
		task["inputSchema"] = inSchema

		// Wire persona from the workflow's inputs scope. compose's
		// preflight auto-adds `inputVariables.persona` (default
		// "individual") to the workflow when an entity write is present,
		// so this resolves at runtime without the agent having to thread
		// persona through the spec. Without this wiring, the runtime
		// entity_activity reads context.get("persona") -> None and the
		// new-borrower path crashes with a Pydantic Literal validator
		// error -- the inputSchema above describes the *shape* of the
		// resolved context, not the wiring that populates it. Caller-
		// supplied inputMappings.persona always wins.
		mappings := asMap(task["inputMappings"])
		if _, has := mappings["persona"]; !has {
			mappings["persona"] = "inputs.persona"
		}
		task["inputMappings"] = mappings

		out := asMap(task["outputSchema"])
		if _, has := out["borrower_id"]; !has {
			out["borrower_id"] = map[string]any{
				"type":        "string",
				"description": "ID of the " + taskType,
			}
		}
		// BC coalesces identityKey -> key on ingest (CreateTaskV2 pre-validator),
		// but compose runs before ingest, so we have to honor both spellings
		// here -- otherwise a spec using the agent-friendly `identityKey` loses
		// the outputSchema entry for the lookup field.
		key, _ := task["key"].(string)
		if key == "" {
			key, _ = task["identityKey"].(string)
		}
		if key != "" {
			if _, has := out[key]; !has {
				out[key] = map[string]any{
					"type":        "string",
					"description": humanizeKey(key),
				}
			}
		}
		task["outputSchema"] = out
	}
	return nil
}
