package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// diffBundle builds an export bundle with one conditional node whose body and
// specRef the caller controls, so a test can vary exactly one thing.
//
// Mirrors the REAL /v2/workflows/{id}/export shape, verified against a live
// bundle: the version is at the ROOT as sourceVersion, and bundle.workflow holds
// only the authoring body -- no version and no status anywhere in the export.
func diffBundle(version int, opts diffBundleOpts) json.RawMessage {
	specRef := `"specRef": "fetch", "workflowAlias": "kyb",`
	if opts.noSpecRef {
		specRef = ""
	}
	alias := "fetch-111111"
	if opts.rotatedAlias != "" {
		alias = opts.rotatedAlias
	}
	operator := "not_in"
	value := `["", "document-not-found"]`
	if opts.scalarOperator {
		operator = "not_equals"
		value = `"document-not-found"`
	}
	extraVar := ""
	if opts.extraCustomVar {
		extraVar = `, "spare": {"expression": "result = 2", "returnValue": "result"}`
	}
	expression := "result = 1"
	if opts.changedExpression {
		expression = "result = 1 + 1 + 1"
	}
	edgeHandle := "branch-else"
	if opts.rewiredEdge {
		edgeHandle = "output"
	}

	return json.RawMessage(fmt.Sprintf(`{
  "sourceAlias": "kyb",
  "sourceVersion": %d,
  "workflow": {
    "label": "KYB",
    "inputVariables": {"ruc": {"type": "string", "required": true}},
    "customVariables": {"quick": {"expression": %q, "returnValue": "result"}%s},
    "nodes": [
      {"nodeId": "start-a", "type": "start", "label": "Start", "taskAlias": "start-a"},
      {"nodeId": %q, "type": "conditional", "label": "Art7 con problemas?", "taskAlias": %q},
      {"nodeId": "end-z", "type": "end", "label": "End", "taskAlias": "end-z"}
    ],
    "edges": [
      {"id": "e1", "sourceNodeId": "start-a", "targetNodeId": %q},
      {"id": "e2", "sourceNodeId": %q, "targetNodeId": "end-z", "sourceHandle": %q}
    ]
  },
  "tasks": [
    {"alias": "start-a", "type": "start", "label": "Start", "specRef": "start", "workflowAlias": "kyb"},
    {"alias": %q, "type": "conditional", "label": "Art7 con problemas?", %s
     "branches": [{"id": "b1", "conditions": {"operator": "OR", "items": [
        {"field": "acto_art7", "operator": %q, "value": %s}]}}]},
    {"alias": "end-z", "type": "end", "label": "End", "specRef": "end", "workflowAlias": "kyb"}
  ]
}`, version, expression, extraVar, alias, alias, alias, alias, edgeHandle, alias, specRef, operator, value))
}

type diffBundleOpts struct {
	noSpecRef         bool
	scalarOperator    bool
	rotatedAlias      string
	extraCustomVar    bool
	changedExpression bool
	rewiredEdge       bool
}

type fakeDoer struct{ byPath map[string]json.RawMessage }

func (f fakeDoer) Do(method, module, path string, body any) (json.RawMessage, int, error) {
	raw, ok := f.byPath[path]
	if !ok {
		return nil, 404, fmt.Errorf("no stub for %s", path)
	}
	return raw, 200, nil
}

func loadTwo(t *testing.T, a, b json.RawMessage) diffReport {
	t.Helper()
	doer := fakeDoer{byPath: map[string]json.RawMessage{
		"/v2/workflows/id-a/export": a,
		"/v2/workflows/id-b/export": b,
	}}
	specA, sideA, err := loadSideForDiff(doer, "id-a")
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	specB, sideB, err := loadSideForDiff(doer, "id-b")
	if err != nil {
		t.Fatalf("load B: %v", err)
	}
	return buildDiffReport(specA, sideA, specB, sideB)
}

