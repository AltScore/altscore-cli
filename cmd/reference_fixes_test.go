package cmd

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// readCmdSource reads a file from this package's directory. The help-text
// assertions below are deliberately source-level: the drift they guard against
// is in human-facing prose, which has no runtime representation to assert on.
func readCmdSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// Regression guards for the Jul-2026 reference audit: CLI call sites and help
// text that had drifted from borrower-central. Each case below was a real
// defect -- a route that does not exist, a param BC ignores, or help naming
// something that was deleted. The BC-side facts each assertion encodes are
// noted so a future BC change makes the reason findable.

func TestDocumentsUploadUsesTheRouteThatExists(t *testing.T) {
	// BC has POST /v1/documents/{id}/attachments/upload (documents/handler.py:172).
	// It has never had the singular PUT .../attachment this used to send.
	src := readCmdSource(t, "resource.go")
	if strings.Contains(src, `"/attachment"`) {
		t.Error("documents upload still targets the singular /attachment route, which BC does not have")
	}
	if !strings.Contains(src, "/attachments/upload") {
		t.Error("documents upload should POST to /attachments/upload")
	}
}

func TestMetricsHasNoSetTestCommand(t *testing.T) {
	// BC exposes no PUT /v1/metrics/{id}/is-test, so `metrics set-test` 404d.
	// Asserted on the built command tree rather than the def, so it fails if the
	// command is ever reintroduced by any route.
	var metrics *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Name() == "metrics" {
			metrics = c
			break
		}
	}
	if metrics == nil {
		t.Fatal("metrics command not registered")
	}
	for _, sub := range metrics.Commands() {
		if sub.Name() == "set-test" {
			t.Error("metrics exposes set-test, but BC has no /v1/metrics/{id}/is-test route")
		}
	}
	// Sanity: a resource that DOES have the route still offers it, so this test
	// cannot pass merely because set-test was dropped everywhere.
	var rules *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Name() == "evaluation-rules" {
			rules = c
			break
		}
	}
	if rules == nil {
		t.Fatal("evaluation-rules command not registered")
	}
	found := false
	for _, sub := range rules.Commands() {
		if sub.Name() == "set-test" {
			found = true
		}
	}
	if !found {
		t.Error("evaluation-rules should still expose set-test (BC has that route)")
	}
}

func TestAltdataUsesSourceIdAlias(t *testing.T) {
	// documentation/handler.py declares Query(..., alias="sourceId") as REQUIRED on
	// both data-dictionary and output-example, so `source_id` is a hard 400.
	src := readCmdSource(t, "altdata.go")
	if strings.Contains(src, "source_id=") {
		t.Error("altdata still sends source_id; BC requires the sourceId alias and 400s otherwise")
	}
	for _, want := range []string{"data-dictionary?sourceId=", "output-example?sourceId="} {
		if !strings.Contains(src, want) {
			t.Errorf("expected %q in altdata.go", want)
		}
	}
}

func TestAltdataSearchDropsLocaleAndEscapesQuery(t *testing.T) {
	// BC's search accepts query/country/sourceId/page/per-page -- no `locale`.
	// And an unescaped space made Go emit a malformed request line, so the
	// command's own documented example ("credit score") 400d before the handler.
	src := readCmdSource(t, "altdata.go")
	if strings.Contains(src, "locale=") || strings.Contains(src, `"locale"`) {
		t.Error("altdata search still sends/declares locale, which BC ignores")
	}
	if !strings.Contains(src, "url.QueryEscape(args[0])") {
		t.Error("altdata search must escape the query; a bare space produces a malformed request")
	}
}

func TestQueryEscapedSearchIsSafeForMultiWord(t *testing.T) {
	// Guards the actual property rather than the call site.
	if got := url.QueryEscape("credit score"); strings.Contains(got, " ") {
		t.Errorf("QueryEscape left a raw space: %q", got)
	}
}

func TestCommentTaskTypeIsGone(t *testing.T) {
	// BC deleted TaskType.COMMENT (91d05553, #1234). Keeping it in the allowlist
	// let a comment node clear preflight and then fail at POST /v2/tasks
	// mid-compose, orphaning every task created before it.
	if validTaskTypes["comment"] {
		t.Error("`comment` is no longer a BC task type; annotations belong in the top-level notes array")
	}
}

func TestWorkflowCategoriesIncludeRecommendation(t *testing.T) {
	// BC CategoryEnum has five members; the help text listed four.
	if !validWorkflowCategories["RECOMMENDATION"] {
		t.Error("RECOMMENDATION missing from validWorkflowCategories")
	}
	src := readCmdSource(t, "root.go")
	if !strings.Contains(src, "RECOMMENDATION") {
		t.Error("workflows-v2 help must list RECOMMENDATION alongside the other categories")
	}
}

func TestQueryOutputsDropsUnsupportedTestFlags(t *testing.T) {
	// GET /v1/executions/outputs accepts neither include-tests nor test-only.
	// --test-only was the dangerous one: silently dropped, so the command
	// returned NON-test records while the caller believed they were filtered.
	src := readCmdSource(t, "executions.go")
	if strings.Contains(src, `"test-only"`) || strings.Contains(src, `"include-tests"`) {
		t.Error("query-outputs must not offer test-mode flags that BC's outputs endpoint ignores")
	}
}

