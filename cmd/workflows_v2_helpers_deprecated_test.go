package cmd

import (
	"io"
	"strings"
	"testing"
)

// runAddNode executes add-node with usage output discarded, so a refusal shows
// up as an error and not as a page of cobra help in the test log.
func runAddNode(args ...string) error {
	cmd := makeWfv2AddNodeCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

// add-node had no vocabulary check at all: --type went straight into the node.
// The refusal has to land before the node is built, and before loadClient, so
// it needs no session and no backend.
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

// The gate must not swallow a current type: that one gets as far as the
// task-reference requirement, which is the pre-existing next check.
func TestAddNode_CurrentTypeStillReachesTheTaskRefCheck(t *testing.T) {
	err := runAddNode("wf-1", "--type", "http", "--node-id", "n1", "--label", "N1")
	if err == nil || !strings.Contains(err.Error(), "--task-alias") {
		t.Fatalf("a current type must fall through to the task-reference check, got: %v", err)
	}
}
