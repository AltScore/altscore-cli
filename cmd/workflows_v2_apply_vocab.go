package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// Compiled-in vocabularies (task types, categories, relationship kinds, inputSchema
// types) with a live fallback, plus the format checks that use them.

// validTaskTypes mirrors the AUTHORABLE half of the backend TaskType enum at
// borrower-central/app/model/workflows_v2/task.py. Sourced once and kept in
// sync as the enum evolves. Used by preflight to reject typos BEFORE creating
// any /v2/tasks rows. Without this check, a typo would orphan all earlier
// tasks in the compose loop (no rollback path exists today).
//
// Types the backend still parses but refuses for new authoring are NOT here;
// they live in deprecatedTaskTypes below and are refused outright.
var validTaskTypes = map[string]bool{
	"http": true, "conditional": true,
	"start": true, "end": true, "wait": true,
	"evaluate-rules": true, "altdata-enrichment": true,
	"child-workflow": true, "exception": true,
	"mapping-table": true, "scorecard": true, "rule-tree": true,
	"compute-variables": true, "data-store-write": true, "data-store-query": true,
	"customer": true, "deal": true, "credit-line": true,
	"list-of-similars": true, "asset": true, "relationships": true,
	"package-io": true, "sftp": true, "notices": true,
	"contact": true, "document-extraction": true,
	"spreadsheet-extraction": true, "category": true,
}

// deprecatedTaskTypes are the task types AltScore has retired. The backend
// still parses them, so live workflows keep running, but it refuses them for
// NEW authoring -- so every CLI authoring path refuses them too. This list is
// compiled in precisely so the refusal also holds offline, where no live
// vocabulary can be consulted.
//
// "New" is the whole rule, and it is the backend's: BC diffs the incoming graph
// against the stored one and only refuses a deprecated type it does not already
// hold, so a workflow that ALREADY carries such a node stays editable. It has
// to: making a legacy workflow uneditable blocks the one thing someone opens it
// to do, which is migrate it off the retired type. See
// deprecatedTaskTypeRefused for the mirror, which keys on the TYPE being
// present in the target -- exactly what BC diffs.
//
// That is the opposite of the policy for an UNKNOWN type (see
// warnUnverifiedVocabularyValue): an unknown type may simply be newer than
// this build, so preflight cannot tell "invalid" from "valid but newer" and
// fails open. A deprecated type is different in kind -- this build KNOWS the
// backend refuses it for new work -- so there is nothing to verify and nothing
// to fail open about.
//
// fetchServerTaskTypes unions the backend's own `deprecated` list into this
// map, so a type retired after this binary shipped is honoured without a CLI
// rebuild. Entries are only ever added, never removed.
var deprecatedTaskTypes = map[string]bool{
	"create-alert": true, "create-borrower": true, "create-identity": true,
	"data-store": true, "end-old": true, "fetch-borrower-entities": true,
	"fetch-entity": true, "html-template": true, "pdf-report": true,
	"soap": true, "update-borrower": true, "update-borrower-name": true,
	"webhook": true,
}

// deprecatedTaskTypeReplacements names what to author instead, per retired
// type. A type absent from this map has no replacement and is simply refused.
var deprecatedTaskTypeReplacements = map[string]string{
	"create-borrower":         `the "customer" task with operation=write`,
	"create-identity":         `the entity-specific tasks ("customer" / "deal" / "asset" / "contact")`,
	"data-store":              `"data-store-write" or "data-store-query"`,
	"fetch-borrower-entities": `the "customer" or "deal" task with operation=read`,
	"fetch-entity":            `the "customer" or "deal" task with operation=read`,
	"pdf-report":              `endConfig on the "end" task`,
	"soap":                    `the "http" task`,
	"update-borrower":         `the "customer" task with operation=write`,
}

// deprecatedTaskTypeRefused reports whether a task type must be refused as new
// authoring. It is the CLI's mirror of the backend's diff-based rule: BC
// refuses a deprecated type only when the target does not already hold it, so
// `existingTypes` is the set of task types the apply target currently carries.
//
// Keyed on the TYPE, not on node identity, because that is precisely what the
// backend diffs. Mirroring it exactly is the point: a CLI stricter than the
// server makes a legacy workflow uneditable, and a CLI looser than the server
// waves a spec through to a 4xx it could have explained locally.
//
// `existingTypes` is nil on every CREATE path -- nothing is carried forward
// there, so every deprecated type is new and refused. Nil is also what a failed
// target lookup yields, which keeps the refusal standing rather than opening a
// hole whenever the network hiccups.
func deprecatedTaskTypeRefused(taskType string, existingTypes map[string]bool) bool {
	return deprecatedTaskTypes[taskType] && !existingTypes[taskType]
}