func TestParentFlagIsGone(t *testing.T) {
	// Zero assignments existed repo-wide; every `def.ParentFlag != ""` guard was
	// statically false.
	src := readCmdSource(t, "resource.go")
	if strings.Contains(src, "ParentFlag") {
		t.Error("ParentFlag was dead code and should stay removed")
	}
}

func TestInputMappingErrorListsEveryReservedScope(t *testing.T) {
	// The message hardcoded six namespaces while reservedMappingScopes held seven --
	// `self` (added with the End node's own-output scope, cli#92) was missing, so the
	// error told authors a valid namespace was invalid. Derive it from the map instead.
	src := readCmdSource(t, "workflows_v2_apply_refs.go")
	if strings.Contains(src, "Valid namespaces: inputs, custom, system, task_outputs, task_outputs_by_type, entity") {
		t.Error("inputMappings error still hardcodes the namespace list; derive it from reservedMappingScopes")
	}
	listed := reservedScopesList()
	for scope := range reservedMappingScopes {
		if !strings.Contains(listed, scope) {
			t.Errorf("reservedScopesList() omits %q", scope)
		}
	}
	if !strings.Contains(listed, "self") {
		t.Error("`self` must be listed: compute-variable dependencies and End's own outputs use it")
	}
}

// The 13 types AltScore retired. BC still parses them so live workflows keep
// running, but it refuses them for new authoring -- so no CLI path may author
// one either. Leaving one in validTaskTypes let a delivery engineer clear
// preflight and hit the refusal at POST /v2/tasks mid-compose, orphaning every
// task created before it (there is no rollback path). Listed literally rather
// than derived from deprecatedTaskTypes so dropping an entry from that map is
// a test failure, not a silently smaller check.
var retiredTaskTypes = []string{
	"create-alert", "create-borrower", "create-identity", "data-store",
	"end-old", "fetch-borrower-entities", "fetch-entity", "html-template",
	"pdf-report", "soap", "update-borrower", "update-borrower-name", "webhook",
}

func TestDeprecatedTaskTypesAreRefused(t *testing.T) {
	for _, typ := range retiredTaskTypes {
		if validTaskTypes[typ] {
			t.Errorf("%q is retired and must not be in validTaskTypes", typ)
		}
		if !deprecatedTaskTypes[typ] {
			t.Errorf("%q must be in deprecatedTaskTypes -- that map is what makes the refusal work offline", typ)
		}
	}
}

// A type can never be both authorable and retired: every authoring path checks
// the deprecated map first, so an entry in both would make the refusal depend
// on check order.
func TestNoTaskTypeIsBothValidAndDeprecated(t *testing.T) {
	for typ := range deprecatedTaskTypes {
		if validTaskTypes[typ] {
			t.Errorf("%q is in both validTaskTypes and deprecatedTaskTypes", typ)
		}
	}
}

func TestDeprecatedTaskTypeReplacementsNameRetiredTypes(t *testing.T) {
	// Guidance for a type that is not retired is stale guidance.
	for typ := range deprecatedTaskTypeReplacements {
		if !deprecatedTaskTypes[typ] {
			t.Errorf("deprecatedTaskTypeReplacements has %q, which is not deprecated", typ)
		}
	}
	// The eight with a documented successor must keep naming it; the other
	// five are refused with no replacement on purpose.
	for _, typ := range []string{
		"create-borrower", "create-identity", "data-store", "fetch-borrower-entities",
		"fetch-entity", "pdf-report", "soap", "update-borrower",
	} {
		if deprecatedTaskTypeReplacements[typ] == "" {
			t.Errorf("%q has a replacement task type; the refusal must name it", typ)
		}
	}
}

func TestClosestTaskTypeNeverSuggestsARetiredType(t *testing.T) {
	// A "did you mean" that points at a type apply refuses is worse than no
	// suggestion: it sends the author round the same loop.
	for _, typ := range retiredTaskTypes {
		for _, probe := range []string{typ, typ + "s", typ + "-x"} {
			if s := closestTaskType(probe); deprecatedTaskTypes[s] {
				t.Errorf("closestTaskType(%q) suggested retired type %q", probe, s)
			}
		}
	}
}

func TestTasksV2HasNoGetSoapMethodsCommand(t *testing.T) {
	// `soap` is deleted from BC entirely, so GET
	// /v2/tasks/{alias}/services/methods has nothing left to introspect.
	// Asserted on the built command tree so it fails if the command is
	// reintroduced by any route.
	var tasks *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Name() == "tasks-v2" {
			tasks = c
			break
		}
	}
	if tasks == nil {
		t.Fatal("tasks-v2 command not registered")
	}
	found := map[string]bool{}
	for _, sub := range tasks.Commands() {
		found[sub.Name()] = true
	}
	if found["get-soap-methods"] {
		t.Error("tasks-v2 still exposes get-soap-methods, but the soap task type is gone")
	}
	// Sanity: the group is intact, so this cannot pass by tasks-v2 losing
	// every subcommand.
	for _, want := range []string{"create", "create-version", "list", "get", "delete"} {
		if !found[want] {
			t.Errorf("tasks-v2 should still expose %q", want)
		}
	}
	// The group's Long help doubles as the type palette an author reads first.
	if strings.Contains(tasks.Long, "webhook") {
		t.Error("tasks-v2 help still lists `webhook` as a common type; it is retired")
	}
	if strings.Contains(tasks.Long, "soap") {
		t.Error("tasks-v2 help still mentions soap")
	}
}
