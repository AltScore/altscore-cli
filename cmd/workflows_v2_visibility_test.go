package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildVisibilityBody_OnlyShowInDeal(t *testing.T) {
	body, err := buildVisibilityBody(wfv2VisibilityFlags{ShowInDeal: "true"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := string(body); got != `{"showInDeal":true}` {
		t.Errorf("body = %s, want {\"showInDeal\":true}", got)
	}
}

func TestBuildVisibilityBody_AllThree(t *testing.T) {
	body, err := buildVisibilityBody(wfv2VisibilityFlags{Hidden: "true", ShowInCustomer: "false", ShowInDeal: "true"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := `{"hidden":true,"showInCustomer":false,"showInDeal":true}`
	if got := string(body); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestBuildVisibilityBody_FalseIsSentNotOmitted(t *testing.T) {
	body, err := buildVisibilityBody(wfv2VisibilityFlags{Hidden: "false"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := string(body); got != `{"hidden":false}` {
		t.Errorf("body = %s, want {\"hidden\":false}", got)
	}
}

func TestBuildVisibilityBody_NoFlagsErrors(t *testing.T) {
	_, err := buildVisibilityBody(wfv2VisibilityFlags{})
	if err == nil {
		t.Fatal("expected an error with no flags, got nil")
	}
	if !strings.Contains(err.Error(), "at least one of") {
		t.Errorf("error = %q, want mention of 'at least one of'", err)
	}
}

func TestBuildVisibilityBody_RejectsNonBoolean(t *testing.T) {
	_, err := buildVisibilityBody(wfv2VisibilityFlags{ShowInCustomer: "yes"})
	if err == nil {
		t.Fatal("expected an error for --show-in-customer=yes, got nil")
	}
	if !strings.Contains(err.Error(), "--show-in-customer") || !strings.Contains(err.Error(), `"yes"`) {
		t.Errorf("error = %q, want it to name the flag and the bad value", err)
	}
}

func TestSetWorkflowVisibility_PatchesAliasEndpoint(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"alias":"my-wf","modifiedCount":3,"showInDeal":true}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	body, err := buildVisibilityBody(wfv2VisibilityFlags{ShowInDeal: "true"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	data, err := setWorkflowVisibility(c, "my-wf", body)
	if err != nil {
		t.Fatalf("set visibility: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", gotMethod)
	}
	if gotPath != "/v2/workflows/my-wf/visibility" {
		t.Errorf("path = %s, want /v2/workflows/my-wf/visibility", gotPath)
	}
	if gotBody != `{"showInDeal":true}` {
		t.Errorf("body = %s, want {\"showInDeal\":true}", gotBody)
	}
	if !strings.Contains(string(data), `"modifiedCount":3`) {
		t.Errorf("response not passed through: %s", data)
	}
}
