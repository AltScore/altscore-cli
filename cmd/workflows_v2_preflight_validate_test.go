package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// preflightTestCmd returns a bare cobra command whose stderr is captured in the
// returned buffer, so serverPreflightValidate's findings output can be asserted.
func preflightTestCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	var errb bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errb)
	return cmd, &errb
}

// minimalPreflightSpec returns a fresh (start -> http -> end) spec, already
// split into the Tasks / ExtraNodes buckets the build pipeline expects (RunE
// does this split before calling compose). Fresh maps each call: compose
// mutates the spec in place, so tests must not share backing maps.
func minimalPreflightSpec() *composeSpec {
	return &composeSpec{
		Label:      "Preflight test",
		Category:   "EVALUATION",
		ExtraNodes: []map[string]any{{"ref": "start", "type": "start", "label": "Start"}},
		Tasks: []map[string]any{
			{"ref": "check", "type": "http", "label": "Check", "url": "https://example.com"},
			{"ref": "end", "type": "end", "label": "End"},
		},
		Edges: []map[string]any{
			{"from": "start", "to": "check"},
			{"from": "check", "to": "end"},
		},
	}
}

// --- serverPreflightValidate: findings handling & fail-open -----------------

// An error-severity finding on the real path aborts (returns an error) and
// prints the code + message.
func TestServerPreflightValidate_ErrorAborts(t *testing.T) {
	var validateCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v2/workflows/validate" {
			atomic.AddInt32(&validateCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"valid":false,"findings":[
				{"code":"MULTIPLE_END_NODES","severity":"error","nodeId":null,"edgeId":null,"params":{},"message":"a workflow must have exactly one end node"}
			]}`))
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()
	capture := newComposeCapture()

	err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, capture, true)
	if err == nil {
		t.Fatal("expected abort error on error-severity finding, got nil")
	}
	if atomic.LoadInt32(&validateCalls) != 1 {
		t.Errorf("expected exactly 1 validate call, got %d", validateCalls)
	}
	out := errb.String()
	for _, want := range []string{"[ERROR]", "MULTIPLE_END_NODES", "exactly one end node", "aborting"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, out)
		}
	}
}

// Warnings never abort; they print prominently and apply continues.
func TestServerPreflightValidate_WarningProceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true,"findings":[
			{"code":"CONDITIONAL_BRANCH_WITHOUT_EDGE","severity":"warning","nodeId":"route","edgeId":null,"params":{},"message":"branch 'reject' has no outgoing edge"}
		]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()
	capture := newComposeCapture()
	capture.refByNodeID["route"] = "route"

	err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, capture, true)
	if err != nil {
		t.Fatalf("warnings must not abort; got: %v", err)
	}
	out := errb.String()
	for _, want := range []string{"[WARN]", "CONDITIONAL_BRANCH_WITHOUT_EDGE", "no outgoing edge", "WILL fail at runtime"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, out)
		}
	}
}

// A 404 (endpoint absent on an older backend) fails open: no error, a dim
// note, and apply proceeds.
func TestServerPreflightValidate_404FailsOpen(t *testing.T) {
	var validateCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&validateCalls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()

	err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, newComposeCapture(), true)
	if err != nil {
		t.Fatalf("404 must fail open (nil error); got: %v", err)
	}
	if atomic.LoadInt32(&validateCalls) != 1 {
		t.Errorf("expected 1 validate attempt, got %d", validateCalls)
	}
	if out := errb.String(); !strings.Contains(out, "skipped") || !strings.Contains(out, "unavailable") {
		t.Errorf("expected fail-open note mentioning 'skipped'/'unavailable'; got:\n%s", out)
	}
}

// A non-JSON 200 body is treated as an older/foreign backend: fail open.
func TestServerPreflightValidate_NonJSONFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()

	err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, newComposeCapture(), true)
	if err != nil {
		t.Fatalf("non-JSON must fail open (nil error); got: %v", err)
	}
	if out := errb.String(); !strings.Contains(out, "skipped") {
		t.Errorf("expected fail-open note; got:\n%s", out)
	}
}

// The server's nodeId is mapped back to the spec-local ref for readable output.
func TestServerPreflightValidate_NodeIDMappedToRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":false,"findings":[
			{"code":"CONDITIONAL_EDGE_MISSING_BRANCH_HANDLE","severity":"error","nodeId":"conditional-1a2b3c","edgeId":"conditional-1a2b3c->fin","params":{},"message":"edge leaving a conditional has no sourceHandle"}
		]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()
	capture := newComposeCapture()
	// Server assigned "conditional-1a2b3c"; the author's spec called it "route".
	capture.refByNodeID["conditional-1a2b3c"] = "route"

	_ = serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, capture, true)
	out := errb.String()
	if !strings.Contains(out, `node "route"`) {
		t.Errorf("expected finding rendered with mapped ref 'route'; got:\n%s", out)
	}
	if strings.Contains(out, `node "conditional-1a2b3c"`) {
		t.Errorf("raw server nodeId should have been mapped to the ref; got:\n%s", out)
	}
	// The edgeId (already spec-readable) is surfaced verbatim.
	if !strings.Contains(out, `edge "conditional-1a2b3c->fin"`) {
		t.Errorf("expected edge id in output; got:\n%s", out)
	}
}

// An unknown finding code is still rendered generically (message + severity)
// and, at error severity, still aborts.
func TestServerPreflightValidate_UnknownCodeRenderedGenerically(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":false,"findings":[
			{"code":"SOME_FUTURE_RULE","severity":"error","nodeId":null,"edgeId":null,"params":{},"message":"a rule this CLI has never heard of"}
		]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()

	err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, newComposeCapture(), true)
	if err == nil {
		t.Fatal("unknown error-severity code must still abort")
	}
	out := errb.String()
	for _, want := range []string{"SOME_FUTURE_RULE", "never heard of"} {
		if !strings.Contains(out, want) {
			t.Errorf("unknown code must render generically with %q; got:\n%s", want, out)
		}
	}
}

// In dry-run mode (abortOnError=false) an error finding is printed but does not
// abort, and a clean pass reports "no issues found".
func TestServerPreflightValidate_DryRunNeverAborts(t *testing.T) {
	// Error finding, but abortOnError=false -> no error returned.
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":false,"findings":[
			{"code":"MULTIPLE_END_NODES","severity":"error","nodeId":null,"edgeId":null,"params":{},"message":"two end nodes"}
		]}`))
	}))
	defer srvErr.Close()
	c := newTestClient(t, srvErr.URL)
	cmd, errb := preflightTestCmd()
	if err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, newComposeCapture(), false); err != nil {
		t.Fatalf("dry-run must never abort; got: %v", err)
	}
	if !strings.Contains(errb.String(), "MULTIPLE_END_NODES") {
		t.Errorf("dry-run should still print findings; got:\n%s", errb.String())
	}

	// Clean pass in dry-run reports "no issues found".
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true,"findings":[]}`))
	}))
	defer srvOK.Close()
	c2 := newTestClient(t, srvOK.URL)
	cmd2, errb2 := preflightTestCmd()
	if err := serverPreflightValidate(c2, cmd2, map[string]any{"label": "x"}, newComposeCapture(), false); err != nil {
		t.Fatalf("clean pass must not error; got: %v", err)
	}
	if !strings.Contains(errb2.String(), "no issues found") {
		t.Errorf("clean dry-run pass should report 'no issues found'; got:\n%s", errb2.String())
	}
}

// --- applyAssembleValidateAndPost: end-to-end gating ------------------------

// applyServerHandler builds an httptest server that returns validateBody for
// POST /v2/workflows/validate (or validateStatus when non-200) and counts task
// creates. It fails the test on any unexpected route.
func applyServerHandler(t *testing.T, validateStatus int, validateBody string, validateCalls, taskCalls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/workflows/validate":
			atomic.AddInt32(validateCalls, 1)
			if validateStatus != http.StatusOK {
				w.WriteHeader(validateStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(validateBody))
		case r.Method == "POST" && r.URL.Path == "/v2/tasks":
			n := atomic.AddInt32(taskCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"alias":"srv-task-` + strconv.Itoa(int(n)) + `","version":1}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}

// Error findings abort the real apply BEFORE any /v2/tasks POST.
func TestApplyAssembleValidateAndPost_ErrorAbortsBeforeTaskCreate(t *testing.T) {
	var validateCalls, taskCalls int32
	body := `{"valid":false,"findings":[
		{"code":"CONDITIONAL_EDGE_MISSING_BRANCH_HANDLE","severity":"error","nodeId":"check","edgeId":null,"params":{},"message":"bad branch handle"}
	]}`
	srv := applyServerHandler(t, http.StatusOK, body, &validateCalls, &taskCalls)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()

	wf, err := applyAssembleValidateAndPost(c, cmd, minimalPreflightSpec(), false, false, false, false, false)
	if err == nil {
		t.Fatal("expected apply to abort on error finding")
	}
	if wf != nil {
		t.Errorf("expected nil workflow on abort, got: %v", wf)
	}
	if got := atomic.LoadInt32(&taskCalls); got != 0 {
		t.Errorf("NO /v2/tasks POST must happen when validation errors; got %d", got)
	}
	if got := atomic.LoadInt32(&validateCalls); got != 1 {
		t.Errorf("expected exactly 1 validate call, got %d", got)
	}
	if !strings.Contains(errb.String(), "CONDITIONAL_EDGE_MISSING_BRANCH_HANDLE") {
		t.Errorf("stderr should carry the failing code; got:\n%s", errb.String())
	}
}

// Warnings only: apply proceeds and posts every node's task.
func TestApplyAssembleValidateAndPost_WarningProceedsAndPosts(t *testing.T) {
	var validateCalls, taskCalls int32
	body := `{"valid":true,"findings":[
		{"code":"CONDITIONAL_BRANCH_WITHOUT_EDGE","severity":"warning","nodeId":"check","edgeId":null,"params":{},"message":"a branch has no edge"}
	]}`
	srv := applyServerHandler(t, http.StatusOK, body, &validateCalls, &taskCalls)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()

	wf, err := applyAssembleValidateAndPost(c, cmd, minimalPreflightSpec(), false, false, false, false, false)
	if err != nil {
		t.Fatalf("warnings must not block apply; got: %v", err)
	}
	if wf == nil {
		t.Fatal("expected an assembled workflow body")
	}
	// start + http + end each get a backing task POST.
	if got := atomic.LoadInt32(&taskCalls); got != 3 {
		t.Errorf("expected 3 /v2/tasks POSTs (start, check, end); got %d", got)
	}
	if got := atomic.LoadInt32(&validateCalls); got != 1 {
		t.Errorf("expected exactly 1 validate call, got %d", got)
	}
	if !strings.Contains(errb.String(), "[WARN]") {
		t.Errorf("warning should be printed; got:\n%s", errb.String())
	}
}

// A 404 from the validate endpoint fails open: apply proceeds and posts tasks.
func TestApplyAssembleValidateAndPost_404FailsOpenAndPosts(t *testing.T) {
	var validateCalls, taskCalls int32
	srv := applyServerHandler(t, http.StatusNotFound, "", &validateCalls, &taskCalls)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()

	wf, err := applyAssembleValidateAndPost(c, cmd, minimalPreflightSpec(), false, false, false, false, false)
	if err != nil {
		t.Fatalf("404 must fail open and let apply proceed; got: %v", err)
	}
	if wf == nil {
		t.Fatal("expected an assembled workflow body after fail-open")
	}
	if got := atomic.LoadInt32(&taskCalls); got != 3 {
		t.Errorf("expected 3 /v2/tasks POSTs after fail-open; got %d", got)
	}
	if got := atomic.LoadInt32(&validateCalls); got != 1 {
		t.Errorf("expected 1 validate attempt, got %d", got)
	}
	if !strings.Contains(errb.String(), "skipped") {
		t.Errorf("expected a fail-open note on stderr; got:\n%s", errb.String())
	}
}

// The dry-assembly validation pass POSTs nothing to /v2/tasks even when the
// validator reports warnings (mirrors the --dry-run apply path).
func TestDryAssemblyValidation_PostsNoTasks(t *testing.T) {
	var validateCalls, taskCalls int32
	body := `{"valid":true,"findings":[
		{"code":"CONDITIONAL_BRANCH_WITHOUT_EDGE","severity":"warning","nodeId":"check","edgeId":null,"params":{},"message":"a branch has no edge"}
	]}`
	srv := applyServerHandler(t, http.StatusOK, body, &validateCalls, &taskCalls)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()

	// Mirror the RunE dry-run branch: dry compose + capture, then validate.
	capture := newComposeCapture()
	wf, err := composeWorkflowBody(c, minimalPreflightSpec(), true, false, true, false, true, true, capture)
	if err != nil {
		t.Fatalf("dry compose failed: %v", err)
	}
	serverPreflightValidate(c, cmd, wf, capture, false)

	if got := atomic.LoadInt32(&taskCalls); got != 0 {
		t.Errorf("dry-run must not POST any /v2/tasks; got %d", got)
	}
	if len(capture.tasks) != 3 {
		t.Errorf("expected 3 captured task bodies (start, check, end); got %d", len(capture.tasks))
	}
	if !strings.Contains(errb.String(), "[WARN]") {
		t.Errorf("dry-run should print the warning finding; got:\n%s", errb.String())
	}
}

// --- honoring the server verdict (finding 1) --------------------------------

// A `valid:false` verdict with NO error-severity finding must still abort -- the
// verdict is authoritative, not just the error-severity findings.
func TestServerPreflightValidate_ValidFalseWithoutErrorAborts(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no findings", `{"valid":false,"findings":[]}`},
		{"warning only", `{"valid":false,"findings":[
			{"code":"SOME_ADVISORY","severity":"warning","nodeId":null,"edgeId":null,"params":{},"message":"heads up"}
		]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			cmd, errb := preflightTestCmd()

			err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, newComposeCapture(), true)
			if err == nil {
				t.Fatal("valid:false must abort even with no error-severity finding")
			}
			out := errb.String()
			for _, want := range []string{"FAILED", "valid=false", "aborting"} {
				if !strings.Contains(out, want) {
					t.Errorf("stderr missing %q; got:\n%s", want, out)
				}
			}
		})
	}
}

