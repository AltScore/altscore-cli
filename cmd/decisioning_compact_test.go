package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecisioningListsAreCompactByDefault(t *testing.T) {
	big := strings.Repeat("x", 2000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rule-trees" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": "rt-1", "code": "credit-tree", "label": "Credit tree", "workflowAlias": "credit-flow",
			"rules": []any{map[string]any{"note": big}, map[string]any{"note": big}}, "updatedAt": "2026-10-05T00:00:00",
		}})
	}))
	t.Cleanup(srv.Close)
	routeLoadClientTo(t, srv.URL)

	def := ResourceDef{Name: "rule-trees", Module: "borrower_central", BasePath: "/v1/rule-trees", Actions: []string{"list"}, WorkflowAlias: true, CompactList: decisioningCompactList}
	stdout, _, err := runListCmd(t, makeListCmd(def))
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeRows(t, stdout)
	if len(rows) != 1 || rows[0]["code"] != "credit-tree" || rows[0]["workflowAlias"] != "credit-flow" || rows[0]["rules"] != float64(2) {
		t.Fatalf("expected one compact row with code, owner and a rule count, got %v", rows)
	}
	if strings.Contains(stdout, big) {
		t.Error("the compact row must not carry the rule bodies")
	}
	stdout, _, err = runListCmd(t, makeListCmd(def), "--full")
	if err != nil || !strings.Contains(stdout, big) {
		t.Errorf("--full keeps the raw items, got %v", err)
	}
	if ex := makeListCmd(def).Example; strings.Contains(ex, "status=ACTIVE") || !strings.Contains(ex, "workflow-alias=") {
		t.Errorf("a decisioning entity has no status filter; its example must filter by workflow-alias, got:\n%s", ex)
	}
}
