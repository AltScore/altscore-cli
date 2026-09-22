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

// Borrower Central runs a sync execute inline and caps it at ten minutes; the
// transport must not give up before the server does.
const wfv2SyncExecuteHeaderTimeout = 10 * time.Minute

func useSyncExecuteTimeout(c *client.Client, executionMode string) {
	if strings.EqualFold(executionMode, "async") {
		return
	}
	c.HTTPClient = client.LongResponseClient(wfv2SyncExecuteHeaderTimeout)
}

const responseHeaderTimeoutText = "timeout awaiting response headers"

func explainExecuteTimeout(err error, workflowRef string) error {
	if err == nil || !strings.Contains(err.Error(), responseHeaderTimeoutText) {
		return err
	}
	return fmt.Errorf("%w\n"+
		"# the server accepted the request but did not answer in time; the execution is probably still running.\n"+
		"# Find it: altscore executions list --per-page 5   (look for workflowAlias/workflowId %s)\n"+
		"# Next time: add --wait (submits async and polls) or --execution-mode async.", err, workflowRef)
}

// The response nests the draft under `workflow`, so top-level `.id` is null.
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

// The lock service tells a re-acquire from a steal by this id, so it must be stable
// for the life of the process and distinct across processes and machines.
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

var errNoLockIdentity = errors.New("either --lock-token or --client-id is required")
