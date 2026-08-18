package cmd

// `workflows-v2 import` findings rendering and exit policy.
//
// Honest limit, stated here rather than implied by a test name: the CLI cannot
// prove "no write happened" -- the server decides that. What these assert is
// the property this repo actually controls, that the CLI issues exactly one
// import request and never retries a refused one.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func importTestCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	cmd.SetOut(&bytes.Buffer{})
	return cmd, &errBuf
}

// importServer serves POST /v2/workflows/import with the given status+body and
// counts the calls, erroring on any other path.
func importServer(t *testing.T, status int, body string, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/workflows/import" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func runImport(t *testing.T, srv *httptest.Server, bundle string) (*bytes.Buffer, error) {
	t.Helper()
	c := newTestClient(t, srv.URL)
	cmd, errBuf := importTestCmd()

	payload, err := json.Marshal(map[string]any{"workflowData": json.RawMessage(bundle)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data, _, doErr := c.Do("POST", "borrower_central", "/v2/workflows/import", json.RawMessage(payload))
	if doErr != nil {
		return errBuf, doErr
	}
	reportImportFindings(cmd, data)
	return errBuf, nil
}

const _bundle = `{"workflow":{"label":"X","nodes":[]},"tasks":[]}`

func TestImport_CleanResponseIsQuietAndExitsZero(t *testing.T) {
	var calls int32
	srv := importServer(t, http.StatusCreated,
		`{"workflowId":"w1","workflowAlias":"a","tasksCreated":1,"validation":{"valid":true,"findings":[]}}`, &calls)
	defer srv.Close()

	errBuf, err := runImport(t, srv, _bundle)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got := errBuf.String(); strings.Contains(got, "[ERROR]") || strings.Contains(got, "[WARN]") {
		t.Errorf("expected quiet stderr, got:\n%s", got)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("expected exactly 1 import call, got %d", n)
	}
}

func TestImport_NoValidationKeyIsNotTreatedAsInvalid(t *testing.T) {
	// An older backend omits `validation` entirely. Decoding into
	// `validationResponse` would give Valid=false and its caller's rule
	// (!Valid || errs>0) would declare this completed import invalid. The
	// dedicated envelope with a *pointer* Validation is what prevents that.
	var calls int32
	srv := importServer(t, http.StatusCreated,
		`{"workflowId":"w1","workflowAlias":"a","tasksCreated":1}`, &calls)
	defer srv.Close()

	errBuf, err := runImport(t, srv, _bundle)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got := errBuf.String(); strings.Contains(got, "FAILED") || strings.Contains(got, "invalid") {
		t.Errorf("an absent validation key must not read as invalid, got:\n%s", got)
	}
}

func TestImport_WarningsPrintedAndExitZero(t *testing.T) {
	var calls int32
	srv := importServer(t, http.StatusCreated, `{"workflowId":"w1","validation":{"valid":true,"findings":[
		{"code":"IDENTITY_KEY_NOT_REGISTERED","severity":"warning","nodeId":"n1","edgeId":"","params":{},"message":"identifies by 'rfc'"}
	]}}`, &calls)
	defer srv.Close()

	errBuf, err := runImport(t, srv, _bundle)
	// The write already happened; a non-zero exit would teach CI to retry a
	// completed import.
	if err != nil {
		t.Fatalf("warnings must not fail the command, got %v", err)
	}
	got := errBuf.String()
	for _, want := range []string{"[WARN]", "IDENTITY_KEY_NOT_REGISTERED", "identifies by 'rfc'", `(node "n1")`} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q, got:\n%s", want, got)
		}
	}
}

