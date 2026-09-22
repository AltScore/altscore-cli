package cmd

import (
	"fmt"
	"io"
	"strings"
)

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

// A nil capture is the import case: a bundle's node ids are already the author's own.
func printFindingLines(w io.Writer, prefix string, findings []validationFinding, capture *composeCapture) {
	for _, f := range findings {
		fmt.Fprintf(w, "#   [%s] %s\n", prefix, formatValidationFinding(f, capture))
	}
}
