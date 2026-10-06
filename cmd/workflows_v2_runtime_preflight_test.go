package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractionSchemaMustBeAnObjectSchema(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{"bare map", map[string]any{"holder": "string", "balance": "number"}, "INVALID_EXTRACTION_SCHEMA"},
		{"flat property", map[string]any{"type": "object", "properties": map[string]any{"holder": map[string]any{"type": "string"}, "balance": "number"}}, "[balance] are not objects"},
		{"no properties", map[string]any{"type": "object"}, "at least one entry"},
	}
	for _, tc := range cases {
		err := preflightTasks(docExtractionSpec(docExtractionTask("read", map[string]any{"documentUrl": "{{inputs.url}}", "extractionSchema": tc.schema}, nil)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", tc.name, tc.want, err)
		}
	}
	if err := preflightTasks(docExtractionSpec(docExtractionTask("read", map[string]any{"documentUrl": "{{inputs.url}}", "extractionSchema": scalarSchema()}, nil))); err != nil {
		t.Errorf("a valid object schema must pass, got %v", err)
	}
}

func TestDocumentSourceThatIsABareUnmappedPlaceholderIsRefused(t *testing.T) {
	bare := docExtractionTask("read", map[string]any{"documentUrl": "{{doc_url}}", "extractionSchema": scalarSchema()}, nil)
	err := preflightTasks(docExtractionSpec(bare))
	if err == nil || !strings.Contains(err.Error(), "{{inputs.doc_url}}") {
		t.Fatalf("a dotless placeholder nothing supplies must be refused with the path to write, got %v", err)
	}
	for name, task := range map[string]map[string]any{
		"mapped token": docExtractionTask("read", map[string]any{"documentUrl": "{{doc_url}}", "extractionSchema": scalarSchema()},
			map[string]any{"doc_url": "inputs.statement_url"}),
		"dotted path": docExtractionTask("read", map[string]any{"documentUrl": "{{inputs.doc_url}}", "extractionSchema": scalarSchema()}, nil),
		"literal url": docExtractionTask("read", map[string]any{"documentUrl": "https://files.example.com/a.pdf", "extractionSchema": scalarSchema()}, nil),
	} {
		if err := preflightTasks(docExtractionSpec(task)); err != nil {
			t.Errorf("%s must pass, got %v", name, err)
		}
	}
}

func singleTaskSpec(task map[string]any) *composeSpec {
	return &composeSpec{
		Label: "Runtime preflight", Category: "OTHER", ExtraNodes: startNode,
		Tasks: []map[string]any{task},
		Edges: []map[string]any{{"from": "start", "to": task["ref"]}},
	}
}

func TestContactSendsEmailOnlyAndNeedsARecipient(t *testing.T) {
	contact := func(cfg map[string]any) map[string]any {
		return map[string]any{"ref": "notify", "type": "contact", "label": "Notify", "contactConfig": cfg}
	}
	if err := preflightTasks(singleTaskSpec(contact(map[string]any{"channel": "whatsapp", "to": "{x}"}))); err == nil || !strings.Contains(err.Error(), "email only") {
		t.Errorf("a non-email channel must be refused, got %v", err)
	}
	if err := preflightTasks(singleTaskSpec(contact(map[string]any{"channel": "email", "to": " "}))); err == nil || !strings.Contains(err.Error(), "contactConfig.to") {
		t.Errorf("an empty recipient must be refused, got %v", err)
	}
	for _, cfg := range []map[string]any{{"channel": "email", "to": "{cust_email}"}, {"to": "ops@example.com"}} {
		if err := preflightTasks(singleTaskSpec(contact(cfg))); err != nil {
			t.Errorf("%v must pass, got %v", cfg, err)
		}
	}
}

