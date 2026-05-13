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

// childInputVariablesCache memoizes /v2/workflows/<alias>/latest within a
// single compose run for the single remaining purpose: surfacing a stderr
// warning when a single-mode child-workflow has required inputs that the
// parent task's inputMappings doesn't cover. Schema fill is now done
// server-side at GET /v2/tasks time (DerivedSchemaService) so the CLI no
// longer mirrors child inputSchema / outputSchema into the parent body.
var childInputVariablesCache = map[string]map[string]any{}

func lookupChildInputVariables(c *client.Client, alias string, dryRun bool) (map[string]any, error) {
	if cached, ok := childInputVariablesCache[alias]; ok {
		return cached, nil
	}
	if c == nil {
		if dryRun {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("no client available for child-workflow lookup")
	}
	data, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+alias+"/latest", nil)
	if err != nil {
		if dryRun {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("look up child workflow %q: %w", alias, err)
	}
	var wf map[string]any
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse child workflow %q: %w", alias, err)
	}
	iv := asMap(wf["inputVariables"])
	childInputVariablesCache[alias] = iv
	return iv, nil
}

// normalizeChildWorkflowTask warns when a single-mode child-workflow's
// required inputs aren't covered by the parent's inputMappings. The
// inputSchema / outputSchema fill that this normalizer used to perform now
// lives in BC's DerivedSchemaService and surfaces at GET /v2/tasks time as
// ``derivedSchema``. preflightTasks already renamed executorAlias ->
// executorId; by the time this runs the alias lives on ``executorId``.
func normalizeChildWorkflowTask(c *client.Client, task map[string]any, dryRun bool) error {
	executor, _ := task["executorId"].(string)
	if executor == "" {
		return nil
	}
	runInBatch, _ := task["runInBatch"].(bool)
	// Coverage warning only applies to single-mode dispatch; batch mode
	// reads inputs from the resolved inputExpression, not inputMappings.
	if runInBatch {
		return nil
	}
	inputVariables, err := lookupChildInputVariables(c, executor, dryRun)
	if err != nil {
		return err
	}
	mappings := asMap(task["inputMappings"])
	missing := make([]string, 0)
	for name, raw := range inputVariables {
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
	return nil
}

// normalizeAltdataTask normalises altdata-enrichment task bodies:
// stamps default packageAlias / dataAge on each sourcesConfig entry and
// fills the runtime defaults (mode, savePackages, timeout). The
// inputKeys auto-fill has moved to BC's DerivedInputKeysService and is
// exposed at GET /v2/tasks time via the ``derivedInputKeys`` field.
// Authored ``inputKeys`` on the task body are still respected by the
// runtime activity -- the derivation is a display-only fallback.
func normalizeAltdataTask(c *client.Client, task map[string]any, dryRun bool) error {
	sources := asSlice(task["sourcesConfig"])
	if len(sources) == 0 {
		return nil
	}

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

	if _, has := task["mode"]; !has {
		task["mode"] = "single"
	}
	if _, has := task["savePackages"]; !has {
		task["savePackages"] = true
	}
	if _, has := task["timeout"]; !has {
		task["timeout"] = 60
	}
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

// normalizeConditionalTask fills in defaults (branch ids, order, label) and
// shuffles the isElse branch to the end of the slice. The legacy-shape
// rejections (branch.expression, branch.is_else snake_case, missing isElse,
// missing structured conditions) now live server-side on
// CreateTaskV2._reject_legacy_conditional_shapes -- a direct POST to
// /v2/tasks with any of those shapes is a 400 either way, so the CLI
// duplicating the check stopped earning its keep. We still validate the
// conditions object shape here so compose can surface a clear error before
// the network call, but the legacy-rewrite branches are gone.
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
// outputSchema entries when these tasks are placed on a canvas. The
// outputSchema derivation now lives server-side in BC's
// DerivedSchemaService; compose only runs existence + workflow-scope
// checks here.

// entityCache memoizes /v1/{resource} lookups within a compose run, one
// entry per (resource, codeOrId).
var entityCache = map[string]map[string]any{}

// lookupEntity fetches /v1/{resource}?code=<codeOrId> (best-effort) and
// returns the first matching record. Used for non-blocking warnings when
// a task references something that doesn't exist on the tenant and for
// the workflow-alias-scope check (see validateEntityWorkflowAliasMatch).
//
// The lookup runs in dry-run too -- skipping it makes dry-run preview a
// different shape than real compose, defeating the purpose of dry-run.
// The dryRun flag is kept in the signature for symmetry with sibling
// normalizers and so callers can opt in to dry-run-only printf behavior.
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
// of {ruleCode, ruleId} references and verifies each referenced entity
// exists on the tenant (warning by default, hard error with --publish).
// The canonical alerts/alerts_count outputSchema is now derived server-side
// at GET /v2/tasks time (see BC's DerivedSchemaService).
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
	return nil
}

// normalizeMappingTableTask validates mappingTableConfig.entries[] and
// stamps a stable UUID on entries that omit `id` (the runtime
// MappingTableEntry pydantic model requires it). The per-entry outputSchema
// fill (3 canonical outputs per entry) has moved to BC's
// DerivedSchemaService and is exposed at GET /v2/tasks time via the
// ``derivedSchema`` field. Top-level inputMappings mirroring has likewise
// moved to BC's DerivedMappingEntriesService and is exposed via the
// ``derivedMappingEntries`` field -- the runtime activity reads
// entries[].inputVariable directly, so the mirror was never required for
// execution, only for the Hub's mapping picker.
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
		// Best-effort entity existence + workflow-scope checks (the
		// remaining purposes of lookupEntity now that schema derivation
		// has moved server-side).
		entity, _ := lookupEntity(c, "mapping-tables", ref, dryRun)
		if entity == nil && c != nil {
			if err := missingEntityHandler(opts, dryRun, "mapping-tables", ref); err != nil {
				return fmt.Errorf("entries[%d]: %w", i, err)
			}
		}
		if err := validateEntityWorkflowAliasMatch(entity, predictedAlias, "mapping-tables", ref); err != nil {
			return fmt.Errorf("entries[%d]: %w", i, err)
		}
		entries[i] = em
	}
	cfg["entries"] = entries
	task["mappingTableConfig"] = cfg
	// Top-level inputMappings mirroring has moved to BC's
	// DerivedMappingEntriesService (exposed at GET /v2/tasks via
	// ``derivedMappingEntries``). The runtime activity reads
	// entries[].inputVariable directly, so this mirror was always a
	// display-only concern -- the Hub picker now consumes the derived
	// field instead of relying on the CLI to pre-fill the persisted body.
	return nil
}