func TestDiffIdenticalVersionsReportNothing(t *testing.T) {
	r := loadTwo(t, diffBundle(47, diffBundleOpts{}), diffBundle(47, diffBundleOpts{}))
	if len(r.NodesChanged) != 0 || len(r.NodesOnlyInA) != 0 || len(r.NodesOnlyInB) != 0 {
		t.Fatalf("expected no node differences, got %+v", r)
	}
	if len(r.EdgesOnlyInA) != 0 || len(r.EdgesOnlyInB) != 0 {
		t.Fatalf("expected no edge differences, got %+v", r)
	}
	if got := renderDiffReport(r); !strings.Contains(got, "No differences") {
		t.Fatalf("expected a no-differences line, got:\n%s", got)
	}
}

func TestDiffReportsATaskBodyChangeApplyDiffWouldMiss(t *testing.T) {
	// The real case: a condition operator degraded from an array-valued not_in
	// to a scalar not_equals. It lives inside the task body, which apply --diff
	// never inspects (it compares type / label / config / inputMappings).
	r := loadTwo(t,
		diffBundle(47, diffBundleOpts{}),
		diffBundle(48, diffBundleOpts{scalarOperator: true}))

	if len(r.NodesChanged) != 1 {
		t.Fatalf("expected exactly one changed node, got %+v", r.NodesChanged)
	}
	changed := r.NodesChanged[0]
	if changed.Type != "conditional" {
		t.Fatalf("expected the conditional node, got %q", changed.Type)
	}
	fields := []string{}
	for _, f := range changed.Fields {
		fields = append(fields, f.Field)
	}
	if len(fields) != 1 || fields[0] != "branches" {
		t.Fatalf("expected only branches to differ, got %v", fields)
	}
	if changed.Fields[0].BytesA == changed.Fields[0].BytesB {
		t.Fatalf("expected differing byte counts, got %+v", changed.Fields[0])
	}
}

func TestDiffSurfacesSpecRefCensus(t *testing.T) {
	r := loadTwo(t,
		diffBundle(47, diffBundleOpts{}),
		diffBundle(48, diffBundleOpts{noSpecRef: true}))

	if len(r.A.TasksMissingRef) != 0 {
		t.Fatalf("A is CLI-authored, expected no missing specRef, got %v", r.A.TasksMissingRef)
	}
	if len(r.B.TasksMissingRef) != 1 || r.B.TasksMissingRef[0] != "fetch-111111" {
		t.Fatalf("expected the builder-touched task named, got %v", r.B.TasksMissingRef)
	}
	got := renderDiffReport(r)
	if !strings.Contains(got, "1 of 3 tasks missing specRef") {
		t.Fatalf("expected the census counts, got:\n%s", got)
	}
	if !strings.Contains(got, "Hub builder") {
		t.Fatalf("expected the census to say what a missing specRef means, got:\n%s", got)
	}
}

func TestDiffCleanSpecRefSaysSo(t *testing.T) {
	r := loadTwo(t, diffBundle(47, diffBundleOpts{}), diffBundle(47, diffBundleOpts{}))
	if got := renderDiffReport(r); !strings.Contains(got, "present on all 3 tasks in A") {
		t.Fatalf("expected the all-present line, got:\n%s", got)
	}
}

func TestDiffReportsAliasRotationAsRenameNotAddRemove(t *testing.T) {
	// This is the downstream symptom of the specRef loss: export + re-apply
	// mints a fresh alias, so the node looks deleted and a different one added.
	r := loadTwo(t,
		diffBundle(47, diffBundleOpts{}),
		diffBundle(48, diffBundleOpts{rotatedAlias: "fetch-999999", noSpecRef: true}))

	if len(r.NodesRenamed) != 1 {
		t.Fatalf("expected one rename, got %+v", r.NodesRenamed)
	}
	if r.NodesRenamed[0][0] != "fetch-111111" || r.NodesRenamed[0][1] != "fetch-999999" {
		t.Fatalf("unexpected rename pair %+v", r.NodesRenamed[0])
	}
	if len(r.NodesOnlyInA) != 0 || len(r.NodesOnlyInB) != 0 {
		t.Fatalf("a rename must not also read as add/remove, got A=%v B=%v", r.NodesOnlyInA, r.NodesOnlyInB)
	}
	got := renderDiffReport(r)
	if !strings.Contains(got, "ALIAS ROTATION") || !strings.Contains(got, "dangling") {
		t.Fatalf("expected the rotation warning, got:\n%s", got)
	}
}

