package cmd

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func makeSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema [resource]",
		Short: "Show API schemas for a resource (create body, response shape, query filters)",
		Long: `Query the schema registry to get exact field names and types for any CRUD resource.
v2 workflow and task shapes live in 'altscore workflows-v2 schema-guide'; asking
this command for tasks-v2 or workflows-v2 answers from there.

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

			raw, status, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				if section, ok := schemaGuideRedirects[strings.ToLower(firstArg(args))]; ok && status == http.StatusNotFound {
					return printSchemaGuideRedirect(c, cmd.ErrOrStderr(), args[0], section)
				}
				return err
			}
			return output.RawJSON(raw)
		},
	}
	cmd.Flags().String("action", "", "filter to specific action: create, update, response, or filters")
	return cmd
}

// v2 workflow and task shapes are served by the schema guide; the CRUD registry 404s them.
var schemaGuideRedirects = map[string]string{
	"tasks-v2": "tasks", "task-v2": "tasks", "tasks": "tasks", "task": "tasks",
	"workflows-v2": "", "workflow-v2": "", "nodes": "nodes", "edges": "edges",
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func printSchemaGuideRedirect(c *client.Client, stderr io.Writer, resource, section string) error {
	var args []string
	if section != "" {
		args = []string{section}
	}
	data, _, err := c.Do("GET", "borrower_central", wfv2SchemaGuidePath(args, false), nil)
	if err != nil {
		return err
	}
	command := strings.TrimSpace("altscore workflows-v2 schema-guide " + section)
	if section == "tasks" {
		index, err := compactTaskTypeIndex(data)
		if err == nil {
			fmt.Fprintf(stderr, "# %q is not in the schema registry (CRUD resources only); v2 task bodies are documented by '%s <type>'. Its type index:\n", resource, command)
			return output.JSON(index)
		}
	}
	fmt.Fprintf(stderr, "# %q is not in the schema registry (CRUD resources only); this is '%s':\n", resource, command)
	return output.RawJSON(data)
}
