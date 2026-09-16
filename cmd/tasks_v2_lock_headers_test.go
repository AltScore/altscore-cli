package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskVersionLockHeaders_SendsIdentityAndTokenOnlyWhenPresent(t *testing.T) {
	h := taskVersionLockHeaders("", "cli-p-host-1")
	if h["X-Lock-Client-Id"] != "cli-p-host-1" {
		t.Errorf("client id header: %v", h)
	}
	if _, ok := h["X-Lock-Token"]; ok {
		t.Errorf("token header must be absent without a token: %v", h)
	}
	h = taskVersionLockHeaders("tok-1", "cli-p-host-1")
	if h["X-Lock-Token"] != "tok-1" {
		t.Errorf("token header: %v", h)
	}
}

func TestPostTaskVersion_SendsHeadersAndExplainsA423(t *testing.T) {
	var gotClient, gotToken string
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v2/tasks/fetch-ecu" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotClient = r.Header.Get("X-Lock-Client-Id")
		gotToken = r.Header.Get("X-Lock-Token")
		w.WriteHeader(status)
		if status == http.StatusCreated {
			_, _ = w.Write([]byte(`{"alias":"fetch-ecu","version":2}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"LOCKED","message":"Workflow is locked by another user","details":{}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	data, err := postTaskVersion(c, "fetch-ecu", json.RawMessage(`{"label":"x","type":"http"}`), "tok-1", "cli-test-host-7")
	if err != nil {
		t.Fatal(err)
	}
	if gotClient != "cli-test-host-7" || gotToken != "tok-1" {
		t.Errorf("headers on the wire: client=%q token=%q", gotClient, gotToken)
	}
	if !strings.Contains(string(data), `"version":2`) {
		t.Errorf("response passthrough: %s", data)
	}

	status = http.StatusLocked
	_, err = postTaskVersion(c, "fetch-ecu", json.RawMessage(`{"label":"x","type":"http"}`), "", "cli-test-host-7")
	if err == nil {
		t.Fatal("expected the 423 to surface")
	}
	for _, want := range []string{"423", "fetch-ecu", "lock acquire", "--lock-token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("423 hint missing %q: %v", want, err)
		}
	}
}