// warnDeprecatedTaskTypeCarriedForward is the non-fatal counterpart of
// deprecatedTaskTypeError: the target already carries this retired type, so the
// node travels through unchanged instead of blocking the apply. Visible on
// stderr because the author should still migrate it -- just not at the cost of
// being unable to touch the workflow at all.
func warnDeprecatedTaskTypeCarriedForward(path, taskType string) {
	fmt.Fprintf(os.Stderr,
		"# WARNING: %s: task type %q is DEPRECATED and cannot be newly authored, but the "+
			"apply target already carries it -- CARRIED FORWARD unchanged. %s "+
			"Mirrors the backend, which refuses a retired type only for newly added work so a "+
			"legacy workflow stays editable.\n",
		path, taskType, deprecatedTaskTypeGuidance(taskType),
	)
}

// deprecatedTaskTypeGuidance names what to author instead, or says there is
// nothing. Shared by the refusal and the carry-forward warning so the advice
// cannot drift between them.
func deprecatedTaskTypeGuidance(taskType string) string {
	if r := deprecatedTaskTypeReplacements[taskType]; r != "" {
		return fmt.Sprintf("Use %s instead.", r)
	}
	return "It has no replacement -- drop the node."
}

// deprecatedTaskTypeError is the one refusal message every authoring path
// shares. `path` is the caller-formatted prefix (e.g. `node ref="score"`).
func deprecatedTaskTypeError(path, taskType string) error {
	guidance := deprecatedTaskTypeGuidance(taskType)
	return fmt.Errorf(
		"%s: task type %q is DEPRECATED and can no longer be authored. %s "+
			"A workflow that ALREADY contains this type keeps working and stays editable "+
			"-- only adding one is refused, and that refusal does not depend on the "+
			"backend being reachable. "+
			"Run 'altscore workflows-v2 schema-guide taskTypes' for the live palette.",
		path, taskType, guidance,
	)
}

// fetchLiveTaskTypes, when set, lazily returns the LIVE backend's task-type
// list so preflight can accept types added to the backend after this binary
// was built (the compiled-in validTaskTypes map above is only a mirror).
// composeWorkflowBody wires it to fetchServerTaskTypes before preflight;
// unit tests leave it nil, keeping preflight fully offline.
var fetchLiveTaskTypes func() map[string]bool

// fetchServerTaskTypes queries GET /v1/meta/workflows-v2-schema?section=taskTypes,
// the machine-readable type list BC derives from its TaskType enum at request
// time. Returns nil on any transport/shape error -- callers fall back to the
// compiled-in mirror, which is exactly the pre-existing behavior.
//
// The payload's `deprecated` array is UNIONED into deprecatedTaskTypes rather
// than replacing it: the compiled-in entries are what keeps the refusal
// working offline, and the live half retires a type without a CLI release.
// A deprecated type never comes back as authorable, even when the backend
// still lists it under `values`.
func fetchServerTaskTypes(c *client.Client) map[string]bool {
	data := fetchMetaSection(c, "taskTypes")
	if data == nil {
		return nil
	}
	var payload struct {
		TaskTypes struct {
			Values     []string `json:"values"`
			Deprecated []string `json:"deprecated"`
		} `json:"taskTypes"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	for _, v := range payload.TaskTypes.Deprecated {
		deprecatedTaskTypes[v] = true
	}
	if len(payload.TaskTypes.Values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(payload.TaskTypes.Values))
	for _, v := range payload.TaskTypes.Values {
		if deprecatedTaskTypes[v] {
			continue
		}
		out[v] = true
	}
	return out
}

// validWorkflowCategories mirrors the backend's WorkflowCategory enum.
// Common confusion: CUSTOMER and DEAL are entity TYPES (config.entityType),
// not categories. Keep these distinct in error messages.
var validWorkflowCategories = map[string]bool{
	"ACTION":         true,
	"EVALUATION":     true,
	"CONTACT":        true,
	"RECOMMENDATION": true,
	"OTHER":          true,
}

// fetchLiveWorkflowCategories, when set, lazily returns the LIVE backend's
// workflow-category vocabulary so validation can accept categories the backend
// gained after this binary was built (validWorkflowCategories above is only a
// mirror of the CategoryEnum). composeWorkflowBody wires it to
// fetchServerWorkflowCategories before preflight; unit tests leave it nil,
// keeping validation fully offline. Mirrors the fetchLiveTaskTypes hook.
var fetchLiveWorkflowCategories func() map[string]bool

// Live-category list, fetched at most once per compose and only when a category
// is missing from the compiled-in mirror. liveWorkflowCategoriesFetched guards
// the at-most-once semantics even when the fetch returns nil (offline or an
// older backend without the section). Reset when the hook is wired.
var (
	liveWorkflowCategories        map[string]bool
	liveWorkflowCategoriesFetched bool
)

// fetchServerWorkflowCategories queries
// GET /v1/meta/workflows-v2-schema?section=workflowCategories, the sorted string
// list BC derives from its CategoryEnum at request time. Returns nil on any
// transport/shape error (incl. a 404 from an older backend that lacks the
// section) so callers fall back to the compiled-in mirror -- exactly the
// pre-existing behavior. Mirrors fetchServerTaskTypes.
func fetchServerWorkflowCategories(c *client.Client) map[string]bool {
	data := fetchMetaSection(c, "workflowCategories")
	if data == nil {
		return nil
	}
	var payload struct {
		WorkflowCategories struct {
			Values []string `json:"values"`
		} `json:"workflowCategories"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.WorkflowCategories.Values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(payload.WorkflowCategories.Values))
	for _, v := range payload.WorkflowCategories.Values {
		out[strings.ToUpper(v)] = true
	}
	return out
}

