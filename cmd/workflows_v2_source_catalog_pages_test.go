package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func resetSourceCatalogCaches() {
	sourceCatalogListCache = map[string][]map[string]any{}
	altdataSourceStatusCache = map[string]map[string]any{}
}

func catalogRows(from, n int) []map[string]any {
	rows := make([]map[string]any, 0, n)
	for i := from; i < from+n; i++ {
		rows = append(rows, map[string]any{"sourceId": fmt.Sprintf("AAA-PUB-%04d", i), "sourceVersion": "v1"})
	}
	return rows
}

// sources-status held 226 rows against per-page 200: a source on page 2 used to be "not found".
func TestSourceLookupReadsEveryCatalogPage(t *testing.T) {
	resetSourceCatalogCaches()
	defer resetSourceCatalogCaches()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.URL.Path == "/v2/workflows/external-sources-status" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		rows := catalogRows(0, 200)
		if page == 2 {
			rows = append(catalogRows(200, 25), map[string]any{"sourceId": "USA-PUB-0015", "sourceVersion": "v1"})
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer srv.Close()

	s, err := lookupAltdataSourceStatus(newTestClient(t, srv.URL), "USA-PUB-0015", "v1", false)
	if err != nil || s["sourceId"] != "USA-PUB-0015" {
		t.Fatalf("a source on catalog page 2 must be found, got %v, %v", s, err)
	}
	if got := len(sourceCatalogListCache["/v2/workflows/sources-status?per-page=200"]); got != 226 {
		t.Fatalf("expected all 226 rows cached, got %d", got)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Fatalf("expected 2 page requests (the match is on the microservice catalog), got %d", got)
	}
}

func TestSourceCatalogStopsWhenTheBackendIgnoresPage(t *testing.T) {
	resetSourceCatalogCaches()
	defer resetSourceCatalogCaches()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_ = json.NewEncoder(w).Encode(catalogRows(0, 200))
	}))
	defer srv.Close()

	rows, err := fetchSourceCatalog(newTestClient(t, srv.URL), "/v2/workflows/sources-status?per-page=200")
	if err != nil || len(rows) != 200 {
		t.Fatalf("expected the 200 rows once, got %d, %v", len(rows), err)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Fatalf("a repeated page must end the walk after one repeat, got %d requests", got)
	}
}

func TestSourceCatalogWithoutPerPageIsOneRequest(t *testing.T) {
	resetSourceCatalogCaches()
	defer resetSourceCatalogCaches()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.URL.Query().Get("page") != "" {
			t.Errorf("a path without per-page must not be paged, got %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(catalogRows(0, 3))
	}))
	defer srv.Close()

	rows, err := fetchSourceCatalog(newTestClient(t, srv.URL), "/v2/workflows/external-sources-status")
	if err != nil || len(rows) != 3 || atomic.LoadInt32(&requests) != 1 {
		t.Fatalf("expected one request and 3 rows, got %d rows, %d requests, %v", len(rows), requests, err)
	}
}
