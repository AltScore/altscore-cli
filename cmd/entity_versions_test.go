package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// borrower-central #2402 (HQ-2013): GET /v1/{entities}/{id}/versions[/{version}] on exactly
// these four resources, and an edit no longer reaches a published workflow before its
// next publish.
var versionedDecisioningResources = []string{"mapping-tables", "rule-trees", "scorecards", "evaluation-rules"}

func TestDecisioningEntitiesCarryVersionCommandsAndSayWhatAnEditDoes(t *testing.T) {
	for _, name := range versionedDecisioningResources {
		t.Run(name, func(t *testing.T) {
			group := findGroup(t, name)
			if !strings.Contains(group.Long, "An edit does NOT reach production on its own") {
				t.Errorf("%s --help must say an edit waits for the next publish", name)
			}
			for _, sub := range []string{"versions", "get-version"} {
				if findSubcommand(t, name, sub) == nil {
					t.Errorf("%s has no %s command", name, sub)
				}
			}
			update := findSubcommand(t, name, "update")
			if update == nil || !strings.Contains(update.Long, "Published workflow versions keep") {
				t.Errorf("%s update --help must say published versions keep their pinned version", name)
			}
			del := findSubcommand(t, name, "delete")
			if del == nil || !strings.Contains(del.Long, "survive the delete") {
				t.Errorf("%s delete --help must say the versions survive", name)
			}
			get := findSubcommand(t, name, "get")
			if get == nil || !strings.Contains(get.Long, "version, createdAt") {
				t.Errorf("%s get --help must list version among the response fields", name)
			}
		})
	}
	// Only these four are versioned: a stray Versioned would advertise routes BC lacks.
	for _, name := range []string{"borrowers", "policy-rules", "evaluators"} {
		if findSubcommand(t, name, "get-version") != nil || findSubcommand(t, name, "versions") != nil {
			t.Errorf("%s is not a versioned entity but has version commands", name)
		}
		if strings.Contains(findGroup(t, name).Long, "An edit does NOT reach production") {
			t.Errorf("%s carries the versioned-entity note", name)
		}
	}
}

func findGroup(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, g := range rootCmd.Commands() {
		if g.Name() == name {
			return g
		}
	}
	t.Fatalf("command group %s not registered", name)
	return nil
}

func TestParseEntityVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"3", 3, false},
		{"v3", 3, false},
		{" 12 ", 12, false},
		{"0", 0, true},
		{"-1", 0, true},
		{"latest", 0, true},
		{"v", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		got, err := parseEntityVersion(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseEntityVersion(%q) = %d, %v; want %d, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

type recordedRequest struct {
	method, path string
	body         map[string]any
}

func newRecordingBC(t *testing.T, respond func(r *http.Request) (int, string)) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{method: r.Method, path: r.URL.Path}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.body)
		}
		mu.Lock()
		seen = append(seen, rec)
		mu.Unlock()
		status, body := respond(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), seen...)
	}
}