func TestNoticesNeedConfigAndAV2Severity(t *testing.T) {
	notice := func(cfg any) map[string]any {
		task := map[string]any{"ref": "flag", "type": "notices", "label": "Flag"}
		if cfg != nil {
			task["noticesConfig"] = cfg
		}
		return task
	}
	if err := preflightTasks(singleTaskSpec(notice(nil))); err == nil || !strings.Contains(err.Error(), "requires noticesConfig") {
		t.Errorf("a notices node without config must be refused, got %v", err)
	}
	if err := preflightTasks(singleTaskSpec(notice(map[string]any{"message": "x", "severity": "debug"}))); err == nil || !strings.Contains(err.Error(), "v1 severity") {
		t.Errorf("a v1 severity must be refused, got %v", err)
	}
	for _, sev := range []string{"", "info", "warning", "error"} {
		if err := preflightTasks(singleTaskSpec(notice(map[string]any{"message": "x", "severity": sev}))); err != nil {
			t.Errorf("severity %q must pass, got %v", sev, err)
		}
	}
}

// A fake tenant: one ACTIVE alias, one DRAFT-only alias, one id, and a 500 for "flaky".
func newChildLookupServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/workflows" {
			alias, status := r.URL.Query().Get("alias"), r.URL.Query().Get("status")
			switch {
			case alias == "flaky":
				w.WriteHeader(http.StatusInternalServerError)
				return
			case alias == "kyc-person" && status == "ACTIVE", alias == "kyc-draft" && status == "DRAFT":
				_, _ = w.Write([]byte(`[{"id": "wf-1", "alias": "` + alias + `", "status": "` + status + `", "isLatest": true}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if r.URL.Path == "/v2/workflows/0123456789abcdef01234567" {
			_, _ = w.Write([]byte(`{"id": "0123456789abcdef01234567", "status": "ACTIVE"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestChildWorkflowExecutorMustExist(t *testing.T) {
	c := newTestClient(t, newChildLookupServer(t).URL)
	for _, ok := range []string{"kyc-person", "kyc-draft", "0123456789abcdef01234567", "flaky"} {
		if err := checkChildWorkflowExecutor(c, ok); err != nil {
			t.Errorf("executor %q must not be refused (active, draft warns, id, or unreachable fails open), got %v", ok, err)
		}
	}
	err := checkChildWorkflowExecutor(c, "kyc-persn")
	if err == nil || !strings.Contains(err.Error(), "neither a workflow id nor the alias") {
		t.Fatalf("an executor that resolves to nothing must be refused, got %v", err)
	}
	if err := checkChildWorkflowExecutor(nil, "kyc-persn"); err != nil {
		t.Errorf("offline (no client) must not refuse, got %v", err)
	}
}

func TestEvaluationRuleOperatorsAreChecked(t *testing.T) {
	body := json.RawMessage(`{"label": "KYC complete", "code": "kyc-complete", "conditions": {"operator": "AND", "items": [
		{"field": "tax_id", "operator": "isNotEmpty", "valueType": "value"},
		{"operator": "OR", "items": [{"field": "score", "operator": "gte", "value": "600"}]}]}}`)
	if err := validateEvaluationRuleBody(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"is_not_empty"`) || strings.Contains(string(body), "isNotEmpty") {
		t.Errorf("a camelCase spelling known only in snake_case must be rewritten, got %s", body)
	}
	if !strings.Contains(string(body), `"gte"`) {
		t.Errorf("a valid alias must be left as written, got %s", body)
	}

	bad := json.RawMessage(`{"conditions": {"operator": "AND", "items": [{"field": "tax_id", "operator": "notEmpty"}]}}`)
	err := validateEvaluationRuleBody(&bad)
	if err == nil || !strings.Contains(err.Error(), `did you mean "is_not_empty"?`) {
		t.Fatalf("an unknown operator must be refused with a suggestion, got %v", err)
	}
	if !strings.Contains(string(bad), "notEmpty") {
		t.Errorf("a refused body must not be rewritten, got %s", bad)
	}

	untouched := json.RawMessage(`{"label": "no conditions"}`)
	if err := validateEvaluationRuleBody(&untouched); err != nil || string(untouched) != `{"label": "no conditions"}` {
		t.Errorf("a body without conditions passes unchanged, got %v %s", err, untouched)
	}
}
