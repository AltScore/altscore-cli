package cmd

import (
	"strings"
	"testing"
)

// `self.<field>` is a node's OWN late-resolved output -- today the End node's
// pdf_url / pdf_error. BC leaves the literal in place at graph time and fills it
// in a second pass inside end_activity, so apply must treat it as a reserved
// scope: never rewritten to a server alias, never rejected as an unknown ref.

func TestSelfIsAReservedMappingScope(t *testing.T) {
	if !reservedMappingScopes["self"] {
		t.Fatal("self must be a reserved mapping scope; otherwise apply rewrites or rejects {{self.pdf_url}}")
	}
}

func TestReservedScopesListIsDerivedAndSorted(t *testing.T) {
	got := reservedScopesList()
	for scope := range reservedMappingScopes {
		if !strings.Contains(got, scope) {
			t.Errorf("reservedScopesList() omitted %q -- error hints would drift from the map", scope)
		}
	}
	if !sortedCSV(got) {
		t.Errorf("reservedScopesList() not sorted: %q", got)
	}
}

func sortedCSV(s string) bool {
	parts := strings.Split(s, ", ")
	for i := 1; i < len(parts); i++ {
		if parts[i-1] > parts[i] {
			return false
		}
	}
	return true
}

func TestSelfRefSurvivesTemplateRewrite(t *testing.T) {
	refMap := map[string]string{"scoring": "scoring-engine-score-30402a"}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "self ref is left verbatim",
			in:   `{"report_url": "{{self.pdf_url}}"}`,
			want: `{"report_url": "{{self.pdf_url}}"}`,
		},
		{
			name: "self ref alongside a rewritten spec ref",
			in:   `{"u": "{{self.pdf_url}}", "s": "{{task_outputs.scoring.score}}"}`,
			want: `{"u": "{{self.pdf_url}}", "s": "{{task_outputs.scoring-engine-score-30402a.score}}"}`,
		},
		{
			name: "pdf_error too",
			in:   `{{self.pdf_error}}`,
			want: `{{self.pdf_error}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteRefsInTemplate(tc.in, refMap, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestUnknownHeadStillRejected(t *testing.T) {
	// Reserving `self` must not weaken the typo guard for everything else.
	if _, err := rewriteRefsInTemplate(`{{nope.field}}`, map[string]string{}, nil); err == nil {
		t.Fatal("expected an error for an unknown head")
	}
}
