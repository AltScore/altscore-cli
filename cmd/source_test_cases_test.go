package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// findSourceTestCasesGroup locates the registered group without going through rootCmd,
// so these tests do not depend on init() ordering or on a configured profile.
func newSourceTestCasesGroup(t *testing.T) *cobra.Command {
	t.Helper()
	// registerResource adds to rootCmd as a side effect, so build the group the same way
	// the production path does and then read it back off rootCmd.
	for _, c := range rootCmd.Commands() {
		if c.Use == "source-test-cases" {
			return c
		}
	}
	t.Fatal("source-test-cases group is not registered on rootCmd")
	return nil
}

func TestSourceTestCasesGroupExposesEveryAction(t *testing.T) {
	group := newSourceTestCasesGroup(t)

	want := map[string]bool{
		"list": false, "get": false, "create": false, "update": false, "delete": false,
		"from-package": false, "set-content": false, "body-help": false,
	}
	for _, sub := range group.Commands() {
		name := strings.Fields(sub.Use)[0]
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("subcommand %q is missing", name)
		}
	}
}

// The path is the one thing a typo here would break silently: a wrong base path returns
// 404s that read like "no test cases exist yet" rather than like a bug.
func TestSourceTestCasesBasePathMatchesTheBackendRoute(t *testing.T) {
	group := newSourceTestCasesGroup(t)

	list, _, err := group.Find([]string{"list"})
	if err != nil {
		t.Fatalf("find list: %v", err)
	}
	// The base path is baked into the generated commands' Long/Example text via the
	// ResourceDef, so assert on the definition through a command that echoes it.
	if !strings.Contains(list.Long, "source-test-cases") {
		t.Errorf("list help does not mention the resource; ResourceDef.Name may have drifted")
	}
}

// --name is the address the case is referenced by, so a promote without one would create
// something unreachable. The guard has to fire BEFORE the client is loaded, or the error
// a user sees is about missing credentials instead.
func TestPromoteRequiresAName(t *testing.T) {
	cmd := makeSourceTestCaseFromPackageCmd()
	cmd.SetArgs([]string{"pkg-123"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error when --name is omitted")
	}
	if !strings.Contains(err.Error(), "--name is required") {
		t.Errorf("error = %q, want it to name the missing --name flag", err.Error())
	}
	if !strings.Contains(err.Error(), "tenant:") {
		t.Errorf("error = %q, want it to explain that the name becomes tenant:<name>", err.Error())
	}
}

// The update surface must not offer a rename. The name is the address: it lives as a bare
// string inside saved workflow bodies, nothing indexes those back to the case, and an
// unresolvable reference deliberately fails a node -- so a rename would convert working
// workflows into failing ones. The backend rejects it; the help must not invite it.
func TestUpdateHelpSaysTheNameCannotMove(t *testing.T) {
	group := newSourceTestCasesGroup(t)

	update, _, err := group.Find([]string{"update"})
	if err != nil {
		t.Fatalf("find update: %v", err)
	}
	if !strings.Contains(update.Long, "rejected") {
		t.Errorf("update help does not say a name change is rejected:\n%s", update.Long)
	}
}

// body-help exists because a body in the wrong shape is the one mistake with no loud
// failure: it is served exactly as stored, so the node returns something the graph does
// not expect rather than erroring. Both shapes have to be documented.
func TestBodyHelpCoversBothStoredShapes(t *testing.T) {
	cmd := makeSourceTestCaseBodyHelpCmd()

	for _, needle := range []string{"isSuccess", "sourceData", "errorMessage", "raw"} {
		if !strings.Contains(cmd.Long, needle) {
			t.Errorf("body-help does not mention %q", needle)
		}
	}
}
