package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const noDictSourceID = "ECU-PUB-0063"

// One source with a nested outputSchema whose dictionary endpoint answers 404, as the live one does.
func newNoDictionaryBackend(t *testing.T) *httptest.Server {
	t.Helper()
	resetSourceCatalogCaches()
	t.Cleanup(resetSourceCatalogCaches)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/workflows/sources-status":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"sourceId": noDictSourceID, "sourceVersion": "v2", "enabled": true, "status": "active",
				"outputSchema": map[string]any{noDictSourceID: map[string]any{
					"data": map[string]any{"type": "object", "properties": map[string]any{
						"isSuccess": map[string]any{"type": "boolean"},
						"records": map[string]any{"type": "array", "items": map[string]any{
							"type": "object", "properties": map[string]any{"amount": map[string]any{"type": []any{"number", "null"}}},
						}},
					}},
					"sourceData": map[string]any{"type": "object"},
				}},
			}})
		case "/v1/documentation/data-dictionary":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"NotFoundError","message":"no results found"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDictionaryFallsBackToOutputSchemaOn404(t *testing.T) {
	srv := newNoDictionaryBackend(t)
	var stderr bytes.Buffer
	data, err := fetchAltdataDictionary(newTestClient(t, srv.URL), noDictSourceID, "", &stderr)
	if err != nil {
		t.Fatalf("a source with no dictionary must be answered from its outputSchema, got %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r["field"].(string)], _ = r["dataType"].(string)
		if r["sourceId"] != noDictSourceID || r["version"] != "v2" {
			t.Errorf("every row names the source and the resolved version, got %v", r)
		}
	}
	want := map[string]string{"data.isSuccess": "boolean", "data.records": "array", "data.records[].amount": "number|null", "sourceData": "object"}
	for field, typ := range want {
		if got[field] != typ {
			t.Errorf("field %s: want type %q, got %q (all: %v)", field, typ, got[field], got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("expected exactly %d paths, got %v", len(want), got)
	}
	if msg := stderr.String(); !strings.Contains(msg, "has no data dictionary") || strings.Contains(msg, "404") {
		t.Errorf("stderr must explain the fallback without echoing the HTTP error, got %q", msg)
	}
}

func TestDictionaryKeepsNon404Errors(t *testing.T) {
	resetSourceCatalogCaches()
	t.Cleanup(resetSourceCatalogCaches)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	if _, err := fetchAltdataDictionary(newTestClient(t, srv.URL), noDictSourceID, "v2", &bytes.Buffer{}); err == nil {
		t.Fatal("a 500 must stay an error, not turn into an outputSchema listing")
	}
}

func TestDescribeTakesTheVersionPositionally(t *testing.T) {
	srv := newNoDictionaryBackend(t)
	routeLoadClientTo(t, srv.URL)

	stdout, _, err := runListCmd(t, makeAltdataDescribeCmd(), noDictSourceID, "v2")
	if err != nil {
		t.Fatalf("describe <id> <version> must work like dictionary <id> <version>, got %v", err)
	}
	if !strings.Contains(stdout, `"selectedVersion": "v2"`) {
		t.Errorf("expected v2 selected, got %s", stdout)
	}
	if _, _, err := runListCmd(t, makeAltdataDescribeCmd(), noDictSourceID, "v2", "--version", "v1"); err == nil || !strings.Contains(err.Error(), "version given twice") {
		t.Errorf("conflicting versions must be refused, got %v", err)
	}
}
