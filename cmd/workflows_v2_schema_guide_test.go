package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWfv2SchemaGuidePath(t *testing.T) {
	cases := []struct {
		name string
		args []string
		full bool
		want string
	}{
		{"index", nil, false, "/v1/meta/workflows-v2-schema"},
		{"full", nil, true, "/v1/meta/workflows-v2-schema?full=true"},
		{"section", []string{"nodes"}, false, "/v1/meta/workflows-v2-schema?section=nodes"},
		{"section wins over full", []string{"nodes"}, true, "/v1/meta/workflows-v2-schema?section=nodes"},
		{"task type", []string{"tasks", "deal"}, false, "/v1/meta/workflows-v2-schema?section=tasks&type=deal"},
		{"task type with dash", []string{"tasks", "package-io"}, false, "/v1/meta/workflows-v2-schema?section=tasks&type=package-io"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wfv2SchemaGuidePath(tc.args, tc.full); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWfv2SchemaGuideCmdAcceptsSectionAndType(t *testing.T) {
	cmd := makeWfv2SchemaGuideCmd()
	if err := cmd.Args(cmd, []string{"tasks", "deal"}); err != nil {
		t.Fatalf("two positional args must be accepted: %v", err)
	}
	if err := cmd.Args(cmd, []string{"tasks", "deal", "extra"}); err == nil {
		t.Fatal("three positional args must be rejected")
	}
	if cmd.Flags().Lookup("full") == nil {
		t.Fatal("--full flag missing")
	}
}

func TestResolveTaskTypeName(t *testing.T) {
	cases := []struct {
		in, want string
		changed  bool
	}{
		{"compute-variables", "compute-variables", false},
		{"compute", "compute-variables", true},
		{"rule_tree", "rule-tree", true},
		{"computeVariables", "compute-variables", true},
		{"Rule-Tree", "rule-tree", true},
		{"data-store", "data-store", false}, // two types start with it: the server answers
		{"nonsense", "nonsense", false},
	}
	for _, tc := range cases {
		got, changed := resolveTaskTypeName(tc.in, validTaskTypes)
		if got != tc.want || changed != tc.changed {
			t.Errorf("resolveTaskTypeName(%q) = %q, %v; want %q, %v", tc.in, got, changed, tc.want, tc.changed)
		}
	}
}

const guideTasksFixture = `{"tasks": {
  "description": "Tasks live at /v2/tasks.",
  "perType": {"artifact": {"purpose": "Read one TABLE artifact's published rows onto the node output. More text."}, "deal": ["a list, as some live entries are"]},
  "perTypeAuto": {"_common": {"alias": "str"}, "artifact": {"x": 1}, "deal": {"y": 2}, "document-extraction": {"extractionSchema": "object"}},
  "deprecatedTypes": {"description": "Types a new workflow must not use.", "create-borrower": "Use 'customer' with operation=write."}
}}`

func TestCompactTaskTypeIndex(t *testing.T) {
	index, err := compactTaskTypeIndex(json.RawMessage(guideTasksFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []taskTypeRow{
		{Type: "artifact", Notes: true, Purpose: "Read one TABLE artifact's published rows onto the node output."},
		{Type: "deal", Notes: true},
		{Type: "document-extraction", Notes: false},
	}
	if len(index.Types) != len(want) {
		t.Fatalf("expected %d types (no _common), got %+v", len(want), index.Types)
	}
	for i := range want {
		if index.Types[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, index.Types[i], want[i])
		}
	}
	if index.Deprecated["create-borrower"] == "" || index.Deprecated["description"] != "" {
		t.Errorf("deprecated keeps the types and drops the description key, got %v", index.Deprecated)
	}
}

func TestSearchGuide(t *testing.T) {
	guide := json.RawMessage(`{
	  "conditions": {"operators": "Use is_not_empty for presence checks.", "list": ["equals", "is_not_empty"]},
	  "tasks": {"perTypeAuto": {"document-extraction": {"extractionSchema": "object", "extraction": "nested"}}},
	  "nodes": {"note": "nothing here"}
	}`)
	matches, total, err := searchGuide(guide, "EXTRACTION", 25)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || matches[0].Path != "tasks.perTypeAuto.document-extraction" || matches[0].Open != "altscore workflows-v2 schema-guide tasks document-extraction" {
		t.Fatalf("a matching key is one hit for its whole subtree, opened by its task command; got %d %+v", total, matches)
	}
	matches, total, _ = searchGuide(guide, "is_not_empty", 1)
	if total != 2 || len(matches) != 1 || matches[0].Open != "altscore workflows-v2 schema-guide conditions" {
		t.Fatalf("text hits count in total, the limit caps what prints; got %d %+v", total, matches)
	}
	if _, _, err := searchGuide(guide, "  ", 25); err == nil {
		t.Error("an empty word must be refused")
	}
}

func TestSchemaGuideCommandCompactsTasksAndResolvesTypes(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery)
		switch {
		case r.URL.Query().Get("full") == "true":
			_, _ = w.Write([]byte(`{"tasks": {"perTypeAuto": {"contact": {"channel": "email"}}}}`))
		case r.URL.Query().Get("type") != "":
			_, _ = w.Write([]byte(`{"tasks": {"perType": {}}}`))
		default:
			_, _ = w.Write([]byte(guideTasksFixture))
		}
	}))
	t.Cleanup(srv.Close)
	routeLoadClientTo(t, srv.URL)

	stdout, stderr, err := runListCmd(t, makeWfv2SchemaGuideCmd(), "tasks")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"document-extraction"`) || strings.Contains(stdout, "perTypeAuto") || !strings.Contains(stderr, "3 task types") {
		t.Errorf("'schema-guide tasks' must print the compact type index, got stdout %s stderr %s", stdout, stderr)
	}
	stdout, _, err = runListCmd(t, makeWfv2SchemaGuideCmd(), "tasks", "--full")
	if err != nil || !strings.Contains(stdout, "perTypeAuto") {
		t.Errorf("--full keeps the raw section, got %v %s", err, stdout)
	}

	_, stderr, err = runListCmd(t, makeWfv2SchemaGuideCmd(), "tasks", "compute")
	if err != nil || !strings.Contains(stderr, `read as "compute-variables"`) || seen[len(seen)-1] != "section=tasks&type=compute-variables" {
		t.Errorf("a near-miss type must be resolved before the request, got %v %q %v", err, stderr, seen)
	}

	stdout, stderr, err = runListCmd(t, makeWfv2SchemaGuideCmd(), "--search", "channel")
	if err != nil || !strings.Contains(stdout, "schema-guide tasks contact") || !strings.Contains(stderr, "1 match(es)") {
		t.Errorf("--search must print the hit and the command that opens it, got %v %s %s", err, stdout, stderr)
	}
	if _, _, err := runListCmd(t, makeWfv2SchemaGuideCmd(), "nodes", "--search", "x"); err == nil {
		t.Error("--search with a section argument must be refused")
	}
}