func TestDiffReportsVariableAndEdgeChanges(t *testing.T) {
	r := loadTwo(t,
		diffBundle(47, diffBundleOpts{}),
		diffBundle(48, diffBundleOpts{
			extraCustomVar: true, changedExpression: true, rewiredEdge: true}))

	if len(r.CustomVarsOnlyInB) != 1 || r.CustomVarsOnlyInB[0] != "spare" {
		t.Fatalf("expected the added custom variable, got %v", r.CustomVarsOnlyInB)
	}
	if len(r.CustomVarsChanged) != 1 || r.CustomVarsChanged[0].Name != "quick" {
		t.Fatalf("expected the changed expression, got %+v", r.CustomVarsChanged)
	}
	if len(r.EdgesOnlyInA) != 1 || r.EdgesOnlyInA[0].Handle != "branch-else" {
		t.Fatalf("expected the old edge handle, got %+v", r.EdgesOnlyInA)
	}
	if len(r.EdgesOnlyInB) != 1 || r.EdgesOnlyInB[0].Handle != "output" {
		t.Fatalf("expected the new edge handle, got %+v", r.EdgesOnlyInB)
	}
}

func TestDiffIgnoresBookkeepingFields(t *testing.T) {
	// taskVersion bumps on every write; reporting it would mark every node
	// changed and bury the real hunks.
	for field := range nodeFieldDropList {
		if field == "ref" {
			continue
		}
		if !nodeFieldDropList[field] {
			t.Fatalf("%s should be dropped", field)
		}
	}
	a := map[string]any{"nodes": []any{map[string]any{"ref": "n1", "type": "http", "taskVersion": 1.0}}}
	b := map[string]any{"nodes": []any{map[string]any{"ref": "n1", "type": "http", "taskVersion": 9.0}}}
	r := buildDiffReport(a, sideSummary{}, b, sideSummary{})
	if len(r.NodesChanged) != 0 {
		t.Fatalf("taskVersion alone must not count as a change, got %+v", r.NodesChanged)
	}
}

func TestDiffReadsVersionFromBundleRootAndDegradesOnStatus(t *testing.T) {
	// sourceVersion lives at the bundle ROOT; bundle.workflow carries only the
	// authoring body, with no version and no status at all.
	doer := fakeDoer{byPath: map[string]json.RawMessage{
		"/v2/workflows/id-a/export": diffBundle(47, diffBundleOpts{}),
	}}
	_, side, err := loadSideForDiff(doer, "id-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fmt.Sprint(side.Version) != "47" {
		t.Fatalf("expected version 47 from sourceVersion, got %v", side.Version)
	}
	if side.Alias != "kyb" {
		t.Fatalf("expected alias kyb, got %q", side.Alias)
	}
	// No stub for the plain workflow GET, so the status lookup fails. That must
	// not fail the diff.
	if side.Status != "" {
		t.Fatalf("expected an empty status when the lookup fails, got %q", side.Status)
	}
	if got := renderDiffReport(diffReport{A: side, B: side}); !strings.Contains(got, "kyb v47 -") {
		t.Fatalf("expected the header to render a missing status as a dash, got:\n%s", got)
	}
}

func TestDiffExportFailurePropagates(t *testing.T) {
	doer := fakeDoer{byPath: map[string]json.RawMessage{}}
	if _, _, err := loadSideForDiff(doer, "missing"); err == nil {
		t.Fatal("expected an error when the export call fails")
	}
}
