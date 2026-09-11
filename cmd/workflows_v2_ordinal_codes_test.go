package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// cv builds a customVariable definition the way both a saved workflow and an
// apply spec carry one.
func cv(expression, returnValue string) map[string]any {
	return map[string]any{"expression": expression, "returnValue": returnValue}
}

// TestOrdinalCodesOf_FlagsTheCodedShapes covers what the advisory exists for: a
// variable whose entire output vocabulary is a handful of small integers, in the
// forms a v1 port actually produces -- the -1/0/1 "missing, clean, hit" triple,
// the bare 0/1 pre-computed boolean, and the float branch (`result = 0.0`) that a
// real ported expression mixes with integer branches.
func TestOrdinalCodesOf_FlagsTheCodedShapes(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want string
	}{
		{
			name: "missing/clean/hit triple",
			expr: `raw = inputs.get('task_outputs.fetch.amount')
if raw is None:
    result = -1
elif raw == 0:
    result = 0
else:
    result = 1`,
			want: "-1/0/1",
		},
		{
			name: "pre-computed boolean",
			expr: `hits = inputs.get('task_outputs.fetch.hits') or []
if len(hits) > 0:
    result = 1
else:
    result = 0`,
			want: "0/1",
		},
		{
			name: "float branch counts as its integer code",
			expr: `raw = inputs.get('task_outputs.fetch.debt')
result = -1
if raw is not None:
    result = 0.0`,
			want: "-1/0",
		},
		{
			name: "severity ladder",
			expr: `sev = inputs.get('task_outputs.fetch.severity')
if sev == 'GRAVE':
    result = 2
elif sev == 'LEVE':
    result = 1
else:
    result = 0`,
			want: "0/1/2",
		},
		{
			name: "trailing comment on an unquoted line does not hide the assignment",
			expr: `n = inputs.get('task_outputs.fetch.n')
result = 0  # clean
if n:
    result = 1  # hit`,
			want: "0/1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codes, ok := ordinalCodesOf(cv(tc.expr, "result"))
			if !ok {
				t.Fatalf("expected %s to be flagged as ordinal-coded", tc.name)
			}
			if got := formatOrdinalCodes(codes); got != tc.want {
				t.Errorf("codes = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOrdinalCodesOf_LeavesRealComputationAlone is the half that matters most: an
// advisory that fires on genuine derivations trains the reader to ignore it. Every
// case here MUST stay silent.
//
// The categorical case is the load-bearing one. `fiscalia_indicator` in a live
// port returns 'GRAVE'/'NO TIENE' -- a named category, which is exactly what the
// guidance asks a ported indicator to become. Flagging it on the `_indicator`
// suffix would tell the author to undo the right thing, which is why detection is
// by shape and the name is never consulted.
func TestOrdinalCodesOf_LeavesRealComputationAlone(t *testing.T) {
	cases := []struct {
		name string
		expr string
		rv   string
	}{
		{
			name: "named categories are the DESIRED end state, not a finding",
			rv:   "result",
			expr: `sev = inputs.get('task_outputs.fetch.severity')
if sev in ('GENOCIDIO', 'EXTORSION'):
    result = 'GRAVE'
elif sev:
    result = 'LEVE'
else:
    result = 'NO TIENE'`,
		},
		{
			name: "a computed ratio with a coded fallback is a derivation",
			rv:   "result",
			expr: `raw = inputs.get('task_outputs.fetch.debt')
sales = inputs.get('task_outputs.fetch.sales')
result = -1
if raw is not None and sales:
    result = raw / sales`,
		},
		{
			name: "a sentinel outside the code vocabulary disqualifies",
			rv:   "result",
			expr: `raw = inputs.get('task_outputs.fetch.debt')
if raw is None:
    result = -1
elif raw == 0:
    result = 0
else:
    result = 999999`,
		},
		{
			name: "one distinct code is a constant, not a vocabulary",
			rv:   "result",
			expr: `raw = inputs.get('task_outputs.fetch.debt')
if raw is None:
    result = 0`,
		},
		{
			name: "an augmented assignment means it is accumulating",
			rv:   "result",
			expr: `result = 0
for d in inputs.get('task_outputs.fetch.debts') or []:
    result += 1`,
		},
		{
			name: "a real aggregate",
			rv:   "result",
			expr: `debts = inputs.get('task_outputs.fetch.debts') or []
result = max([d.get('amount') or 0 for d in debts] or [0])`,
		},
		{
			name: "a dict builder never assigns the name at statement level",
			rv:   "out",
			expr: `out = {}
out['score'] = 1
out['band'] = 0`,
		},
		{
			name: "no returnValue means nothing to match assignments against",
			rv:   "",
			expr: `result = 1`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if codes, ok := ordinalCodesOf(cv(tc.expr, tc.rv)); ok {
				t.Errorf("expected silence, got flagged with codes %v", codes)
			}
		})
	}
}

// TestOrdinalCodesOf_ComparisonIsNotAnAssignment guards the RE2 workaround. Go has
// no lookahead, so "= not followed by =" is spelled by hand; get it wrong and
// `result == 0` reads as an assignment, which would disqualify (or worse, count)
// every variable that tests its own accumulator.
func TestOrdinalCodesOf_ComparisonIsNotAnAssignment(t *testing.T) {
	expr := `flag = inputs.get('task_outputs.fetch.flag')
result = 0
if flag:
    result = 1
# a self-comparison below must not read as an assignment
if result == 1:
    pass`
	codes, ok := ordinalCodesOf(cv(expr, "result"))
	if !ok {
		t.Fatalf("comparison line should not have disqualified the variable")
	}
	if got := formatOrdinalCodes(codes); got != "0/1" {
		t.Errorf("codes = %q, want 0/1", got)
	}
}

// TestStripTrailingComment_LeavesQuotedLinesIntact proves the conservative choice:
// a `#` inside a string is not a comment, and cutting there would turn a
// disqualifying assignment into a line that matches nothing -- a FALSE POSITIVE.
// Losing a genuine comment on a quoted line costs a miss instead, which is the
// right way round for an advisory.
func TestStripTrailingComment_LeavesQuotedLinesIntact(t *testing.T) {
	line := `    result = tag.split("#")[0]`
	if got := stripTrailingComment(line); got != line {
		t.Errorf("quoted line was truncated: %q", got)
	}
	// And the variable it belongs to stays unflagged, because that assignment
	// survives to disqualify it.
	expr := `tag = inputs.get('task_outputs.fetch.tag') or ""
result = 0
if tag:
    result = tag.split("#")[0]`
	if _, ok := ordinalCodesOf(cv(expr, "result")); ok {
		t.Error("a string-splitting assignment must disqualify the variable")
	}
}

// TestAdviseOrdinalCodeVars_AggregatesAndNamesTheReplacement checks the reported
// line: one line for many variables (a migration carrying twenty must not print
// twenty), the count against the real denominator, a capped sample, and an advice
// string that names where the logic should go instead. "Do not do that" with no
// destination is unactionable.
func TestAdviseOrdinalCodeVars_AggregatesAndNamesTheReplacement(t *testing.T) {
	coded := `n = inputs.get('task_outputs.fetch.n')
result = 0
if n:
    result = 1`
	vars := map[string]any{
		"clean_var": cv(`result = sum(inputs.get('task_outputs.fetch.xs') or [])`, "result"),
	}
	for _, n := range []string{"a_ind", "b_ind", "c_ind", "d_ind", "e_ind", "f_ind", "g_ind"} {
		vars[n] = cv(coded, "result")
	}

	f, ok := adviseOrdinalCodeVars(vars)
	if !ok {
		t.Fatal("expected a finding")
	}
	if f.Count != 7 || f.Total != 8 {
		t.Errorf("count/total = %d/%d, want 7/8", f.Count, f.Total)
	}
	if f.Practice != "ordinal-codes" {
		t.Errorf("practice = %q", f.Practice)
	}
	for _, want := range []string{"evaluation-rule", "mapping-table", "includeNa", "indicatorVariables"} {
		if !strings.Contains(f.Advice, want) {
			t.Errorf("advice does not mention %q: %s", want, f.Advice)
		}
	}

	line := f.line()
	if strings.Count(line, "\n") != 1 {
		t.Errorf("finding must be ONE line, got: %q", line)
	}
	if !strings.Contains(line, "7 of 8") {
		t.Errorf("line does not carry the ratio: %q", line)
	}
	if !strings.Contains(line, "a_ind (0/1)") {
		t.Errorf("line does not sample a variable with its codes: %q", line)
	}
	// Capped sample, with the overflow acknowledged rather than dropped silently.
	if !strings.Contains(line, "(+2 more)") {
		t.Errorf("sample not capped with a +N more suffix: %q", line)
	}
	if strings.Contains(line, "clean_var") {
		t.Errorf("the computed variable leaked into the sample: %q", line)
	}
}

// TestAdviseOrdinalCodeVars_SilentWhenNothingIsCoded covers the two ways a
// workflow earns silence: no variables at all, and no coded ones.
func TestAdviseOrdinalCodeVars_SilentWhenNothingIsCoded(t *testing.T) {
	if _, ok := adviseOrdinalCodeVars(nil); ok {
		t.Error("no customVariables must be silent")
	}
	clean := map[string]any{
		"total": cv(`result = sum(inputs.get('task_outputs.fetch.xs') or [])`, "result"),
		"label": cv(`result = 'GRAVE' if inputs.get('task_outputs.fetch.hit') else 'LIMPIO'`, "result"),
	}
	if _, ok := adviseOrdinalCodeVars(clean); ok {
		t.Error("a workflow with no coded variables must be silent")
	}
}

// TestPrintReadabilityFindings_KeepsTheBlockShape confirms apply and lint print
// the same thing: a header, one line per finding, and the guide pointer. Nothing
// here may be an error or move an exit code -- it is stderr text only.
func TestPrintReadabilityFindings_KeepsTheBlockShape(t *testing.T) {
	var buf bytes.Buffer
	printReadabilityFindings(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("no findings must print nothing, got %q", buf.String())
	}

	f, ok := adviseOrdinalCodeVars(map[string]any{
		"x_ind": cv("n = inputs.get('task_outputs.fetch.n')\nresult = 0\nif n:\n    result = 1", "result"),
	})
	if !ok {
		t.Fatal("expected a finding")
	}
	printReadabilityFindings(&buf, []readabilityFinding{f})
	out := buf.String()
	for _, want := range []string{
		"readability advisory (client handoff): 1 finding(s)",
		"never fails lint or blocks apply",
		"[ordinal-codes] 1 of 1",
		"schema-guide handoffReadability",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block missing %q:\n%s", want, out)
		}
	}
}
