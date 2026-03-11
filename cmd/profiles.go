package cmd

import (
	"fmt"
	"os"
	"sort"

	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

var profilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "Manage named profiles",
	Long: `Manage named profiles stored in ~/.config/altscore/config.toml.

Each profile contains credentials and settings for a specific AltScore
environment and tenant. Use 'altscore login --profile <name>' to create
profiles, then switch between them with --profile or set-default.`,
}

var profilesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configured profiles",
	Long: `List all profiles stored in the config file.

The default profile is marked with an asterisk (*). Each profile shows
its environment and tenant ID.`,
	Example: `  altscore profiles list`,
	RunE:    runProfilesList,
}

var profilesShowCmd = &cobra.Command{
	Use:   "show [name]",
	Short: "Show details of a profile",
	Long: `Show details of a profile. Secrets are redacted in the output.

If no name is given, shows the default profile.`,
	Example: `  # Show default profile
  altscore profiles show

  # Show a specific profile
  altscore profiles show prod`,
	Args: cobra.MaximumNArgs(1),
	RunE: runProfilesShow,
}

var profilesSetDefaultCmd = &cobra.Command{
	Use:   "set-default <name>",
	Short: "Set the default profile",
	Long: `Set which profile is used when no --profile flag is given.

The default profile is used for all commands unless overridden by
--profile flag or the ALTSCORE_PROFILE environment variable.`,
	Example: `  altscore profiles set-default prod`,
	Args:    cobra.ExactArgs(1),
	RunE:    runProfilesSetDefault,
}

var profilesDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Remove a profile",
	Long: `Remove a profile from the config file.

This deletes the stored credentials and settings for the named profile.
If the deleted profile was the default, default_profile is cleared.`,
	Example: `  altscore profiles delete old-staging`,
	Args:    cobra.ExactArgs(1),
	RunE:    runProfilesDelete,
}

func init() {
	profilesCmd.AddCommand(profilesListCmd)
	profilesCmd.AddCommand(profilesShowCmd)
	profilesCmd.AddCommand(profilesSetDefaultCmd)
	profilesCmd.AddCommand(profilesDeleteCmd)
	rootCmd.AddCommand(profilesCmd)
}

func runProfilesList(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if len(cfg.Profiles) == 0 {
		fmt.Fprintln(os.Stderr, "No profiles configured. Run: altscore login --profile <name> --environment <env>")
		return nil
	}

	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	type profileEntry struct {
		Name        string `json:"name"`
		Environment string `json:"environment"`
		TenantID    string `json:"tenant_id,omitempty"`
		Default     bool   `json:"default"`
	}

	entries := make([]profileEntry, 0, len(names))
	for _, name := range names {
		p := cfg.Profiles[name]
		entries = append(entries, profileEntry{
			Name:        name,
			Environment: p.Environment,
			TenantID:    p.TenantID,
			Default:     name == cfg.DefaultProfile,
		})
	}

	return output.JSON(entries)
}

func runProfilesShow(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	name := config.ResolveProfile(cfg, flagProfile)
	if len(args) > 0 {
		name = args[0]
	}

	p, ok := cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}

	redacted := struct {
		Name         string `json:"name"`
		Environment  string `json:"environment"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		TenantID     string `json:"tenant_id,omitempty"`
		HasToken     bool   `json:"has_token"`
		Default      bool   `json:"default"`
	}{
		Name:         name,
		Environment:  p.Environment,
		ClientID:     p.ClientID,
		ClientSecret: redact(p.ClientSecret),
		TenantID:     p.TenantID,
		HasToken:     p.AccessToken != "",
		Default:      name == cfg.DefaultProfile,
	}

	return output.JSON(redacted)
}

func runProfilesSetDefault(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if _, ok := cfg.Profiles[name]; !ok {
		return fmt.Errorf("profile %q not found", name)
	}

	cfg.DefaultProfile = name
	if err := config.Save(cfg); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Default profile set to %q.\n", name)
	return nil
}

func runProfilesDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if _, ok := cfg.Profiles[name]; !ok {
		return fmt.Errorf("profile %q not found", name)
	}

	delete(cfg.Profiles, name)
	if cfg.DefaultProfile == name {
		cfg.DefaultProfile = ""
	}

	if err := config.Save(cfg); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Profile %q deleted.\n", name)
	return nil
}

func redact(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}
