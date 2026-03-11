package cmd

import (
	"fmt"
	"os"

	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/spf13/cobra"
)

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Print current profile credentials as env vars",
	Long: `Output the active profile's credentials as environment variable assignments
compatible with .env files and the AltScore Python SDK.

Writes to stdout so you can redirect to a file:

  altscore env > .env
  altscore env --profile prod > .env.prod

Or load directly into your shell:

  export $(altscore env | xargs)`,
	Example: `  altscore env
  altscore env --profile staging > .env
  altscore env --profile prod >> .env.prod`,
	RunE: runEnv,
}

func init() {
	rootCmd.AddCommand(envCmd)
}

func runEnv(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	profileName := config.ResolveProfile(cfg, flagProfile)
	profile := config.GetProfile(cfg, profileName)

	if flagEnvironment != "" {
		profile.Environment = flagEnvironment
	}
	if flagTenant != "" {
		profile.TenantID = flagTenant
	}

	fmt.Fprintf(os.Stderr, "# profile: %s\n", profileName)

	fmt.Printf("ALTSCORE_CLIENT_ID=%s\n", profile.ClientID)
	fmt.Printf("ALTSCORE_CLIENT_SECRET=%s\n", profile.ClientSecret)
	fmt.Printf("ALTSCORE_USER_TOKEN=%s\n", profile.AccessToken)
	fmt.Printf("ALTSCORE_ENVIRONMENT=%s\n", profile.Environment)
	fmt.Printf("ALTSCORE_TENANT=%s\n", profile.TenantID)

	return nil
}
