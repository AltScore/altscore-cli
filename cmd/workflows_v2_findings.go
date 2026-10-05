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
// An empty workflowAlias leaves the server's message as it is.
func printFindingLines(w io.Writer, prefix string, findings []validationFinding, capture *composeCapture, workflowAlias string) {
	for _, f := range findings {
		fmt.Fprintf(w, "#   [%s] %s%s\n", prefix, formatValidationFinding(f, capture), entityCreateFix(f, workflowAlias))
	}
}

var entityResourceByNotFoundCode = map[string]string{
	"SCORECARD_NOT_FOUND":       "scorecards",
	"RULE_TREE_NOT_FOUND":       "rule-trees",
	"MAPPING_TABLE_NOT_FOUND":   "mapping-tables",
	"EVALUATION_RULE_NOT_FOUND": "evaluation-rules",
}

// The server says "Create it" without the command. Kept on the finding's own line, after
// the server's text, so a filter keyed on the quoted code still matches the line.
func entityCreateFix(f validationFinding, workflowAlias string) string {
	resource, ok := entityResourceByNotFoundCode[f.Code]
	if !ok || workflowAlias == "" {
		return ""
	}
	code := ""
	if c, _ := f.Params["code"].(string); c != "" {
		code = fmt.Sprintf(" whose \"code\" is %q", c)
	}
	return fmt.Sprintf(" Fix: altscore %s create --workflow-alias %s --body @<file>%s, then dry-run again.", resource, workflowAlias, code)
}
