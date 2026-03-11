package cmd

import (
	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Show the active configuration",
	Long: `Show the fully resolved configuration for the active profile.

This displays which profile is active, what environment and tenant are
in effect, and how they were resolved (flags, env vars, or config file).
Secrets are redacted in the output.`,
	Example: `  # Show config for default profile
  altscore config

  # Show config for a specific profile
  altscore config --profile prod

  # Show config with environment override
  altscore config --environment production`,
	RunE: runConfig,
}

func init() {
	rootCmd.AddCommand(configCmd)
}

func runConfig(cmd *cobra.Command, args []string) error {
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

	configPath, _ := config.Path()

	resolved := struct {
		ConfigFile   string `json:"config_file"`
		Profile      string `json:"profile"`
		Environment  string `json:"environment"`
		TenantID     string `json:"tenant_id,omitempty"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		HasToken     bool   `json:"has_token"`
		PerPage      int    `json:"per_page"`
	}{
		ConfigFile:   configPath,
		Profile:      profileName,
		Environment:  profile.Environment,
		TenantID:     profile.TenantID,
		ClientID:     profile.ClientID,
		ClientSecret: redact(profile.ClientSecret),
		HasToken:     profile.AccessToken != "",
		PerPage:      cfg.Defaults.PerPage,
	}

	return output.JSON(resolved)
}
