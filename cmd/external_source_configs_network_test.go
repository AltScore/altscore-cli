package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNetworkBody(t *testing.T) {
	cases := []struct {
		direct  bool
		country string
		want    string
		wantErr bool
	}{
		{country: "ec", want: `{"country":"EC","mode":"altscore_proxy"}`},
		{country: " ANY ", want: `{"country":"ANY","mode":"altscore_proxy"}`},
		{direct: true, want: `{"mode":"direct"}`},
		{direct: true, country: "EC", wantErr: true},
		{wantErr: true},
	}
	for _, tc := range cases {
		got, err := networkBody(tc.direct, tc.country)
		if tc.wantErr {
			if err == nil {
				t.Errorf("networkBody(%v, %q): want error, got %s", tc.direct, tc.country, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("networkBody(%v, %q): %v", tc.direct, tc.country, err)
		}
		if string(got) != tc.want {
			t.Errorf("networkBody(%v, %q) = %s, want %s", tc.direct, tc.country, got, tc.want)
		}
	}
}

// set-network must hit the dedicated route, never PUT /{id}: that one replaces
// settings whole and would wipe the source's URL and auth.
func TestSetExternalSourceNetwork_UsesTheNetworkRoute(t *testing.T) {
	var method, path string
	var sent map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sent)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cfg-1","settings":{"network":{"mode":"altscore_proxy","country":"EC"}}}`))
	}))
	defer srv.Close()

	body, err := networkBody(false, "EC")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setExternalSourceNetwork(newTestClient(t, srv.URL), "cfg-1", body); err != nil {
		t.Fatal(err)
	}
	if method != "PUT" || path != "/v1/external-source-configs/cfg-1/network" {
		t.Errorf("sent %s %s, want PUT /v1/external-source-configs/cfg-1/network", method, path)
	}
	if sent["mode"] != "altscore_proxy" || sent["country"] != "EC" {
		t.Errorf("body = %v", sent)
	}
}