// warnUnverifiedVocabularyValue reports a value this build does not recognize
// and could NOT confirm against the backend, then lets it through.
//
// Why fail open here. Every vocabulary that calls this (task type, workflow
// category, relationship kind, inputSchema type) is enforced by a Pydantic enum
// on the backend, so the API rejects a genuine typo on write regardless of what
// preflight decides. Preflight is an error-message optimization, not a safety
// boundary. Rejecting instead asserts knowledge this build does not have -- it
// cannot tell "invalid" from "valid but newer than me" without the backend --
// and because there is no --no-preflight escape hatch, a stale mirror plus an
// unreachable meta endpoint becomes an unrecoverable hard block on a perfectly
// valid spec. That is not hypothetical: `secret` (inputSchema type) and
// `RECOMMENDATION` (workflow category) were both missing from their mirrors.
//
// Condition operators deliberately do NOT use this: borrower-central logs
// "Unknown operator" and evaluates the item to False rather than failing
// (standard_class_activity.py), so a typo there silently makes a branch never
// match. Nothing downstream catches it, so preflight stays strict.
func warnUnverifiedVocabularyValue(path, value, vocabulary string) {
	fmt.Fprintf(os.Stderr,
		"# WARNING: %s=%q is not in this CLI build's compiled-in %s list, and the "+
			"live backend could not be consulted (offline, an older backend, or the "+
			"meta endpoint is unavailable) -- proceeding, since the backend validates "+
			"%s on write and will reject it if it is genuinely wrong. "+
			"Update altscore-cli to refresh its offline list.\n",
		path, value, vocabulary, vocabulary,
	)
}

// checkWorkflowAlias validates the spec's explicit workflow alias against the
// same shape the backend enforces at POST /v2/workflows ("alias must be
// kebab-case: lowercase letters, digits, and hyphens; start with a letter or
// digit; length 1-100 characters"). An empty alias is fine -- the server
// slugifies the label instead, which always yields a conforming alias. The
// alias is also what every credit-decisioning entity gets stamped with, so a
// bad one is caught here rather than after entities have been re-scoped.
func checkWorkflowAlias(alias string) error {
	if alias == "" {
		return nil
	}
	if validAliasPattern.MatchString(alias) && len(alias) <= 100 {
		return nil
	}
	return fmt.Errorf(
		"workflow alias %q is not kebab-case. The server derives the workflow's URL paths from it, so it "+
			"must be lowercase alphanumeric with internal dashes only (regex: ^[a-z0-9][a-z0-9-]*$) and at "+
			"most 100 characters. Don't use spaces, underscores, slashes, uppercase, or other punctuation. "+
			"Try %q.",
		alias, slugifyWorkflowLabel(alias),
	)
}

