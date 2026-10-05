package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// fakeListBackend serves sources-status and /v2/workflows from fixed rows, filtered and
// paged the way borrower-central does (default per-page 10), and records every query.
type fakeListBackend struct {
	*httptest.Server
	mu        sync.Mutex
	queries   []url.Values
	sources   []map[string]any
	workflows []map[string]any
}

func newFakeListBackend(t *testing.T, sources, workflows []map[string]any) *fakeListBackend {
	t.Helper()
	resetSourceCatalogCaches()
	t.Cleanup(resetSourceCatalogCaches)
	f := &fakeListBackend{sources: sources, workflows: workflows}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.queries = append(f.queries, q)
		f.mu.Unlock()
		var rows []map[string]any
		switch r.URL.Path {
		case "/v2/workflows/sources-status":
			rows = filterSourceRows(f.sources, q)
		case "/v2/workflows":
			for _, row := range f.workflows {
				if s := q.Get("status"); s == "" || row["status"] == s {
					rows = append(rows, row)
				}
			}
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(fakePageBody(rows, q))
	}))
	t.Cleanup(f.Close)
	return f
}

func fakePageBody(rows []map[string]any, q url.Values) []byte {
	page, perPage := 1, 10
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 0 {
		page = n
	}
	if n, err := strconv.Atoi(q.Get("per-page")); err == nil && n > 0 {
		perPage = n
	}
	from := min((page-1)*perPage, len(rows))
	body, _ := json.Marshal(append([]map[string]any{}, rows[from:min(from+perPage, len(rows))]...))
	return body
}

// country matches the sourceId's prefix and never an INT row, as the live catalog does.
func filterSourceRows(rows []map[string]any, q url.Values) []map[string]any {
	var out []map[string]any
	for _, row := range rows {
		id, _ := row["sourceId"].(string)
		prefix, _, _ := strings.Cut(id, "-")
		if c := q.Get("country"); c != "" && !strings.EqualFold(prefix, c) {
			continue
		}
		if s := q.Get("status"); s != "" && row["status"] != s {
			continue
		}
		if s := q.Get("search"); s != "" && !strings.Contains(id, s) {
			continue
		}
		out = append(out, row)
	}
	return out
}

func (f *fakeListBackend) requests() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.queries...)
}

func fakeSourceRow(id, version, status string, inputs ...[2]string) map[string]any {
	fields := []any{}
	for _, in := range inputs {
		fields = append(fields, map[string]any{"field": in[0], "required": in[1], "type": "string"})
	}
	return map[string]any{
		"sourceId": id, "sourceVersion": version, "status": status, "enabled": true, "timeout": 60,
		"country": nil, "groupId": nil, "inputFields": fields,
		"name":         map[string]any{"en": "Name " + id, "es": "Nombre " + id},
		"description":  map[string]any{"en": "What " + id + " returns", "es": ""},
		"stats":        map[string]any{"errorRateLast4Hours": 0.5},
		"outputSchema": map[string]any{id: map[string]any{"type": "object", "properties": map[string]any{"score": map[string]any{"type": "number"}}}},
	}
}

// 230 rows over two pages of 200: 150 ECU (every tenth down), 30 INT, 50 USA.
func fakeSourceCatalog() []map[string]any {
	var rows []map[string]any
	for i := 0; i < 150; i++ {
		status := "active"
		if i%10 == 0 {
			status = "down"
		}
		rows = append(rows, fakeSourceRow(fmt.Sprintf("ECU-PUB-%04d", i), "v1", status,
			[2]string{"personId", "REQUIRED"}, [2]string{"taxId", "AT_LEAST_ONE"}, [2]string{"name", "OPTIONAL"}))
	}
	for i := 0; i < 30; i++ {
		rows = append(rows, fakeSourceRow(fmt.Sprintf("INT-PUB-%04d", i), "v1", "active", [2]string{"fullName", "REQUIRED"}))
	}
	for i := 0; i < 50; i++ {
		rows = append(rows, fakeSourceRow(fmt.Sprintf("USA-PUB-%04d", i), "v2", "active"))
	}
	return rows
}

// runListCmd runs a freshly built command against the fake and returns stdout and stderr.
func runListCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, string, error) {
	t.Helper()
	var stderr bytes.Buffer
	cmd.SetArgs(append([]string{}, args...)) // nil args make cobra read the test binary's os.Args
	cmd.SetErr(&stderr)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	stdout := captureStdoutOf(t, func() error { return cmd.Execute() })
	return stdout.text, stderr.String(), stdout.err
}

