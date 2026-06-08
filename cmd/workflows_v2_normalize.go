package cmd

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
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
	// Note: the altdata-enrichment empty-inputKeys case is no longer rejected
	// here. The create path (deriveAltdataInputKeysForCreate) synthesizes
	// inputKeys from each source's required inputFields, and only falls back to
	// errAltdataMissingInputKeys when that derivation fails.
	return nil
}

// errAltdataMissingInputKeys is the fallback error surfaced when an
// altdata-enrichment create body has sources but no inputKeys AND the CLI
// could not derive them (e.g. a source lookup network error). The happy path
// derives inputKeys automatically; this only fires when derivation is
// impossible.
func errAltdataMissingInputKeys(cause error) error {
	return fmt.Errorf(
		"altdata-enrichment task with non-empty sourcesConfig but empty inputKeys, and the CLI could not derive them (%w) -- "+
			"the Hub UI will show an unwired source. Run 'altscore workflows-v2 sources-status --filter id=<SOURCE_ID>' "+
			"to see the source's required inputFields, then add an inputKeys entry per field, "+
			`e.g. inputKeys: {"personId": "{{personId}}"}.`, cause)
}

// deriveAltdataInputKeysForCreate auto-fills inputKeys on an altdata-enrichment
// `tasks-v2 create` / `create-version` body that supplies sourcesConfig but
// omits inputKeys. It reuses the same per-source inputFields lookup the apply
// path uses (lookupAltdataSourceInputFields) and writes one inputKeys entry per
// required field. The body is normalized in place. Non-altdata bodies, bodies
// without sources, and bodies that already carry inputKeys are left untouched.
// On a source-lookup failure it returns errAltdataMissingInputKeys so the
// caller surfaces the clear "add inputKeys" guidance instead of shipping an
// unwired task.
func deriveAltdataInputKeysForCreate(c *client.Client, body *json.RawMessage) error {
	if body == nil || len(*body) == 0 {
		return nil
	}
	var task map[string]any
	if err := json.Unmarshal(*body, &task); err != nil {
		return nil
	}
	if t, _ := task["type"].(string); t != "altdata-enrichment" {
		return nil
	}
	sources := asSlice(task["sourcesConfig"])
	if len(sources) == 0 {
		return nil
	}
	if len(asMap(task["inputKeys"])) > 0 {
		return nil
	}

	inputKeys := map[string]any{}
	seen := map[string]bool{}
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
		fields, err := lookupAltdataSourceInputFields(c, sid, ver, false)
		if err != nil {
			return errAltdataMissingInputKeys(fmt.Errorf("source %s %s lookup failed: %w", sid, ver, err))
		}
		for _, f := range fields {
			if seen[f] {
				continue
			}
			seen[f] = true
			inputKeys[f] = "{{" + f + "}}"
		}
	}
	if len(inputKeys) == 0 {
		return errAltdataMissingInputKeys(fmt.Errorf("no required inputFields found on the configured source(s)"))
	}
	task["inputKeys"] = inputKeys
	rewritten, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("re-encode task body with derived inputKeys: %w", err)
	}
	*body = json.RawMessage(rewritten)
	fmt.Fprintf(os.Stderr, "# derived inputKeys from source inputFields: %v\n", sortedKeys(seen))
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
	// Publish -- whether the caller passed --publish. When true,
	// "referenced entity missing on tenant" is promoted from a stderr
	// warning to a hard error. A published workflow that references a
	// missing scorecard / rule-tree / mapping-table / evaluation-rule
	// will fail 100% of its executions at runtime ("Scorecard not found:
	// X"), and the failure surfaces only when someone tries to actually
	// run the workflow -- often hours or days after apply. Catching it
	// at publish time turns a runtime mystery into a clear apply error.
	Publish bool
	// AutoRescopeEntities -- when true, validateEntityWorkflowAliasMatch
	// downgrades to a stderr warning (so the per-entity rescope loop in
	// apply can re-stamp the entity after task creation). When false (the
	// default, matching legacy compose behavior), the mismatch is a hard
	// error so the agent fixes it explicitly. Apply sets this true unless
	// --skip-rescope is passed; non-apply callers leave it false.
	//
	// IMPORTANT: AutoRescopeEntities only governs the case where the
	// entity's current workflowAlias is EMPTY (unscoped) or already MATCHES
	// the target. When the entity is currently owned by a DIFFERENT
	// workflow, validateEntityWorkflowAliasMatch always returns a hard
	// error unless AllowStealOwnership is also set -- each v2 workflow
	// owns its credit-decisioning entities 1:1, and silently re-stamping
	// would steal ownership from the previous workflow and break its Hub
	// element panel.
	AutoRescopeEntities bool
	// AllowStealOwnership -- opt-in escape hatch for the rare case where
	// the user really does want apply to transfer an entity's ownership
	// from another workflow to this one (workflow rename / identity
	// migration / decommissioning the old owner). When false (default),
	// apply refuses to re-stamp an entity whose workflowAlias points at
	// another workflow and instructs the spec author to clone the entity
	// with a new code instead. When true, the legacy "silently re-stamp"
	// behavior is restored. Only apply's --allow-steal-ownership flag
	// sets this; non-apply callers leave it false.
	AllowStealOwnership bool
	// AutoDefaults -- when true (the default; disabled by --no-auto-defaults),
	// apply injects opinionated convenience defaults: end-node borrower_id /
	// billable_id wiring + forced PDF generation (see applyAutoEndDefaults),
	// and deal-contact identity_value back-fill (see normalizeEntityWriteTask).
	// Each is only applied when the field is absent -- caller-supplied values
	// always win.
	AutoDefaults bool
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
// referenceable (the apply pipeline still wires it correctly and the
// runtime resolves it), but invisible to the editor, so a human
// reviewing the workflow in the Hub sees an unconfigured-looking task
// that secretly pulls from another workflow's entity. That cross-workflow
// drift is hard to undo once it spreads.
//
// Allowed: entity has no workflowAlias (global / shared by intent) OR
// the lookup didn't find anything (lookupEntity already warned, no
// scope to check) OR we don't know the predicted alias (compose context
// missing -- non-apply callers).
//
// Cross-ownership doctrine: each v2 workflow owns its credit-decisioning
// entities 1:1. When an entity's current workflowAlias is non-empty and
// belongs to ANOTHER workflow, apply refuses to re-stamp it -- silently
// stealing ownership would break the previous owner's Hub element panel
// (entities only appear in the panel when their workflowAlias matches).
// The only paths forward are (a) clone the entity with a new code so each
// workflow has its own copy, or (b) pass --allow-steal-ownership when
// the user truly wants to transfer ownership (rare: workflow rename /
// identity migration / decommissioning the old owner).
//
// Decision matrix:
//   - actual == ""             -> OK (entity is unscoped, apply will claim it)
//   - actual == predictedAlias -> OK (entity already belongs here)
//   - actual != predictedAlias AND AllowStealOwnership=true   -> warning + auto-rescope
//   - actual != predictedAlias AND AutoRescopeEntities=true   -> hard error (steal refused)
//   - actual != predictedAlias AND AutoRescopeEntities=false  -> hard error (legacy)
func validateEntityWorkflowAliasMatch(opts *composeNormalizeOpts, entity map[string]any, predictedAlias, resourceKind, ref string) error {
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
	// Cross-owned: another workflow owns this entity. Refuse to re-stamp
	// unless the user explicitly opted in via --allow-steal-ownership.
	if opts != nil && opts.AllowStealOwnership {
		fmt.Fprintf(os.Stderr,
			"# warning: %s %q is owned by workflowAlias=%q -- --allow-steal-ownership was set, apply will transfer ownership to %q after task creation\n",
			resourceKind, ref, actual, predictedAlias)
		return nil
	}
	return fmt.Errorf(
		"%s %q (code=%q) is currently owned by workflow %q, but this apply targets workflow %q. "+
			"Each v2 workflow owns its credit-decisioning entities 1:1 -- silently re-stamping would "+
			"steal ownership from %q and make the entity disappear from its Hub elements panel. "+
			"Fix: clone the entity with a new code dedicated to %q, then update your spec to reference "+
			"the new code. Example:\n"+
			"    altscore %s get %s > /tmp/clone.json\n"+
			"    # edit /tmp/clone.json: set \"code\" to a fresh value (e.g. %q) and \"workflowAlias\" to %q\n"+
			"    altscore %s create --body @/tmp/clone.json\n"+
			"If you really do want to transfer ownership of %q from %q to %q (rare: workflow rename, "+
			"identity migration, decommissioning the old owner), re-run apply with --allow-steal-ownership.",
		resourceKind, ref, ref, actual, predictedAlias,
		actual,
		predictedAlias,
		resourceKind, id,
		predictedAlias+"-"+ref, predictedAlias,
		resourceKind,
		ref, actual, predictedAlias)
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
		return normalizeEntityWriteTask(task, opts)
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
// “derivedSchema“. preflightTasks already renamed executorAlias ->
// executorId; by the time this runs the alias lives on “executorId“.
func normalizeChildWorkflowTask(c *client.Client, task map[string]any, dryRun bool) error {
	executor, _ := task["executorId"].(string)
	if executor == "" {
		return nil
	}
	runInBatch, _ := task["runInBatch"].(bool)
	// Coverage warning only applies to single-mode dispatch; batch mode
	// reads inputs from the resolved inputExpression, not inputMappings.
	if runInBatch {
		// In batch mode, every item in the upstream array auto-binds
		// to the child workflow's input variables by field name (no
		// inputMappings needed). A common trap is writing a mapping
		// like {"borrower_id": "borrower_id"} -- the runtime's
		// resolver treats the value as a path expression, fails the
		// path-separator check, and 400s with a cryptic error. Auto-
		// clear bare-name mappings here with a stderr warning so the
		// spec ships clean.
		mappings := asMap(task["inputMappings"])
		if len(mappings) == 0 {
			return nil
		}
		bareNames := make([]string, 0)
		validMappings := map[string]any{}
		for name, raw := range mappings {
			val, _ := raw.(string)
			// A valid mapping value is either a task-output path or an
			// inputs.* reference (both have a dot). Bare field names
			// (no dot) are the trap case.
			if val != "" && !strings.Contains(val, ".") {
				bareNames = append(bareNames, name)
				continue
			}
			validMappings[name] = raw
		}
		if len(bareNames) > 0 {
			// Prefer the alias when ref was stripped by the dispatcher;
			// fall back to executor so the warning always names something
			// the user can grep for.
			label := localRef(task, "")
			if label == "" {
				label, _ = task["alias"].(string)
			}
			if label == "" {
				label = executor
			}
			fmt.Fprintf(os.Stderr,
				"# warning: child-workflow %q runInBatch=true has bare-name inputMappings %v. "+
					"In batch mode, every item dict's fields auto-bind to the child's inputs by name; "+
					"a mapping like {\"x\": \"x\"} fails at runtime with a path-separator error. "+
					"Dropping the bare entries; ensure your item dicts carry these fields directly.\n",
				label, bareNames)
			task["inputMappings"] = validMappings
		}
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

func normalizeAltdataTask(c *client.Client, task map[string]any, dryRun bool) error {
	sources := asSlice(task["sourcesConfig"])
	if len(sources) == 0 {
		return nil
	}

	inputKeys := asMap(task["inputKeys"])

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

	// inputKeys auto-fill (different concern from inputSchema fill that
	// moved to BC's DerivedSchemaService): the runtime activity reads the
	// per-source required fields from inputKeys, and an agent that omits
	// inputKeys ends up with an unwired source the first time someone
	// runs the workflow. Look the source up once per id and wire one
	// entry per required field. inputSchema is no longer touched here --
	// it's computed server-side at GET /v2/tasks time.
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

		// Validate every referenced source against the union of source
		// catalogs, regardless of whether inputKeys were hand-supplied --
		// the runtime can only serve sources present in one of them, so a
		// definitive miss is a hard error (usually a typo). Transient/dry-run
		// lookup failures are tolerated; the backend revalidates on save.
		fields, err := lookupAltdataSourceInputFields(c, sid, ver, dryRun)
		if err != nil {
			if errors.Is(err, errSourceNotFound) {
				return fmt.Errorf("source %s %s not found in sources-status or external-sources-status "+
					"(verify with 'altscore workflows-v2 sources-status' / 'external-sources-status')", sid, ver)
			}
			fmt.Fprintf(os.Stderr,
				"# warning: could not validate source %s %s (%v); proceeding -- the backend revalidates on save\n",
				sid, ver, err)
			continue
		}
		if !derivedInputKeys {
			continue
		}
		for _, f := range fields {
			if seenInput[f] {
				continue
			}
			seenInput[f] = true
			inputKeys[f] = "{{" + f + "}}"
		}
	}
	if derivedInputKeys && len(inputKeys) > 0 {
		task["inputKeys"] = inputKeys
	}

	// Populate outputSchema from each source's declared output metadata.
	// BC's DerivedSchemaService computes a parallel derivedSchema.output at
	// GET time, but the Hub UI's variable-mapping pickers only read the
	// persisted outputSchema field -- CLI-created altdata tasks otherwise
	// show "no outputs" in downstream task editors until the user re-saves
	// in the Hub. Match the EXACT wrap the Hub itself applies when it falls
	// back from outputSchema to derivedSchema (see
	// altscore-ai-chat/lib/workflow-outputs/calculate-available-outputs.ts):
	//
	//   outputSchema[sourceId] = {
	//     type: "object",
	//     title: sourceId,
	//     properties: <catalog[sourceId]>,   // the {data, sourceData} blob
	//   }
	//
	// This shape satisfies BC's Task.outputSchema validator
	// (Dict[str, SchemaTypes] -> ObjectSchema for each source entry) AND
	// yields the same downstream variable paths the Hub generates from its
	// derivedSchema fallback, so users who already mapped against derived
	// paths don't see surprising differences once outputSchema is persisted.
	// User-supplied entries on the task body win on collision.
	outputSchema := asMap(task["outputSchema"])
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
		if _, has := outputSchema[sid]; has {
			// User-supplied entry wins.
			continue
		}
		catalogOutput, err := lookupAltdataSourceOutputSchema(c, sid, ver, dryRun)
		if err != nil {
			// Best-effort: warn and skip; don't block apply on a single
			// failed source lookup (network blip, unknown source id).
			fmt.Fprintf(os.Stderr,
				"# warning: could not populate outputSchema for source %s %s (%v); the Hub will fall back to derivedSchema\n",
				sid, ver, err)
			continue
		}
		entry := asMap(catalogOutput[sid])
		if len(entry) == 0 {
			// Catalog didn't declare an outputSchema for this source --
			// nothing useful to mirror. Skip; derivedSchema fallback will
			// kick in at the Hub layer if BC computes anything later.
			continue
		}
		outputSchema[sid] = map[string]any{
			"type":       "object",
			"title":      sid,
			"properties": entry,
		}
	}
	if len(outputSchema) > 0 {
		task["outputSchema"] = outputSchema
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

// errSourceNotFound is the sentinel returned when a source-version is absent
// from every source catalog (as opposed to a transport/parse failure). Callers
// use errors.Is to hard-fail on a genuine bad reference while tolerating a
// transient blip.
var errSourceNotFound = errors.New("source not found in any catalog")

// sourceStatusCatalogs are the endpoints whose UNION defines every source an
// altdata-enrichment node may reference. The runtime serves both: a source
// routes to ConfigurableHttpGateway when an ExternalSourceConfig exists
// (surfaced by external-sources-status), otherwise to the AltData microservice
// (sources-status). Microservice is listed first so its entry wins on the rare
// chance an id exists in both catalogs.
var sourceStatusCatalogs = []string{
	"/v2/workflows/sources-status?per-page=200",
	"/v2/workflows/external-sources-status",
}

// sourceCatalogListCache memoizes each catalog endpoint's full response within
// a single compose run, so resolving N sources hits each endpoint at most once.
var sourceCatalogListCache = map[string][]map[string]any{}

// altdataSourceStatusCache memoizes resolved per-source-version entries within
// a single compose run. Both inputFields (for inputKeys auto-fill) and
// outputSchema (mirrored onto the task body for the Hub's variable-mapping
// pickers) come from the same entry. A nil value means "looked up and
// definitively absent from every catalog"; stored to short-circuit retries.
var altdataSourceStatusCache = map[string]map[string]any{}

// lookupAltdataSourceStatus resolves a single source-version's status entry
// against the union of the source catalogs (see sourceStatusCatalogs). One
// resolution per (sourceID, version) per compose run. Returns errSourceNotFound
// when the source is absent from every catalog that responded cleanly; surfaces
// the transport error instead if a catalog could not be fetched.
func lookupAltdataSourceStatus(c *client.Client, sourceID, version string, dryRun bool) (map[string]any, error) {
	cacheKey := sourceID + "|" + version
	if cached, ok := altdataSourceStatusCache[cacheKey]; ok {
		if cached == nil {
			return nil, errSourceNotFound
		}
		return cached, nil
	}
	if c == nil {
		if dryRun {
			fmt.Fprintf(os.Stderr, "# (dry-run) skipping live source lookup for %s %s\n", sourceID, version)
			return nil, fmt.Errorf("dry-run: no client for source lookup")
		}
		return nil, fmt.Errorf("no client available for source lookup")
	}

	var transportErr error
	for _, path := range sourceStatusCatalogs {
		sources, err := fetchSourceCatalog(c, path)
		if err != nil {
			transportErr = err
			if dryRun {
				fmt.Fprintf(os.Stderr, "# (dry-run) source catalog fetch failed (%s): %v\n", path, err)
			}
			continue
		}
		for _, s := range sources {
			if sid, _ := s["sourceId"].(string); sid != sourceID {
				continue
			}
			if version != "" {
				if sver, _ := s["sourceVersion"].(string); sver != version {
					continue
				}
			}
			altdataSourceStatusCache[cacheKey] = s
			return s, nil
		}
	}

	// A transport error means we never got a clean "absent" answer from at
	// least one catalog -- don't poison the cache or report a false
	// not-found; surface the real error so the caller can tolerate it.
	if transportErr != nil {
		return nil, transportErr
	}
	altdataSourceStatusCache[cacheKey] = nil
	return nil, errSourceNotFound
}

// fetchSourceCatalog GETs one catalog endpoint and parses it into a list,
// memoized per run. Both catalogs return the same entry shape (sourceId,
// sourceVersion, inputFields, outputSchema).
func fetchSourceCatalog(c *client.Client, path string) ([]map[string]any, error) {
	if cached, ok := sourceCatalogListCache[path]; ok {
		return cached, nil
	}
	data, _, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, err
	}
	var sources []map[string]any
	if err := json.Unmarshal(data, &sources); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	sourceCatalogListCache[path] = sources
	return sources, nil
}

// lookupAltdataSourceInputFields returns the required input field names for
// a source. Used by normalizeAltdataTask to auto-populate inputKeys (a
// runtime wiring concern -- distinct from schema derivation).
func lookupAltdataSourceInputFields(c *client.Client, sourceID, version string, dryRun bool) ([]string, error) {
	s, err := lookupAltdataSourceStatus(c, sourceID, version, dryRun)
	if err != nil {
		// Preserve the existing dry-run stub behavior so compose without
		// network connectivity can still validate inputKeys plumbing.
		if dryRun && c == nil {
			return []string{"personId", "taxId"}, nil
		}
		if dryRun {
			fmt.Fprintf(os.Stderr, "# (dry-run) using stub inputFields for %s %s after lookup error\n", sourceID, version)
			return []string{"personId", "taxId"}, nil
		}
		return nil, err
	}
	fields := asSlice(s["inputFields"])
	fieldNames := make([]string, 0, len(fields))
	for _, f := range fields {
		fm, _ := f.(map[string]any)
		if name, _ := fm["field"].(string); name != "" {
			fieldNames = append(fieldNames, name)
		}
	}
	return fieldNames, nil
}

// lookupAltdataSourceOutputSchema returns the per-source JSON-Schema-shaped
// output map declared by the altdata catalog. The catalog already returns
// outputSchema in the BC DerivedSchemaService-compatible shape, namely
// {<sourceId>: {properties: {<key>: {type, title, ...}}, type: "object",
// title: ...}}, so this passes it through verbatim and the caller merges
// into task["outputSchema"]. Returns nil (not an error) if the catalog
// entry has no outputSchema -- some sources just don't declare one.
func lookupAltdataSourceOutputSchema(c *client.Client, sourceID, version string, dryRun bool) (map[string]any, error) {
	s, err := lookupAltdataSourceStatus(c, sourceID, version, dryRun)
	if err != nil {
		return nil, err
	}
	return asMap(s["outputSchema"]), nil
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
// outputSchema entries when these tasks are placed on a canvas. The
// outputSchema derivation now lives server-side in BC's
// DerivedSchemaService; compose only runs existence + workflow-scope
// checks here.

// entityCache memoizes /v1/{resource} lookups within a compose run, one
// entry per (resource, codeOrId).
var entityCache = map[string]map[string]any{}

// tenantDecisionKeysCache memoizes the GET /v1/decisions result for the
// active client. A nil value after `Fetched=true` means we tried and
// failed; callers should skip the warning quietly in that case.
var tenantDecisionKeysCache map[string]bool
var tenantDecisionKeysFetched bool

// fetchTenantDecisionKeys pulls the tenant's decision-key catalog so we
// can compare a rule's decisionKey against the canonical case-sensitive
// set. Decisions are stored as data-model records under
// /v1/data-models?entity-type=decision; the user-facing case-sensitive
// key is in each record's `key` field. Best-effort: returns nil on any
// error so callers can skip the warning quietly.
func fetchTenantDecisionKeys(c *client.Client) map[string]bool {
	if tenantDecisionKeysFetched {
		return tenantDecisionKeysCache
	}
	tenantDecisionKeysFetched = true
	if c == nil {
		return nil
	}
	data, _, err := c.Do("GET", "borrower_central", "/v1/data-models?entity-type=decision&per-page=200", nil)
	if err != nil {
		return nil
	}
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, d := range arr {
		if k, ok := d["key"].(string); ok && k != "" {
			out[k] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	tenantDecisionKeysCache = out
	return out
}

// warnIfDecisionKeyUnknown checks whether a rule's `decisionKey` matches
// any case-sensitive entry on the tenant's /v1/decisions catalog. Prints
// a stderr warning (non-blocking) on mismatch so compose can finish and
// the user can fix the case before the rule tree fires at run time.
// `context` is a short label (e.g. "rulesConfig[0]") for the warning.
func warnIfDecisionKeyUnknown(c *client.Client, decisionKey, ruleCode, context string) {
	if decisionKey == "" {
		return
	}
	known := fetchTenantDecisionKeys(c)
	if known == nil || known[decisionKey] {
		return
	}
	// Heuristic: detect case-only mismatches so the warning can suggest
	// the exact fix instead of just listing every known key.
	suggestion := ""
	for k := range known {
		if strings.EqualFold(k, decisionKey) {
			suggestion = k
			break
		}
	}
	if suggestion != "" {
		fmt.Fprintf(os.Stderr,
			"# warning: %s rule %q has decisionKey=%q -- tenant /v1/decisions has %q (case differs). "+
				"BC accepts the mismatch on create but the rule tree FAILS at execute time when recording the decision. "+
				"Update the rule: `altscore evaluation-rules update <id> --body '{\"decisionKey\": \"%s\"}'`.\n",
			context, ruleCode, decisionKey, suggestion, suggestion,
		)
		return
	}
	keys := []string{}
	for k := range known {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(os.Stderr,
		"# warning: %s rule %q has decisionKey=%q -- not in the tenant's /v1/decisions catalog. "+
			"BC accepts the mismatch on create but the rule tree FAILS at execute time when recording the decision. "+
			"Valid keys: %v. Run `altscore decisions list` to confirm.\n",
		context, ruleCode, decisionKey, keys,
	)
}

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
		if err := validateEntityWorkflowAliasMatch(opts, entity, predictedAlias, "evaluation-rules", ref); err != nil {
			return fmt.Errorf("rulesConfig[%d]: %w", i, err)
		}
		// Cross-check the rule's decisionKey against the tenant's
		// /v1/decisions catalog. Case mismatches pass create cleanly but
		// blow up at run time -- warn now (non-blocking) so the user can
		// fix the entity before the workflow executes.
		if entity != nil {
			dk, _ := entity["decisionKey"].(string)
			warnIfDecisionKeyUnknown(c, dk, ref, fmt.Sprintf("rulesConfig[%d]", i))
		}
	}
	return nil
}

// normalizeMappingTableTask validates mappingTableConfig.entries[] and
// stamps a stable UUID on entries that omit `id` (the runtime
// MappingTableEntry pydantic model requires it). The per-entry outputSchema
// fill (3 canonical outputs per entry) has moved to BC's
// DerivedSchemaService and is exposed at GET /v2/tasks time via the
// “derivedSchema“ field. Top-level inputMappings mirroring is preserved
// because it controls runtime wiring, not schema display.
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
		if err := validateEntityWorkflowAliasMatch(opts, entity, predictedAlias, "mapping-tables", ref); err != nil {
			return fmt.Errorf("entries[%d]: %w", i, err)
		}
		entries[i] = em
	}
	cfg["entries"] = entries
	task["mappingTableConfig"] = cfg

	// Mirror per-entry inputVariables into the task's top-level inputMappings.
	// The runtime activity resolves entries[].inputVariable directly; the
	// mirroring keeps a stale Hub-UI rendering quirk happy (the panel shows
	// "N entries" but expanded properties read from top-level
	// inputMappings). Mirroring is per-field-name extracted from
	// inputVariable's last dotted segment.
	mirrorEntryInputsToTopLevel(task, entries)
	return nil
}

// mirrorEntryInputsToTopLevel ensures the task's top-level inputMappings
// reflect every entry's inputVariable. The runtime activity resolves
// entries[].inputVariable directly, but keeping inputMappings in sync at
// the top level matches the Hub editor's persisted shape. Caller-supplied
// entries in inputMappings are preserved. inputSchema is no longer touched
// here -- derived server-side at GET /v2/tasks time.
func mirrorEntryInputsToTopLevel(task map[string]any, entries []any) {
	mappings := asMap(task["inputMappings"])
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
	}
	task["inputMappings"] = mappings
}

// normalizeScorecardTask validates that scorecardConfig references an
// existing /v1/scorecards entity, defaults totalScoreVariable /
// breakdownVariable when omitted, and mirrors top-level inputMappings into
// the nested scorecardConfig.inputMappings (the runtime activity reads its
// per-rule inputs from the nested map specifically). outputSchema fill has
// moved to BC's DerivedSchemaService and is exposed at GET /v2/tasks time
// via the “derivedSchema“ field.
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
	if err := validateEntityWorkflowAliasMatch(opts, entity, predictedAlias, "scorecards", ref); err != nil {
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
// “derivedSchema“ field.
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
	if err := validateEntityWorkflowAliasMatch(opts, entity, predictedAlias, "rule-trees", ref); err != nil {
		return err
	}
	// Cross-check the decisionKey on every rule the tree references. The
	// rule tree's runtime contract is: pick the first matching rule and
	// record its decisionKey on the execution. A case-mismatched key
	// blows up at that recording step -- warn here so the agent can fix
	// the rule entity before the workflow ever fires.
	if entity != nil {
		if rules, ok := entity["rules"].([]any); ok {
			for i, raw := range rules {
				rm, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				rcode, _ := rm["ruleCode"].(string)
				rid, _ := rm["ruleId"].(string)
				rref := rcode
				if rref == "" {
					rref = rid
				}
				if rref == "" {
					continue
				}
				ruleEntity, _ := lookupEntity(c, "evaluation-rules", rref, dryRun)
				if ruleEntity == nil {
					continue
				}
				dk, _ := ruleEntity["decisionKey"].(string)
				warnIfDecisionKeyUnknown(c, dk, rref, fmt.Sprintf("rule-tree %q rules[%d]", ref, i))
			}
		}
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
func normalizeEntityWriteTask(task map[string]any, opts *composeNormalizeOpts) error {
	if opts == nil {
		opts = &composeNormalizeOpts{}
	}
	taskType, _ := task["type"].(string)

	// Auto-fill each inline deal contact's identity_value so the contact's
	// borrower upsert keys on a real value. The deal-contact upsert resolves
	// (or creates) the borrower by identity_key (default "tax_id") + identity_value;
	// a contact that carries only e.g. `tax_id` but no identity_value resolves to a
	// null identity. We copy the value from the field named by identity_key (so the
	// source may itself be an `{{inputs.*}}` or `{{task_outputs.*}}` template),
	// defaulting identity_key to "tax_id". Caller-supplied identity_value always
	// wins. Gated by --no-auto-defaults.
	if taskType == "deal" && opts.AutoDefaults {
		for _, ci := range asSlice(task["contacts"]) {
			contact, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			identityKey, _ := contact["identity_key"].(string)
			if strings.TrimSpace(identityKey) == "" {
				identityKey = "tax_id"
				contact["identity_key"] = identityKey
			}
			if iv, _ := contact["identity_value"].(string); strings.TrimSpace(iv) != "" {
				continue // caller already mapped it
			}
			if src, _ := contact[identityKey].(string); strings.TrimSpace(src) != "" {
				contact["identity_value"] = src
			} else {
				fmt.Fprintf(os.Stderr,
					"# warning: deal contact id=%v has no identity_value and no %q field to source it from; "+
						"the contact's borrower will resolve to a null identity\n",
					contact["id"], identityKey)
			}
		}
	}

	// Track deal_contact (singular) and deal_contacts (plural) keys so we
	// can warn when both forms are present pointing at different context
	// keys -- the singular accumulates one contact per task run while the
	// plural fans out over a list, and mixing them on the same deal task
	// usually means the agent meant to use only the plural form.
	var dealContactKey, dealContactsKey string

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
		if taskType == "deal" {
			srcType, _ := sm["type"].(string)
			switch srcType {
			case "deal_contacts":
				// Plural form: runtime reads context[<key>] as a list of
				// contact dicts and creates one DealContact per item.
				// `key` is required -- without it the activity has no
				// context entry to iterate.
				key, _ := sm["key"].(string)
				if strings.TrimSpace(key) == "" {
					return fmt.Errorf("sourcesConfig[%d]: deal task with type=\"deal_contacts\" requires a non-empty \"key\" "+
						"(the context entry holding the list of contact dicts to fan out into DealContact rows)", i)
				}
				dealContactsKey = key
			case "deal_contact":
				if key, _ := sm["key"].(string); key != "" {
					dealContactKey = key
				}
			}
		}
	}

	if dealContactKey != "" && dealContactsKey != "" && dealContactKey != dealContactsKey {
		fmt.Fprintf(os.Stderr,
			"# warning: deal task has both deal_contact (key=%q) and deal_contacts (key=%q) entries; "+
				"the singular accumulation may overlap with the bulk items -- usually you want only the plural form\n",
			dealContactKey, dealContactsKey)
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
