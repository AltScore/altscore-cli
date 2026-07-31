package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// discardCmd is a bare command whose stderr goes to a buffer the test can read.
func discardCmd() (*cobra.Command, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetErr(buf)
	cmd.SetOut(buf)
	return cmd, buf
}

func lockConflictEnvelope(code string) string {
	return `{"code":"` + code + `","message":"workflow is being edited",` +
		`"details":{"lockedBy":{"userId":"u1","email":"someone@altscore.ai","clientId":"tab-abc"}}}`
}

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

// TestAcquireApplyLock_RefusesLiveSession is the behaviour change that matters
// most: BC reports SELF_LOCK_CONFLICT for the same user in another TAB, so the
// old "self conflict means a stale lock, force-release it" rule stole locks from
// live Hub editors. A renewed lock must be refused, and nothing may be released.
func TestAcquireApplyLock_RefusesLiveSession(t *testing.T) {
	var forceReleases int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/workflows/kyb/lock":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(lockConflictEnvelope("SELF_LOCK_CONFLICT")))
		case r.Method == "GET" && r.URL.Path == "/v2/workflows/kyb/lock":
			_, _ = w.Write([]byte(`{"isLocked":true,"canEdit":true,"lock":{
				"lockedBy":{"email":"someone@altscore.ai","clientId":"3f1c-browser-tab"},
				"lockedAt":"2026-07-31T05:00:00+00:00",
				"expiresAt":"2026-07-31T05:05:00+00:00","renewCount":4}}`))
		case r.Method == "DELETE" && r.URL.Path == "/v2/workflows/kyb/lock/force":
			atomic.AddInt32(&forceReleases, 1)
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cmd, _ := discardCmd()
	_, err := acquireApplyLock(newTestClient(t, srv.URL), cmd, "kyb", "apply-123", false)
	if err == nil {
		t.Fatal("expected apply to refuse a live session's lock, got nil error")
	}
	if !strings.Contains(err.Error(), "someone@altscore.ai") {
		t.Errorf("refusal should name the holder, got: %v", err)
	}
	if got := atomic.LoadInt32(&forceReleases); got != 0 {
		t.Errorf("force-released a live lock %d time(s); want 0", got)
	}
}

// TestAcquireApplyLock_ReclaimsAbandonedApplyLock keeps the legitimate recovery:
// apply stamps its own clientId prefix and never heartbeats, so renewCount 0
// under that prefix is provably a crashed predecessor, not a live editor.
func TestAcquireApplyLock_ReclaimsAbandonedApplyLock(t *testing.T) {
	var acquires, forceReleases int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/workflows/kyb/lock":
			if atomic.AddInt32(&acquires, 1) == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(lockConflictEnvelope("SELF_LOCK_CONFLICT")))
				return
			}
			_, _ = w.Write([]byte(`{"lockToken":"fresh-token","lockId":"l2"}`))
		case r.Method == "GET" && r.URL.Path == "/v2/workflows/kyb/lock":
			_, _ = w.Write([]byte(`{"isLocked":true,"canEdit":true,"lock":{
				"lockedBy":{"email":"axel@altscore.ai","clientId":"apply-1753900000"},
				"lockedAt":"2026-07-31T05:00:00+00:00",
				"expiresAt":"2026-07-31T05:05:00+00:00","renewCount":0}}`))
		case r.Method == "DELETE" && r.URL.Path == "/v2/workflows/kyb/lock/force":
			atomic.AddInt32(&forceReleases, 1)
			_, _ = w.Write([]byte(`{"success":true,"released":true}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cmd, _ := discardCmd()
	token, err := acquireApplyLock(newTestClient(t, srv.URL), cmd, "kyb", "apply-999", false)
	if err != nil {
		t.Fatalf("expected the abandoned apply lock to be reclaimed: %v", err)
	}
	if token != "fresh-token" {
		t.Errorf("token = %q, want fresh-token", token)
	}
	if got := atomic.LoadInt32(&forceReleases); got != 1 {
		t.Errorf("force-releases = %d, want 1", got)
	}
}

// TestAcquireApplyLock_ForceLockTakesLiveLock covers the explicit override.
func TestAcquireApplyLock_ForceLockTakesLiveLock(t *testing.T) {
	var acquires, forceReleases int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/workflows/kyb/lock":
			if atomic.AddInt32(&acquires, 1) == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(lockConflictEnvelope("LOCK_CONFLICT")))
				return
			}
			_, _ = w.Write([]byte(`{"lockToken":"stolen-token"}`))
		case r.Method == "GET" && r.URL.Path == "/v2/workflows/kyb/lock":
			_, _ = w.Write([]byte(`{"isLocked":true,"canEdit":false,"lock":{
				"lockedBy":{"email":"other@altscore.ai","clientId":"browser"},
				"expiresAt":"2026-07-31T05:05:00+00:00","renewCount":7}}`))
		case r.Method == "DELETE" && r.URL.Path == "/v2/workflows/kyb/lock/force":
			atomic.AddInt32(&forceReleases, 1)
			_, _ = w.Write([]byte(`{"success":true,"released":true}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cmd, _ := discardCmd()
	token, err := acquireApplyLock(newTestClient(t, srv.URL), cmd, "kyb", "apply-1", true)
	if err != nil {
		t.Fatalf("--force-lock should take the lock: %v", err)
	}
	if token != "stolen-token" {
		t.Errorf("token = %q, want stolen-token", token)
	}
	if got := atomic.LoadInt32(&forceReleases); got != 1 {
		t.Errorf("force-releases = %d, want 1", got)
	}
}

func TestIsAbandonedApplyLock(t *testing.T) {
	cases := []struct {
		name string
		info *wfv2LockInfo
		want bool
	}{
		{"nil", nil, false},
		{"not locked", &wfv2LockInfo{IsLocked: false, ClientID: "apply-1"}, false},
		{"apply lock never renewed", &wfv2LockInfo{IsLocked: true, ClientID: "apply-1753", RenewCount: 0}, true},
		{"apply lock renewed", &wfv2LockInfo{IsLocked: true, ClientID: "apply-1753", RenewCount: 1}, false},
		{"browser tab", &wfv2LockInfo{IsLocked: true, ClientID: "8f3c-2a1b", RenewCount: 0}, false},
		{"agent lock", &wfv2LockInfo{IsLocked: true, ClientID: "agent-42", RenewCount: 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAbandonedApplyLock(tc.info); got != tc.want {
				t.Errorf("isAbandonedApplyLock() = %v, want %v", got, tc.want)
			}
		})
	}
}

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

func TestIsLockConflictErr(t *testing.T) {
	if isLockConflictErr(nil) {
		t.Error("nil error is not a lock conflict")
	}
	if !isLockConflictErr(fmt.Errorf("HTTP 409 SELF_LOCK_CONFLICT: editing in another tab")) {
		t.Error("SELF_LOCK_CONFLICT should be recognised")
	}
	if !isLockConflictErr(fmt.Errorf("HTTP 409 LOCK_CONFLICT: held by someone else")) {
		t.Error("LOCK_CONFLICT should be recognised")
	}
	if isLockConflictErr(fmt.Errorf("HTTP 500 InternalError: boom")) {
		t.Error("a 500 is not a lock conflict")
	}
}
