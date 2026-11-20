package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const ppWorkflowID = "11111111-2222-3333-4444-555555555555"

type ppRequest struct {
	Method, Path, Body string
}

// Serves one canned response per "METHOD path" and records every request.
func ppServer(t *testing.T, routes map[string]func(w http.ResponseWriter)) (*httptest.Server, *[]ppRequest) {
	t.Helper()
	var seen []ppRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen = append(seen, ppRequest{r.Method, r.URL.Path, string(raw)})
		w.Header().Set("Content-Type", "application/json")
		if handler, ok := routes[r.Method+" "+r.URL.Path]; ok {
			handler(w)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func ppJSON(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestPreflightBatchPolicyBody(t *testing.T) {
	for name, body := range map[string]string{
		"empty clears":                `{}`,
		"post-processing only":        `{"postProcessing":{"enabled":true,"workflowAlias":"post-scoring"}}`,
		"delivery only":               `{"delivery":{"enabled":true,"to":"ops@example.com"}}`,
		"post on, delivery off":       `{"delivery":{"enabled":false,"to":"ops@example.com"},"postProcessing":{"enabled":true,"workflowAlias":"post-scoring"}}`,
		"post off, delivery on":       `{"delivery":{"enabled":true,"to":"ops@example.com"},"postProcessing":{"enabled":false}}`,
		"auto retry untouched":        `{"autoRetry":{"enabled":true,"maxRounds":2}}`,
		"post disabled without alias": `{"postProcessing":{"enabled":false}}`,
	} {
		if _, err := preflightBatchPolicyBody(json.RawMessage(body)); err != nil {
			t.Errorf("%s: should pass, got: %v", name, err)
		}
	}
	for name, tc := range map[string]struct{ body, want string }{
		"both enabled":  {`{"delivery":{"enabled":true,"to":"ops@example.com"},"postProcessing":{"enabled":true,"workflowAlias":"post-scoring"}}`, "BATCH_POST_PROCESSING_EXCLUDES_DELIVERY"},
		"snake_case":    {`{"delivery":{"enabled":true},"post_processing":{"enabled":true,"workflow_alias":"post-scoring"}}`, "BATCH_POST_PROCESSING_EXCLUDES_DELIVERY"},
		"missing alias": {`{"postProcessing":{"enabled":true}}`, "BATCH_POST_PROCESSING_WORKFLOW_REQUIRED"},
		"blank alias":   {`{"postProcessing":{"enabled":true,"workflowAlias":"  "}}`, "BATCH_POST_PROCESSING_WORKFLOW_REQUIRED"},
		"not an object": {`[1,2]`, "must be a JSON object"},
		"null":          {`null`, "must be a JSON object"},
	} {
		_, err := preflightBatchPolicyBody(json.RawMessage(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error mentioning %q, got: %v", name, tc.want, err)
		}
	}
}

func TestSetWorkflowBatchPolicy_PutsTheWholeBodyToTheIdRoute(t *testing.T) {
	srv, seen := ppServer(t, map[string]func(http.ResponseWriter){
		"PUT /v2/workflows/" + ppWorkflowID + "/batch-policy": ppJSON(200,
			`{"workflowId":"`+ppWorkflowID+`","batchPolicy":{"delivery":null,"autoRetry":null,"postProcessing":{"enabled":true,"workflowAlias":"post-scoring"}}}`),
	})
	var errb bytes.Buffer
	body := `{"postProcessing":{"enabled":true,"workflowAlias":"post-scoring"}}`
	data, err := setWorkflowBatchPolicy(newTestClient(t, srv.URL), ppWorkflowID, json.RawMessage(body), &errb)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0].Method != "PUT" || (*seen)[0].Body != body {
		t.Fatalf("want one PUT carrying the body verbatim, got %+v", *seen)
	}
	if !strings.Contains(string(data), `"post-scoring"`) {
		t.Errorf("response not passed through: %s", data)
	}
	if errb.Len() != 0 {
		t.Errorf("no warning expected when the policy was stored, got %q", errb.String())
	}
}

func TestSetWorkflowBatchPolicy_ExclusivityIsRefusedBeforeAnyRequest(t *testing.T) {
	srv, seen := ppServer(t, nil)
	_, err := setWorkflowBatchPolicy(newTestClient(t, srv.URL), ppWorkflowID,
		json.RawMessage(`{"delivery":{"enabled":true,"to":"ops@example.com"},"postProcessing":{"enabled":true,"workflowAlias":"post-scoring"}}`), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want the exclusivity error, got: %v", err)
	}
	if len(*seen) != 0 {
		t.Errorf("no request may be sent, got %+v", *seen)
	}
}

func TestSetWorkflowBatchPolicy_WarnsWhenAnOlderBackendDropsPostProcessing(t *testing.T) {
	srv, _ := ppServer(t, map[string]func(http.ResponseWriter){
		"PUT /v2/workflows/" + ppWorkflowID + "/batch-policy": ppJSON(200,
			`{"workflowId":"`+ppWorkflowID+`","batchPolicy":{"delivery":null,"autoRetry":null}}`),
	})
	var errb bytes.Buffer
	_, err := setWorkflowBatchPolicy(newTestClient(t, srv.URL), ppWorkflowID,
		json.RawMessage(`{"postProcessing":{"enabled":true,"workflowAlias":"post-scoring"}}`), &errb)
	if err != nil {
		t.Fatalf("the write succeeded, so no error: %v", err)
	}
	if !strings.Contains(errb.String(), "WARNING") || !strings.Contains(errb.String(), "ignored") {
		t.Errorf("want a warning that the section was ignored, got %q", errb.String())
	}
}

func TestSetWorkflowBatchPolicy_SurfacesTheIncompatibleFlowRefusal(t *testing.T) {
	srv, _ := ppServer(t, map[string]func(http.ResponseWriter){
		"PUT /v2/workflows/" + ppWorkflowID + "/batch-policy": ppJSON(400,
			`{"code":"BadInputError","message":"'scoring' cannot run as post-processing. It requires inputs a finished batch cannot give it: person_id","details":{"errorSubCode":"BATCH_POST_PROCESSING_WORKFLOW_INCOMPATIBLE","field":"postProcessing.workflowAlias"}}`),
	})
	_, err := setWorkflowBatchPolicy(newTestClient(t, srv.URL), ppWorkflowID,
		json.RawMessage(`{"postProcessing":{"enabled":true,"workflowAlias":"scoring"}}`), io.Discard)
	if err == nil {
		t.Fatal("want the backend's refusal")
	}
	for _, want := range []string{"BATCH_POST_PROCESSING_WORKFLOW_INCOMPATIBLE", "person_id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestBatchPolicy_AliasResolvesToTheActiveVersion(t *testing.T) {
	srv, seen := ppServer(t, map[string]func(http.ResponseWriter){
		"GET /v2/workflows": ppJSON(200, `[{"id":"`+ppWorkflowID+`","alias":"scoring","status":"ACTIVE","version":4}]`),
		"GET /v2/workflows/" + ppWorkflowID: ppJSON(200,
			`{"id":"`+ppWorkflowID+`","alias":"scoring","batchPolicy":{"postProcessing":{"enabled":true,"workflowAlias":"post-scoring"}}}`),
	})
	var errb bytes.Buffer
	out, err := getWorkflowBatchPolicy(newTestClient(t, srv.URL), "scoring", &errb)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out["workflowId"] != ppWorkflowID {
		t.Errorf("workflowId = %v", out["workflowId"])
	}
	policy, _ := out["batchPolicy"].(map[string]any)
	if policy == nil || policy["postProcessing"] == nil {
		t.Errorf("batchPolicy not read off the workflow document: %v", out)
	}
	if !strings.Contains(errb.String(), "ACTIVE") || !strings.Contains(errb.String(), ppWorkflowID) {
		t.Errorf("stderr should say which version the alias resolved to, got %q", errb.String())
	}
	if last := (*seen)[len(*seen)-1]; last.Path != "/v2/workflows/"+ppWorkflowID {
		t.Errorf("last request = %+v", last)
	}
}

func TestBatchPolicy_GetWithoutAPolicyPrintsNull(t *testing.T) {
	srv, seen := ppServer(t, map[string]func(http.ResponseWriter){
		"GET /v2/workflows/" + ppWorkflowID: ppJSON(200, `{"id":"`+ppWorkflowID+`","alias":"scoring"}`),
	})
	out, err := getWorkflowBatchPolicy(newTestClient(t, srv.URL), ppWorkflowID, io.Discard)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v, present := out["batchPolicy"]; !present || v != nil {
		t.Errorf("want batchPolicy: null, got %v", out)
	}
	if len(*seen) != 1 {
		t.Errorf("an id needs no alias lookup, got %+v", *seen)
	}
}

func TestBatchPolicy_UnknownAliasIsAClearError(t *testing.T) {
	srv, _ := ppServer(t, map[string]func(http.ResponseWriter){"GET /v2/workflows": ppJSON(200, `[]`)})
	_, err := getWorkflowBatchPolicy(newTestClient(t, srv.URL), "nope", io.Discard)
	if err == nil || !strings.Contains(err.Error(), `no ACTIVE or DRAFT workflow has alias "nope"`) {
		t.Fatalf("got: %v", err)
	}
}

const ppCandidatesBody = `{"workflowId":"` + ppWorkflowID + `","candidates":[
 {"workflowId":"aaaaaaaa-0000-0000-0000-000000000001","alias":"post-scoring","label":"Post Scoring","version":2,"compatible":true,"reason":null},
 {"workflowId":"aaaaaaaa-0000-0000-0000-000000000002","alias":"onboarding","label":"Onboarding","version":7,"compatible":false,
  "reason":{"code":"REQUIRES_INPUT","inputs":["person_id"],"message":"It requires inputs a finished batch cannot give it: person_id"}},
 {"workflowId":"` + ppWorkflowID + `","alias":"scoring","label":"Scoring","version":4,"compatible":false,
  "reason":{"code":"SAME_WORKFLOW","inputs":[]}}]}`

func TestPostProcessingCandidates_FetchFilterAndTable(t *testing.T) {
	srv, seen := ppServer(t, map[string]func(http.ResponseWriter){
		"GET /v2/workflows/" + ppWorkflowID + "/post-processing-candidates": ppJSON(200, ppCandidatesBody),
	})
	res, err := fetchPostProcessingCandidates(newTestClient(t, srv.URL), ppWorkflowID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if (*seen)[0].Method != "GET" || len(res.Candidates) != 3 {
		t.Fatalf("request %+v, candidates %d", (*seen)[0], len(res.Candidates))
	}

	var table bytes.Buffer
	if err := renderPostProcessingCandidates(&table, res.Candidates); err != nil {
		t.Fatalf("render: %v", err)
	}
	lines := strings.Split(strings.TrimRight(table.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want a header and 3 rows, got:\n%s", table.String())
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "ALIAS LABEL VERSION COMPATIBLE REASON" {
		t.Errorf("header = %q", lines[0])
	}
	for i, want := range [][]string{
		{"post-scoring", "Post Scoring", "2", "yes", "-"},
		{"onboarding", "Onboarding", "7", "no", "It requires inputs a finished batch cannot give it: person_id"},
		{"scoring", "Scoring", "4", "no", "SAME_WORKFLOW"}, // no message: falls back to the code
	} {
		for _, cell := range want {
			if !strings.Contains(lines[i+1], cell) {
				t.Errorf("row %d %q lacks %q", i+1, lines[i+1], cell)
			}
		}
	}

	only := filterCompatibleCandidates(res.Candidates)
	if len(only) != 1 || only[0].Alias != "post-scoring" {
		t.Errorf("--compatible-only kept %+v", only)
	}
	// The default JSON output keeps the response's field names.
	encoded, _ := json.Marshal(postProcessingCandidates{WorkflowID: res.WorkflowID, Candidates: only})
	for _, key := range []string{`"workflowId"`, `"candidates"`, `"alias"`, `"label"`, `"version"`, `"compatible"`, `"reason"`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("JSON output lacks %s: %s", key, encoded)
		}
	}
}

func TestPostProcessingReasonText_FallsBackToCodeAndInputs(t *testing.T) {
	cand := postProcessingCandidate{Reason: &postProcessingReason{Code: "REQUIRES_ONE_OF", Inputs: []string{"tax_id", "person_id"}}}
	if got := cand.reasonText(); got != "REQUIRES_ONE_OF: tax_id, person_id" {
		t.Errorf("reasonText = %q", got)
	}
}

func TestPostProcessingCandidates_BackendWithoutTheRoute(t *testing.T) {
	t.Run("404", func(t *testing.T) {
		srv, _ := ppServer(t, nil)
		_, err := fetchPostProcessingCandidates(newTestClient(t, srv.URL), ppWorkflowID)
		if err == nil || !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "does not serve") {
			t.Fatalf("want the 404 with the not-deployed hint, got: %v", err)
		}
	})
	t.Run("200 from an older two-segment route", func(t *testing.T) {
		srv, _ := ppServer(t, map[string]func(http.ResponseWriter){
			"GET /v2/workflows/" + ppWorkflowID + "/post-processing-candidates": ppJSON(200, `{"id":"x","alias":"y"}`),
		})
		_, err := fetchPostProcessingCandidates(newTestClient(t, srv.URL), ppWorkflowID)
		if err == nil || !strings.Contains(err.Error(), "no 'candidates' list") {
			t.Fatalf("a response without candidates must be an error, not an empty table: %v", err)
		}
	})
}

func TestCreatePostProcessingTemplate(t *testing.T) {
	route := "POST /v2/workflows/" + ppWorkflowID + "/post-processing-template"
	t.Run("201", func(t *testing.T) {
		srv, seen := ppServer(t, map[string]func(http.ResponseWriter){
			route: ppJSON(201, `{"workflowId":"`+ppWorkflowID+`","workflowAlias":"post-scoring","published":true,"batchPolicy":{"postProcessing":{"enabled":true,"workflowAlias":"post-scoring"}}}`),
		})
		data, err := createPostProcessingTemplate(newTestClient(t, srv.URL), ppWorkflowID)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if (*seen)[0].Method != "POST" || (*seen)[0].Body != "" {
			t.Errorf("want a body-less POST, got %+v", (*seen)[0])
		}
		if !strings.Contains(string(data), `"workflowAlias":"post-scoring"`) {
			t.Errorf("response not passed through: %s", data)
		}
	})
	t.Run("alias taken", func(t *testing.T) {
		srv, _ := ppServer(t, map[string]func(http.ResponseWriter){
			route: ppJSON(409, `{"code":"ConflictError","message":"conflict","details":{"errorSubCode":"BATCH_POST_PROCESSING_TEMPLATE_ALIAS_TAKEN"}}`),
		})
		_, err := createPostProcessingTemplate(newTestClient(t, srv.URL), ppWorkflowID)
		if err == nil || !strings.Contains(err.Error(), "BATCH_POST_PROCESSING_TEMPLATE_ALIAS_TAKEN") || strings.Contains(err.Error(), "does not serve") {
			t.Fatalf("got: %v", err)
		}
	})
	t.Run("backend without the route", func(t *testing.T) {
		srv, _ := ppServer(t, map[string]func(http.ResponseWriter){route: ppJSON(405, `{"detail":"Method Not Allowed"}`)})
		_, err := createPostProcessingTemplate(newTestClient(t, srv.URL), ppWorkflowID)
		if err == nil || !strings.Contains(err.Error(), "HTTP 405") || !strings.Contains(err.Error(), "does not serve") {
			t.Fatalf("got: %v", err)
		}
	})
}

func TestRetryBatchPostProcessing(t *testing.T) {
	const batchID = "batch-demo-001"
	route := "POST /v2/workflows/" + batchID + "/post-process"
	t.Run("202", func(t *testing.T) {
		srv, seen := ppServer(t, map[string]func(http.ResponseWriter){
			route: ppJSON(202, `{"batchId":"`+batchID+`","status":"post_processing","runId":"run-demo-1"}`),
		})
		data, err := retryBatchPostProcessing(newTestClient(t, srv.URL), batchID)
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		if len(*seen) != 1 || (*seen)[0].Method != "POST" || (*seen)[0].Body != "" {
			t.Errorf("want one body-less POST, got %+v", *seen)
		}
		if !strings.Contains(string(data), `"runId":"run-demo-1"`) {
			t.Errorf("response not passed through: %s", data)
		}
	})
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"not retryable", 409,
			`{"code":"ConflictError","message":"conflict","details":{"errorSubCode":"BATCH_POST_PROCESSING_NOT_RETRYABLE","status":"processing","message":"Post-processing can be run again once the batch has finished: it is 'processing'."}}`,
			[]string{"HTTP 409", "BATCH_POST_PROCESSING_NOT_RETRYABLE", "it is 'processing'", "execution-batches get " + batchID}},
		{"already running", 409,
			`{"code":"ConflictError","message":"conflict","details":{"errorSubCode":"BATCH_POST_PROCESSING_ALREADY_RUNNING","message":"The post-processing of this batch is already running."}}`,
			[]string{"HTTP 409", "BATCH_POST_PROCESSING_ALREADY_RUNNING", "already running"}},
		{"no post-processing", 400,
			`{"code":"BadInputError","message":"wrong input values","details":{"errorSubCode":"BATCH_HAS_NO_POST_PROCESSING","message":"This batch was not launched with a post-processing flow."}}`,
			[]string{"HTTP 400", "BATCH_HAS_NO_POST_PROCESSING", "not launched with a post-processing flow", "batch-policy set"}},
		{"message only on the envelope", 409,
			`{"code":"ConflictError","message":"The post-processing of this batch is already running.","details":{"errorSubCode":"BATCH_POST_PROCESSING_ALREADY_RUNNING"}}`,
			[]string{"already running", "BATCH_POST_PROCESSING_ALREADY_RUNNING"}},
		{"batch not found on a current backend", 404,
			`{"code":"NotFoundError","message":"Batch batch-demo-001 not found","details":{}}`,
			[]string{"HTTP 404", "not found", "check the id"}},
		{"backend without the route", 405, `{"detail":"Method Not Allowed"}`,
			[]string{"HTTP 405", "Method Not Allowed", "does not serve POST /v2/workflows/{batchId}/post-process"}},
		{"unrelated server error", 500, `oops`, []string{"HTTP 500: oops"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := ppServer(t, map[string]func(http.ResponseWriter){route: ppJSON(tc.status, tc.body)})
			_, err := retryBatchPostProcessing(newTestClient(t, srv.URL), batchID)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestPrintPublishResponseWarnings(t *testing.T) {
	t.Run("post-processing warning", func(t *testing.T) {
		var errb bytes.Buffer
		n := printPublishResponseWarnings(&errb, json.RawMessage(`{"id":"wf","status":"ACTIVE","warnings":[
		 {"code":"POST_PROCESSING_FLOW_INCOMPATIBLE","message":"This flow runs when batches of other workflows finish, and the version just published can no longer run that way.",
		  "reason":{"code":"REQUIRES_INPUT","inputs":["person_id"]},
		  "usedBy":[{"workflowId":"`+ppWorkflowID+`","alias":"scoring","label":"Scoring","version":4,"status":"ACTIVE"}]}]}`))
		if n != 1 {
			t.Fatalf("printed %d warning(s), want 1", n)
		}
		for _, want := range []string{"# WARNING [POST_PROCESSING_FLOW_INCOMPATIBLE]", "can no longer run that way", "used by scoring", "IS published"} {
			if !strings.Contains(errb.String(), want) {
				t.Errorf("stderr %q lacks %q", errb.String(), want)
			}
		}
	})
	for name, body := range map[string]string{
		"no warnings key":           `{"id":"wf","status":"ACTIVE"}`,
		"null":                      `{"id":"wf","warnings":null}`,
		"empty list":                `{"id":"wf","warnings":[]}`,
		"object-shaped (PATCH's)":   `{"id":"wf","warnings":{"inputMappingErrors":["x"]}}`,
		"not JSON":                  `ok`,
		"list of non-warning items": `{"warnings":[{}]}`,
	} {
		var errb bytes.Buffer
		if n := printPublishResponseWarnings(&errb, json.RawMessage(body)); n != 0 || errb.Len() != 0 {
			t.Errorf("%s: want nothing printed, got %d / %q", name, n, errb.String())
		}
	}
}

func TestPublishWorkflowV2_ReturnsTheResponseWithWarningsUntouched(t *testing.T) {
	const body = `{"id":"` + ppWorkflowID + `","status":"ACTIVE","warnings":[{"code":"POST_PROCESSING_FLOW_INCOMPATIBLE","message":"m"}]}`
	srv, _ := ppServer(t, map[string]func(http.ResponseWriter){
		"POST /v2/workflows/" + ppWorkflowID + "/publish": ppJSON(200, body),
	})
	data, err := publishWorkflowV2(newTestClient(t, srv.URL), ppWorkflowID, "")
	if err != nil {
		t.Fatalf("a publish that carries warnings is still a success: %v", err)
	}
	if string(data) != body {
		t.Errorf("stdout payload changed: %s", data)
	}
}

func TestServerApplySummary_PrintsPublishWarningsWhenTheBackendSendsThem(t *testing.T) {
	var res serverApplyResult
	if err := json.Unmarshal([]byte(`{"mode":"update","workflowAlias":"post-scoring","tasks":[],"validation":{"valid":true},
	  "publish":{"requested":true,"published":true,"errors":[],"warnings":[{"code":"POST_PROCESSING_FLOW_INCOMPATIBLE","message":"m"}]}}`), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var errb bytes.Buffer
	printServerApplySummary(&errb, &res)
	if !strings.Contains(errb.String(), "# published") || !strings.Contains(errb.String(), "# WARNING [POST_PROCESSING_FLOW_INCOMPATIBLE]: m") {
		t.Errorf("summary = %q", errb.String())
	}

	var plain serverApplyResult
	_ = json.Unmarshal([]byte(`{"mode":"update","workflowAlias":"a","tasks":[],"validation":{"valid":true},"publish":{"requested":true,"published":true,"errors":[]}}`), &plain)
	errb.Reset()
	printServerApplySummary(&errb, &plain)
	if strings.Contains(errb.String(), "WARNING") {
		t.Errorf("a response without warnings must print none: %q", errb.String())
	}
}

func TestPostProcessingCommandsAreRegistered(t *testing.T) {
	for _, path := range [][]string{
		{"workflows-v2", "batch-policy", "get"},
		{"workflows-v2", "batch-policy", "set"},
		{"workflows-v2", "post-processing", "candidates"},
		{"workflows-v2", "post-processing", "create-template"},
		{"workflows-v2", "post-processing", "row-fields"},
		{"workflows-v2", "batch", "post-process"},
		{"workflows-v2", "batch", "pause"},
		{"workflows-v2", "batch", "continue"},
		{"workflows-v2", "batch", "terminate"},
	} {
		cmd, rest, err := rootCmd.Find(path)
		if err != nil || len(rest) != 0 || cmd.Name() != path[len(path)-1] {
			t.Errorf("command %v not registered (found %q, rest %v, err %v)", path, cmd.Name(), rest, err)
		}
	}
	cand, _, _ := rootCmd.Find([]string{"workflows-v2", "post-processing", "candidates"})
	for _, flag := range []string{"compatible-only", "table"} {
		if cand.Flags().Lookup(flag) == nil {
			t.Errorf("candidates lacks --%s", flag)
		}
	}
	rowFields, _, _ := rootCmd.Find([]string{"workflows-v2", "post-processing", "row-fields"})
	if rowFields.Flags().Lookup("table") == nil {
		t.Error("row-fields lacks --table")
	}
}

const ppRowFieldsBody = `{"sources":[
 {"alias":"scoring","label":"Scoring","fields":["person_id","decision"]},
 {"alias":"onboarding","label":"Onboarding","fields":["person_id","segment"]}],
 "fields":["person_id","decision","segment"],
 "standardFields":["execution_id","item_index","is_success","error_message"]}`

func TestBatchRowFields_RequestPathJSONAndTable(t *testing.T) {
	srv, seen := ppServer(t, map[string]func(http.ResponseWriter){
		"GET /v2/workflows/" + ppWorkflowID + "/batch-row-fields": ppJSON(200, ppRowFieldsBody),
	})
	res, err := fetchBatchRowFields(newTestClient(t, srv.URL), ppWorkflowID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0].Method != "GET" || (*seen)[0].Path != "/v2/workflows/"+ppWorkflowID+"/batch-row-fields" {
		t.Fatalf("requests %+v", *seen)
	}
	if len(res.Sources) != 2 || len(res.Fields) != 3 || len(res.StandardFields) != 4 {
		t.Fatalf("parsed %+v", res)
	}
	encoded, _ := json.Marshal(res)
	for _, key := range []string{`"sources"`, `"alias"`, `"label"`, `"fields"`, `"standardFields"`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("JSON output lacks %s: %s", key, encoded)
		}
	}

	var table bytes.Buffer
	if err := renderBatchRowFields(&table, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	lines := strings.Split(strings.TrimRight(table.String(), "\n"), "\n")
	if len(lines) != 1+3+4 {
		t.Fatalf("want a header, 3 fields and 4 standard fields, got:\n%s", table.String())
	}
	for i, want := range [][]string{
		{"FIELD", "FROM"},
		{"person_id", "scoring, onboarding"},
		{"decision", "scoring"},
		{"segment", "onboarding"},
		{"execution_id", "(standard)"},
	} {
		for _, cell := range want {
			if !strings.Contains(lines[i], cell) {
				t.Errorf("line %d %q lacks %q", i, lines[i], cell)
			}
		}
	}
}

func TestBatchRowFields_EmptyAndBackendWithoutTheRoute(t *testing.T) {
	srv, _ := ppServer(t, map[string]func(http.ResponseWriter){
		"GET /v2/workflows/" + ppWorkflowID + "/batch-row-fields": ppJSON(200, `{"sources":[],"fields":[],"standardFields":["execution_id"]}`),
	})
	res, err := fetchBatchRowFields(newTestClient(t, srv.URL), ppWorkflowID)
	if err != nil || len(res.Sources) != 0 || len(res.Fields) != 0 {
		t.Fatalf("no sources is a valid answer, got %+v, %v", res, err)
	}
	encoded, _ := json.Marshal(res)
	if !strings.Contains(string(encoded), `"sources":[]`) || !strings.Contains(string(encoded), `"fields":[]`) {
		t.Errorf("empty lists must print as [], not null: %s", encoded)
	}

	srv404, _ := ppServer(t, nil)
	_, err = fetchBatchRowFields(newTestClient(t, srv404.URL), ppWorkflowID)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "batch-row-fields") {
		t.Fatalf("want the 404 naming the route, got: %v", err)
	}
	srvOld, _ := ppServer(t, map[string]func(http.ResponseWriter){
		"GET /v2/workflows/" + ppWorkflowID + "/batch-row-fields": ppJSON(200, `{"id":"x"}`),
	})
	_, err = fetchBatchRowFields(newTestClient(t, srvOld.URL), ppWorkflowID)
	if err == nil || !strings.Contains(err.Error(), "no 'fields' list") {
		t.Fatalf("a response without fields must be an error, got: %v", err)
	}
}

func TestBatchRowFields_InputFieldsAreListedUnderInput(t *testing.T) {
	body := `{"sources":[{"alias":"scoring","label":"Scoring","fields":["decision"],"inputFields":["person_id"]}],
 "fields":["decision"],"inputFields":["person_id"],"standardFields":["error_message"]}`
	srv, _ := ppServer(t, map[string]func(http.ResponseWriter){
		"GET /v2/workflows/" + ppWorkflowID + "/batch-row-fields": ppJSON(200, body),
	})
	res, err := fetchBatchRowFields(newTestClient(t, srv.URL), ppWorkflowID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var table bytes.Buffer
	if err := renderBatchRowFields(&table, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	lines := strings.Split(strings.TrimRight(table.String(), "\n"), "\n")
	if len(lines) != 4 || !strings.Contains(lines[2], "input.person_id") || !strings.Contains(lines[2], "includeInput") {
		t.Fatalf("want the input field listed as input.<name>, got:\n%s", table.String())
	}
}

func TestBatchRowFields_OlderBackendWithoutInputFields(t *testing.T) {
	srv, _ := ppServer(t, map[string]func(http.ResponseWriter){
		"GET /v2/workflows/" + ppWorkflowID + "/batch-row-fields": ppJSON(200, ppRowFieldsBody),
	})
	res, err := fetchBatchRowFields(newTestClient(t, srv.URL), ppWorkflowID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	encoded, _ := json.Marshal(res)
	if !strings.Contains(string(encoded), `"inputFields":[]`) {
		t.Errorf("an absent inputFields is printed as an empty list: %s", encoded)
	}
}
