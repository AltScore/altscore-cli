package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const lateSourceID = "USA-PUB-0015"

// fakeAltdataBackend serves a 226-row sources-status catalog at per-page 200, so lateSourceID
// (two versions) sits on page 2, plus the data dictionary. It counts every request by path.
type fakeAltdataBackend struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
	urls []string
}

func newFakeAltdataBackend(t *testing.T) *fakeAltdataBackend {
	t.Helper()
	resetSourceCatalogCaches()
	t.Cleanup(resetSourceCatalogCaches)
	f := &fakeAltdataBackend{hits: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.Path]++
		f.urls = append(f.urls, r.URL.RequestURI())
		f.mu.Unlock()
		q := r.URL.Query()
		switch r.URL.Path {
		case "/v2/workflows/sources-status":
			rows := catalogRows(0, 200)
			if q.Get("page") == "2" {
				rows = catalogRows(200, 24)
				for _, v := range []string{"v1", "v2"} {
					rows = append(rows, map[string]any{
						"sourceId": lateSourceID, "sourceVersion": v, "enabled": true, "name": "Late source",
						"outputSchema": map[string]any{lateSourceID: map[string]any{
							"type":       "object",
							"properties": map[string]any{"score": map[string]any{}, "tradeLines": map[string]any{}},
						}},
					})
				}
			}
			_ = json.NewEncoder(w).Encode(rows)
		case "/v1/documentation/data-dictionary":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"sourceId": q.Get("sourceId"), "version": q.Get("version"), "field": "score"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAltdataBackend) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.hits {
		n += c
	}
	return n
}

func (f *fakeAltdataBackend) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

// Points loadClient at the fake backend, so a command run through cobra reaches it.
func routeLoadClientTo(t *testing.T, baseURL string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ALTSCORE_PROFILE", "")
	t.Setenv("ALTSCORE_ENVIRONMENT", "")
	dir := filepath.Join(home, ".config", "altscore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "[profiles.test]\nenvironment = \"staging\"\naccess_token = \"test-token\"\ntenant_id = \"tenant\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	prevProfile, prevBaseURLs := flagProfile, flagBaseURLs
	flagProfile, flagBaseURLs = "test", []string{"borrower_central=" + baseURL}
	t.Cleanup(func() { flagProfile, flagBaseURLs = prevProfile, prevBaseURLs })
}

func TestAltdataDescribeFindsASourceOnCatalogPage2(t *testing.T) {
	f := newFakeAltdataBackend(t)
	summary, err := describeAltdataSource(newTestClient(t, f.URL), lateSourceID, "")
	if err != nil {
		t.Fatalf("a source on catalog page 2 must be found, got %v", err)
	}
	if summary["latestVersion"] != "v2" || summary["selectedVersion"] != "v2" {
		t.Errorf("expected v2 selected, got latest=%v selected=%v", summary["latestVersion"], summary["selectedVersion"])
	}
	if keys, _ := summary["outputKeys"].([]string); strings.Join(keys, ",") != "score,tradeLines" {
		t.Errorf("expected outputKeys [score tradeLines], got %v", summary["outputKeys"])
	}
	if got := f.count("/v2/workflows/sources-status"); got != 2 {
		t.Errorf("expected both catalog pages read, got %d requests", got)
	}
}

func TestAltdataDescribeNextDoesNotAdvertiseSample(t *testing.T) {
	f := newFakeAltdataBackend(t)
	summary, err := describeAltdataSource(newTestClient(t, f.URL), lateSourceID, "v1")
	if err != nil {
		t.Fatal(err)
	}
	next, _ := summary["next"].(map[string]any)
	if _, ok := next["sample"]; ok {
		t.Errorf("next must not advertise sample, the backend 405s it: %v", next)
	}
	if next["dictionary"] != "altscore altdata dictionary "+lateSourceID+" v1" {
		t.Errorf("next.dictionary must name the selected version, got %v", next["dictionary"])
	}
}

func TestAltdataDictionaryFindsASourceOnCatalogPage2(t *testing.T) {
	f := newFakeAltdataBackend(t)
	data, err := fetchAltdataDictionary(newTestClient(t, f.URL), lateSourceID, "", io.Discard)
	if err != nil {
		t.Fatalf("dictionary must resolve a source on catalog page 2, got %v", err)
	}
	if !strings.Contains(string(data), `"version":"v2"`) {
		t.Errorf("expected the dictionary for the latest version v2, got %s", data)
	}
	if got := f.count("/v2/workflows/sources-status"); got != 2 {
		t.Errorf("expected both catalog pages read, got %d requests", got)
	}
}

func TestAltdataUnknownSourceErrorsWithClosestIDs(t *testing.T) {
	f := newFakeAltdataBackend(t)
	c := newTestClient(t, f.URL)

	_, err := describeAltdataSource(c, "usa-pub-0016", "")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("an unknown id must error, got %v", err)
	}
	if !strings.Contains(err.Error(), "Did you mean "+lateSourceID) {
		t.Errorf("expected %s as the closest id, got %v", lateSourceID, err)
	}

	_, err = fetchAltdataDictionary(c, "NOPE", "", io.Discard)
	if err == nil || strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("an id unlike every catalog id must error with no suggestion, got %v", err)
	}
	if got := f.count("/v1/documentation/data-dictionary"); got != 0 {
		t.Errorf("an unknown id must not reach the dictionary endpoint, got %d requests", got)
	}
}

