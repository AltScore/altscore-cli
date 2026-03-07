package cmd

import (
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func makeSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema [resource]",
		Short: "Show API schemas for a resource (create body, response shape, query filters)",
		Long: `Query the schema registry to get exact field names and types for any resource.

When building workflow tasks that interact with the AltScore API, use this command
to get the correct field names before writing code.

Examples:
  altscore schema                              # list all resources
  altscore schema borrowers                    # full schema
  altscore schema borrowers --action create    # just create body fields
  altscore schema identities --action response # identity response shape`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := "/v1/meta/schemas"
			if len(args) > 0 {
				path += "?resource=" + args[0]
				action, _ := cmd.Flags().GetString("action")
				if action != "" {
					path += "&action=" + action
				}
			}

			raw, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(raw)
		},
	}
	cmd.Flags().String("action", "", "filter to specific action: create, update, response, or filters")
	return cmd
}
