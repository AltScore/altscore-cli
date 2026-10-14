package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

const (
	activeWorkflowID = "11111111-1111-4111-8111-111111111111"
	draftWorkflowID  = "22222222-2222-4222-8222-222222222222"
)

// One alias with an ACTIVE version and a DRAFT open over it, behaving as borrower-central
// does: GET /{alias}/latest is the highest version of any status (the DRAFT), a live run of
// latest executes the ACTIVE and a test run the highest, the async response names only the
// executionId, and an execution answers only under the workflow version that ran it.
type draftOverActiveBC struct {
	mu     sync.Mutex
	ran    string
	polled []string
	v1Down bool
}

func newDraftOverActiveBC(t *testing.T) (*httptest.Server, *draftOverActiveBC) {
	t.Helper()
	f := &draftOverActiveBC{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/execute"):
			if r.Header.Get("X-Execution-Mode") != "async" {
				t.Errorf("--wait must submit async, got X-Execution-Mode=%q", r.Header.Get("X-Execution-Mode"))
			}
			f.ran = activeWorkflowID
			if strings.Contains(r.Header.Get("X-Tags"), "test") {
				f.ran = draftWorkflowID
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"executionId":"exec-1"}`))
		case r.Method == http.MethodGet && p == "/v1/executions/exec-1":
			if f.v1Down {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "exec-1", "workflowId": f.ran, "workflowVersion": "v3"})
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/executions/exec-1"):
			wid := strings.TrimSuffix(strings.TrimPrefix(p, "/v2/workflows/"), "/executions/exec-1")
			f.polled = append(f.polled, wid)
			if wid != f.ran {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"NotFoundError","message":"no results found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"executionId": "exec-1", "workflowId": wid, "status": "completed"})
		case r.Method == http.MethodGet && (p == "/v2/workflows/my-wf/latest" || p == "/v2/workflows/my-wf"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": draftWorkflowID, "alias": "my-wf", "status": "DRAFT", "version": 4})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func TestWaitPollsTheWorkflowVersionTheServerRan(t *testing.T) {
	cases := []struct {
		name     string
		cmd      func() *cobra.Command
		args     []string
		wantPoll string
	}{
		{"execute-by-alias latest, live run: the ACTIVE under an open DRAFT", makeWfv2ExecuteByAliasCmd,
			[]string{"my-wf", "latest", "--body", "{}", "--wait", "--poll-interval", "10ms"}, activeWorkflowID},
		{"execute-by-alias with no version is latest", makeWfv2ExecuteByAliasCmd,
			[]string{"my-wf", "--body", "{}", "--wait", "--poll-interval", "10ms"}, activeWorkflowID},
		{"execute-by-alias latest --test: the DRAFT", makeWfv2ExecuteByAliasCmd,
			[]string{"my-wf", "latest", "--body", "{}", "--wait", "--test", "--poll-interval", "10ms"}, draftWorkflowID},
		{"execute <alias> is alias@latest server-side", makeWfv2ExecuteCmd,
			[]string{"my-wf", "--body", "{}", "--wait", "--skip-status-check", "--poll-interval", "10ms"}, activeWorkflowID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, f := newDraftOverActiveBC(t)
			routeLoadClientTo(t, srv.URL)
			stdout, stderr, err := runListCmd(t, tc.cmd(), tc.args...)
			if err != nil {
				t.Fatalf("--wait failed: %v\nstderr:\n%s", err, stderr)
			}
			if len(f.polled) == 0 {
				t.Fatal("nothing was polled")
			}
			for _, wid := range f.polled {
				if wid != tc.wantPoll {
					t.Errorf("polled the execution under %s, want only %s (polled %v)", wid, tc.wantPoll, f.polled)
				}
			}
			if !strings.Contains(stdout, `"completed"`) {
				t.Errorf("expected the final execution record, got:\n%s", stdout)
			}
		})
	}
}

func TestWaitFallsBackToTheExactIDOnlyWhenTheRecordIsUnreadable(t *testing.T) {
	srv, f := newDraftOverActiveBC(t)
	f.v1Down = true
	f.ran = activeWorkflowID
	c := newTestClient(t, srv.URL)
	wait := wfv2WaitFlags{wait: true, timeout: 5 * time.Second, pollInterval: 10 * time.Millisecond}
	submit := json.RawMessage(`{"executionId":"exec-1"}`)

	// 'execute <uuid>' ran exactly that id, so it still polls.
	var stderr bytes.Buffer
	if err := captureStdoutOf(t, func() error {
		return runExecuteWithOptionalWait(c, nil, &stderr, submit, activeWorkflowID, wait, false)
	}).err; err != nil {
		t.Fatalf("an exact id must still be polled: %v", err)
	}

	// An alias resolved server-side has no safe guess: say so instead of polling a wrong id.
	f.polled = nil
	err := captureStdoutOf(t, func() error {
		return runExecuteWithOptionalWait(c, nil, &stderr, submit, "", wait, false)
	}).err
	if err == nil || !strings.Contains(err.Error(), "altscore executions get exec-1") {
		t.Fatalf("expected an error naming the follow-up command, got %v", err)
	}
	if len(f.polled) != 0 {
		t.Errorf("nothing may be polled without a workflow id, polled %v", f.polled)
	}
}

func TestExecutionWorkflowIDPrefersTheResponse(t *testing.T) {
	srv, f := newDraftOverActiveBC(t)
	f.ran = draftWorkflowID
	c := newTestClient(t, srv.URL)
	// A sync response names the version it ran; no lookup is needed.
	if got := executionWorkflowID(c, json.RawMessage(`{"executionId":"exec-1","workflowId":"`+activeWorkflowID+`"}`), "exec-1"); got != activeWorkflowID {
		t.Errorf("sync response workflowId must win, got %q", got)
	}
	if got := executionWorkflowID(c, json.RawMessage(`{"executionId":"exec-1"}`), "exec-1"); got != draftWorkflowID {
		t.Errorf("async: the v1 record names the version, got %q", got)
	}
}