// A warning-only `valid:false` still prints the warning it was given before the
// verdict line, so the operator sees what the server flagged.
func TestServerPreflightValidate_ValidFalseStillPrintsWarnings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":false,"findings":[
			{"code":"SOME_ADVISORY","severity":"warning","nodeId":null,"edgeId":null,"params":{},"message":"heads up"}
		]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()
	if err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, newComposeCapture(), true); err == nil {
		t.Fatal("expected abort")
	}
	if out := errb.String(); !strings.Contains(out, "[WARN]") || !strings.Contains(out, "SOME_ADVISORY") {
		t.Errorf("expected the warning to be printed alongside the verdict; got:\n%s", out)
	}
}

// Severity matching is case-insensitive: an uppercase "ERROR" still aborts.
func TestServerPreflightValidate_UppercaseSeverityAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":false,"findings":[
			{"code":"MULTIPLE_END_NODES","severity":"ERROR","nodeId":null,"edgeId":null,"params":{},"message":"two end nodes"}
		]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()
	err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, newComposeCapture(), true)
	if err == nil {
		t.Fatal("uppercase ERROR severity must be treated as an error and abort")
	}
	out := errb.String()
	for _, want := range []string{"[ERROR]", "MULTIPLE_END_NODES", "1 error(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, out)
		}
	}
}

