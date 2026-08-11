package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// --- the operator mirror ----------------------------------------------------

// canonicalConditionOperators is borrower-central's canonical vocabulary,
// app/model/evaluation_rules/condition_operators.py and
// app/service/condition_evaluator.py (WORKFLOW_CONDITION_OPERATORS), which are
// kept in step there by a guard test. Every one of these is what a `get` or an
// `export` returns, because ConditionItem canonicalises on read -- so every one
// of them lands back in `apply` on the next round trip.
var canonicalConditionOperators = []string{
	"equals", "not_equals",
	"greater_than", "greater_than_or_equals", "less_than", "less_than_or_equals",
	"between", "contains", "not_contains", "starts_with", "ends_with",
	"in", "not_in",
	"is_true", "is_false", "is_null", "is_not_null", "is_empty", "is_not_empty",
	"is_altdata_empty", "is_altdata_not_calculated", "is_altdata_error",
	"is_altdata_null", "is_not_altdata_null",
	"array_contains_any", "array_contains_all",
}

// preHundredOneConditionOperators is the mirror exactly as it shipped before
// #101. Asserted so widening the list can never silently drop a spelling that
// used to apply cleanly: back-compat is the whole constraint here.
var preHundredOneConditionOperators = []string{
	"eq", "neq",
	"gt", "gte", "lt", "lte",
	"contains", "startsWith", "endsWith",
	"in", "notIn", "between",
	"isNull", "isNotNull",
	"isAltdataEmpty", "isAltdataNotCalculated",
	"isAltdataError", "isAltdataNull", "isNotAltdataNull",
	"arrayContainsAny", "arrayContainsAll",
}

func TestConditionOperators_MirrorAcceptsEveryCanonicalName(t *testing.T) {
	defer resetLiveConditionOperators()
	resetLiveConditionOperators()
	fetchLiveConditionOperators = func() map[string]bool {
		t.Fatalf("a canonical operator must be on the compiled-in fast path, not fetched")
		return nil
	}
	for _, op := range canonicalConditionOperators {
		if !conditionOperators[op] {
			t.Errorf("canonical operator %q missing from the mirror: every export -> apply round trip warns on it", op)
			continue
		}
		if err := checkConditionOperator(op, "branches[0].conditions.items[0]"); err != nil {
			t.Errorf("canonical operator %q rejected offline: %v", op, err)
		}
	}
}

func TestConditionOperators_MirrorKeepsEveryPre101Spelling(t *testing.T) {
	for _, op := range preHundredOneConditionOperators {
		if !conditionOperators[op] {
			t.Errorf("operator %q was accepted before #101 and no longer is -- that is a breaking change", op)
		}
	}
}

// The five operators that had no accepted spelling at all before #101. Offline
// they were a hard apply failure, and is_not_empty is the one used against the
// altdata failure sentinel, so this is not a hypothetical set.
func TestConditionOperators_PreviouslyUnspellableOperators(t *testing.T) {
	defer resetLiveConditionOperators()
	resetLiveConditionOperators()
	for _, op := range []string{"not_contains", "is_true", "is_false", "is_empty", "is_not_empty"} {
		if err := checkConditionOperator(op, "branches[0]"); err != nil {
			t.Errorf("operator %q must be accepted offline: %v", op, err)
		}
	}
}

// A symbol operator is repaired to canonical by BC's ConditionItem rather than
// rejected (its own test asserts that), so the CLI must not be stricter than
// the write boundary it validates against.
func TestConditionOperators_SymbolAliasesAreAccepted(t *testing.T) {
	defer resetLiveConditionOperators()
	resetLiveConditionOperators()
	for _, op := range []string{"=", "==", "!=", "<>", ">", ">=", "<", "<="} {
		if err := checkConditionOperator(op, "branches[0]"); err != nil {
			t.Errorf("symbol operator %q must be accepted (BC repairs it): %v", op, err)
		}
	}
}

// An operator no backend has ever served must still be fatal offline: widening
// the mirror must not turn it into a rubber stamp.
func TestConditionOperators_UnknownStaysFatal(t *testing.T) {
	defer resetLiveConditionOperators()
	resetLiveConditionOperators()
	if err := checkConditionOperator("approximately_equals", "branches[0]"); err == nil {
		t.Fatal("an operator unknown to every source must stay fatal")
	}
}

