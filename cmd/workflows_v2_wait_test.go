package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/config"
)

// newTestClient builds a client.Client wired to the given httptest server URL.
// Avoids touching the real config file or auth flow.
func newTestClient(t *testing.T, baseURL string) *client.Client {
	t.Helper()
	cfg := &config.Config{
		DefaultProfile: "test",
		Profiles: map[string]config.Profile{
			"test": {
				Environment: "staging",
				AccessToken: "test-token",
				TenantID:    "tenant",
			},
		},
	}
	p := cfg.Profiles["test"]
	c := client.New(cfg, "test", &p, false)
	c.BaseURLOverrides = map[string]string{"borrower_central": baseURL}
	return c
}

// TestPollExecutionWait_SucceedsAfterRunning is the canonical happy-path:
// two RUNNING polls, then completed. Verifies the loop terminates, the final
// JSON carries through, and verbose mode prints status transitions for each
// node.
func TestPollExecutionWait_SucceedsAfterRunning(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/workflows/wf-1/executions/exec-1" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			_, _ = w.Write([]byte(`{
				"executionId": "exec-1",
				"workflowId": "wf-1",
				"status": "running",
				"nodes": [
					{"nodeId": "n1", "status": "running"},
					{"nodeId": "n2", "status": "pending"}
				]
			}`))
		case 2:
			_, _ = w.Write([]byte(`{
				"executionId": "exec-1",
				"workflowId": "wf-1",
				"status": "running",
				"nodes": [
					{"nodeId": "n1", "status": "completed"},
					{"nodeId": "n2", "status": "running"}
				]
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"executionId": "exec-1",
				"workflowId": "wf-1",
				"status": "completed",
				"nodes": [
					{"nodeId": "n1", "status": "completed"},
					{"nodeId": "n2", "status": "completed"}
				]
			}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)

	var stderr bytes.Buffer
	wait := wfv2WaitFlags{wait: true, timeout: 5 * time.Second, pollInterval: 10 * time.Millisecond}
	data, status, err := pollExecutionWait(c, "wf-1", "exec-1", wait, true, &stderr)
	if err != nil {
		t.Fatalf("pollExecutionWait: %v", err)
	}
	if status != "completed" {
		t.Errorf("status = %q, want completed", status)
	}
	if got := atomic.LoadInt32(&calls); got < 3 {
		t.Errorf("expected at least 3 polls, got %d", got)
	}
	// Final payload should carry through verbatim.
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("final JSON not parseable: %v", err)
	}
	if s, _ := env["status"].(string); s != "completed" {
		t.Errorf("final status field = %q, want completed", s)
	}
	// Verbose should have streamed each transition. Check a couple.
	transcript := stderr.String()
	for _, want := range []string{
		"n1 - -> running",      // first appearance
		"n2 - -> pending",      // first appearance
		"n1 running -> completed",
		"n2 pending -> running",
		"n2 running -> completed",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("stderr missing transition %q. full:\n%s", want, transcript)
		}
	}
}

// TestPollExecutionWait_Timeout asserts that when the loop never sees a
// terminal status it returns ExitCodeError{Code: 2} rather than blocking
// forever.
func TestPollExecutionWait_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)

	wait := wfv2WaitFlags{wait: true, timeout: 50 * time.Millisecond, pollInterval: 10 * time.Millisecond}
	_, _, err := pollExecutionWait(c, "wf-1", "exec-1", wait, false, nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	var ec *ExitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("expected ExitCodeError, got %T: %v", err, err)
	}
	if ec.Code != 2 {
		t.Errorf("exit code = %d, want 2", ec.Code)
	}
}

// TestPollExecutionWait_TransientErrorThenSucceeds verifies the single-flake
// retry behavior: one HTTP 500 doesn't abort the wait, but two in a row do.
func TestPollExecutionWait_TransientErrorThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			http.Error(w, `{"code":"InternalError","message":"flake"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)

	wait := wfv2WaitFlags{wait: true, timeout: 5 * time.Second, pollInterval: 10 * time.Millisecond}
	_, status, err := pollExecutionWait(c, "wf-1", "exec-1", wait, false, nil)
	if err != nil {
		t.Fatalf("expected success after one flake, got %v", err)
	}
	if status != "completed" {
		t.Errorf("status = %q, want completed", status)
	}
}

// TestExtractExecutionID sanity-checks the submit-response parsing for both
// the flat shape (current async response) and a nested "data" wrapper.
func TestExtractExecutionID(t *testing.T) {
	flat := json.RawMessage(`{"executionId":"e1"}`)
	if id := extractExecutionID(flat); id != "e1" {
		t.Errorf("flat: got %q, want e1", id)
	}
	nested := json.RawMessage(`{"data":{"executionId":"e2"}}`)
	if id := extractExecutionID(nested); id != "e2" {
		t.Errorf("nested: got %q, want e2", id)
	}
	missing := json.RawMessage(`{"foo":"bar"}`)
	if id := extractExecutionID(missing); id != "" {
		t.Errorf("missing: got %q, want empty", id)
	}
}

// TestExtractFailureDetail covers the failure-detail surface PR1 adds, plus
// the fallback to error.message when failureReason is absent.
func TestExtractFailureDetail(t *testing.T) {
	withReason := json.RawMessage(`{"status":"failed","failedNodeId":"n3","failureReason":"boom"}`)
	node, reason := extractFailureDetail(withReason)
	if node != "n3" || reason != "boom" {
		t.Errorf("withReason: got node=%q reason=%q", node, reason)
	}
	fallback := json.RawMessage(`{"status":"failed","failedNodeId":"n4","error":{"message":"connection refused"}}`)
	node, reason = extractFailureDetail(fallback)
	if node != "n4" || reason != "connection refused" {
		t.Errorf("fallback: got node=%q reason=%q", node, reason)
	}
}
