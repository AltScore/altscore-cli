package cmd

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/AltScore/altscore-cli/internal/config"
)

func TestLoginEnvironment(t *testing.T) {
	cases := []struct {
		name, flag, existing, want string
	}{
		{"new profile defaults to production", "", "", "production"},
		{"flag picks the environment", "staging", "", "staging"},
		{"re-login keeps the stored environment", "", "sandbox", "sandbox"},
		{"flag wins over the stored environment", "production", "staging", "production"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := loginEnvironment(tc.flag, tc.existing); got != tc.want {
				t.Errorf("loginEnvironment(%q, %q) = %q, want %q", tc.flag, tc.existing, got, tc.want)
			}
		})
	}
}

func TestParseTenantResponse(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		// The shape production returns: Frontegg's tenant record, as an array.
		{"frontegg array", `[{"_id":"x","tenantId":"pokemon-0cc5","name":"Pokemon","isReseller":false}]`, "pokemon-0cc5"},
		{"first non-empty entry", `[{"tenantId":""},{"tenantId":"la-fabril-91bd"}]`, "la-fabril-91bd"},
		{"empty array", `[]`, ""},
		{"bare object", `{"tenantId":"demo-ec-90e2"}`, "demo-ec-90e2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTenantResponse([]byte(tc.body))
			if err != nil || got != tc.want {
				t.Errorf("parseTenantResponse(%s) = (%q, %v), want (%q, nil)", tc.body, got, err, tc.want)
			}
		})
	}

	if _, err := parseTenantResponse([]byte(`"pokemon"`)); err == nil {
		t.Error("parseTenantResponse of a JSON string should fail")
	}
}

func TestTenantFromToken(t *testing.T) {
	jwt := func(payload string) string {
		return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
	}
	cases := []struct {
		name, token, want string
	}{
		{"tenantId claim", jwt(`{"sub":"x","tenantId":"pokemon-0cc5","type":"tenantApiToken"}`), "pokemon-0cc5"},
		{"padded payload", strings.Replace(jwt(`{"tenantId":"ab"}`), ".sig", "==.sig", 1), "ab"},
		{"no claim", jwt(`{"sub":"x"}`), ""},
		{"not a jwt", "opaque-token", ""},
		{"garbage payload", "a.!!!.c", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tenantFromToken(tc.token); got != tc.want {
				t.Errorf("tenantFromToken(%q) = %q, want %q", tc.token, got, tc.want)
			}
		})
	}
}

func TestResolveLoginTenant(t *testing.T) {
	stored := config.Profile{ClientID: "client-a", TenantID: "tenant-a"}

	t.Run("detected tenant wins over the stored one", func(t *testing.T) {
		got, warning := resolveLoginTenant("tenant-b", nil, "client-a", stored)
		if got != "tenant-b" || warning != "" {
			t.Errorf("got (%q, %q), want (\"tenant-b\", \"\")", got, warning)
		}
	})

	t.Run("failed read keeps the tenant of unchanged credentials", func(t *testing.T) {
		got, warning := resolveLoginTenant("", errors.New("tenant detection returned HTTP 502"), "client-a", stored)
		if got != "tenant-a" {
			t.Errorf("tenant = %q, want the stored \"tenant-a\"", got)
		}
		if !strings.Contains(warning, "HTTP 502") || !strings.Contains(warning, "keeping") {
			t.Errorf("warning should name the failure and the kept tenant, got %q", warning)
		}
	})

	t.Run("failed read with new credentials stores no tenant", func(t *testing.T) {
		got, warning := resolveLoginTenant("", errors.New("boom"), "client-b", stored)
		if got != "" {
			t.Errorf("tenant = %q, want empty: the stored tenant belongs to other credentials", got)
		}
		if !strings.Contains(warning, "without one") {
			t.Errorf("warning should say the profile has no tenant, got %q", warning)
		}
	})

	t.Run("empty answer is reported like a failure", func(t *testing.T) {
		got, warning := resolveLoginTenant("", nil, "client-b", config.Profile{})
		if got != "" || !strings.Contains(warning, "no tenant") {
			t.Errorf("got (%q, %q), want an empty tenant and a 'no tenant' warning", got, warning)
		}
	})
}
