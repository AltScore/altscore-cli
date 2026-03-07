package cmd

import (
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func makeWfExecuteCmd() *cobra.Command {
	var bodyFlag string
	var async bool
	var tags string

	cmd := &cobra.Command{
		Use:   "execute <id>",
		Short: "Execute a workflow by ID",
		Long: `Execute a workflow by its ID. Pass the workflow input as --body.

By default execution is synchronous. Use --async to return immediately
with an execution ID. Use --tags to tag the execution for filtering.`,
		Example: `  altscore workflows execute <id> --body '{"income": 5000}'
  altscore workflows execute <id> --body '{"income": 5000}' --async
  altscore workflows execute <id> --body '{"income": 5000}' --tags "test,poc"`,
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

			headers := map[string]string{}
			if async {
				headers["X-Execution-Mode"] = "async"
			}
			if tags != "" {
				headers["X-Tags"] = tags
			}

			path := fmt.Sprintf("/v1/workflows/%s/execute", args[0])
			data, _, err := c.DoWithHeaders("POST", "borrower_central", path, body, headers)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	cmd.Flags().BoolVar(&async, "async", false, "execute asynchronously (returns execution ID immediately)")
	cmd.Flags().StringVar(&tags, "tags", "", "comma-separated tags for the execution (sets X-Tags header)")
	return cmd
}

func makeWfExecuteByAliasCmd() *cobra.Command {
	var bodyFlag string
	var async bool
	var tags string

	cmd := &cobra.Command{
		Use:   "execute-by-alias <alias> <version>",
		Short: "Execute a workflow by alias and version",
		Long: `Execute a workflow by its alias and version string (e.g. "v1"). Pass the
workflow input as --body. Supports the same --async and --tags flags.`,
		Example: `  altscore workflows execute-by-alias my-workflow v1 --body '{"income": 5000}'
  altscore workflows execute-by-alias my-workflow v1 --body '{"income": 5000}' --async`,
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

			headers := map[string]string{}
			if async {
				headers["X-Execution-Mode"] = "async"
			}
			if tags != "" {
				headers["X-Tags"] = tags
			}

			path := fmt.Sprintf("/v1/workflows/%s/%s/execute", args[0], args[1])
			data, _, err := c.DoWithHeaders("POST", "borrower_central", path, body, headers)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	cmd.Flags().BoolVar(&async, "async", false, "execute asynchronously")
	cmd.Flags().StringVar(&tags, "tags", "", "comma-separated tags (sets X-Tags header)")
	return cmd
}

func makeWfUpdateSchemaCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "update-schema <id>",
		Short: "Update a workflow's input schema",
		Long: `Update the input schema for a workflow. The body should contain an
inputSchema field with a JSON Schema string.`,
		Example: `  altscore workflows update-schema <id> --body '{"inputSchema": "{\"type\":\"object\",\"properties\":{\"income\":{\"type\":\"number\"}}}"}'`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/workflows/%s/input-schema", args[0])
			data, _, err := c.Do("PATCH", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}
