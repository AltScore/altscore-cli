package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// A run is a failure for CI purposes when the flow did not complete or an expectation
// (including the decision, which is a normal expectation now) did not hold. "no_expectations"
// is a preview, not a red result, so it does not fail the command.
func runStatusFails(status string) bool {
	return status == "failed" || status == "error"
}

func makeWftRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <test-case-id>",
		Short: "Run a single workflow test case",
		Long: `Run a single workflow test case against the latest workflow version. Returns the
status (passed/failed/error/no_expectations), the decision the flow reached, each
expectation result, and the output. Exits non-zero when the case failed or errored.`,
		Example: `  altscore workflow-test-cases run <test-case-id>
  altscore workflow-test-cases run <test-case-id> | jq '.decision'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", "borrower_central", "/v1/workflow-test-cases/"+args[0]+"/run", nil)
			if err != nil {
				return err
			}
			if err := output.RawJSON(data); err != nil {
				return err
			}
			var res struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(data, &res)
			if runStatusFails(res.Status) {
				return &ExitCodeError{Code: 1, Err: fmt.Errorf("test case %s %s", args[0], res.Status)}
			}
			return nil
		},
	}
}

func makeWftRunAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run-all <workflow-alias>",
		Short: "Run every test case of a workflow",
		Long: `Run all test cases for a workflow alias and print a summary with total, passed,
failed, error and no-expectations counts plus each case's result. There is no bulk
endpoint, so this lists the cases and runs them one by one. Exits non-zero when any
case failed or errored.`,
		Example: `  altscore workflow-test-cases run-all kyc-demo
  altscore workflow-test-cases run-all kyc-demo | jq '.passed'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			alias := args[0]
			listData, _, err := c.Do("GET", "borrower_central", "/v1/workflow-test-cases?workflow-alias="+alias, nil)
			if err != nil {
				return err
			}
			var cases []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(listData, &cases); err != nil {
				return fmt.Errorf("parse workflow test cases: %w", err)
			}

			results := make([]json.RawMessage, 0, len(cases))
			counts := map[string]int{}
			for _, tc := range cases {
				runData, _, runErr := c.Do("POST", "borrower_central", "/v1/workflow-test-cases/"+tc.ID+"/run", nil)
				if runErr != nil {
					// A transport failure for one case is itself an error result, not a reason
					// to abandon the rest of the suite.
					counts["error"]++
					results = append(results, json.RawMessage(fmt.Sprintf(
						`{"workflowTestCaseId":%q,"name":%q,"status":"error","errorMessage":%q}`,
						tc.ID, tc.Name, runErr.Error())))
					continue
				}
				var res struct {
					Status string `json:"status"`
				}
				_ = json.Unmarshal(runData, &res)
				counts[res.Status]++
				results = append(results, runData)
			}

			summary := map[string]any{
				"workflowAlias":  alias,
				"total":          len(cases),
				"passed":         counts["passed"],
				"failed":         counts["failed"],
				"error":          counts["error"],
				"noExpectations": counts["no_expectations"],
				"results":        results,
			}
			if err := output.JSON(summary); err != nil {
				return err
			}
			if counts["failed"]+counts["error"] > 0 {
				return &ExitCodeError{Code: 1, Err: fmt.Errorf(
					"%d of %d test cases did not pass", counts["failed"]+counts["error"], len(cases))}
			}
			return nil
		},
	}
}

func makeWftStalenessCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "staleness <test-case-id>",
		Short: "Check whether a workflow test case is stale",
		Long: `Report whether a test case is out of date with its workflow: the version it was
built against vs the current one, plus any required inputs now missing or unknown.`,
		Example: `  altscore workflow-test-cases staleness <test-case-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", "borrower_central", "/v1/workflow-test-cases/"+args[0]+"/staleness", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