// checkWorkflowCategory validates a workflow's category, consulting the live
// backend at most once when the (upper-cased) category is absent from the
// compiled-in mirror. Empty category is always fine (the field is optional).
//   - compiled-in-known           -> accept (fast path, no fetch)
//   - live-known (newer backend)  -> warn + accept
//   - unknown to a reachable backend -> reject, listing the live vocabulary
//   - backend unreachable (offline / older backend / no hook wired)
//     -> warn + accept (see warnUnverifiedVocabularyValue)
func checkWorkflowCategory(category string) error {
	if category == "" {
		return nil
	}
	up := strings.ToUpper(category)
	if validWorkflowCategories[up] {
		return nil
	}
	if !liveWorkflowCategoriesFetched && fetchLiveWorkflowCategories != nil {
		liveWorkflowCategories = fetchLiveWorkflowCategories()
		liveWorkflowCategoriesFetched = true
	}
	if liveWorkflowCategories[up] {
		fmt.Fprintf(os.Stderr,
			"# WARNING: workflow.category=%q is newer than this CLI build "+
				"(absent from its compiled-in list) but IS accepted by the live backend -- proceeding. "+
				"Update altscore-cli to refresh its offline category list.\n",
			category,
		)
		return nil
	}
	if len(liveWorkflowCategories) > 0 {
		return fmt.Errorf(
			"workflow.category=%q is not a valid value. "+
				"The live backend was consulted and does not list it either (%d categories): %v. "+
				"Note: CUSTOMER and DEAL are workflow ENTITY TYPES (config.entityType), not categories.",
			category, len(liveWorkflowCategories), sortedBoolMapKeys(liveWorkflowCategories),
		)
	}
	warnUnverifiedVocabularyValue("workflow.category", category, "workflow-category")
	return nil
}

// validRelKinds mirrors the backend relationships-kind Literal
// (app/model/core/relationships.py). Only a mirror -- checkRelationshipKind
// consults the live backend once before rejecting, so a stale mirror can no
// longer cause a FALSE REJECTION of a valid relationship kind.
var validRelKinds = map[string]bool{
	"shareholder": true, "employee": true, "family": true,
	"other": true, "unspecified": true,
}

// fetchLiveRelationshipKinds, when set, lazily returns the LIVE backend's
// relationship-kind vocabulary. composeWorkflowBody wires it to
// fetchServerRelationshipKinds before preflight; unit tests leave it nil.
var fetchLiveRelationshipKinds func() map[string]bool

// Live relationship-kind list, fetched at most once per compose and only on the
// first miss. liveRelationshipKindsFetched guards at-most-once even when the
// fetch returns nil. Reset when the hook is wired.
var (
	liveRelationshipKinds        map[string]bool
	liveRelationshipKindsFetched bool
)

