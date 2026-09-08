package cmd

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The cache sits behind every vocabulary fetcher; fetchServerTaskTypes is the
// representative. $HOME is redirected so the test never touches the user's
// real ~/.config/altscore.

func vocabularyServer(t *testing.T, status int, body string, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/meta/workflows-v2-schema" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		atomic.AddInt32(calls, 1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestVocabularyCache_SecondLookupReadsDisk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls int32
	srv := vocabularyServer(t, http.StatusOK, `{"taskTypes":{"values":["http","brand-new-node"]}}`, &calls)
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	for i := 0; i < 3; i++ {
		got := fetchServerTaskTypes(c)
		if !got["brand-new-node"] {
			t.Fatalf("lookup %d: live type missing from %v", i, got)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("expected exactly 1 GET across 3 lookups, got %d", n)
	}
}

func TestVocabularyCache_ExpiresAfterTTL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls int32
	srv := vocabularyServer(t, http.StatusOK, `{"taskTypes":{"values":["http"]}}`, &calls)
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	base := time.Now()
	vocabularyNow = func() time.Time { return base }
	defer func() { vocabularyNow = time.Now }()

	fetchServerTaskTypes(c)
	vocabularyNow = func() time.Time { return base.Add(vocabularyCacheTTL - time.Minute) }
	fetchServerTaskTypes(c)
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("a fresh entry must be reused, got %d GETs", n)
	}
	vocabularyNow = func() time.Time { return base.Add(vocabularyCacheTTL + time.Minute) }
	fetchServerTaskTypes(c)
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("an expired entry must refetch, got %d GETs", n)
	}
}

// A failed fetch caches nothing: the next lookup tries the backend again, and
// a stale entry is never served as a fallback (that would reject values the
// backend has since added).
func TestVocabularyCache_FailureIsNotCached(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls int32
	srv := vocabularyServer(t, http.StatusServiceUnavailable, "", &calls)
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	if got := fetchServerTaskTypes(c); got != nil {
		t.Fatalf("a 5xx must yield nil (compiled-in fallback), got %v", got)
	}
	fetchServerTaskTypes(c)
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("failures must not be cached, got %d GETs", n)
	}
}

// Entries are keyed by the backend the client will hit, so two backends never
// read each other's vocabulary.
func TestVocabularyCache_KeyedByBackendURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var callsA, callsB int32
	srvA := vocabularyServer(t, http.StatusOK, `{"taskTypes":{"values":["only-on-a"]}}`, &callsA)
	defer srvA.Close()
	srvB := vocabularyServer(t, http.StatusOK, `{"taskTypes":{"values":["only-on-b"]}}`, &callsB)
	defer srvB.Close()

	a := fetchServerTaskTypes(newTestClient(t, srvA.URL))
	b := fetchServerTaskTypes(newTestClient(t, srvB.URL))
	if !a["only-on-a"] || a["only-on-b"] || !b["only-on-b"] || b["only-on-a"] {
		t.Errorf("vocabularies crossed backends: a=%v b=%v", a, b)
	}
	if callsA != 1 || callsB != 1 {
		t.Errorf("each backend must be fetched once, got a=%d b=%d", callsA, callsB)
	}
}

// The taskTypes section carries a `deprecated` array alongside `values`. It is
// UNIONED into the compiled-in deprecatedTaskTypes map rather than replacing
// it: the compiled-in entries are what makes the refusal work offline, and the
// live half lets BC retire a type without a CLI release. A type BC reports as
// deprecated never comes back as authorable, even while it is still listed
// under `values` (which is BC's actual shape -- it keeps parsing them).
func TestFetchServerTaskTypes_UnionsTheServerDeprecatedList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls int32
	srv := vocabularyServer(t, http.StatusOK,
		`{"taskTypes":{"values":["http","legacy-thing"],"deprecated":["legacy-thing","soap"]}}`, &calls)
	defer srv.Close()
	defer delete(deprecatedTaskTypes, "legacy-thing")

	got := fetchServerTaskTypes(newTestClient(t, srv.URL))
	if !got["http"] {
		t.Errorf("a current type must stay authorable, got %v", got)
	}
	if got["legacy-thing"] {
		t.Errorf("a server-deprecated type must not come back as authorable, got %v", got)
	}
	if !deprecatedTaskTypes["legacy-thing"] {
		t.Error("the server's deprecated list must union into deprecatedTaskTypes")
	}
	if !deprecatedTaskTypes["soap"] {
		t.Error("compiled-in deprecated entries must survive the union")
	}
}

// An older backend answers without the key. That must leave the compiled-in
// list untouched and still return the live values -- the offline refusal is
// the compiled-in map's job, not the payload's.
func TestFetchServerTaskTypes_MissingDeprecatedKeyIsHarmless(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var calls int32
	srv := vocabularyServer(t, http.StatusOK, `{"taskTypes":{"values":["http","end"]}}`, &calls)
	defer srv.Close()

	got := fetchServerTaskTypes(newTestClient(t, srv.URL))
	if !got["http"] || !got["end"] {
		t.Errorf("values must still parse without a deprecated key, got %v", got)
	}
	for _, typ := range retiredTaskTypes {
		if !deprecatedTaskTypes[typ] {
			t.Errorf("%q dropped out of deprecatedTaskTypes after a payload with no deprecated key", typ)
		}
	}
}
