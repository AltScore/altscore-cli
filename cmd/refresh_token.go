package cmd

import (
	"fmt"
	"os"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/spf13/cobra"
)

var refreshTokenCmd = &cobra.Command{
	Use:   "refresh-token",
	Short: "Force a new access token for the current profile",
	Long: `Force a new access token by re-authenticating with stored credentials.

Useful when your permissions have changed server-side but your existing
token still carries old claims. Since the token is still valid, the
automatic refresh on 401 never triggers — this command forces a new one.

No interactive prompts needed; it reuses the client_id and client_secret
already stored in the profile.`,
	Example: `  # Refresh token for the default profile
  altscore refresh-token

  # Refresh token for a specific profile
  altscore refresh-token --profile prod`,
	RunE: runRefreshToken,
}

func init() {
	rootCmd.AddCommand(refreshTokenCmd)
}

func runRefreshToken(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	profileName := config.ResolveProfile(cfg, flagProfile)
	profile := config.GetProfile(cfg, profileName)

	if profile.ClientID == "" || profile.ClientSecret == "" {
		return fmt.Errorf("profile %q has no stored credentials. Run: altscore login --profile %s", profileName, profileName)
	}

	if profile.Environment == "" {
		return fmt.Errorf("no environment configured for profile %q. Run: altscore login --profile %s", profileName, profileName)
	}

	authURL, err := client.ModuleURL(profile.Environment, "auth")
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Refreshing token for profile %q (%s)...\n", profileName, profile.Environment)

	token, err := client.Authenticate(authURL, profile.ClientID, profile.ClientSecret)
	if err != nil {
		return err
	}

	// Update only the access token in the stored profile
	stored := cfg.Profiles[profileName]
	stored.AccessToken = token
	cfg.Profiles[profileName] = stored

	if err := config.Save(cfg); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Token refreshed for profile %q.\n", profileName)
	return nil
}
