package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSchemaRedirectsV2ShapesToTheGuideOnlyOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/meta/schemas":
			if r.URL.Query().Get("resource") == "nodes" {
				_, _ = w.Write([]byte(`{"resource":"nodes","fields":["registry"]}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Unknown resource: ` + r.URL.Query().Get("resource") + `"}`))
		case "/v1/meta/workflows-v2-schema":
			_, _ = w.Write([]byte(guideTasksFixture))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	routeLoadClientTo(t, srv.URL)

	stdout, stderr, err := runListCmd(t, makeSchemaCmd(), "tasks-v2")
	if err != nil {
		t.Fatalf("schema tasks-v2 must answer from the guide, got %v", err)
	}
	if !strings.Contains(stdout, `"document-extraction"`) || !strings.Contains(stderr, "workflows-v2 schema-guide tasks <type>") || strings.Contains(stderr, "404") {
		t.Errorf("expected the task type index and the right command, got stdout %s stderr %s", stdout, stderr)
	}

	stdout, _, err = runListCmd(t, makeSchemaCmd(), "nodes")
	if err != nil || !strings.Contains(stdout, `"registry"`) {
		t.Errorf("a name the registry knows must keep the registry answer, got %v %s", err, stdout)
	}

	if _, _, err := runListCmd(t, makeSchemaCmd(), "borrowerz"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("an unknown CRUD resource must stay a 404, got %v", err)
	}
}