// fetchServerRelationshipKinds queries
// GET /v1/meta/workflows-v2-schema?section=relationshipKinds, the sorted string
// list BC derives from the relationships-kind Literal at request time. Returns
// nil on any transport/shape error so callers fall back to the compiled-in
// mirror. Mirrors fetchServerTaskTypes.
func fetchServerRelationshipKinds(c *client.Client) map[string]bool {
	data := fetchMetaSection(c, "relationshipKinds")
	if data == nil {
		return nil
	}
	var payload struct {
		RelationshipKinds struct {
			Values []string `json:"values"`
		} `json:"relationshipKinds"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.RelationshipKinds.Values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(payload.RelationshipKinds.Values))
	for _, v := range payload.RelationshipKinds.Values {
		out[v] = true
	}
	return out
}

// checkRelationshipKind validates a relationships item's kind, consulting the
// live backend at most once when the kind is absent from the compiled-in
// mirror. `path` is the caller-formatted field prefix (e.g.
// `node ref="x": relationshipsConfig.items[0]`). Rejects only when a REACHABLE
// backend also disowns the kind; an unverifiable kind warns and proceeds
// (warnUnverifiedVocabularyValue).
func checkRelationshipKind(kind, path string) error {
	if validRelKinds[kind] {
		return nil
	}
	if !liveRelationshipKindsFetched && fetchLiveRelationshipKinds != nil {
		liveRelationshipKinds = fetchLiveRelationshipKinds()
		liveRelationshipKindsFetched = true
	}
	if liveRelationshipKinds[kind] {
		fmt.Fprintf(os.Stderr,
			"# WARNING: %s.relationship=%q is newer than this CLI build "+
				"(absent from its compiled-in list) but IS accepted by the live backend -- proceeding. "+
				"Update altscore-cli to refresh its offline relationship-kind list.\n",
			path, kind,
		)
		return nil
	}
	if len(liveRelationshipKinds) > 0 {
		return fmt.Errorf(
			"%s.relationship=%q is not a known relationship kind. "+
				"The live backend was consulted and does not list it either (%d kinds): %v",
			path, kind, len(liveRelationshipKinds), sortedBoolMapKeys(liveRelationshipKinds),
		)
	}
	warnUnverifiedVocabularyValue(path+".relationship", kind, "relationship-kind")
	return nil
}

// validAliasPattern matches the alias regex the backend treats as URL-safe.
// Lowercase alphanumeric with internal dashes; backend does additional
// length/uniqueness checks but at minimum aliases must match this shape so
// they round-trip through path parameters.
var validAliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validInputSchemaTypes mirrors the SchemaTypes Pydantic discriminated union
// in borrower-central. The backend's error message lies ("permitted:
// 'array'"); the real enum is below. Used by preflight to reject typos in
// inputSchema.<field>.type before the API round-trip. Only a mirror --
// checkInputSchemaType consults the live backend once before rejecting.
//
// `secret` is how a node reads a stored tenant secret: declare
// inputSchema.<field> = {type: "secret", default: "<secretId>"} and the runtime
// swaps the secretId for the secret's value before the activity runs (see
// resolved_secret_inputs in borrower-central's standard_class_activity.py).
// It was missing here until it turned up as a false rejection.
var validInputSchemaTypes = map[string]bool{
	"string":  true,
	"integer": true,
	"number":  true,
	"boolean": true,
	"object":  true,
	"array":   true,
	"secret":  true,
}

// fetchLiveInputSchemaTypes, when set, lazily returns the LIVE backend's
// inputSchema-type vocabulary. composeWorkflowBody wires it to
// fetchServerInputSchemaTypes before preflight; unit tests leave it nil.
var fetchLiveInputSchemaTypes func() map[string]bool

// Live inputSchema-type list, fetched at most once per compose and only on the
// first miss. liveInputSchemaTypesFetched guards at-most-once even when the
// fetch returns nil. Reset when the hook is wired.
var (
	liveInputSchemaTypes        map[string]bool
	liveInputSchemaTypesFetched bool
)

// fetchServerInputSchemaTypes queries
// GET /v1/meta/workflows-v2-schema?section=inputSchemaTypes, the sorted string
// list BC derives from the SchemaTypes discriminated union at request time.
// Returns nil on any transport/shape error so callers fall back to the
// compiled-in mirror. Mirrors fetchServerTaskTypes.
func fetchServerInputSchemaTypes(c *client.Client) map[string]bool {
	data := fetchMetaSection(c, "inputSchemaTypes")
	if data == nil {
		return nil
	}
	var payload struct {
		InputSchemaTypes struct {
			Values []string `json:"values"`
		} `json:"inputSchemaTypes"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.InputSchemaTypes.Values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(payload.InputSchemaTypes.Values))
	for _, v := range payload.InputSchemaTypes.Values {
		out[v] = true
	}
	return out
}

// checkInputSchemaType validates a schema field's type, consulting the live
// backend at most once when the type is absent from the compiled-in mirror.
// `path` is the caller-formatted field path (e.g. `workflow.inputVariables.x.type`
// or `node ref="y": inputSchema.z.type`), so the `=%q` in the message reads as
// `path=type`. Rejects only when a REACHABLE backend also disowns the type; an
// unverifiable type warns and proceeds (warnUnverifiedVocabularyValue).
func checkInputSchemaType(t, path string) error {
	if validInputSchemaTypes[t] {
		return nil
	}
	if !liveInputSchemaTypesFetched && fetchLiveInputSchemaTypes != nil {
		liveInputSchemaTypes = fetchLiveInputSchemaTypes()
		liveInputSchemaTypesFetched = true
	}
	if liveInputSchemaTypes[t] {
		fmt.Fprintf(os.Stderr,
			"# WARNING: %s=%q is newer than this CLI build "+
				"(absent from its compiled-in list) but IS accepted by the live backend -- proceeding. "+
				"Update altscore-cli to refresh its offline type list.\n",
			path, t,
		)
		return nil
	}
	if len(liveInputSchemaTypes) > 0 {
		return fmt.Errorf(
			"%s=%q is not a valid type. "+
				"The live backend was consulted and does not list it either (%d types): %v.",
			path, t, len(liveInputSchemaTypes), sortedBoolMapKeys(liveInputSchemaTypes),
		)
	}
	warnUnverifiedVocabularyValue(path, t, "inputSchema-type")
	return nil
}

// closestTaskType returns the canonical TaskType nearest to a given typo by
// Levenshtein distance, or "" if nothing is meaningfully close. Deprecated
// types are skipped: a suggestion the author cannot act on is worse than none.
func closestTaskType(input string) string {
	best := ""
	bestDist := -1
	cutoff := len(input) / 2
	if cutoff < 2 {
		cutoff = 2
	}
	for t := range validTaskTypes {
		if deprecatedTaskTypes[t] {
			continue
		}
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
