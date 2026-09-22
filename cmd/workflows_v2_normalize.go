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

func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Every spelling borrower-central's write boundary accepts. Regenerate from its
// condition_evaluator.py and evaluation_rules/condition_operators.py; never hand-edit one entry.
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

// Left nil by unit tests, which keeps operator validation fully offline.
var fetchLiveConditionOperators func() map[string]bool

// The fetched flag holds at-most-once even when the fetch returns nil (offline).
var (
	liveConditionOperators        map[string]bool
	liveConditionOperatorsFetched bool
)

func fetchServerConditionOperators(c *client.Client) map[string]bool {
	data := fetchMetaSection(c, "conditionOperators")
	if data == nil {
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

func sortedBoolMapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Structural + sourcable. compose runs only the structural half, because its
// preflight precedes the normalize step that fills inputKeys.
func validateTaskV2Body(body json.RawMessage, existingTypes map[string]bool) error {
	if err := validateTaskV2BodyStructural(body, existingTypes); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	var task map[string]any
	if err := json.Unmarshal(body, &task); err != nil {
		return nil
	}
	return nil
}

func errAltdataMissingInputKeys(cause error) error {
	return fmt.Errorf(
		"altdata-enrichment task with non-empty sourcesConfig but empty inputKeys, and the CLI could not derive them (%w) -- "+
			"the Hub UI will show an unwired source. Run 'altscore workflows-v2 sources-status --filter id=<SOURCE_ID>' "+
			"to see the source's required inputFields, then add an inputKeys entry per field, "+
			`e.g. inputKeys: {"personId": "{{personId}}"}.`, cause)
}

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

// existingTypes is what the write TARGET already carries; nil means every
// deprecated type is refused as new authoring.
func validateTaskV2BodyStructural(body json.RawMessage, existingTypes map[string]bool) error {
	if len(body) == 0 {
		return nil
	}
	var task map[string]any
	if err := json.Unmarshal(body, &task); err != nil {
		return nil
	}
	taskType, _ := task["type"].(string)
	// Before the switch, not in a default branch: the refusal must hold for every
	// deprecated type, whether or not one ever grows a case.
	if deprecatedTaskTypeRefused(taskType, existingTypes) {
		return deprecatedTaskTypeError("task body", taskType)
	}
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
		// Caught offline because a wrong transform assigns verbatim, quietly
		// near-duplicating the rows a normalized category already holds.
		switch tr, _ := cfg["valueTransform"].(string); tr {
		case "", "none", "trim", "upper", "lower":
		default:
			return fmt.Errorf("category task has categoryConfig.valueTransform %q; must be \"none\", \"trim\", \"upper\" or \"lower\"", tr)
		}
	case "child-workflow":
		if runInBatch, _ := task["runInBatch"].(bool); runInBatch {
			if expr, _ := task["inputExpression"].(string); strings.TrimSpace(expr) == "" {
				return fmt.Errorf("child-workflow task with runInBatch=true requires inputExpression " +
					"(an expression resolving to a list, e.g. \"inputs.cuit_list\"); without it BC's batch dispatcher has nothing to fan out over")
			}
		}
		// Also reached by tasks-v2 create, which never runs compose preflight.
		dispatchMode, _ := task["dispatchMode"].(string)
		if dispatchMode != "" && dispatchMode != "inline" && dispatchMode != "async-batch" {
			return fmt.Errorf("child-workflow task has dispatchMode %q; must be \"inline\" or \"async-batch\" (default: \"inline\")", dispatchMode)
		}
		if irp, _ := task["invalidRowPolicy"].(string); irp != "" && irp != "fail" && irp != "skip" {
			return fmt.Errorf("child-workflow task has invalidRowPolicy %q; must be \"fail\" or \"skip\" (default: \"fail\")", irp)
		}
		if dispatchMode == "async-batch" {
			if expr, _ := task["inputExpression"].(string); strings.TrimSpace(expr) == "" {
				return fmt.Errorf("child-workflow task with dispatchMode=\"async-batch\" requires inputExpression " +
					"(an expression resolving to a list); async dispatch batches over a list, and without one the node resolves a dict and fails at runtime")
			}
		}
	}
	return nil
}

type composeNormalizeOpts struct {
	PredictedAlias  string
	CustomVariables map[string]any
	// InputVariables lets the mapping-table normalizer wrap a bare name as
	// "inputs.<name>", the only form the Hub picker surfaces as a source.
	InputVariables map[string]any
	// Publish turns "referenced entity missing" into a hard error: a published
	// workflow that references one fails every execution, hours after apply.
	Publish bool
	// AutoRescopeEntities only softens the unscoped-or-matching case; an entity
	// owned by a DIFFERENT workflow still hard-errors unless AllowStealOwnership.
	AutoRescopeEntities bool
	// AllowStealOwnership transfers an entity another workflow owns; refusing by
	// default keeps the 1:1 ownership its Hub elements panel depends on.
	AllowStealOwnership bool
	// AutoDefaults fills only absent fields -- caller-supplied values always win.
	AutoDefaults bool
}

// Dry-runs neither error nor warn: agents iterating on a spec are not expected
// to have created the entities yet.
func missingEntityHandler(opts *composeNormalizeOpts, dryRun bool, resourceKind, ref string) error {
	msg := fmt.Sprintf("%s task references %q which was not found on the tenant", resourceKind, ref)
	if opts != nil && opts.Publish {
		// Hard error even in dry-run: --dry-run --publish has to show the same
		// outcome as a real publish.
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
	if dryRun {
		return nil
	}
	fmt.Fprintf(os.Stderr, "# warning: %s\n", msg)
	return nil
}

// The Hub elements panel filters entities by exact workflowAlias, so one scoped
// to another workflow still resolves at runtime but is invisible to the editor.
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

func normalizeTaskBody(c *client.Client, task map[string]any, opts *composeNormalizeOpts, dryRun bool) error {
	if opts == nil {
		opts = &composeNormalizeOpts{}
	}
	taskType, _ := task["type"].(string)
	switch taskType {
	case "altdata-enrichment":
		return normalizeAltdataTask(c, task, dryRun)
	// compute-variables needs no case: BC derives its outputSchema server-side.
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

// Mirrors inputMappings both ways: schema-guide documents the nested form while
// the runtime reads the top-level map.
func normalizeCategoryTask(task map[string]any) error {
	cfg := asMap(task["categoryConfig"])
	if cfg == nil {
		return nil
	}
	mirrorNestedInputMappings(task, cfg, "categoryConfig")
	return nil
}

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

// The child sees only the resolved inputExpression, or failing that this node's
// inputMappings -- nothing else reaches it from the parent.
func normalizeChildWorkflowTask(c *client.Client, task map[string]any, dryRun bool) error {
	executor, _ := task["executorId"].(string)
	if executor == "" {
		return nil
	}
	runInBatch, _ := task["runInBatch"].(bool)
	// Batch mode reads its inputs from the resolved inputExpression.
	if runInBatch {
		mappings := asMap(task["inputMappings"])
		if len(mappings) == 0 {
			return nil
		}
		bareNames := make([]string, 0)
		validMappings := map[string]any{}
		for name, raw := range mappings {
			val, _ := raw.(string)
			// Valid values are dotted paths; a bare field name is the trap case.
			if val != "" && !strings.Contains(val, ".") {
				bareNames = append(bareNames, name)
				continue
			}
			validMappings[name] = raw
		}
		if len(bareNames) > 0 {
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
	// A single child fed by an inputExpression never reads inputMappings either, so
	// checking them would flag a correctly authored node.
	if expr, _ := task["inputExpression"].(string); strings.TrimSpace(expr) != "" {
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

// dataAge is deliberately not defaulted (a test asserts it): an authored value
// overrides the freshness the source publishes, which is right for almost no node.
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

	// The runtime reads each source's required fields from inputKeys; omitting the
	// map ships a source nobody wired.
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

		// Validate every referenced source even when inputKeys were hand-supplied: a
		// definitive catalog miss is a typo, while a transient failure is tolerated.
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

	// inputKeys only NAMES the task variable feeding each source field; the VALUE
	// comes from inputMappings, so an unmapped required field 404s at runtime.
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

	// outputSchema is server-derived; authored entries pass through and win on reconcile.

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

// Separates a genuinely bad reference from a transport blip, which callers tolerate.
var errSourceNotFound = errors.New("source not found in any catalog")

// The union of both catalogs is what a node may reference; the microservice is
// first so its entry wins when an id exists in both.
var sourceStatusCatalogs = []string{
	"/v2/workflows/sources-status?per-page=200",
	"/v2/workflows/external-sources-status",
}

var sourceCatalogListCache = map[string][]map[string]any{}

// A nil value means looked up and definitively absent, stored to short-circuit retries.
var altdataSourceStatusCache = map[string]map[string]any{}

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

	// A failed fetch never becomes a cached absence or a false not-found.
	if transportErr != nil {
		return nil, transportErr
	}
	altdataSourceStatusCache[cacheKey] = nil
	return nil, errSourceNotFound
}

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

func lookupAltdataSourceInputFields(c *client.Client, sourceID, version string, dryRun bool) ([]string, error) {
	s, err := lookupAltdataSourceStatus(c, sourceID, version, dryRun)
	if err != nil {
		// Stub so compose can validate inputKeys plumbing with no network.
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

// An absent flag counts as required, matching the backend source-status model.
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

// Mirrors the backend publish gate (ValidateSourceInputsUC), so a compose-time
// warning matches what publish enforces.
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
		return true
	}
	matches := templatePlaceholderRegex.FindAllStringSubmatch(tmpl, -1)
	if len(matches) == 0 {
		return true
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

var entityCache = map[string]map[string]any{}

// A nil value after Fetched means the lookup failed; callers skip the warning quietly.
var tenantDecisionKeysCache map[string]bool
var tenantDecisionKeysFetched bool

// The case-sensitive decision keys live as data-model records, not under /v1/decisions.
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

func warnIfDecisionKeyUnknown(c *client.Client, decisionKey, ruleCode, context string) {
	if decisionKey == "" {
		return
	}
	known := fetchTenantDecisionKeys(c)
	if known == nil || known[decisionKey] {
		return
	}
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

// Runs in dry-run too: skipping it would make the preview a different shape.
// dryRun stays in the signature for symmetry with the sibling normalizers.
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
	entityCache[cacheKey] = nil
	return nil, nil
}

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
		if entity != nil {
			dk, _ := entity["decisionKey"].(string)
			warnIfDecisionKeyUnknown(c, dk, ref, fmt.Sprintf("rulesConfig[%d]", i))
		}
	}
	return nil
}

// Entries that omit `id` get a UUID: the runtime MappingTableEntry model requires one.
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
	// The Hub sorts entries by `order` and collapses duplicates, so two entries at
	// order=0 render as a single-entry panel even though both persist.
	seenOrders := map[float64]int{}
	hasDupOrder := false
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		ord, hasOrd := em["order"]
		if !hasOrd {
			hasDupOrder = true
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

	// A bare entry inputVariable only resolves through the runtime's
	// context[<last-segment>] fallback, which these top-level mappings populate.
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

	mirrorEntryInputsToTopLevel(task, entries)
	return nil
}

// The runtime resolves entries[].inputVariable directly; the top-level mirror is
// what the Hub editor renders from.
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
		// A bare name would mirror to a self-referential mapping /v2/tasks rejects,
		// and would clobber the backing top-level entry.
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
	// Pre-flight owns exactly the set reconcileEntityScopes re-stamps: otherwise a
	// cross-owned bucket table is refused only after the workflow is published.
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
	if v, _ := cfg["totalScoreVariable"].(string); v == "" {
		cfg["totalScoreVariable"] = "total_score"
	}
	if v, _ := cfg["breakdownVariable"].(string); v == "" {
		cfg["breakdownVariable"] = "score_breakdown"
	}
	task["scorecardConfig"] = cfg

	mirrorNestedInputMappings(task, cfg, "scorecardConfig")
	return nil
}

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
				// As with the scorecard's nested tables, reconcileEntityScopes re-stamps
				// these rules, so pre-flight owns the same set.
				if err := validateEntityWorkflowAliasMatch(opts, ruleEntity, predictedAlias, "evaluation-rules", rref); err != nil {
					return fmt.Errorf("rule-tree %q rules[%d]: %w", ref, i, err)
				}
				dk, _ := ruleEntity["decisionKey"].(string)
				warnIfDecisionKeyUnknown(c, dk, rref, fmt.Sprintf("rule-tree %q rules[%d]", ref, i))
			}
		}
	}

	mirrorNestedInputMappings(task, cfg, "ruleTreeConfig")
	return nil
}

// The runtime needs BOTH maps: the Hub panel renders the top-level one, while the
// scorecard / rule-tree activities resolve per-rule fields from the nested one.
func mirrorNestedInputMappings(task map[string]any, cfg map[string]any, cfgKey string) {
	topLevel := asMap(task["inputMappings"])
	nested := asMap(cfg["inputMappings"])
	if len(topLevel) == 0 && len(nested) == 0 {
		return
	}
	if len(topLevel) == 0 && len(nested) > 0 {
		mirror := map[string]any{}
		for k, v := range nested {
			mirror[k] = v
		}
		task["inputMappings"] = mirror
	}
	if len(nested) == 0 && len(topLevel) > 0 {
		mirror := map[string]any{}
		for k, v := range topLevel {
			mirror[k] = v
		}
		cfg["inputMappings"] = mirror
		task[cfgKey] = cfg
	}
}

// sourcesConfig[].type "identity_key" is silently dropped at runtime, which
// matches "identity" literally, so the identity never lands on the borrower.
func normalizeEntityWriteTask(task map[string]any, opts *composeNormalizeOpts) error {
	if opts == nil {
		opts = &composeNormalizeOpts{}
	}
	taskType, _ := task["type"].(string)

	// The contact's borrower upsert keys on identity_key + identity_value, so a
	// contact carrying only e.g. tax_id would resolve to a null identity.
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
				continue
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
			// The runtime reads the value from context[<key>]; a spec literal would
			// persist as a stale template in the Hub source picker.
			delete(sm, "value")
			task["sourcesConfig"].([]any)[i] = sm
		}
	}

	operation, _ := task["operation"].(string)
	if operation == "write" {
		// persona is workflow design, not display: the server derives the persona
		// input entry but never sets task.persona nor wires inputMappings.persona.
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
			if v, ok := task["persona"].(string); !ok || strings.TrimSpace(v) == "" {
				task["persona"] = "individual"
			}
		}
		// (persona wired to a non-input source, e.g. custom.* -> leave as-is)
	}
	return nil
}
