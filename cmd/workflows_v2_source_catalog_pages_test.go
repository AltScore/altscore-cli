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

// A walk that hits the page cap must fail, not cache a partial catalog that reads as "not found".
func TestSourceCatalogPastThePageCapIsAnErrorNotATruncation(t *testing.T) {
	resetSourceCatalogCaches()
	defer resetSourceCatalogCaches()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		_ = json.NewEncoder(w).Encode(catalogRows(page*200, 200)) // always a full, new page
	}))
	defer srv.Close()

	path := "/v2/workflows/sources-status?per-page=200"
	if rows, err := fetchSourceCatalog(newTestClient(t, srv.URL), path); err == nil {
		t.Fatalf("expected an error past %d pages, got %d rows", maxCatalogPages, len(rows))
	}
	if _, cached := sourceCatalogListCache[path]; cached {
		t.Fatal("a truncated catalog must not be cached")
	}
}

// Only a whole repeated page means the backend ignored `page`; a coinciding first row does not.
func TestSourceCatalogKeepsAPageWhoseFirstRowCoincides(t *testing.T) {
	resetSourceCatalogCaches()
	defer resetSourceCatalogCaches()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := catalogRows(0, 200)
		if r.URL.Query().Get("page") == "2" {
			rows = append(catalogRows(0, 1), map[string]any{"sourceId": "USA-PUB-0015", "sourceVersion": "v1"})
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer srv.Close()

	rows, err := fetchSourceCatalog(newTestClient(t, srv.URL), "/v2/workflows/sources-status?per-page=200")
	if err != nil || len(rows) != 202 || rows[201]["sourceId"] != "USA-PUB-0015" {
		t.Fatalf("page 2 must be kept whole, got %d rows, %v", len(rows), err)
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
