package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestExplainExecuteTimeout_OnlyWrapsTheHeaderTimeout(t *testing.T) {
	base := fmt.Errorf("request failed: Post \"https://bc/v2/workflows/wf/1/execute\": net/http: timeout awaiting response headers")
	err := explainExecuteTimeout(base, "validaci-n-documentos-persona-moral")
	if !errors.Is(err, base) {
		t.Fatal("must wrap the original error")
	}
	for _, want := range []string{"still running", "altscore executions list", "validaci-n-documentos-persona-moral", "--wait"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hint missing %q: %v", want, err)
		}
	}

	other := errors.New("HTTP 400 BadRequestError: Required variable 'rfc' is missing")
	if got := explainExecuteTimeout(other, "wf"); got != other {
		t.Errorf("non-timeout errors must pass through untouched, got %v", got)
	}
	if got := explainExecuteTimeout(nil, "wf"); got != nil {
		t.Errorf("nil stays nil, got %v", got)
	}
}

func TestUseSyncExecuteTimeout_WidensOnlyTheSyncPath(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	shared := c.HTTPClient
	useSyncExecuteTimeout(c, "async")
	if c.HTTPClient != shared {
		t.Error("async submit must keep the shared client")
	}
	useSyncExecuteTimeout(c, "")
	if c.HTTPClient == shared {
		t.Error("sync submit must get its own client with the long response-header timeout")
	}
}

func TestNoteCreateDraftResult_NamesTheDraftAndWhetherItIsNew(t *testing.T) {
	var buf bytes.Buffer
	noteCreateDraftResult(&buf, json.RawMessage(`{"workflow":{"id":"eb1fdd2b","status":"DRAFT","version":8},"created":false,"message":"Existing draft returned"}`))
	got := buf.String()
	for _, want := range []string{"existing draft", "id=eb1fdd2b", "v8", "status=DRAFT", ".workflow"} {
		if !strings.Contains(got, want) {
			t.Errorf("note missing %q: %s", want, got)
		}
	}

	buf.Reset()
	noteCreateDraftResult(&buf, json.RawMessage(`{"workflow":{"id":"new-1","status":"DRAFT","version":9},"created":true,"message":"New draft created"}`))
	if got := buf.String(); !strings.HasPrefix(got, "# draft: id=new-1") {
		t.Errorf("new draft note: %s", got)
	}

	buf.Reset()
	noteCreateDraftResult(&buf, json.RawMessage(`{"unexpected":true}`))
	noteCreateDraftResult(&buf, json.RawMessage(`not json`))
	if buf.Len() != 0 {
		t.Errorf("unknown shapes must stay silent, got %q", buf.String())
	}
}

func TestDefaultLockClientID_IsPerProcessAndNamesTheProfile(t *testing.T) {
	id := defaultLockClientID("campofino")
	if !strings.HasPrefix(id, "cli-campofino-") {
		t.Errorf("prefix: %s", id)
	}
	if !strings.HasSuffix(id, fmt.Sprintf("-%d", os.Getpid())) {
		t.Errorf("must end with the pid: %s", id)
	}
	if !strings.HasPrefix(defaultLockClientID(""), "cli-default-") {
		t.Errorf("empty profile falls back to default: %s", defaultLockClientID(""))
	}
}