func TestEntityVersionCommandsReadTheVersionRoutes(t *testing.T) {
	srv, seen := newRecordingBC(t, func(r *http.Request) (int, string) {
		if strings.HasSuffix(r.URL.Path, "/versions") {
			return 200, `[{"version":2,"source":"api"},{"version":1,"source":"api"}]`
		}
		return 200, `{"id":"mt 1","code":"band","version":2}`
	})
	routeLoadClientTo(t, srv.URL)
	def := ResourceDef{Name: "mapping-tables", Singular: "mapping-table", BasePath: "/v1/mapping-tables", Module: "borrower_central", Versioned: true}

	cases := []struct {
		cmd      string
		args     []string
		wantPath string
	}{
		{"versions", []string{"mt 1"}, "/v1/mapping-tables/mt 1/versions"},
		{"versions", []string{"mt 1", "v2"}, "/v1/mapping-tables/mt 1/versions/2"},
		{"get-version", []string{"mt 1", "2"}, "/v1/mapping-tables/mt 1/versions/2"},
	}
	for _, tc := range cases {
		before := len(seen())
		c := makeEntityVersionsCmd(def)
		if tc.cmd == "get-version" {
			c = makeEntityGetVersionCmd(def)
		}
		stdout, _, err := runListCmd(t, c, tc.args...)
		if err != nil {
			t.Fatalf("%s %v: %v", tc.cmd, tc.args, err)
		}
		got := seen()[before:]
		if len(got) != 1 || got[0].method != "GET" || got[0].path != tc.wantPath {
			t.Errorf("%s %v sent %+v, want one GET %s", tc.cmd, tc.args, got, tc.wantPath)
		}
		if !json.Valid([]byte(stdout)) {
			t.Errorf("%s %v: stdout is not JSON:\n%s", tc.cmd, tc.args, stdout)
		}
	}

	before := len(seen())
	if _, _, err := runListCmd(t, makeEntityGetVersionCmd(def), "mt 1", "latest"); err == nil {
		t.Error("get-version latest must be refused: the route takes a number")
	}
	if len(seen()) != before {
		t.Error("a bad version must be refused before any request")
	}
}

func TestUpdateOfAVersionedEntityDropsTheServerVersion(t *testing.T) {
	srv, seen := newRecordingBC(t, func(r *http.Request) (int, string) { return 200, `{}` })
	routeLoadClientTo(t, srv.URL)

	versioned := ResourceDef{Name: "rule-trees", Singular: "rule-tree", BasePath: "/v1/rule-trees", Module: "borrower_central", WorkflowAlias: true, Versioned: true}
	_, stderr, err := runListCmd(t, makeUpdateCmd(versioned), "rt-1", "--body", `{"label":"Tree","version":7,"workflowAlias":"wf"}`)
	if err != nil {
		t.Fatal(err)
	}
	got := seen()
	if len(got) != 1 || got[0].method != "PATCH" || got[0].path != "/v1/rule-trees/rt-1" {
		t.Fatalf("expected one PATCH /v1/rule-trees/rt-1, got %+v", got)
	}
	if _, has := got[0].body["version"]; has {
		t.Errorf("the PATCH body still carries version: %v", got[0].body)
	}
	if got[0].body["label"] != "Tree" || got[0].body["workflowAlias"] != "wf" {
		t.Errorf("only version may be dropped, got %v", got[0].body)
	}
	if !strings.Contains(stderr, `dropped "version"`) {
		t.Errorf("expected a note on stderr, got %q", stderr)
	}

	// Not every resource's version is the server's: an evaluator's is authored.
	plain := ResourceDef{Name: "evaluators", Singular: "evaluator", BasePath: "/v1/evaluators", Module: "borrower_central"}
	if _, _, err := runListCmd(t, makeUpdateCmd(plain), "ev-1", "--body", `{"version":"v2"}`); err != nil {
		t.Fatal(err)
	}
	last := seen()[len(seen())-1]
	if last.body["version"] != "v2" {
		t.Errorf("a non-versioned resource keeps its body untouched, got %v", last.body)
	}
}

func TestImportBodyDropsTheSourceTenantsVersion(t *testing.T) {
	out, err := normalizeImportBody(json.RawMessage(`[{"code":"a","version":3},{"code":"b"}]`), "mappingTables", "wf", "mapping-tables", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MappingTables []map[string]any `json:"mappingTables"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.MappingTables) != 2 {
		t.Fatalf("expected both items, got %s", out)
	}
	for _, item := range got.MappingTables {
		if _, has := item["version"]; has {
			t.Errorf("import item still carries version: %v", item)
		}
		if item["workflowAlias"] != "wf" {
			t.Errorf("the alias is still stamped, got %v", item)
		}
	}
}

func TestDecisioningCompactRowCarriesTheVersion(t *testing.T) {
	row := compactDecisioningRow(map[string]any{"id": "x", "code": "c", "version": float64(4)}).(compactDecisioningEntity)
	if row.Version != float64(4) {
		t.Errorf("compact row must carry the live version, got %v", row.Version)
	}
}
