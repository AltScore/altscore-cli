package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/AltScore/altscore-cli/internal/version"
	"github.com/spf13/cobra"
)

// makeVersionCmd reports the CLI version, and with --backend the deployed
// backend build. Determining the deployed backend commit used to require
// gcloud spelunking; --backend hits a cheap GET /version endpoint instead.
func makeVersionCmd() *cobra.Command {
	var backend bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version (and with --backend, the deployed backend build)",
		Long: `Prints the altscore CLI version.

With --backend, also calls GET /version on Borrower Central and prints the
deployed service build as JSON: {"service","commit","builtAt"}. This avoids
gcloud spelunking to find which commit is live in the resolved environment.

If the backend does not expose /version (404), a clear message is printed to
stderr and the command exits non-zero -- the endpoint may not be deployed yet.`,
		Example: `  altscore version
  altscore version --backend
  altscore version --backend --environment staging`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !backend {
				return output.RawJSON(json.RawMessage(
					fmt.Sprintf(`{"cli":%q}`, version.Version)))
			}

			c, err := loadClient()
			if err != nil {
				return err
			}
			data, status, err := c.Do("GET", "borrower_central", "/version", nil)
			if err != nil {
				if status == 404 || strings.Contains(err.Error(), "HTTP 404") {
					return fmt.Errorf(
						"backend GET /version returned 404 -- the deployed backend may not expose this endpoint yet")
				}
				return fmt.Errorf("backend version check failed: %w", err)
			}
			if len(data) == 0 {
				return errors.New("backend GET /version returned an empty body")
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().BoolVar(&backend, "backend", false, "also query the backend's GET /version endpoint and print {service,commit,builtAt}")
	return cmd
}