// --- lint: merging the oracle's findings ------------------------------------

func TestMergeServerFindings_DropsTheLocalCopyOfTheSameProblem(t *testing.T) {
	report := lintReport{Issues: []lintIssue{
		{Severity: "warning", Source: "local", serverCode: "MISSING_START_NODE", Message: "no node of type 'start'"},
		{Severity: "error", Source: "local", NodeID: "n1", serverCode: "NODE_MISSING_TASK_REFERENCE", Message: "type=\"http\" has no taskAlias/taskId"},
		{Severity: "error", Source: "local", NodeID: "n9", Message: "nodeId=\"n9\" type=\"http\" is fully disconnected (no edges in or out) -- unreachable"},
	}}
	mergeServerFindings(&report, []validationFinding{
		{Code: "MISSING_START_NODE", Severity: "error", Message: "the graph has no start node"},
		{Code: "NODE_MISSING_TASK_REFERENCE", Severity: "error", NodeID: "n1", Message: "node n1 has no task"},
		{Code: "TASK_REFERENCE_NOT_IN_GRAPH", Severity: "warning", NodeID: "n2", Message: "\"financials_final\" references \"financials-year-check\", which is not in the graph"},
	})

	var local, server int
	for _, issue := range report.Issues {
		switch issue.Source {
		case "local":
			local++
		case "server":
			server++
		default:
			t.Errorf("issue with no source: %+v", issue)
		}
	}
	if server != 3 {
		t.Errorf("expected the 3 server findings kept, got %d", server)
	}
	// Only the orphan check survives locally: the oracle covered the other two.
	if local != 1 {
		t.Errorf("expected 1 local issue to survive dedup, got %d", local)
	}
	if !strings.Contains(report.Issues[0].Message, "fully disconnected") {
		t.Errorf("the surviving local issue should be the orphan one, got %q", report.Issues[0].Message)
	}
	// The dangling task reference -- the finding this whole change exists for --
	// must arrive with its code so a reader can grep for it.
	found := false
	for _, issue := range report.Issues {
		if issue.Code == "TASK_REFERENCE_NOT_IN_GRAPH" && issue.Severity == "warning" {
			found = true
		}
	}
	if !found {
		t.Error("TASK_REFERENCE_NOT_IN_GRAPH finding missing from the merged report")
	}
}

// A finding for one node must not silence the same check on a different node.
func TestMergeServerFindings_DedupIsPerNode(t *testing.T) {
	report := lintReport{Issues: []lintIssue{
		{Severity: "error", Source: "local", NodeID: "n1", serverCode: "NODE_MISSING_TASK_REFERENCE", Message: "n1 local"},
		{Severity: "error", Source: "local", NodeID: "n2", serverCode: "NODE_MISSING_TASK_REFERENCE", Message: "n2 local"},
	}}
	mergeServerFindings(&report, []validationFinding{
		{Code: "NODE_MISSING_TASK_REFERENCE", Severity: "error", NodeID: "n1", Message: "n1 server"},
	})
	var kept []string
	for _, issue := range report.Issues {
		kept = append(kept, issue.Source+":"+issue.Message)
	}
	if len(report.Issues) != 2 {
		t.Fatalf("expected n2's local issue plus n1's server finding, got %v", kept)
	}
	for _, issue := range report.Issues {
		if issue.Source == "local" && issue.NodeID != "n2" {
			t.Errorf("wrong local issue survived: %+v", issue)
		}
	}
}

// A local issue with no server equivalent is never dropped, even when the
// oracle reported plenty of other things.
func TestMergeServerFindings_KeepsLocalOnlyChecks(t *testing.T) {
	report := lintReport{Issues: []lintIssue{
		{Severity: "error", Source: "local", NodeID: "dup", Message: "duplicate nodeId (2 occurrences)"},
		{Severity: "error", Source: "local", EdgeID: "e1", Message: "edges[0]: missing sourceNodeId"},
	}}
	mergeServerFindings(&report, []validationFinding{
		{Code: "INPUT_VARIABLE_NEVER_CONSUMED", Severity: "warning", Message: "ruc is never consumed"},
	})
	if len(report.Issues) != 3 {
		t.Fatalf("expected 2 local + 1 server, got %d", len(report.Issues))
	}
}

