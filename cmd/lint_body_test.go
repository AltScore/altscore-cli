package cmd

import (
	"strings"
	"testing"
)

func TestLintBodyNamesTheDryRun(t *testing.T) {
	_, _, err := runListCmd(t, makeWfv2LintCmd(), "--body", "@spec.json")
	if err == nil || !strings.Contains(err.Error(), "apply --body @spec.json --dry-run") {
		t.Fatalf("lint --body must point at apply --dry-run, got %v", err)
	}
	if strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("--body must be recognised, got %v", err)
	}
	if _, _, err := runListCmd(t, makeWfv2LintCmd()); err == nil || !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("without --body, lint still needs the workflow id, got %v", err)
	}
}
