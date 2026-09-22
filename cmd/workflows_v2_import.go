package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// Not validationResponse: an older backend omits these keys, and absent must read as
// "unknown", never as "false" -- which would abort on an import that already wrote.
type importResponseEnvelope struct {
	Validation *struct {
		Valid    bool                `json:"valid"`
		Findings []validationFinding `json:"findings"`
	} `json:"validation"`
	Notices  []string `json:"notices"`
	Imported *bool    `json:"imported"`
}

func makeWfv2ImportCmd() *cobra.Command {
	var bodyFlag string
	var newLabel string
	var skipEvaluationRules bool
	var skipScorecards bool
	var skipRuleTrees bool
	var skipMappingTables bool

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a v2 workflow from an export bundle",
		Long: `Pass the exported bundle in --body. Use --new-label to override the label
(the alias is derived from it).

The server validates the bundle against THIS tenant before writing anything.
A workflow names its scorecards, rule trees, mapping tables and evaluation
rules by code, and codes resolve per tenant -- so a bundle exported from
another tenant can be perfectly well-formed and still unrunnable here. Any
finding is printed to stderr; the import is refused outright (nothing is
created) only when the bundle references an entity that is neither on this
tenant nor carried by the bundle, since there is nothing to create it from.

The --skip-* flags protect entities this tenant ALREADY has from being
overwritten by the bundle's copies. They do not leave the workflow broken: an
entity the workflow references that is absent here is created anyway, and the
response says so.

The API returns 409 if the alias already exists.`,
		Example: `  altscore workflows-v2 import --body @bundle.json --new-label "Imported v1"
  altscore workflows-v2 import --body @bundle.json --skip-evaluation-rules`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			var bundle any
			if err := json.Unmarshal(body, &bundle); err != nil {
				return fmt.Errorf("invalid JSON body: %w", err)
			}
			payload := map[string]any{"workflowData": bundle}
			if newLabel != "" {
				payload["newLabel"] = newLabel
			}
			if skipEvaluationRules {
				payload["skipEvaluationRules"] = true
			}
			if skipScorecards {
				payload["skipScorecards"] = true
			}
			if skipRuleTrees {
				payload["skipRuleTrees"] = true
			}
			if skipMappingTables {
				payload["skipMappingTables"] = true
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			data, _, err := c.Do("POST", "borrower_central", "/v2/workflows/import", json.RawMessage(raw))
			if err != nil {
				// The client folds a >=400 body into the error and drops the findings
				// JSON, so the server's message has to name the missing entities.
				return err
			}
			reportImportFindings(cmd, data)
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "exported workflow JSON (or pipe via stdin)")
	cmd.Flags().StringVar(&newLabel, "new-label", "", "override label (alias derived from this)")
	cmd.Flags().BoolVar(&skipEvaluationRules, "skip-evaluation-rules", false, "do not overwrite evaluation rules this tenant already has (missing ones are still created)")
	cmd.Flags().BoolVar(&skipScorecards, "skip-scorecards", false, "do not overwrite scorecards this tenant already has (missing ones are still created)")
	cmd.Flags().BoolVar(&skipRuleTrees, "skip-rule-trees", false, "do not overwrite rule trees this tenant already has (missing ones are still created)")
	cmd.Flags().BoolVar(&skipMappingTables, "skip-mapping-tables", false, "do not overwrite mapping tables this tenant already has (missing ones are still created)")
	return cmd
}

// Never non-zero: import MUTATES, and a failing exit on a completed import would teach
// CI to retry it, which either 409s on the alias it just created or mints a duplicate.
func reportImportFindings(cmd *cobra.Command, data json.RawMessage) {
	if len(data) == 0 {
		return
	}
	var env importResponseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}

	errOut := cmd.ErrOrStderr()

	for _, notice := range env.Notices {
		fmt.Fprintf(errOut, "# import: %s\n", notice)
	}

	if env.Validation == nil || len(env.Validation.Findings) == 0 {
		return
	}

	errs, warns := partitionFindings(env.Validation.Findings)
	if len(warns) > 0 {
		fmt.Fprintf(errOut, "# import: %d warning(s) about this tenant:\n", len(warns))
		printFindingLines(errOut, "WARN", warns, nil)
	}
	if len(errs) > 0 {
		fmt.Fprintf(errOut, "# import: %d error(s) carried over from the source workflow:\n", len(errs))
		printFindingLines(errOut, "ERROR", errs, nil)
		fmt.Fprintln(errOut, "# the workflow WAS imported; fix these before publishing it.")
	}
}
