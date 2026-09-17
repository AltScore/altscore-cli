package cmd

import "testing"

// The schema-guide command has three shapes the skill relies on: the index
// (no args), one section, and one task type under tasks. --full is the only
// way to get the whole guide, and a section argument wins over it.
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