func TestImport_CarriedOverErrorsAreReportedButNotFatal(t *testing.T) {
	var calls int32
	srv := importServer(t, http.StatusCreated, `{"workflowId":"w1","validation":{"valid":false,"findings":[
		{"code":"MULTIPLE_END_NODES","severity":"error","nodeId":"","edgeId":"","params":{},"message":"two end nodes"}
	]}}`, &calls)
	defer srv.Close()

	errBuf, err := runImport(t, srv, _bundle)
	if err != nil {
		t.Fatalf("a completed import must not fail the command, got %v", err)
	}
	got := errBuf.String()
	if !strings.Contains(got, "[ERROR]") || !strings.Contains(got, "MULTIPLE_END_NODES") {
		t.Errorf("stderr missing the error finding, got:\n%s", got)
	}
	if !strings.Contains(got, "WAS imported") {
		t.Errorf("stderr must say the workflow landed, got:\n%s", got)
	}
}

func TestImport_NoticesArePrinted(t *testing.T) {
	var calls int32
	srv := importServer(t, http.StatusCreated,
		`{"workflowId":"w1","notices":["Created 1 scorecard(s) despite the skip flag ('sc-1')."]}`, &calls)
	defer srv.Close()

	errBuf, err := runImport(t, srv, _bundle)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !strings.Contains(errBuf.String(), "despite the skip flag") {
		t.Errorf("a write the caller did not ask for must be reported, got:\n%s", errBuf.String())
	}
}

func TestImport_RefusalIsNonZeroAndNamesTheEntities(t *testing.T) {
	// The client folds >=400 bodies into the error and discards the JSON, so the
	// server's MESSAGE has to carry the entity names. This pins that contract
	// from the CLI side.
	var calls int32
	srv := importServer(t, http.StatusUnprocessableEntity, `{"code":"UnprocessableEntity",
		"message":"This workflow references entities that do not exist on this tenant and are not carried by the bundle: scorecard 'sc-1'. Nothing was created.",
		"details":{"errorSubCode":"WORKFLOW_REFERENCES_UNAVAILABLE"}}`, &calls)
	defer srv.Close()

	_, err := runImport(t, srv, _bundle)
	if err == nil {
		t.Fatal("a refused import must be a non-zero exit")
	}
	if !strings.Contains(err.Error(), "sc-1") {
		t.Errorf("the error must name the missing entity, got: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("a refused import must not be retried, got %d calls", n)
	}
}

func TestImport_UnknownFindingCodeRenderedGenerically(t *testing.T) {
	var calls int32
	srv := importServer(t, http.StatusCreated, `{"workflowId":"w1","validation":{"valid":true,"findings":[
		{"code":"SOME_FUTURE_CODE","severity":"warning","nodeId":"","edgeId":"","params":{},"message":"future text"}
	]}}`, &calls)
	defer srv.Close()

	errBuf, _ := runImport(t, srv, _bundle)
	got := errBuf.String()
	if !strings.Contains(got, "SOME_FUTURE_CODE") || !strings.Contains(got, "future text") {
		t.Errorf("an unseen code must still render, got:\n%s", got)
	}
}

func TestImport_MalformedResponseIsNotFatal(t *testing.T) {
	var calls int32
	srv := importServer(t, http.StatusCreated, `not json at all`, &calls)
	defer srv.Close()

	cmd, errBuf := importTestCmd()
	reportImportFindings(cmd, json.RawMessage(`not json at all`))
	if errBuf.Len() != 0 {
		t.Errorf("a body we cannot read must produce no claims, got:\n%s", errBuf.String())
	}
}

func TestPartitionFindings_UnknownSeverityBecomesWarning(t *testing.T) {
	errs, warns := partitionFindings([]validationFinding{
		{Code: "A", Severity: "ERROR"},
		{Code: "B", Severity: "critical"},
	})
	// EqualFold matches uppercase; anything unrecognised must not escalate.
	if len(errs) != 1 || errs[0].Code != "A" {
		t.Errorf("expected A as the only error, got %v", errs)
	}
	if len(warns) != 1 || warns[0].Code != "B" {
		t.Errorf("expected B as a warning, got %v", warns)
	}
}