// normalizeScorecardTask validates that scorecardConfig references an
// existing /v1/scorecards entity, defaults totalScoreVariable /
// breakdownVariable when omitted, and mirrors top-level inputMappings into
// the nested scorecardConfig.inputMappings (the runtime activity reads its
// per-rule inputs from the nested map specifically). outputSchema fill has
// moved to BC's DerivedSchemaService and is exposed at GET /v2/tasks time
// via the ``derivedSchema`` field.
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
	// Default totalScoreVariable / breakdownVariable when omitted so the
	// task body is self-consistent (the runtime activity reads them from
	// here). BC's DerivedSchemaService picks up these values to build the
	// derivedSchema response shape.
	if v, _ := cfg["totalScoreVariable"].(string); v == "" {
		cfg["totalScoreVariable"] = "total_score"
	}
	if v, _ := cfg["breakdownVariable"].(string); v == "" {
		cfg["breakdownVariable"] = "score_breakdown"
	}
	task["scorecardConfig"] = cfg

	// Mirror top-level inputMappings into scorecardConfig.inputMappings when
	// the nested map is empty -- the scorecard activity reads its inputs
	// from the nested map specifically (see graph_workflow.py
	// _resolve_task_variables, the "if scorecardConfig in task_logic" branch).
	// Without this every scorecard rule reads a None field and the total
	// score is 0.
	mirrorNestedInputMappings(task, cfg, "scorecardConfig")
	return nil
}

// normalizeRuleTreeTask validates ruleTreeConfig and mirrors top-level
// inputMappings into the nested ruleTreeConfig.inputMappings. The
// outputSchema fill (outputVariable + 4 canonical companions) has moved to
// BC's DerivedSchemaService and is exposed at GET /v2/tasks time via the
// ``derivedSchema`` field.
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
		cfg["outputType"] = "string"
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
