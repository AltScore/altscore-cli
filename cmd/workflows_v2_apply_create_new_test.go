package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// createNew rides in the request body only when set, so an older backend
// never sees an unknown field.
func TestApplyViaServer_CreateNewIsSentOnlyWhenSet(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"mode":"create","dryRun":true,"workflowAlias":"a","workflow":{"nodes":[],"edges":[]},"tasks":[],"validation":{"valid":true,"findings":[],"skippedNodeIds":[]},"publish":{"requested":false,"published":false,"errors":[]},"lock":{"acquired":false,"released":false}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	cmd, _, _ := serverApplyTestCmd()
	if _, err := applyViaServer(c, cmd, map[string]any{"alias": "a"}, serverApplyOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, present := got["createNew"]; present {
		t.Errorf("createNew must be absent by default, body: %v", got)
	}
	if _, err := applyViaServer(c, cmd, map[string]any{"alias": "a"}, serverApplyOptions{DryRun: true, CreateNew: true}); err != nil {
		t.Fatal(err)
	}
	if got["createNew"] != true {
		t.Errorf("createNew must be true when set, body: %v", got)
	}
}

// A near-match refusal lists the candidates and points at both ways out.
func TestDescribeServerApplyError_AliasNearMatch(t *testing.T) {
	var errb bytes.Buffer
	body := `{"code":"ConflictError","message":"no workflow has alias 'validacion-documentos-persona-moral' but 1 existing workflow is one typo away","details":{"errorSubCode":"APPLY_ALIAS_NEAR_MATCH","alias":"validacion-documentos-persona-moral","candidates":[{"alias":"validaci-n-documentos-persona-moral","label":"Validación Documentos Persona Moral","status":"DRAFT","version":1,"workflowId":"c988be3b","reason":"alias"}]}}`
	err := describeServerApplyError(&errb, http.StatusConflict, json.RawMessage(body))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"--create-new", "set spec.alias"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	out := errb.String()
	for _, want := range []string{"validaci-n-documentos-persona-moral", "DRAFT v1", "Validación Documentos Persona Moral", "matched by alias"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr missing %q:\n%s", want, out)
		}
	}
}
