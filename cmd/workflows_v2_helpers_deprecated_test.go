package cmd

import (
	"io"
	"strings"
	"testing"
)

func runAddNode(args ...string) error {
	cmd := makeWfv2AddNodeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestAddNode_RefusesDeprecatedType(t *testing.T) {
	for _, typ := range retiredTaskTypes {
		err := runAddNode("wf-1", "--type", typ, "--node-id", "n1", "--label", "N1")
		if err == nil {
			t.Errorf("add-node --type %s must be refused", typ)
			continue
		}
		if !strings.Contains(err.Error(), "DEPRECATED") {
			t.Errorf("--type %s: unexpected error: %v", typ, err)
		}
	}
}

func TestAddNode_CurrentTypeStillReachesTheTaskRefCheck(t *testing.T) {
	err := runAddNode("wf-1", "--type", "http", "--node-id", "n1", "--label", "N1")
	if err == nil || !strings.Contains(err.Error(), "--task-alias") {
		t.Fatalf("a current type must fall through to the task-reference check, got: %v", err)
	}
}
