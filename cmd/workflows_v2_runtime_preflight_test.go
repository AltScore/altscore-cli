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

func TestBorrowerIdFieldMustNameANodeInput(t *testing.T) {
	task := map[string]any{"ref": "enrich", "type": "altdata-enrichment", "label": "Enrich", "borrowerIdField": "inputs.party_id"}
	err := preflightTasks(docExtractionSpec(task))
	if err == nil || !strings.Contains(err.Error(), `node ref="enrich"`) || !strings.Contains(err.Error(), `{"borrower_id": "inputs.<name>"}`) {
		t.Fatalf("an inputs. path in borrowerIdField must be refused with the mapping to write, got %v", err)
	}
	for _, ok := range []string{"borrower_id", "party_id", "task_outputs.applicant.borrower_id"} {
		if p := borrowerIdFieldProblem(ok); p != "" {
			t.Errorf("borrowerIdField %q resolves at run time and must pass, got %q", ok, p)
		}
	}
	if p := borrowerIdFieldProblem("{{inputs.party_id}}"); p == "" {
		t.Error("a template in borrowerIdField never resolves and must be refused")
	}
}

func TestFieldsReadFromAnObjectInputMustBeDeclared(t *testing.T) {
	notice := func(message string) map[string]any {
		return map[string]any{"ref": "note", "type": "notices", "label": "Note",
			"noticesConfig": map[string]any{"message": message, "severity": "info"}}
	}
	spec := docExtractionSpec(notice("{{inputs.report.rating}} {{inputs.report.late_days}} {{inputs.open.anything}} " +
		"{{inputs.applicant_id}} {{my_inputs.report.not_an_input_read}} {{inputs.bare.field}}"))
	spec.InputVariables = map[string]any{
		"report":       map[string]any{"type": "object", "properties": map[string]any{"rating": map[string]any{"type": "number"}}},
		"open":         map[string]any{"type": "object", "additionalProperties": true},
		"applicant_id": map[string]any{"type": "string"},
		"bare":         map[string]any{"type": "object"},
	}
	err := preflightTasks(spec)
	if err == nil || !strings.Contains(err.Error(), "the nodes read bare.field, report.late_days, which the object inputs do not declare") {
		t.Fatalf("undeclared fields of object inputs must be listed together, got %v", err)
	}
	for _, never := range []string{"rating", "open.anything", "not_an_input_read", "applicant_id"} {
		if strings.Contains(err.Error(), never) {
			t.Errorf("%q is declared, open on purpose or not an input read, and must not be listed: %v", never, err)
		}
	}
	spec.InputVariables["report"] = map[string]any{"type": "object", "properties": map[string]any{
		"rating": map[string]any{"type": "number"}, "late_days": map[string]any{"type": "number"}}}
	spec.InputVariables["bare"] = map[string]any{"type": "object", "properties": map[string]any{"field": map[string]any{"type": "string"}}}
	if err := preflightTasks(spec); err != nil {
		t.Errorf("a spec whose object inputs declare every field it reads must pass, got %v", err)
	}
}

func TestPythonSandboxRefusalsMirrorTheEvalService(t *testing.T) {
	refused := map[string]string{
		"age = __import__('datetime').date.today().year - 2010": "line 1: __import__(...) is never allowed; write `import datetime` on its own line instead",
		"import os":                                     "line 1: import of 'os' is blocked",
		"import os.path":                                "line 1: import of 'os' is blocked",
		"import json, subprocess as sp":                 "line 1: import of 'subprocess' is blocked",
		"from urllib.parse import quote":                "line 1: import from 'urllib' is blocked",
		"x = 1\ny = 2\nprint(x)":                        "line 3: print(...) is never allowed; use logger.info(...) instead",
		"result = eval('1 + 1')":                        "line 1: eval(...) is never allowed",
		"with open('f') as fh:\n    result = fh.read()": "line 1: open(...) is never allowed",
		"result = os.getcwd()":                          "line 1: access to the 'os' module is blocked",
	}
	for code, want := range refused {
		got := strings.Join(sandboxRefusals(code), "; ")
		if !strings.Contains(got, want) {
			t.Errorf("%q: want a refusal containing %q, got %q", code, want, got)
		}
	}
	allowed := []string{
		"import datetime\nimport math\nfrom dateutil import parser\nresult = math.floor(datetime.date.today().year)",
		"pattern = re.compile(r'\\d+')\nlogger.info('ok')",
		"# print(x) and import os would be refused if they were code\nresult = 1",
		"label = \"print(\" + 'os.path' + '''import sys'''",
		"result = inputs.get('task_outputs.applicant.os.version')",
		"my_os = {'path': 1}\nresult = my_os.get('path')",
	}
	for _, code := range allowed {
		if got := sandboxRefusals(code); len(got) > 0 {
			t.Errorf("%q runs in the sandbox and must pass, got %q", code, got)
		}
	}
	spec := docExtractionSpec(map[string]any{"ref": "note", "type": "notices", "label": "Note",
		"noticesConfig": map[string]any{"message": "hi", "severity": "info"}})
	spec.CustomVariables = map[string]any{
		"age":  map[string]any{"expression": "result = __import__('datetime').date.today().year", "returnValue": "result"},
		"band": map[string]any{"expression": "import math\nresult = math.floor(inputs['x'])", "returnValue": "result"},
	}
	err := preflightTasks(spec)
	if err == nil || !strings.Contains(err.Error(), `customVariables["age"]: the Python sandbox refuses this code`) {
		t.Fatalf("a variable the sandbox refuses must fail the dry-run before any request, got %v", err)
	}
	if strings.Contains(err.Error(), `customVariables["band"]`) {
		t.Errorf("an allowed import must not be reported: %v", err)
	}
}

func TestSetVariableRefusesSandboxedCodeBeforeAnyRequest(t *testing.T) {
	cmd := makeWfv2SetVariableCmd()
	cmd.SetArgs([]string{"wf-1", "--scope", "custom", "--name", "age",
		"--default", `{"expression": "result = __import__('os').getcwd()", "returnValue": "result"}`})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `customVariables["age"]: the Python sandbox refuses this code`) {
		t.Fatalf("set-variable must refuse code the sandbox refuses, before loading a client, got %v", err)
	}
}
