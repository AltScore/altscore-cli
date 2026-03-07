package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func makeWtPublishCmd() *cobra.Command {
	var version int

	cmd := &cobra.Command{
		Use:   "publish <id>",
		Short: "Publish a workflow task version",
		Long: `Publish a specific version of a workflow task, making it available
for use in workflow DAG execution.`,
		Example: `  altscore workflow-tasks publish <id>
  altscore workflow-tasks publish <id> --version 2`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body := json.RawMessage(fmt.Sprintf(`{"version":%d}`, version))
			data, _, err := c.Do("POST", "borrower_central", "/v1/workflow-tasks/"+args[0]+"/publish", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().IntVar(&version, "version", 1, "task version to publish")
	return cmd
}

func makeWtUnpublishCmd() *cobra.Command {
	var version int

	cmd := &cobra.Command{
		Use:   "unpublish <id>",
		Short: "Unpublish a workflow task version",
		Long:  `Unpublish a specific version of a workflow task, removing it from DAG execution.`,
		Example: `  altscore workflow-tasks unpublish <id>
  altscore workflow-tasks unpublish <id> --version 2`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body := json.RawMessage(fmt.Sprintf(`{"version":%d}`, version))
			data, _, err := c.Do("POST", "borrower_central", "/v1/workflow-tasks/"+args[0]+"/unpublish", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().IntVar(&version, "version", 1, "task version to unpublish")
	return cmd
}

func makeWtVersionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "versions <id>",
		Short:   "List versions of a workflow task",
		Long:    `List all versions of a workflow task with their publish status.`,
		Example: `  altscore workflow-tasks versions <id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", "borrower_central", "/v1/workflow-tasks/"+args[0]+"/versions", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeWtValidateCmd() *cobra.Command {
	var bodyFlag string
	var updateTask bool
	var taskAlias string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate workflow task code",
		Long: `Validate Python code structure and extract input/output schemas from
Pydantic models. Optionally update the task's schemas in one call.

Pass the code in --body as {"code": "..."}.  Use --update-task and --task-alias
to validate AND update the task's schemas.`,
		Example: `  # Validate only
  altscore workflow-tasks validate --body '{"code": "from pydantic import BaseModel\n..."}'

  # Validate and update task schemas
  altscore workflow-tasks validate --body '{"code": "..."}' --update-task --task-alias my-task`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			if updateTask || taskAlias != "" {
				var m map[string]any
				if err := json.Unmarshal(body, &m); err != nil {
					return fmt.Errorf("invalid JSON body: %w", err)
				}
				if updateTask {
					m["updateTask"] = true
				}
				if taskAlias != "" {
					m["taskAlias"] = taskAlias
				}
				body, err = json.Marshal(m)
				if err != nil {
					return fmt.Errorf("cannot re-encode body: %w", err)
				}
			}

			data, _, err := c.Do("POST", "borrower_central", "/v1/workflow-tasks/validate-code", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", `JSON body with "code" field (or pipe via stdin)`)
	cmd.Flags().BoolVar(&updateTask, "update-task", false, "also update the task's schemas after validation")
	cmd.Flags().StringVar(&taskAlias, "task-alias", "", "task alias to update (required with --update-task)")
	return cmd
}

func makeWtExecuteCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "execute <id> <version>",
		Short: "Execute a workflow task directly",
		Long: `Execute a saved workflow task by ID and version number. Used to test
individual tasks outside of a workflow DAG.`,
		Example: `  altscore workflow-tasks execute <id> 1 --body '{"inputData": {"x": 5}, "context": {}}'`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/workflow-tasks/%s/%s/execute", args[0], args[1])
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

func makeWtLambdaCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "lambda",
		Short: "Execute inline task code",
		Long: `Execute inline Python code without creating a saved task. Useful for
quick experiments and one-off testing.`,
		Example: `  altscore workflow-tasks lambda --body '{"code": "...", "inputData": {"x": 5}, "context": {}}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			data, _, err := c.Do("POST", "borrower_central", "/v1/workflow-tasks/lambda/execute", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}
