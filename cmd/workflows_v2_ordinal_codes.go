package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Detection is by shape, never by name: the `_indicator` suffix both under-reports and
// over-reports. Deliberately tiny -- values running past this set are a computation.
var ordinalCodeSet = map[float64]bool{-1: true, 0: true, 1: true, 2: true, 3: true}

const ordinalCodeMinDistinct = 2

func ordinalCodeLiteralRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*` + regexp.QuoteMeta(name) + `\s*=\s*(-?\d+(?:\.\d+)?)\s*$`)
}

// RE2 has no lookahead, so "= not followed by =" is spelled out; that is what keeps
// `result == 0` from reading as an assignment.
func ordinalCodeAssignRe(name string) *regexp.Regexp {
	n := regexp.QuoteMeta(name)
	return regexp.MustCompile(`^\s*` + n + `\s*(?:=[^=]|=$|\+=|-=|\*=|/=|%=)`)
}

// Conservative by construction: one assignment the literal pattern does not match leaves
// the whole variable alone.
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
				// A sentinel like 999999 or a real threshold: this is a computed value.
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

// Only on a line with no quote: a `#` inside a string is not a comment, and cutting there
// would hide a disqualifying assignment.
func stripTrailingComment(line string) string {
	if strings.ContainsAny(line, `"'`) {
		return line
	}
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}

func formatOrdinalCodes(codes []float64) string {
	parts := make([]string, len(codes))
	for i, v := range codes {
		parts[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strings.Join(parts, "/")
}

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
