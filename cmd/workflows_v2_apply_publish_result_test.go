package cmd

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

func jsonUnmarshalString(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

// apply's stdout used to echo the create/autosave response, which still says
// DRAFT, after a successful publish. lintAndPublish now hands back the
// workflow as it is after publishing, so the caller prints ACTIVE.
func TestLintAndPublish_ReturnsPublishedWorkflow(t *testing.T) {
	var published int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/workflows/d1/publish":
			atomic.StoreInt32(&published, 1)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == "GET" && r.URL.Path == "/v2/workflows/d1":
			status := "DRAFT"
			if atomic.LoadInt32(&published) == 1 {
				status = "ACTIVE"
			}
			_, _ = w.Write([]byte(`{"id":"d1","status":"` + status + `","version":2,"nodes":[],"edges":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd := &cobra.Command{}
	var errb bytes.Buffer
	cmd.SetOut(&errb)
	cmd.SetErr(&errb)

	data, err := lintAndPublish(c, cmd, "d1", true, "tok")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("returned body is not the workflow JSON: %v", err)
	}
	if got["status"] != "ACTIVE" {
		t.Errorf("returned workflow must reflect the publish, got status=%v", got["status"])
	}
	if !strings.Contains(errb.String(), "# published workflow d1") {
		t.Errorf("publish note missing; got:\n%s", errb.String())
	}
}

// When the post-publish read fails the publish response itself comes back, so
// the caller still has something to print and the publish is not reported as
// a failure.
func TestLintAndPublish_FallsBackToPublishResponse(t *testing.T) {
	var published int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/workflows/d1/publish":
			atomic.StoreInt32(&published, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"published":true}`))
		case r.Method == "GET" && r.URL.Path == "/v2/workflows/d1":
			if atomic.LoadInt32(&published) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"d1","status":"DRAFT","nodes":[],"edges":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	data, err := lintAndPublish(c, cmd, "d1", true, "")
	if err != nil {
		t.Fatalf("a failed re-read must not fail the publish: %v", err)
	}
	if !strings.Contains(string(data), `"published":true`) {
		t.Errorf("expected the publish response as fallback, got %s", data)
	}
}