func TestClosestSourceIDsCapsAtThree(t *testing.T) {
	near := closestSourceIDs("AAA-PUB-00", catalogRows(0, 50))
	if len(near) != 3 || near[0] != "AAA-PUB-0000" {
		t.Errorf("expected the first three prefix matches, got %v", near)
	}
}

// A prefix admits a candidate but never outranks a nearer id: XYZ-ALT-009 is one edit from 0009.
func TestClosestSourceIDsRanksByDistanceNotPrefix(t *testing.T) {
	var rows []map[string]any
	for _, id := range []string{"XYZ-ALT-0090", "XYZ-ALT-0091", "XYZ-ALT-0092", "XYZ-ALT-0009"} {
		rows = append(rows, map[string]any{"sourceId": id})
	}
	if near := closestSourceIDs("XYZ-ALT-009", rows); len(near) == 0 || near[0] != "XYZ-ALT-0009" {
		t.Errorf("expected XYZ-ALT-0009 first, got %v", near)
	}
}

func TestAltdataSampleFailsBeforeAnyRequest(t *testing.T) {
	f := newFakeAltdataBackend(t)
	routeLoadClientTo(t, f.URL)

	// Control: the same wiring does reach the fake, so a zero count below means something.
	dict := makeAltdataDictionaryCmd()
	dict.SetArgs([]string{"NOPE"})
	dict.SilenceUsage, dict.SilenceErrors = true, true
	if err := dict.Execute(); err == nil || f.total() == 0 {
		t.Fatalf("control: dictionary of an unknown id must reach the fake and error, got %v after %d requests", err, f.total())
	}
	before := f.total()

	sample := makeAltdataSampleCmd()
	if !sample.Hidden {
		t.Error("sample must be hidden")
	}
	sample.SetArgs([]string{lateSourceID, "v1"})
	sample.SilenceUsage, sample.SilenceErrors = true, true
	err := sample.Execute()
	if err == nil {
		t.Fatal("sample must fail")
	}
	for _, want := range []string{"altscore altdata describe " + lateSourceID, "outputKeys", "altscore altdata dictionary " + lateSourceID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("sample's error must name %q, got %v", want, err)
		}
	}
	if got := f.total() - before; got != 0 {
		t.Errorf("sample must fail before any request, the fake saw %d: %v", got, f.urls[before:])
	}
}

func TestAltdataSampleIsNotAdvertised(t *testing.T) {
	var group string
	for _, c := range rootCmd.Commands() {
		if c.Name() == "altdata" {
			group = c.Long
			for _, sub := range c.Commands() {
				if sub.Name() == "sample" && !sub.Hidden {
					t.Error("altdata sample must be hidden")
				}
			}
		}
	}
	if strings.Contains(group, "sample") {
		t.Errorf("altdata help must not list sample: %q", group)
	}
}
