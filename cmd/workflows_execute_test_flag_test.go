package cmd

import "testing"

func TestEnsureTestTag(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "test"},
		{"poc", "poc,test"},
		{"test", "test"},
		{"a,test,b", "a,test,b"},
		{"a, test ,b", "a, test ,b"}, // whitespace-trimmed match -> no dup
		{"testing", "testing,test"},   // substring is NOT a match
		{"parity-test", "parity-test,test"},
	}
	for _, c := range cases {
		if got := ensureTestTag(c.in); got != c.want {
			t.Errorf("ensureTestTag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExecHeadersTestFlagInjectsTag(t *testing.T) {
	// --test with no other tags -> X-Tags: test
	h := wfv2ExecHeaders{test: true}
	if got := h.asMap()["X-Tags"]; got != "test" {
		t.Errorf("test-only: X-Tags = %q, want %q", got, "test")
	}
	// --test merges with existing --tags
	h = wfv2ExecHeaders{tags: "poc", test: true}
	if got := h.asMap()["X-Tags"]; got != "poc,test" {
		t.Errorf("test+tags: X-Tags = %q, want %q", got, "poc,test")
	}
	// no --test, no tags -> no X-Tags header
	h = wfv2ExecHeaders{}
	if _, present := h.asMap()["X-Tags"]; present {
		t.Error("no flags: X-Tags should be absent")
	}
	// --test does not clobber other headers
	h = wfv2ExecHeaders{test: true, executionMode: "async", testTaskID: "abc"}
	m := h.asMap()
	if m["X-Tags"] != "test" || m["X-Execution-Mode"] != "async" || m["X-Test-Task-Id"] != "abc" {
		t.Errorf("test+others: got %v", m)
	}
}

func TestSetBodyTestMode(t *testing.T) {
	out, err := setBodyTestMode([]byte(`{"inputs":[{"x":1}],"label":"smoke"}`))
	if err != nil {
		t.Fatalf("setBodyTestMode: %v", err)
	}
	if !jsonHasTrue(out, "testMode") {
		t.Errorf("expected testMode=true in %s", string(out))
	}
	// preserves existing fields
	if !jsonHasKey(out, "label") || !jsonHasKey(out, "inputs") {
		t.Errorf("dropped existing fields: %s", string(out))
	}
}