type capturedStdout struct {
	text string
	err  error
}

func captureStdoutOf(t *testing.T, fn func() error) capturedStdout {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	read := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		read <- b
	}()
	runErr := fn()
	w.Close()
	os.Stdout = orig
	return capturedStdout{text: string(<-read), err: runErr}
}

// What the commands printed before compact rows: the backend's body re-indented.
func oldRawOutput(t *testing.T, body []byte) string {
	t.Helper()
	return captureStdoutOf(t, func() error { return output.RawJSON(body) }).text
}

func decodeRows(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("stdout must be one JSON array: %v\n%s", err, stdout)
	}
	return rows
}

func TestSourcesStatusCompactRowsAcrossPages(t *testing.T) {
	f := newFakeListBackend(t, fakeSourceCatalog(), nil)
	routeLoadClientTo(t, f.URL)

	stdout, stderr, err := runListCmd(t, makeWfv2SourcesStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeRows(t, stdout)
	if len(rows) != 230 {
		t.Fatalf("expected every row of both pages, got %d", len(rows))
	}
	if lines := strings.Count(stdout, "\n"); lines != 232 {
		t.Errorf("expected one row per line inside [ ], got %d lines", lines)
	}
	first := rows[0]
	want := map[string]any{
		"sourceId": "ECU-PUB-0000", "version": "v1", "name": "Name ECU-PUB-0000", "status": "down", "enabled": true,
		"requiredInputs": map[string]any{"REQUIRED": []any{"personId"}, "AT_LEAST_ONE": []any{"taxId"}},
	}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("compact row:\n got %v\nwant %v", first, want)
	}
	if usa := rows[229]; !reflect.DeepEqual(usa["requiredInputs"], map[string]any{}) {
		t.Errorf("a source without required inputs must say so with {}, got %v", usa["requiredInputs"])
	}
	if !strings.Contains(stderr, "# 230 of 230 sources, every page read (compact rows; --full adds outputSchema)") {
		t.Errorf("stderr must carry the count line, got %q", stderr)
	}
	var pages []string
	for _, q := range f.requests() {
		pages = append(pages, q.Get("page")+"/"+q.Get("per-page"))
	}
	if strings.Join(pages, " ") != "1/200 2/200" {
		t.Errorf("expected pages 1 and 2 at per-page 200, got %v", pages)
	}
}

func TestSourcesStatusFullIsTheOldRawOutput(t *testing.T) {
	catalog := fakeSourceCatalog()
	f := newFakeListBackend(t, catalog, nil)
	routeLoadClientTo(t, f.URL)

	stdout, stderr, err := runListCmd(t, makeWfv2SourcesStatusCmd(), "--full", "--page", "1", "--per-page", "10")
	if err != nil {
		t.Fatal(err)
	}
	if want := oldRawOutput(t, fakePageBody(catalog, url.Values{"page": {"1"}, "per-page": {"10"}})); stdout != want {
		t.Errorf("--full --page must print exactly what the command printed before")
	}
	if !strings.Contains(stderr, "# 10 sources on page 1; omit --page to read every page") {
		t.Errorf("a single page must say so on stderr, got %q", stderr)
	}

	stdout, stderr, err = runListCmd(t, makeWfv2SourcesStatusCmd(), "--full")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(catalog)
	if want := oldRawOutput(t, body); stdout != want {
		t.Errorf("--full must print the raw rows of every page as one array, as the old command did for one page")
	}
	if !strings.Contains(stderr, "# 230 of 230 sources, every page read (raw rows)") {
		t.Errorf("stderr must carry the count line, got %q", stderr)
	}
}