// --- banner accuracy (finding 4) --------------------------------------------

// The "branch with no edge WILL fail at runtime" banner prints ONLY when a
// CONDITIONAL_BRANCH_WITHOUT_EDGE finding is present; a different warning gets
// the generic warning block but no branch-specific claim.
func TestServerPreflightValidate_BranchBannerOnlyForBranchCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true,"findings":[
			{"code":"SOME_OTHER_WARNING","severity":"warning","nodeId":null,"edgeId":null,"params":{},"message":"an unrelated advisory"}
		]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()
	if err := serverPreflightValidate(c, cmd, map[string]any{"label": "x"}, newComposeCapture(), true); err != nil {
		t.Fatalf("warning must not abort; got: %v", err)
	}
	out := errb.String()
	if !strings.Contains(out, "[WARN]") || !strings.Contains(out, "SOME_OTHER_WARNING") {
		t.Errorf("expected the warning to be printed; got:\n%s", out)
	}
	if strings.Contains(out, "WILL fail at runtime") {
		t.Errorf("branch-specific banner must NOT print without CONDITIONAL_BRANCH_WITHOUT_EDGE; got:\n%s", out)
	}
}

// --- fail-open split: non-404 4xx is loud (finding 2) -----------------------

// A 422 means the endpoint IS there and rejected the request -- it must produce
// a LOUD "contract mismatch" note (not the dim "older backend" one), then let
// apply proceed and POST every node's task.
func TestApplyAssembleValidateAndPost_422LoudNoteProceedsAndPosts(t *testing.T) {
	var validateCalls, taskCalls int32
	srv := applyServerHandler(t, http.StatusUnprocessableEntity, "", &validateCalls, &taskCalls)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, errb := preflightTestCmd()

	wf, err := applyAssembleValidateAndPost(c, cmd, minimalPreflightSpec(), false, false, false, false, false)
	if err != nil {
		t.Fatalf("a 4xx from the oracle must not block apply; got: %v", err)
	}
	if wf == nil {
		t.Fatal("expected an assembled workflow body after a loud fail-open")
	}
	if got := atomic.LoadInt32(&taskCalls); got != 3 {
		t.Errorf("expected 3 /v2/tasks POSTs after fail-open; got %d", got)
	}
	if got := atomic.LoadInt32(&validateCalls); got != 1 {
		t.Errorf("expected 1 validate attempt, got %d", got)
	}
	out := errb.String()
	if !strings.Contains(out, "contract mismatch") || !strings.Contains(out, "422") {
		t.Errorf("expected a loud contract-mismatch note mentioning HTTP 422; got:\n%s", out)
	}
	if strings.Contains(out, "older backend") {
		t.Errorf("a 422 must NOT be misattributed to an older backend; got:\n%s", out)
	}
}
