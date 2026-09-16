package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AltScore/altscore-cli/internal/client"
)

// wfv2SyncExecuteHeaderTimeout bounds how long a SYNC execute may wait for the
// server to start answering. Borrower Central runs the whole graph inline on
// that path and caps it at ten minutes, so the transport must not give up
// earlier: with the shared 60s bound a run of 85 seconds reported a timeout
// while it completed server-side, and the author then hunted for the
// execution by hand.
const wfv2SyncExecuteHeaderTimeout = 10 * time.Minute

// useSyncExecuteTimeout widens the response-header timeout for a synchronous
// execute submit. Async submits return at once and keep the shared client.
func useSyncExecuteTimeout(c *client.Client, executionMode string) {
	if strings.EqualFold(executionMode, "async") {
		return
	}
	c.HTTPClient = client.LongResponseClient(wfv2SyncExecuteHeaderTimeout)
}

const responseHeaderTimeoutText = "timeout awaiting response headers"

// explainExecuteTimeout turns the bare transport error of a sync execute into
// one the author can act on: the request WAS accepted, the execution is most
// likely still running, and here is how to find it.
func explainExecuteTimeout(err error, workflowRef string) error {
	if err == nil || !strings.Contains(err.Error(), responseHeaderTimeoutText) {
		return err
	}
	return fmt.Errorf("%w\n"+
		"# the server accepted the request but did not answer in time; the execution is probably still running.\n"+
		"# Find it: altscore executions list --per-page 5   (look for workflowAlias/workflowId %s)\n"+
		"# Next time: add --wait (submits async and polls) or --execution-mode async.", err, workflowRef)
}

// noteCreateDraftResult writes one stderr line saying which draft the call
// returned. The response nests the draft under `workflow`, so `.id` at the top
// level is null and a caller piping to jq is left guessing whether a draft was
// created or an existing one came back.
func noteCreateDraftResult(w io.Writer, data json.RawMessage) {
	if w == nil {
		return
	}
	var env struct {
		Created  *bool          `json:"created"`
		Message  string         `json:"message"`
		Workflow map[string]any `json:"workflow"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.Workflow == nil {
		return
	}
	id, _ := env.Workflow["id"].(string)
	status, _ := env.Workflow["status"].(string)
	version := ""
	if v, ok := env.Workflow["version"]; ok && v != nil {
		version = fmt.Sprintf(" v%v", v)
	}
	what := "draft"
	if env.Created != nil && !*env.Created {
		what = "existing draft (already open, returned as-is)"
	}
	fmt.Fprintf(w, "# %s: id=%s%s status=%s -- the draft is under .workflow in the JSON below\n", what, id, version, status)
}

// defaultLockClientID names this CLI process as a lock holder when the caller
// did not pick a client id. The lock service uses the id to tell one editor's
// re-acquire from another editor's steal, so it must be stable for the life
// of the process and distinct across processes and machines.
func defaultLockClientID(profile string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "host"
	}
	if profile == "" {
		profile = "default"
	}
	return fmt.Sprintf("cli-%s-%s-%d", profile, host, os.Getpid())
}

// errNoLockIdentity is kept for callers that still want to refuse a lock with
// neither token nor client id; the CLI itself now falls back to a default id.
var errNoLockIdentity = errors.New("either --lock-token or --client-id is required")
