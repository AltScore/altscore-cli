package cmd

// Shared rendering for server validation findings.
//
// Two commands surface the same finding shape from two different endpoints:
// `apply` pre-flights POST /v2/workflows/validate before creating any task, and
// `import` reads the findings POST /v2/workflows/import reports about the
// destination tenant. They differ in their headline sentence and their exit
// policy, but the per-finding lines are identical and must stay that way -- a
// second copy would drift, which is the whole class of bug the backend change
// behind this exists to remove.

import (
	"fmt"
	"io"
	"strings"
)

// partitionFindings splits findings into errors and warnings.
//
// Severity is lowercase "error"/"warning" today; matched case-insensitively in
// case that ever shifts, and anything not recognised as an error is treated as
// a warning so an unknown severity never silently escalates into an abort.
func partitionFindings(findings []validationFinding) (errs, warns []validationFinding) {
	for _, f := range findings {
		if strings.EqualFold(f.Severity, "error") {
			errs = append(errs, f)
		} else {
			warns = append(warns, f)
		}
	}
	return errs, warns
}

// printFindingLines writes one "#   [PREFIX] ..." line per finding.
//
// `capture` maps a server nodeId back to the author's spec-local ref and may be
// nil, which is the import case: a bundle's node ids ARE the author's own ids,
// so there is nothing to map and the raw id is already the right thing to show.
func printFindingLines(w io.Writer, prefix string, findings []validationFinding, capture *composeCapture) {
	for _, f := range findings {
		fmt.Fprintf(w, "#   [%s] %s\n", prefix, formatValidationFinding(f, capture))
	}
}
