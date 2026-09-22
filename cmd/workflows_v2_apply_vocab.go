package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// Mirrors the AUTHORABLE half of the backend TaskType enum. Preflight rejects a typo here
// before any /v2/tasks row is written, because there is no rollback path.
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
	"artifact": true,
}

// Compiled in so the refusal also holds offline. fetchServerTaskTypes UNIONS the backend's
// own `deprecated` list into this map; entries are only ever added, never removed.
var deprecatedTaskTypes = map[string]bool{
	"create-alert": true, "create-borrower": true, "create-identity": true,
	"data-store": true, "end-old": true, "fetch-borrower-entities": true,
	"fetch-entity": true, "html-template": true, "pdf-report": true,
	"soap": true, "update-borrower": true, "update-borrower-name": true,
	"webhook": true,
}

// A type absent from this map has no replacement and is simply refused.
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

// Mirror of the backend's diff-based rule: BC refuses a retired type only when the target
// does not already hold it. nil existingTypes (a create, or a failed lookup) refuses all.
func deprecatedTaskTypeRefused(taskType string, existingTypes map[string]bool) bool {
	return deprecatedTaskTypes[taskType] && !existingTypes[taskType]
}

func warnDeprecatedTaskTypeCarriedForward(path, taskType string) {
	fmt.Fprintf(os.Stderr,
		"# WARNING: %s: task type %q is DEPRECATED and cannot be newly authored, but the "+
			"apply target already carries it -- CARRIED FORWARD unchanged. %s "+
			"Mirrors the backend, which refuses a retired type only for newly added work so a "+
			"legacy workflow stays editable.\n",
		path, taskType, deprecatedTaskTypeGuidance(taskType),
	)
}

func deprecatedTaskTypeGuidance(taskType string) string {
	if r := deprecatedTaskTypeReplacements[taskType]; r != "" {
		return fmt.Sprintf("Use %s instead.", r)
	}
	return "It has no replacement -- drop the node."
}

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

// Wired by composeWorkflowBody; unit tests leave it nil, which keeps preflight offline.
var fetchLiveTaskTypes func() map[string]bool

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

// CUSTOMER and DEAL are entity TYPES (config.entityType), not categories.
var validWorkflowCategories = map[string]bool{
	"ACTION":         true,
	"EVALUATION":     true,
	"CONTACT":        true,
	"RECOMMENDATION": true,
	"OTHER":          true,
}

var fetchLiveWorkflowCategories func() map[string]bool

// The `Fetched` flag guards the at-most-once semantics even when the fetch returns nil.
var (
	liveWorkflowCategories        map[string]bool
	liveWorkflowCategoriesFetched bool
)

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

// Fails open: the backend enforces each vocabulary with a Pydantic enum and this build cannot
// tell "invalid" from "newer than me". NOT for condition operators: BC evaluates those to False.
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

// The alias is also what every credit-decisioning entity gets stamped with, so a bad one is
// caught here rather than after the entities have been re-scoped.
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

var validRelKinds = map[string]bool{
	"shareholder": true, "employee": true, "family": true,
	"other": true, "unspecified": true,
}

var fetchLiveRelationshipKinds func() map[string]bool

var (
	liveRelationshipKinds        map[string]bool
	liveRelationshipKindsFetched bool
)

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

var validAliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// `secret` is how a node reads a stored tenant secret: {type: "secret", default: "<secretId>"}
// and the runtime swaps in the value before the activity runs.
var validInputSchemaTypes = map[string]bool{
	"string":  true,
	"integer": true,
	"number":  true,
	"boolean": true,
	"object":  true,
	"array":   true,
	"secret":  true,
}

var fetchLiveInputSchemaTypes func() map[string]bool

var (
	liveInputSchemaTypes        map[string]bool
	liveInputSchemaTypesFetched bool
)

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

// Deprecated types are skipped: a suggestion the author cannot act on is worse than none.
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
