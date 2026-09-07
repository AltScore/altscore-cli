package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestPublishWorkflowV2_SendsLockToken pins the fix for HQ #1228: publish is
// gated on the edit lock, so the CLI must present the token it holds. Before
// this, publish POSTed a nil body and every update-path apply 423'd on the lock
// it had just acquired itself.
func TestPublishWorkflowV2_SendsLockToken(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/workflows/draft-1/publish" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workflowId":"draft-1","status":"ACTIVE"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := publishWorkflowV2(c, "draft-1", "tok-42"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var sent map[string]string
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("publish body was not JSON (%q): %v", gotBody, err)
	}
	if sent["lockToken"] != "tok-42" {
		t.Errorf("publish body lockToken = %q, want tok-42 (body: %s)", sent["lockToken"], gotBody)
	}
}

// TestPublishWorkflowV2_NoTokenSendsNoBody keeps the create path byte-identical
// to before: a brand-new workflow holds no lock, so there is nothing to send and
// the optional body must stay absent.
func TestPublishWorkflowV2_NoTokenSendsNoBody(t *testing.T) {
	var contentLength int64 = -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentLength = r.ContentLength
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workflowId":"wf-1"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := publishWorkflowV2(c, "wf-1", ""); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if contentLength > 0 {
		t.Errorf("publish sent a %d-byte body with no lock token; want none", contentLength)
	}
}

// TestResolveWorkflowAlias_UUIDResolves covers the other half of HQ #1228: the
// lock endpoints are alias-keyed, so a UUID has to be translated first. A raw id
// addressed a key nothing had written, and force-release still answered success.
func TestResolveWorkflowAlias_UUIDResolves(t *testing.T) {
	const id = "aab5d352-18d6-40b0-8770-783be91d021f"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/workflows/"+id {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + id + `","alias":"kyb"}`))
	}))
	defer srv.Close()

	alias, err := resolveWorkflowAlias(newTestClient(t, srv.URL), id)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if alias != "kyb" {
		t.Errorf("alias = %q, want kyb", alias)
	}
}

// TestResolveWorkflowAlias_AliasMakesNoRequest keeps the common case free: an
// alias argument must not cost a round-trip.
func TestResolveWorkflowAlias_AliasMakesNoRequest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	alias, err := resolveWorkflowAlias(newTestClient(t, srv.URL), "kyb")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if alias != "kyb" {
		t.Errorf("alias = %q, want kyb", alias)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("made %d HTTP calls for a plain alias, want 0", got)
	}
}

// TestForceReleaseReportedNoLock pins how `lock force-release` reads the
// server's answer: only an explicit released:false means nothing was held. An
// older backend that omits the field must not be reported as a no-op.
func TestForceReleaseReportedNoLock(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"released false", `{"success":true,"released":false}`, true},
		{"released true", `{"success":true,"released":true}`, false},
		// An older backend cannot tell us, so we must not claim it released nothing.
		{"field absent", `{"success":true}`, false},
		{"not json", `oops`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forceReleaseReportedNoLock([]byte(tc.body)); got != tc.want {
				t.Errorf("forceReleaseReportedNoLock(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
