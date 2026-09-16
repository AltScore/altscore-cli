package cmd

import (
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// makePkContentCmd fetches a package's stored payload. `packages get` returns
// the envelope only (alias, source, tags, contentType); the extracted data a
// workflow author wants to see lives one path segment further, and until now
// the only way to read it was the raw `api GET` escape hatch.
func makePkContentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "content <package-id>",
		Short: "Get the stored content of a package",
		Long: `Retrieve the content (the extracted or enriched payload) of one package.

'packages get' returns the envelope: alias, borrowerId, sourceId, tags,
contentType. This command returns what the package holds, which is what a
package-io node reads at run time, so it is the fastest way to check field
names and real values before authoring a rule or a custom variable.`,
		Example: `  altscore packages content <package-id>
  altscore packages content <package-id> | jq 'keys'
  altscore packages list --filter borrower-id=<id> | jq -r '.[] | "\(.id)\t\(.alias)"'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v1/stores/packages/%s/content", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
