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

// conditionOperators is the compiled-in operator vocabulary used by conditional
// task branches and evaluation rules -- the fast path and the offline fallback.
// It mirrors EVERY spelling borrower-central's write boundary accepts: the 26
// canonical names, grouped one per line with their aliases beside them.
//
// Two server sources, unioned, because they differ:
//   - app/service/condition_evaluator.py, WORKFLOW_CONDITION_OPERATORS
//     (CONDITION_OPERATORS + WORKFLOW_ONLY_CONDITION_OPERATORS), which is what
//     GET /v1/meta/workflows-v2-schema?section=conditionOperators serves and so
//     what fetchServerConditionOperators returns: 26 canonical + 21 aliases.
//   - app/model/evaluation_rules/condition_operators.py, whose registry backs
//     the write gate (_reject_unknown_condition_operators on the shared v2 task
//     write schema) and additionally accepts the 8 symbol aliases
//     = == != <> < <= > >=, which ConditionItem repairs to canonical on write.
//
// Until #101 this list held 21 entries, ALL of them aliases, overlapping BC's
// canonical set on three names only (contains, between, in). Since BC
// canonicalises on read, every `get`/`export` returns canonical spellings, so
// the round trip export -> apply warned on every operator of every conditional
// -- 23 of 26 canonical names -- and five (not_contains, is_true, is_false,
// is_empty, is_not_empty) had no accepted spelling at all, making them a hard
// apply failure whenever the meta endpoint was unreachable. Widening is purely
// additive: every form accepted before is still here.
//
// checkConditionOperator consults the live backend once before rejecting, so a
// mirror that falls behind still cannot cause a FALSE REJECTION of a valid
// workflow. Regenerate from the two files above; do not hand-edit one spelling.
var conditionOperators = map[string]bool{
	"array_contains_all": true, "arrayContainsAll": true,
	"array_contains_any": true, "arrayContainsAny": true,
	"array_contains_none": true, "arrayContainsNone": true,
	"between":   true,
	"contains":  true,
	"ends_with": true, "endsWith": true,
	"equals": true, "=": true, "==": true, "eq": true,
	"greater_than": true, ">": true, "gt": true,
	"greater_than_or_equals": true, ">=": true, "gte": true,
	"in":               true,
	"is_altdata_empty": true, "isAltdataEmpty": true,
	"is_altdata_error": true, "isAltdataError": true,
	"is_altdata_not_calculated": true, "isAltdataNotCalculated": true,
	"is_altdata_null": true, "isAltdataNull": true,
	"is_empty":            true,
	"is_false":            true,
	"is_not_altdata_null": true, "isNotAltdataNull": true,
	"is_not_empty": true,
	"is_not_null":  true, "isNotNull": true, "is_set": true,
	"is_null": true, "isNull": true, "not_set": true,
	"is_true":   true,
	"less_than": true, "<": true, "lt": true,
	"less_than_or_equals": true, "<=": true, "lte": true,
	"not_contains": true,
	"not_equals":   true, "!=": true, "<>": true, "ne": true, "neq": true,
	"not_in": true, "notIn": true,
	"starts_with": true, "startsWith": true,
}

// fetchLiveConditionOperators, when set, lazily returns the LIVE backend's
// workflows-v2 conditional-operator vocabulary (canonical names AND their
// aliases, flattened into one set -- they're interchangeable on the wire) so
// validation can accept operators the backend gained after this binary was
// built. composeWorkflowBody wires it to fetchServerConditionOperators before
// normalize runs; unit tests leave it nil, keeping validation fully offline.
// Mirrors the fetchLiveTaskTypes hook in workflows_v2_apply.go.
var fetchLiveConditionOperators func() map[string]bool

// Live-operator list, fetched at most once per compose and only when an
// operator is missing from the compiled-in mirror. liveConditionOperatorsFetched
// guards the at-most-once semantics even when the fetch returns nil (offline or
// an older backend without the endpoint). Reset when the hook is wired.
var (
	liveConditionOperators        map[string]bool
	liveConditionOperatorsFetched bool
)