func TestSourcesStatusFilterNoteSaysWhatWasHidden(t *testing.T) {
	f := newFakeListBackend(t, fakeSourceCatalog(), nil)
	routeLoadClientTo(t, f.URL)

	stdout, stderr, err := runListCmd(t, makeWfv2SourcesStatusCmd(), "--country", "ECU", "--status", "active")
	if err != nil {
		t.Fatal(err)
	}
	if rows := decodeRows(t, stdout); len(rows) != 135 {
		t.Fatalf("expected the 135 active ECU rows, got %d", len(rows))
	}
	for _, want := range []string{
		"# 135 of 230 sources, every page read",
		"# country=ECU status=active hid 95: by status 80 active, 15 down; by sourceId prefix 50 USA, 30 INT, 15 ECU",
		"# 30 of them are INT (international) sources, which no country filter returns",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr must contain %q, got:\n%s", want, stderr)
		}
	}

	_, stderr, err = runListCmd(t, makeWfv2SourcesStatusCmd(), "--status", "active")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "# status=active hid 15: by status 15 down\n") || strings.Contains(stderr, "INT") {
		t.Errorf("a status filter alone must count by status only, got:\n%s", stderr)
	}

	_, stderr, err = runListCmd(t, makeWfv2SourcesStatusCmd(), "--search", "USA")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "# 50 of 50 sources") || strings.Contains(stderr, "hid") {
		t.Errorf("search narrows without a hidden-rows note, got:\n%s", stderr)
	}
}

func TestAltdataSourcesSharesTheCompactListing(t *testing.T) {
	f := newFakeListBackend(t, fakeSourceCatalog(), nil)
	routeLoadClientTo(t, f.URL)

	if _, _, err := runListCmd(t, makeAltdataSourcesCmd(), "--filter", "country"); err == nil || len(f.requests()) != 0 {
		t.Fatalf("a filter without = must fail before any request, got %v after %d", err, len(f.requests()))
	}

	stdout, stderr, err := runListCmd(t, makeAltdataSourcesCmd(), "--filter", "country=ECU", "--filter", "groupId=bureau")
	if err != nil {
		t.Fatal(err)
	}
	if rows := decodeRows(t, stdout); len(rows) != 150 || rows[0]["version"] != "v1" {
		t.Fatalf("expected the 150 compact ECU rows, got %d", len(rows))
	}
	for _, want := range []string{`no "groupId" filter`, "# 150 of 230 sources", "# country=ECU hid 80: by sourceId prefix 50 USA, 30 INT"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr must contain %q, got:\n%s", want, stderr)
		}
	}
}

// "--filter sourceId=X" used to be ignored and answered with the whole catalog.
func TestSourceIDFilterIsReadAsSearch(t *testing.T) {
	f := newFakeListBackend(t, fakeSourceCatalog(), nil)
	routeLoadClientTo(t, f.URL)

	for _, cmd := range []*cobra.Command{makeAltdataSourcesCmd(), makeWfv2SourcesStatusCmd()} {
		stdout, stderr, err := runListCmd(t, cmd, "--filter", "sourceId=ECU-PUB-0001")
		if err != nil {
			t.Fatal(err)
		}
		rows := decodeRows(t, stdout)
		if len(rows) != 1 || rows[0]["sourceId"] != "ECU-PUB-0001" {
			t.Fatalf("%s: expected only ECU-PUB-0001, got %d rows", cmd.Name(), len(rows))
		}
		if !strings.Contains(stderr, "read as search=ECU-PUB-0001") || strings.Contains(stderr, "warning") {
			t.Errorf("%s: stderr must say the filter was read as a search, got:\n%s", cmd.Name(), stderr)
		}
	}
}

func TestSourcesStatusTipOnlyWhenUnfiltered(t *testing.T) {
	f := newFakeListBackend(t, fakeSourceCatalog(), nil)
	routeLoadClientTo(t, f.URL)

	_, stderr, err := runListCmd(t, makeWfv2SourcesStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "--country <ISO3> or --search <word>") {
		t.Errorf("an unfiltered listing must say how to narrow it, got:\n%s", stderr)
	}
	_, stderr, err = runListCmd(t, makeWfv2SourcesStatusCmd(), "--country", "ECU")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "narrows it") {
		t.Errorf("a filtered listing must not carry the tip, got:\n%s", stderr)
	}
}

func fakeWorkflowRows(n int) []map[string]any {
	rows := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		status := "ACTIVE"
		if i%2 == 1 {
			status = "DRAFT"
		}
		rows = append(rows, map[string]any{
			"id": fmt.Sprintf("wf-%03d", i), "alias": fmt.Sprintf("flow-%d", i/2), "label": fmt.Sprintf("Flow %d", i/2),
			"status": status, "version": i%2 + 1, "isLatest": i%2 == 1, "updatedAt": "2026-10-01T00:00:00",
			"nodes":  []any{map[string]any{"nodeId": "start", "type": "start"}},
			"edges":  []any{},
			"config": map[string]any{"taskSnapshots": map[string]any{"big": strings.Repeat("x", 64)}},
		})
	}
	return rows
}

