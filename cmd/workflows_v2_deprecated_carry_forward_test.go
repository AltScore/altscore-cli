package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The deprecation refusal mirrors the backend's diff-based rule: BC refuses a
// retired task type only for NEWLY added work, so a workflow that already
// carries such a node stays editable and can be migrated off it. These tests
// pin both halves -- the refusal that must stay, and the exemption that must
// not become a blanket hole.

// carryForwardSpec is a two-task spec (start -> n1 -> end) parameterized by the
// node type under test, so the deprecation gate is the only thing that can
// fail it.
func carryForwardSpec(taskType string, existing map[string]bool) *composeSpec {
	return &composeSpec{
		Label:             "Legacy flow",
		Alias:             "legacy-flow",
		Category:          "EVALUATION",
		ExtraNodes:        startNode,
		ExistingNodeTypes: existing,
		Tasks: []map[string]any{
			{"ref": "n1", "type": taskType, "label": "Legacy node"},
			{"ref": "e1", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{{"from": "start", "to": "n1"}, {"from": "n1", "to": "e1"}},
	}
}

// CREATE mode carries nothing forward, so every retired type is new authoring
// and must still hard-error -- including with no live hook wired, which is the
// offline case a delivery engineer actually hits. Nothing here may soften into
// a warning: an empty target set is exactly what a create looks like.
func TestPreflightTasks_DeprecatedTypeStillFatalOnCreate(t *testing.T) {
	fetchLiveTaskTypes = nil
	for _, typ := range retiredTaskTypes {
		var err error
		stderr := captureStderr(t, func() { err = preflightTasks(carryForwardSpec(typ, nil)) })
		if err == nil {
			t.Errorf("create with %q must be refused, got nil", typ)
			continue
		}
		if !strings.Contains(err.Error(), "DEPRECATED") {
			t.Errorf("%q: refusal must say the type is deprecated, got: %v", typ, err)
		}
		if strings.Contains(stderr, "CARRIED FORWARD") {
			t.Errorf("%q: a create must never report a carry-forward: %q", typ, stderr)
		}
	}
}

// UPDATE mode over a workflow whose stored graph ALREADY holds the retired type
// is a carry-forward: accepted, with a non-fatal warning that names the node so
// it is visible without blocking. This is the case the strict refusal broke --
// the legacy workflow could not be applied at all, not even to change an
// unrelated node.
func TestPreflightTasks_DeprecatedTypeCarriedForwardOnUpdate(t *testing.T) {
	fetchLiveTaskTypes = nil
	for _, typ := range retiredTaskTypes {
		var err error
		stderr := captureStderr(t, func() {
			err = preflightTasks(carryForwardSpec(typ, map[string]bool{typ: true, "end": true}))
		})
		if err != nil {
			t.Errorf("%q already in the target must be carried forward, got: %v", typ, err)
			continue
		}
		if !strings.Contains(stderr, "CARRIED FORWARD") || !strings.Contains(stderr, typ) {
			t.Errorf("%q: carry-forward must warn on stderr and name the type, got: %q", typ, stderr)
		}
		if !strings.Contains(stderr, `node ref="n1"`) {
			t.Errorf("%q: the warning must name the node, got: %q", typ, stderr)
		}
		// A retired type is deliberately absent from validTaskTypes, so the
		// vocabulary block must be skipped rather than re-diagnosing the node.
		if strings.Contains(stderr, "live backend could not be consulted") {
			t.Errorf("%q was routed through the unverified-vocabulary warning: %q", typ, stderr)
		}
		if strings.Contains(stderr, "newer than this CLI build") {
			t.Errorf("%q was reported as a newer type: %q", typ, stderr)
		}
	}
}

// The assertion that the exemption is not a blanket hole: a target that carries
// ONE retired type does not license a DIFFERENT one. Adding `soap` to a
// workflow that only holds `create-borrower` is new authoring and stays fatal.
func TestPreflightTasks_DifferentDeprecatedTypeStillFatalOnUpdate(t *testing.T) {
	fetchLiveTaskTypes = nil
	existing := map[string]bool{"create-borrower": true, "end": true}
	var err error
	stderr := captureStderr(t, func() { err = preflightTasks(carryForwardSpec("soap", existing)) })
	if err == nil {
		t.Fatal("adding `soap` to a workflow that carries only `create-borrower` must be refused")
	}
	if !strings.Contains(err.Error(), "DEPRECATED") || !strings.Contains(err.Error(), "soap") {
		t.Errorf("refusal must name the newly added type, got: %v", err)
	}
	// And it must be refused, not warned about.
	if strings.Contains(stderr, "CARRIED FORWARD") {
		t.Errorf("a newly added retired type must not be reported as carried forward: %q", stderr)
	}
	// Sanity: the same target DOES exempt the type it actually holds, so the
	// case above cannot pass merely because the exemption never fires.
	var held error
	captureStderr(t, func() { held = preflightTasks(carryForwardSpec("create-borrower", existing)) })
	if held != nil {
		t.Errorf("the type the target holds must still be carried forward, got: %v", held)
	}
}

// End-to-end over the single assembly entry point, not just preflight: the
// whole point of the finding is that a legacy workflow could not be applied AT
// ALL, so clearing preflight is not enough -- the node has to survive
// normalization and reach the flat spec with its type intact. Assembles with a
// nil client (no network: the deprecated branch skips the live-type fetch) and
// asserts the retired node is still in the graph the server would receive.
func TestComposeWorkflowBody_CarriesForwardADeprecatedNode(t *testing.T) {
	spec := &composeSpec{
		Alias:             "legacy-flow",
		Label:             "Legacy flow",
		Category:          "EVALUATION",
		ExistingNodeTypes: map[string]bool{"start": true, "create-borrower": true, "end": true},
		InputVariables:    map[string]any{"tax_id": map[string]any{"type": "string"}},
		ExtraNodes:        []map[string]any{{"ref": "start", "type": "start", "label": "Start"}},
		Tasks: []map[string]any{
			{"ref": "cb", "type": "create-borrower", "label": "Legacy create borrower",
				"inputMappings": map[string]any{"tax_id": "inputs.tax_id"}},
			{"ref": "end", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{{"from": "start", "to": "cb"}, {"from": "cb", "to": "end"}},
	}
	capture := newComposeCapture()
	var wf map[string]any
	var err error
	stderr := captureStderr(t, func() {
		wf, err = composeWorkflowBody(nil, spec, true, false, true, false, false, true, capture)
	})
	if err != nil {
		t.Fatalf("assembling an update that carries a retired node must succeed, got: %v", err)
	}
	if !strings.Contains(stderr, "CARRIED FORWARD") || !strings.Contains(stderr, "create-borrower") {
		t.Errorf("assembly must warn about the carried-forward node, got: %q", stderr)
	}
	flat, err := buildFlatSpecForServer(wf, capture, "legacy-flow")
	if err != nil {
		t.Fatalf("buildFlatSpecForServer: %v", err)
	}
	found := false
	for _, n := range toMapSlice(flat["nodes"]) {
		if n["ref"] == "cb" {
			found = true
			if n["type"] != "create-borrower" {
				t.Errorf("the carried-forward node's type must travel unchanged, got %v", n["type"])
			}
		}
	}
	if !found {
		t.Error("the carried-forward node is missing from the flat spec")
	}
}

// A live backend that retires a type after this binary shipped is honoured the
// same way: refused when new, carried forward when the target holds it. The
// live authorable set never lists a retired type (fetchServerTaskTypes filters
// them out), so the carry-forward must not fall through to the unknown-type
// error.
func TestPreflightTasks_LiveRetiredTypeFollowsTheSameRule(t *testing.T) {
	const typ = "brand-new-retired"
	fetchLiveTaskTypes = func() map[string]bool {
		deprecatedTaskTypes[typ] = true
		return map[string]bool{"http": true, "end": true, "start": true}
	}
	defer func() {
		fetchLiveTaskTypes = nil
		delete(deprecatedTaskTypes, typ)
	}()

	if err := preflightTasks(carryForwardSpec(typ, nil)); err == nil ||
		!strings.Contains(err.Error(), "DEPRECATED") {
		t.Fatalf("a live-retired type must be refused as new authoring, got: %v", err)
	}
	var err error
	stderr := captureStderr(t, func() {
		err = preflightTasks(carryForwardSpec(typ, map[string]bool{typ: true, "end": true}))
	})
	if err != nil {
		t.Fatalf("a live-retired type the target holds must be carried forward, got: %v", err)
	}
	if !strings.Contains(stderr, "CARRIED FORWARD") {
		t.Errorf("expected a carry-forward warning, got: %q", stderr)
	}
	if strings.Contains(stderr, "unknown task type") {
		t.Errorf("a carried-forward type must not be re-diagnosed as unknown: %q", stderr)
	}
}

// workflowNodeTypes is the mechanism the exemption rests on: the node types of
// the workflow apply already looked up by alias. No request of its own -- the
// /v2/workflows listing DTO carries the full nodes[].
func TestWorkflowNodeTypes_ReadsTheTargetGraph(t *testing.T) {
	if got := workflowNodeTypes(nil); got != nil {
		t.Errorf("no target must carry nothing forward, got %v", got)
	}
	listing := map[string]any{
		"alias": "legacy-flow",
		"nodes": []any{
			map[string]any{"nodeId": "n0", "type": "start"},
			map[string]any{"nodeId": "n1", "type": "create-borrower", "taskAlias": "cb-a1b2c3"},
			map[string]any{"nodeId": "n2", "type": "http"},
			map[string]any{"nodeId": "n3"}, // no type: skipped, not recorded as ""
		},
	}
	got := workflowNodeTypes(listing)
	for _, want := range []string{"start", "create-borrower", "http"} {
		if !got[want] {
			t.Errorf("expected %q in the target's node types, got %v", want, got)
		}
	}
	if got[""] {
		t.Error("a node with no type must not register the empty type")
	}
	if len(got) != 3 {
		t.Errorf("expected exactly 3 types, got %v", got)
	}
	// An empty graph is not the same as no target, but both carry nothing.
	if got := workflowNodeTypes(map[string]any{"nodes": []any{}}); len(got) != 0 {
		t.Errorf("an empty graph carries nothing forward, got %v", got)
	}
}

// The structural validator is the only deprecation gate on the tasks-v2 paths,
// so it has to honour the same rule -- and stay silent when it does, because
// its callers warn where they can name the node ref or the task alias.
func TestValidateTaskV2BodyStructural_HonoursTheCarryForward(t *testing.T) {
	body := json.RawMessage(`{"type":"create-borrower","label":"X","alias":"x"}`)
	if err := validateTaskV2BodyStructural(body, nil); err == nil {
		t.Error("with no carry-forward the retired type must be refused")
	}
	var err error
	stderr := captureStderr(t, func() {
		err = validateTaskV2BodyStructural(body, map[string]bool{"create-borrower": true})
	})
	if err != nil {
		t.Errorf("a carried-forward type must pass the structural validator, got: %v", err)
	}
	if stderr != "" {
		t.Errorf("the validator must stay silent; its callers own the warning, got: %q", stderr)
	}
	// A carry-forward for a DIFFERENT type exempts nothing.
	if err := validateTaskV2BodyStructural(body, map[string]bool{"soap": true}); err == nil {
		t.Error("a carry-forward for another type must not exempt create-borrower")
	}
}

// `tasks-v2 create-version <alias>` bumps an EXISTING task. Keeping a type that
// is already retired is a carry-forward the backend accepts by design;
// switching a task TO a retired type is new authoring and stays refused. The
// stored type is therefore read, not inferred from the alias resolving.
func TestDeprecatedCarryForwardForTaskVersion(t *testing.T) {
	cases := []struct {
		name       string
		bodyType   string
		storedType string
		wantSet    bool
		wantCalls  int32
	}{
		{name: "current type never asks", bodyType: "http", storedType: "http", wantCalls: 0},
		{name: "retired type kept is carried forward", bodyType: "soap", storedType: "soap", wantSet: true, wantCalls: 1},
		{name: "task switched TO a retired type is new authoring", bodyType: "soap", storedType: "http", wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				if r.Method != "GET" || r.URL.Path != "/v2/tasks/legacy-task" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"alias":"legacy-task","type":"` + tc.storedType + `","label":"Legacy","version":3}`))
			}))
			defer srv.Close()

			body := json.RawMessage(`{"type":"` + tc.bodyType + `","label":"Legacy"}`)
			got := deprecatedCarryForwardForTaskVersion(newTestClient(t, srv.URL), "legacy-task", body)
			if tc.wantSet != got[tc.bodyType] {
				t.Errorf("carry-forward for %q: want %v, got %v", tc.bodyType, tc.wantSet, got)
			}
			if n := atomic.LoadInt32(&calls); n != tc.wantCalls {
				t.Errorf("expected %d lookups, got %d", tc.wantCalls, n)
			}
		})
	}
}

// A lookup that fails leaves the refusal standing. Being stricter than the
// server is the bug this whole change fixes, but being LOOSER than the server
// is worse: it would wave a spec through to a 4xx the CLI could have explained.
func TestDeprecatedCarryForwardForTaskVersion_FailedLookupRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NotFound","message":"no such task"}`))
	}))
	defer srv.Close()

	body := json.RawMessage(`{"type":"soap","label":"Legacy"}`)
	c := newTestClient(t, srv.URL)
	if got := deprecatedCarryForwardForTaskVersion(c, "missing-task", body); len(got) != 0 {
		t.Errorf("a 404 must carry nothing forward, got %v", got)
	}
	// And the gate it feeds still refuses.
	if err := validateTaskV2Body(body, deprecatedCarryForwardForTaskVersion(c, "missing-task", body)); err == nil {
		t.Error("with no verified carry-forward the retired type must be refused")
	}
}
