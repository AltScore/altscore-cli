package cmd

import (
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func makeEvEvaluateCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "evaluate <id>",
		Short: "Run an evaluator by ID",
		Long: `Run an evaluator by its ID. Pass the evaluation input as --body.

The body must contain an "instance" object with referenceId, referenceDate, and
data (the variables). Optionally include an "entities" array for co-debtors or
guarantors.

Example body:
  {
    "instance": {
      "referenceId": "borrower-123",
      "referenceDate": "2026-03-07T12:00:00",
      "data": {"score": 750, "debt_ratio": 0.3}
    },
    "entities": []
  }

Returns the evaluator output with score, scorecard, metrics, rules, and decision.`,
		Example: `  altscore evaluators evaluate <id> --body '{"instance": {"referenceId": "b-1", "referenceDate": "2026-01-01T00:00:00", "data": {"x": 5}}, "entities": []}'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/evaluators/%s/evaluate", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

func makeEvEvaluateByAliasCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "evaluate-by-alias <alias> <version>",
		Short: "Run an evaluator by alias and version",
		Long: `Run an evaluator by its alias and version (e.g. "scoring" "v3"). Pass
the evaluation input as --body. Same input format as 'evaluate'.`,
		Example: `  altscore evaluators evaluate-by-alias scoring v3 --body '{"instance": {"referenceId": "b-1", "referenceDate": "2026-01-01T00:00:00", "data": {"x": 5}}, "entities": []}'`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/evaluators/%s/%s/evaluate", args[0], args[1])
			data, _, err := c.Do("POST", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}