// The registered command's def; root.go wires wfv2CompactList into it.
func wfv2ListDef() ResourceDef {
	return ResourceDef{Name: "workflows-v2", BasePath: "/v2/workflows", Module: "borrower_central", CompactList: wfv2CompactList}
}

func TestWorkflowsV2ListCompactVersusFull(t *testing.T) {
	workflows := fakeWorkflowRows(105)
	f := newFakeListBackend(t, nil, workflows)
	routeLoadClientTo(t, f.URL)

	stdout, stderr, err := runListCmd(t, makeListCmd(wfv2ListDef()))
	if err != nil {
		t.Fatal(err)
	}
	rows := decodeRows(t, stdout)
	if len(rows) != 105 {
		t.Fatalf("expected both pages at the config's per-page 100, got %d rows", len(rows))
	}
	want := map[string]any{"id": "wf-001", "alias": "flow-0", "label": "Flow 0", "status": "DRAFT", "version": float64(2), "isLatest": true, "updatedAt": "2026-10-01T00:00:00"}
	if !reflect.DeepEqual(rows[1], want) {
		t.Errorf("compact row:\n got %v\nwant %v", rows[1], want)
	}
	if !strings.Contains(stderr, "# 105 workflows-v2, every page read (compact rows; --full prints the raw items)") {
		t.Errorf("stderr must carry the count line, got %q", stderr)
	}

	stdout, _, err = runListCmd(t, makeListCmd(wfv2ListDef()), "--full")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(workflows)
	if stdout != oldRawOutput(t, body) {
		t.Error("--full must print every raw item, as the old command printed one page")
	}

	stdout, stderr, err = runListCmd(t, makeListCmd(wfv2ListDef()), "--full", "--page", "1")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != oldRawOutput(t, fakePageBody(workflows, url.Values{"page": {"1"}, "per-page": {"100"}})) || stderr != "" {
		t.Error("--full --page must be byte-identical to the old command, stderr included")
	}

	before := len(f.requests())
	stdout, _, err = runListCmd(t, makeListCmd(wfv2ListDef()), "--filter", "status=ACTIVE")
	if err != nil {
		t.Fatal(err)
	}
	if rows := decodeRows(t, stdout); len(rows) != 53 || rows[0]["status"] != "ACTIVE" {
		t.Errorf("--filter must still narrow the compact rows, got %d", len(rows))
	}
	for _, q := range f.requests()[before:] {
		if q.Get("status") != "ACTIVE" {
			t.Errorf("every page request must carry the filter, got %v", q)
		}
	}
}

func TestCompactWorkflowRowReadsAGroupedItem(t *testing.T) {
	got := compactWorkflowRow(map[string]any{
		"primaryId": "p-1", "workflowAlias": "flow", "label": "Flow", "status": "ACTIVE", "version": 3,
		"isLatest": true, "lastModified": "2026-10-01", "variables": map[string]any{"a": 1},
	})
	want := compactWorkflow{ID: "p-1", Alias: "flow", Label: "Flow", Status: "ACTIVE", Version: 3, IsLatest: true, UpdatedAt: "2026-10-01"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestWorkflowsV2ListIsWiredCompact(t *testing.T) {
	list, _, err := rootCmd.Find([]string{"workflows-v2", "list"})
	if err != nil || list.Flags().Lookup("full") == nil {
		t.Fatalf("workflows-v2 list must offer --full, got %v", err)
	}
	if !strings.Contains(list.Long, "compact row") {
		t.Errorf("workflows-v2 list help must describe the compact default: %q", list.Long)
	}
	other, _, _ := rootCmd.Find([]string{"evaluators", "list"})
	if other != nil && other.Flags().Lookup("full") != nil {
		t.Error("a resource without CompactList keeps the plain list")
	}
}

func TestPrintCompactRowsEmptyIsAnArray(t *testing.T) {
	out := captureStdoutOf(t, func() error { return printCompactRows([]compactWorkflow{}) })
	if out.err != nil || out.text != "[]\n" {
		t.Errorf("an empty listing must print [], got %q, %v", out.text, out.err)
	}
}