// Unknown severities are treated as warnings, never dropped: the server owns the
// vocabulary and lint must not swallow a finding it cannot classify.
func TestMergeServerFindings_UnknownSeverityBecomesWarning(t *testing.T) {
	report := lintReport{Issues: []lintIssue{}}
	mergeServerFindings(&report, []validationFinding{
		{Code: "SOMETHING_NEW", Severity: "advisory", Message: "hello"},
	})
	if len(report.Issues) != 1 || report.Issues[0].Severity != "warning" {
		t.Fatalf("expected one warning-severity issue, got %+v", report.Issues)
	}
}

// --- lint: talking to the oracle -------------------------------------------

func TestFetchWorkflowValidation_PostsTheDefinitionVerbatim(t *testing.T) {
	var calls int32
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v2/workflows/validate" {
			atomic.AddInt32(&calls, 1)
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			gotBody = string(buf)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"valid":true,"findings":[],"skippedNodeIds":["n7"]}`))
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	resp, reason := fetchWorkflowValidation(c, map[string]any{
		"id": "wf-1", "status": "ACTIVE", "nodes": []any{}, "edges": []any{},
	})
	if resp == nil {
		t.Fatalf("expected a response, got skip reason %q", reason)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected exactly one validate call, got %d", calls)
	}
	// `status` is load-bearing: the oracle resolves task bodies under the
	// drafts-float / published-pin rule for the submitted status.
	for _, want := range []string{`"workflow"`, `"status":"ACTIVE"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body missing %s; got: %s", want, gotBody)
		}
	}
	// No inline task bodies: the server resolves them from the repository.
	if strings.Contains(gotBody, `"tasks"`) {
		t.Errorf("lint must not send an inline tasks map; got: %s", gotBody)
	}
	if len(resp.SkippedNodeIDs) != 1 || resp.SkippedNodeIDs[0] != "n7" {
		t.Errorf("skippedNodeIds not parsed: %+v", resp.SkippedNodeIDs)
	}
}

// Fail open on every unusable answer, and say which kind it was: an old backend
// and a contract mismatch are different problems for whoever reads the note.
func TestFetchWorkflowValidation_FailsOpenWithADistinctReason(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantReason string
	}{
		{"missing endpoint", http.StatusNotFound, `{"detail":"not found"}`, "older backend"},
		{"server error", http.StatusInternalServerError, `boom`, "HTTP 500"},
		{"contract mismatch", http.StatusBadRequest, `{"message":"workflow.nodes must be a list"}`, "contract mismatch"},
		{"non-JSON body", http.StatusOK, `<html>proxy</html>`, "unrecognized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			resp, reason := fetchWorkflowValidation(newTestClient(t, srv.URL), map[string]any{"id": "wf-1"})
			if resp != nil {
				t.Fatalf("expected a fail-open skip, got a response")
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason %q does not mention %q", reason, tc.wantReason)
			}
		})
	}
}

// The local pass must stamp every issue it produces, including any check added
// later: an unsourced issue in the JSON is unattributable.
func TestLintWorkflowV2_StampsEveryIssueAsLocal(t *testing.T) {
	report := lintWorkflowV2(map[string]any{
		"id":    "wf-1",
		"nodes": []any{map[string]any{"nodeId": "a", "type": "http"}, map[string]any{"nodeId": "a", "type": "http"}},
		"edges": []any{map[string]any{"id": "e1", "sourceNodeId": "ghost", "targetNodeId": "a"}},
	})
	if len(report.Issues) == 0 {
		t.Fatal("expected issues from a graph with a duplicate id, no start/end and a ghost edge")
	}
	for _, issue := range report.Issues {
		if issue.Source != "local" {
			t.Errorf("issue not stamped local: %+v", issue)
		}
		if issue.Code != "" {
			t.Errorf("a local issue must not carry a server code: %+v", issue)
		}
	}
}