// fetchServerConditionOperators queries
// GET /v1/meta/workflows-v2-schema?section=conditionOperators and flattens the
// `workflow` subsection (the superset a v2 conditional node accepts) into a set
// of every accepted string -- canonical operator names plus each one's aliases.
// Returns nil on any transport/shape error so callers fall back to the
// compiled-in mirror, which is exactly the pre-existing behavior. Mirrors
// fetchServerTaskTypes.
func fetchServerConditionOperators(c *client.Client) map[string]bool {
	data, _, err := c.Do("GET", "borrower_central", "/v1/meta/workflows-v2-schema?section=conditionOperators", nil)
	if err != nil {
		return nil
	}
	var payload struct {
		ConditionOperators struct {
			Workflow map[string]struct {
				Aliases []string `json:"aliases"`
			} `json:"workflow"`
		} `json:"conditionOperators"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.ConditionOperators.Workflow) == 0 {
		return nil
	}
	out := make(map[string]bool, len(payload.ConditionOperators.Workflow))
	for name, spec := range payload.ConditionOperators.Workflow {
		out[name] = true
		for _, alias := range spec.Aliases {
			out[alias] = true
		}
	}
	return out
}

// checkConditionOperator validates a single condition operator, consulting the
// live backend at most once when the operator is absent from the compiled-in
// mirror. Mirrors preflightTasks' live-type fallback semantics:
//   - compiled-in-known           -> accept (fast path, no fetch)
//   - live-known (newer backend)  -> warn + accept
//   - unknown to a reachable backend -> reject, listing the live vocabulary
//   - backend unreachable (offline / older backend / no hook wired)
//     -> reject against the compiled-in list, exactly as before
func checkConditionOperator(op, path string) error {
	if conditionOperators[op] {
		return nil
	}
	if !liveConditionOperatorsFetched && fetchLiveConditionOperators != nil {
		liveConditionOperators = fetchLiveConditionOperators()
		liveConditionOperatorsFetched = true
	}
	if liveConditionOperators[op] {
		fmt.Fprintf(os.Stderr,
			"# WARNING: %s.operator %q is newer than this CLI build "+
				"(absent from its compiled-in list) but IS accepted by the live backend -- proceeding. "+
				"Update altscore-cli to refresh its offline operator list.\n",
			path, op,
		)
		return nil
	}
	if len(liveConditionOperators) > 0 {
		return fmt.Errorf(
			"%s.operator %q is not a known condition operator. "+
				"The live backend was consulted and does not list it either (%d operators). valid: %v",
			path, op, len(liveConditionOperators), sortedBoolMapKeys(liveConditionOperators),
		)
	}
	return fmt.Errorf(
		"%s.operator %q is not a known condition operator "+
			"(the live backend could not be checked -- offline or an older backend; "+
			"validated against this build's compiled-in list only). valid: %v",
		path, op, sortedBoolMapKeys(conditionOperators),
	)
}

// sortedBoolMapKeys returns the keys of a set map in deterministic order, so
// error messages listing the valid operators are stable across runs.
func sortedBoolMapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	case "category":
		cfg := asMap(task["categoryConfig"])
		if key, _ := cfg["categoryKey"].(string); strings.TrimSpace(key) == "" {
			return fmt.Errorf("category task requires categoryConfig.categoryKey -- the node resolves the tenant's category by KEY (e.g. \"segmentation\") and never takes a category id")
		}
		switch op, _ := cfg["operation"].(string); op {
		case "read", "assign":
		case "":
			return fmt.Errorf("category task requires categoryConfig.operation: \"read\" or \"assign\"")
		default:
			return fmt.Errorf("category task has categoryConfig.operation %q; must be \"read\" or \"assign\"", op)
		}
		switch vf, _ := cfg["valueFormat"].(string); vf {
		case "", "string":
		case "json":
			if len(asSlice(cfg["valueFields"])) == 0 {
				return fmt.Errorf("category task with categoryConfig.valueFormat=\"json\" requires categoryConfig.valueFields: the object's keys, in order (they also fix the join order of the node's `codes` output)")
			}
		default:
			return fmt.Errorf("category task has categoryConfig.valueFormat %q; must be \"string\" or \"json\"", vf)
		}
		switch root, _ := cfg["entityRoot"].(string); root {
		case "", "borrower", "deal":
		default:
			return fmt.Errorf("category task has categoryConfig.entityRoot %q; must be \"borrower\" or \"deal\"", root)
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
	// billable_id wiring + default PDF generation (see applyAutoEndDefaults),
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
// cross-checking entity scopes, declared inputVariables for the persona
// input opt-in). Pass nil when running outside compose -- the normalizers
// degrade gracefully (e.g. mismatch warnings are skipped).
func normalizeTaskBody(c *client.Client, task map[string]any, opts *composeNormalizeOpts, dryRun bool) error {
	if opts == nil {
		opts = &composeNormalizeOpts{}
	}
	taskType, _ := task["type"].(string)
	switch taskType {
	case "altdata-enrichment":
		return normalizeAltdataTask(c, task, dryRun)
	// compute-variables needs no normalization: its outputSchema is derived
	// server-side by BC's DerivedSchemaService from selectedVariables plus
	// the workflow's live customVariables.
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
	case "category":
		return normalizeCategoryTask(task)
	}
	return nil
}

// normalizeCategoryTask covers the category task type. The node takes no
// entity reference to rewrite (it resolves its category by KEY at runtime), so
// the only gap worth closing offline is the inputMappings placement: the value
// being assigned is supplied through inputMappings, and an author copying the
// nested form out of schema-guide writes it under categoryConfig while the
// runtime reads the top-level map. Mirroring both ways is the same fix
// scorecard and rule-tree already carry.
func normalizeCategoryTask(task map[string]any) error {
	cfg := asMap(task["categoryConfig"])
	if cfg == nil {
		return nil
	}
	mirrorNestedInputMappings(task, cfg, "categoryConfig")
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

// applyAltdataSourceDefaults fills packageAlias on each sourcesConfig entry.
//
// dataAge is deliberately NOT defaulted, and a test asserts it stays that way.
// An absent dataAge means "use the freshness this source publishes"
// (cacheMaxSeconds, read from the altdata catalog by the runtime), which is the
// right answer for almost every node: it runs from 24h to 15 days depending on
// the source. Stamping 30 here made that unreachable, because an authored
// dataAge overrides the published policy, so every applied workflow quietly
// asked for a half-hourly refresh of data its source says is good for days.
//
// An author who wants a specific window still sets one and it is honoured as
// written. See borrower-central app/model/altdata/source_cache_policy.py.
func applyAltdataSourceDefaults(sources []any) []any {
	for i, s := range sources {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if _, has := sm["packageAlias"]; !has {
			if sid, _ := sm["sourceId"].(string); sid != "" {
				sm["packageAlias"] = strings.ToLower(strings.ReplaceAll(sid, "-", "_"))
			}
		}
		sources[i] = sm
	}
	return sources
}

func normalizeAltdataTask(c *client.Client, task map[string]any, dryRun bool) error {
	sources := asSlice(task["sourcesConfig"])
	if len(sources) == 0 {
		return nil
	}

	inputKeys := asMap(task["inputKeys"])

	task["sourcesConfig"] = applyAltdataSourceDefaults(sources)

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

	// Warn when a REQUIRED source field has no value wired through inputMappings.
	// inputKeys only NAMES the task variable feeding each source field; the VALUE
	// comes from inputMappings, the only field the runtime resolves. A required
	// field with no mapping resolves to empty at runtime and the source returns
	// 404 (the backend now blocks publish on this) -- surface it at compose time
	// with the exact fix. Batch mode is exempt: row inputs come from
	// inputRowsExpression, not inputMappings.
	if mode, _ := task["mode"].(string); mode != "batch" {
		inputMappings := asMap(task["inputMappings"])
		seenReq := map[string]bool{}
		var unmapped []string
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
			reqFields, err := lookupAltdataSourceRequiredFields(c, sid, ver, dryRun)
			if err != nil {
				continue // lookup failure already warned above; don't double-report
			}
			for _, f := range reqFields {
				if seenReq[f] {
					continue
				}
				if !altdataRequiredFieldSatisfied(f, inputKeys, inputMappings) {
					seenReq[f] = true
					unmapped = append(unmapped, f)
				}
			}
		}
		if len(unmapped) > 0 {
			name, _ := task["alias"].(string)
			if name == "" {
				name, _ = task["label"].(string)
			}
			if name == "" {
				name = "altdata-enrichment"
			}
			fmt.Fprintf(os.Stderr,
				"# warning: altdata task %q: required source input(s) %v have no inputMappings entry; "+
					"they resolve to empty at runtime and the source returns 404 (the backend blocks publish on this). "+
					"Add an inputMappings entry, e.g. {%q: \"inputs.%s\"}.\n",
				name, unmapped, unmapped[0], unmapped[0])
		}
	}

	// outputSchema is no longer filled here: BC stamps it at task-create time
	// AND serves a reconciled outputSchema + derivedSchema on every GET
	// (DerivedSchemaService covers altdata-enrichment), so the CLI-side
	// catalog mirror became redundant duplication. User-authored entries on
	// the task body still pass through untouched and win on reconcile.

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

// lookupAltdataSourceRequiredFields returns the names of a source's REQUIRED
// input fields (a subset of inputFields). Reuses the per-run source-status
// cache populated by lookupAltdataSourceInputFields, so it adds no extra fetch.
func lookupAltdataSourceRequiredFields(c *client.Client, sourceID, version string, dryRun bool) ([]string, error) {
	s, err := lookupAltdataSourceStatus(c, sourceID, version, dryRun)
	if err != nil {
		return nil, err
	}
	var required []string
	for _, f := range asSlice(s["inputFields"]) {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := fm["field"].(string); name != "" && altdataFieldRequired(fm) {
			required = append(required, name)
		}
	}
	return required, nil
}

// altdataFieldRequired reads a source inputField's `required` flag. The source
// catalogs emit it as the string "REQUIRED"/"OPTIONAL" (some as a bool); an
// absent flag defaults to required, matching the backend source-status model.
func altdataFieldRequired(fm map[string]any) bool {
	switch v := fm["required"].(type) {
	case bool:
		return v
	case string:
		return !strings.EqualFold(v, "OPTIONAL") && !strings.EqualFold(v, "false")
	default:
		return true
	}
}

// altdataRequiredFieldSatisfied reports whether a required source field will
// receive a value at runtime: mapped directly by name, or its inputKeys entry
// is a literal/constant, or every {{var}} that entry references is itself in
// inputMappings (or a runtime builtin). Mirrors the backend publish gate
// (ValidateSourceInputsUC) so CLI compose-time warnings match what publish
// enforces.
func altdataRequiredFieldSatisfied(field string, inputKeys, inputMappings map[string]any) bool {
	if _, ok := inputMappings[field]; ok {
		return true
	}
	entry, ok := inputKeys[field]
	if !ok {
		return false
	}
	tmpl, ok := entry.(string)
	if !ok {
		return true // a non-template literal value -> supplied
	}
	matches := templatePlaceholderRegex.FindAllStringSubmatch(tmpl, -1)
	if len(matches) == 0 {
		return true // constant literal -> supplied
	}
	for _, m := range matches {
		v := strings.TrimSpace(m[1])
		if strings.HasPrefix(v, "secret:") || v == "source_id" || v == "version" {
			continue
		}
		if _, ok := inputMappings[v]; !ok {
			return false
		}
	}
	return true
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
	if err := checkConditionOperator(op, path); err != nil {
		return err
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

	// Caller-supplied top-level inputMappings (already ref-rewritten upstream).
	// A bare entry inputVariable is only acceptable when one of these provides
	// the real scoped value: the runtime falls back to context[<last-segment>],
	// which top-level inputMappings populate via resolvedInputs.
	callerMappings := asMap(task["inputMappings"])

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
		// Wrap a bare inputVariable as inputs.<name> when it's a recognized
		// workflow input. Otherwise it must already be scoped: task outputs are
		// never promoted to the context root, so a bare name only resolves via
		// the context[<last-segment>] fallback, populated solely by a matching
		// top-level inputMappings entry. Without one it silently hits the default
		// bucket and mirrors into a path-less mapping /v2/tasks rejects -- fail loud.
		if !strings.Contains(inVar, ".") {
			if _, isInput := inputVars[inVar]; isInput {
				em["inputVariable"] = "inputs." + inVar
				inVar = "inputs." + inVar
			}
		}
		if !isScopedRef(inVar) {
			field := lastDotSegment(inVar)
			if _, has := callerMappings[field]; !has {
				return fmt.Errorf(
					"mappingTableConfig.entries[%d].inputVariable %q is an unscoped bare name. "+
						"Scope it (e.g. task_outputs.<producingTask>.%s, inputs.%s, custom.%s, system.%s) "+
						"or add an explicit inputMappings.%s on this node.",
					i, inVar, field, field, field, field, field)
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
		// Only mirror scoped refs; a bare name yields a self-referential mapping
		// /v2/tasks rejects. normalizeMappingTableTask already guarantees any bare
		// name here has a backing top-level mapping, which we must not clobber.
		if !isScopedRef(inVar) {
			continue
		}
		field := lastDotSegment(inVar)
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
	// Cross-check the workflow scope of every mapping table the scorecard's
	// rules link to. reconcileEntityScopes re-stamps exactly this set after
	// apply (see its scorecard case), so pre-flight has to validate exactly
	// this set too: a cross-owned bucket table that is only reachable THROUGH
	// the scorecard otherwise clears pre-flight, the workflow is created and
	// published, and the re-stamp is refused on stderr with everything already
	// persisted. The table keeps the previous owner's alias and vanishes from
	// this workflow's Hub elements panel -- the exact drift
	// validateEntityWorkflowAliasMatch exists to prevent. Mirrors the rule-tree
	// recursion in normalizeRuleTreeTask.
	if entity != nil {
		if rules, ok := entity["rules"].([]any); ok {
			for i, raw := range rules {
				rm, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				mcode, _ := rm["mappingTableCode"].(string)
				mid, _ := rm["mappingTableId"].(string)
				mref := mcode
				if mref == "" {
					mref = mid
				}
				if mref == "" {
					continue
				}
				mtEntity, _ := lookupEntity(c, "mapping-tables", mref, dryRun)
				if mtEntity == nil {
					continue
				}
				if err := validateEntityWorkflowAliasMatch(opts, mtEntity, predictedAlias, "mapping-tables", mref); err != nil {
					return fmt.Errorf("scorecard %q rules[%d]: %w", ref, i, err)
				}
			}
		}
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
				// Same reasoning as the scorecard's nested mapping tables:
				// reconcileEntityScopes re-stamps every rule this tree
				// references, so pre-flight owns the same set. Without this the
				// cross-ownership refusal only surfaces after the workflow is
				// live, with the rule stranded on the old alias.
				if err := validateEntityWorkflowAliasMatch(opts, ruleEntity, predictedAlias, "evaluation-rules", rref); err != nil {
					return fmt.Errorf("rule-tree %q rules[%d]: %w", ref, i, err)
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
// the RUNTIME gaps between the canonical Hub-shaped body and what an agent
// typically writes from the schema-guide:
//   - sourcesConfig[].type = "identity_key" -> "identity" (runtime checks
//     `source["type"] == "identity"` literally; "identity_key" is silently
//     dropped at runtime because the canonical entity_activity branch
//     filter doesn't match it, so the borrower never gets the identity
//     stamped on it)
//   - For operation=write, resolve the borrower persona: either wire it from a
//     declared workflow input (inputMappings.persona -> inputs.persona) or set
//     the task literal ("individual") so CreateBorrower's strict Literal
//     validator has a value at runtime.
//
// The persona INPUT-display entry and the outputSchema (borrower_id + lookup
// key) that this normalizer used to mirror onto the task body are now derived
// server-side by BC's DerivedSchemaService (_derive_customer_write /
// _derive_deal / _derive_asset, deployed in #1526) and served as derivedSchema
// on every GET, so the CLI no longer authors them. The runtime wiring above
// has no server replacement (derivation is display-only) and stays.
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
		// persona is required by CreateBorrower's Literal validator. It is a
		// property of the workflow's DESIGN (a cedula flow is always
		// "individual", a RUC flow always "business"), not a per-execution
		// choice -- so by default it lives on the task as a literal
		// (CustomerTaskData.persona) and never surfaces as a user-facing
		// input. The runtime resolves persona as `context.get("persona") or
		// task.persona`, so a resolved context value still wins.
		//
		// This is RUNTIME wiring, NOT a display fill. BC's DerivedSchemaService
		// (schema_derivation.py, deployed in #1526) now derives the persona
		// INPUT entry into derivedSchema for customer / create-borrower, so the
		// CLI no longer authors inputSchema.persona -- but the display
		// derivation never sets task.persona nor wires inputMappings.persona,
		// so those two runtime steps stay here.
		//
		// Two opt-ins keep persona as a real workflow input: the agent
		// declares inputVariables.persona, or wires an entity-write task's
		// inputMappings.persona to inputs.*. In those cases we ensure the
		// wiring; otherwise we set the task literal.
		mappings := asMap(task["inputMappings"])
		personaSrc, hasPersonaMapping := mappings["persona"].(string)
		personaFromInput := hasPersonaMapping && strings.HasPrefix(strings.TrimSpace(personaSrc), "inputs.")
		_, personaDeclaredInput := asMap(opts.InputVariables)["persona"]

		if personaFromInput || personaDeclaredInput {
			if !hasPersonaMapping {
				mappings["persona"] = "inputs.persona"
				task["inputMappings"] = mappings
			}
		} else if !hasPersonaMapping {
			// No input opt-in and no explicit mapping: default the task
			// literal so the runtime resolves persona without an input.
			if v, ok := task["persona"].(string); !ok || strings.TrimSpace(v) == "" {
				task["persona"] = "individual"
			}
		}
		// (persona wired to a non-input source, e.g. custom.* -> leave as-is)

		// outputSchema fill removed: BC's DerivedSchemaService now derives
		// customer/deal/asset outputSchema server-side (_derive_customer_write /
		// _derive_deal / _derive_asset) and serves it as derivedSchema on every
		// GET. The derived shape matches the runtime output exactly -- the
		// per-source-row keys plus the entity id (borrower_id for customer,
		// asset_id for asset, deal_id for deal) -- and is strictly more accurate
		// than the CLI's one-size fill, which stamped borrower_id even on
		// asset/deal (whose activities return asset_id/deal_id) and a lookup key
		// that the write activities never emit as an output.
	}
	return nil
}
