package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Ordinal-code advisory.
//
// A v1 evaluator carries a family of fields that collapse a finding into a small
// integer -- 2/1/0/-1, or 1/0 with -1 for "missing". They exist because the v1
// engine had to move a decision between two Python functions as a number, and a
// faithful port carries them across unchanged: the workflow reproduces every
// historical decision exactly and the client still cannot read it. 2 outranks 1
// only in the author's head, the finding behind the code is invisible, and
// changing the coding needs a republish.
//
// The v2 shape is a compute-variable that emits the real quantity (the amount,
// the count, the category as the source spells it) plus an evaluation-rule when
// the answer is boolean or a mapping-table when it has levels. Doctrine and the
// -1/includeNa argument: `altscore workflows-v2 schema-guide v1Migration`,
// section indicatorVariables.
//
// DETECTION IS BY SHAPE, NOT BY NAME, for two independent reasons. The `_indicator`
// suffix under-reports: a live port had 6 variables carrying it and a 7th,
// `pjex_sri_billing_permission`, with the identical -1/0/1 shape and no suffix. It
// also OVER-reports: `fiscalia_indicator` there returns 'GRAVE'/'NO TIENE', which
// is a named category -- precisely what the guidance asks for -- so flagging it on
// the name would tell the author to undo the right thing.

// ordinalCodeSet is the vocabulary that marks an assignment as a code rather than
// a quantity. Deliberately tiny: a variable whose values run past this is
// computing something, not encoding a verdict.
var ordinalCodeSet = map[float64]bool{-1: true, 0: true, 1: true, 2: true, 3: true}

const ordinalCodeMinDistinct = 2

// ordinalCodeLiteral matches a whole-line assignment of a name to a bare numeric
// literal: `result = 1`, `result = -1`, `result = 0.0`. The float form is not
// hypothetical -- a live port assigns `result = 0.0` on one branch and `result = -1`
// on another.
//
// The name is interpolated per variable (its returnValue), so both patterns are
// built at call time rather than being package-level.
func ordinalCodeLiteralRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*` + regexp.QuoteMeta(name) + `\s*=\s*(-?\d+(?:\.\d+)?)\s*$`)
}

// ordinalCodeAssignRe matches ANY assignment to the name, literal or not,
// including the augmented forms. A single match that ordinalCodeLiteralRe does not
// also match disqualifies the variable: it is computing a value somewhere, so the
// integers are not its whole vocabulary.
//
// RE2 has no lookahead, so `=` not followed by `=` is spelled as "= then a
// non-`=` character, or `=` at end of line". That is what keeps `result == 0`
// (a comparison) from reading as an assignment.
func ordinalCodeAssignRe(name string) *regexp.Regexp {
	n := regexp.QuoteMeta(name)
	return regexp.MustCompile(`^\s*` + n + `\s*(?:=[^=]|=$|\+=|-=|\*=|/=|%=)`)
}

// ordinalCodesOf reports the distinct codes a custom variable assigns to its
// returnValue, and whether the variable qualifies as ordinal-coded: every
// assignment is a bare literal, the literals all come from ordinalCodeSet, and
// there are at least ordinalCodeMinDistinct of them.
//
// Conservative by construction. One assignment the literal pattern does not match
// -- an arithmetic result, a lookup, a variable, a call -- and the whole variable
// is left alone, because that is a derivation whose branches happen to include a
// small number. A variable that never assigns its returnValue at the statement
// level (it builds a dict, or returns an expression) matches nothing and is
// likewise left alone.
func ordinalCodesOf(def map[string]any) ([]float64, bool) {
	if def == nil {
		return nil, false
	}
	expr, _ := def["expression"].(string)
	name, _ := def["returnValue"].(string)
	name = strings.TrimSpace(name)
	if expr == "" || name == "" {
		return nil, false
	}
	// A returnValue is a Python identifier. Anything else is not something we
	// can match assignments against, and is not worth guessing at.
	if !pyIdentifier.MatchString(name) {
		return nil, false
	}

	literal := ordinalCodeLiteralRe(name)
	assign := ordinalCodeAssignRe(name)

	seen := map[float64]bool{}
	for _, line := range strings.Split(expr, "\n") {
		line = stripTrailingComment(line)
		if m := literal.FindStringSubmatch(line); m != nil {
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				return nil, false
			}
			if !ordinalCodeSet[v] {
				// A literal outside the code vocabulary (a sentinel like
				// 999999, a real threshold) means this is a computed value.
				return nil, false
			}
			seen[v] = true
			continue
		}
		if assign.MatchString(line) {
			return nil, false
		}
	}
	if len(seen) < ordinalCodeMinDistinct {
		return nil, false
	}
	codes := make([]float64, 0, len(seen))
	for v := range seen {
		codes = append(codes, v)
	}
	sort.Float64s(codes)
	return codes, true
}

var pyIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// stripTrailingComment drops a trailing `#` comment, but ONLY from a line with no
// quote character. A `#` inside a string is not a comment, and cutting there would
// turn a disqualifying assignment into a line that matches nothing -- a false
// positive. Losing a comment on a quoted line costs a miss instead, which is the
// right way round for an advisory.
func stripTrailingComment(line string) string {
	if strings.ContainsAny(line, `"'`) {
		return line
	}
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}

// formatOrdinalCodes renders a code list the way it reads in the source: -1/0/1,
// integers without a decimal point even when the expression wrote 0.0.
func formatOrdinalCodes(codes []float64) string {
	parts := make([]string, len(codes))
	for i, v := range codes {
		parts[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strings.Join(parts, "/")
}

// adviseOrdinalCodeVars flags custom variables whose entire output vocabulary is a
// handful of small integer codes. Aggregated into one readabilityFinding, so a
// migration carrying twenty of them prints one line, not twenty.
func adviseOrdinalCodeVars(customVariables map[string]any) (readabilityFinding, bool) {
	total := len(customVariables)
	if total == 0 {
		return readabilityFinding{}, false
	}
	var coded []string
	for name, raw := range customVariables {
		def, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if codes, isCoded := ordinalCodesOf(def); isCoded {
			coded = append(coded, fmt.Sprintf("%s (%s)", name, formatOrdinalCodes(codes)))
		}
	}
	if len(coded) == 0 {
		return readabilityFinding{}, false
	}
	sort.Strings(coded)
	return readabilityFinding{
		Practice: "ordinal-codes",
		Count:    len(coded),
		Total:    total,
		Sample:   coded,
		Advice: "custom variable(s) return nothing but a small integer code. The variable has " +
			"already made the decision, in a vocabulary only its author can read: the client " +
			"cannot tell whether 2 outranks 1, cannot see which finding produced it, and cannot " +
			"change the coding without a republish. Emit the underlying VALUE (the amount, the " +
			"count, the category as the source spells it) and move the coding onto an " +
			"evaluation-rule (yes/no) or a mapping-table (named levels, which is also where a " +
			"scorecard's points belong). A `-1` for missing is an includeNa bucket, not a value. " +
			"See `altscore workflows-v2 schema-guide v1Migration` -> indicatorVariables",
	}, true
}
