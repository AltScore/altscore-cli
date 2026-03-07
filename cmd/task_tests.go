package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func makeTtRunCmd() *cobra.Command {
	var version int

	cmd := &cobra.Command{
		Use:   "run <test-id>",
		Short: "Run a single task test",
		Long: `Run a single task test case against a specific task version. Returns
the test status (passed/failed/error), actual output, and any differences.`,
		Example: `  altscore task-tests run <test-id>
  altscore task-tests run <test-id> --version 2`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body := json.RawMessage(fmt.Sprintf(`{"taskVersion":%d}`, version))
			data, _, err := c.Do("POST", "borrower_central", "/v1/task-tests/"+args[0]+"/run", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().IntVar(&version, "version", 1, "task version to test against")
	return cmd
}

func makeTtRunAllCmd() *cobra.Command {
	var version int

	cmd := &cobra.Command{
		Use:   "run-all <task-id>",
		Short: "Run all tests for a task",
		Long: `Run all test cases for a workflow task against a specific version. Returns
a summary with total, passed, failed, error counts and individual results.`,
		Example: `  altscore task-tests run-all <task-id>
  altscore task-tests run-all <task-id> --version 2`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body := json.RawMessage(fmt.Sprintf(`{"taskVersion":%d}`, version))
			path := fmt.Sprintf("/v1/task-tests/by-task/%s/run-all", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().IntVar(&version, "version", 1, "task version to test against")
	return cmd
}

func makeTtByTaskCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "by-task <task-id>",
		Short: "List tests for a specific task",
		Long: `List all test cases attached to a workflow task by its ID. Shorthand for
listing with a taskId filter.`,
		Example: `  altscore task-tests by-task <task-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v1/task-tests/by-task/%s", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
