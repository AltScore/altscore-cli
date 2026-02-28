package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	loginEnvironment string
	loginClientID    string
	loginSecret      string
	loginTenantID    string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate and create or update a profile",
	Long: `Authenticate with the AltScore API using OAuth2 client credentials.

This command creates or updates a named profile in the config file with
the provided credentials and environment. It exchanges the credentials
for an access token and stores everything for future use.

If --client-id and --client-secret are not provided, they will be read
from environment variables (ALTSCORE_CLIENT_ID, ALTSCORE_CLIENT_SECRET)
or prompted interactively.

The profile name defaults to "default" unless --profile is specified.
Use --environment to set which AltScore environment to target.`,
	Example: `  # Interactive login, creates "default" profile
  altscore login --environment staging

  # Login with a named profile
  altscore login --profile prod --environment production

  # Non-interactive login (CI/CD)
  altscore login --profile staging --environment staging \
    --client-id abc123 --client-secret secret... --tenant-id tenant-uuid

  # Login using environment variables
  ALTSCORE_CLIENT_ID=abc ALTSCORE_CLIENT_SECRET=secret \
    altscore login --profile staging --environment staging`,
	RunE: runLogin,
}

func init() {
	loginCmd.Flags().StringVar(&loginEnvironment, "environment", "", "target environment (production, staging, sandbox) [required]")
	loginCmd.Flags().StringVar(&loginClientID, "client-id", "", "OAuth2 client ID (or set ALTSCORE_CLIENT_ID)")
	loginCmd.Flags().StringVar(&loginSecret, "client-secret", "", "OAuth2 client secret (or set ALTSCORE_CLIENT_SECRET)")
	loginCmd.Flags().StringVar(&loginTenantID, "tenant-id", "", "tenant ID for multi-tenant access")
	rootCmd.AddCommand(loginCmd)
}

func runLogin(cmd *cobra.Command, args []string) error {
	if loginEnvironment == "" {
		return fmt.Errorf("--environment is required (production, staging, sandbox)")
	}

	// Validate environment
	if _, err := client.GetBaseURLs(loginEnvironment); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	profileName := config.ResolveProfile(cfg, flagProfile)
	if flagProfile != "" {
		profileName = flagProfile
	} else if profileName == "" {
		profileName = "default"
	}

	// Resolve client ID
	clientID := loginClientID
	if clientID == "" {
		clientID = os.Getenv("ALTSCORE_CLIENT_ID")
	}
	if clientID == "" {
		clientID, err = prompt("Client ID: ")
		if err != nil {
			return err
		}
	}

	// Resolve client secret
	secret := loginSecret
	if secret == "" {
		secret = os.Getenv("ALTSCORE_CLIENT_SECRET")
	}
	if secret == "" {
		secret, err = promptSecret("Client Secret: ")
		if err != nil {
			return err
		}
	}

	// Resolve tenant ID
	tenantID := loginTenantID
	if tenantID == "" {
		existing := cfg.Profiles[profileName]
		if existing.TenantID != "" {
			tenantID = existing.TenantID
			fmt.Fprintf(os.Stderr, "Using existing tenant ID: %s\n", tenantID)
		} else {
			tenantID, err = prompt("Tenant ID (optional, press Enter to skip): ")
			if err != nil {
				return err
			}
		}
	}

	// Authenticate
	authURL, err := client.ModuleURL(loginEnvironment, "auth")
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Authenticating against %s...\n", loginEnvironment)
	token, err := client.Authenticate(authURL, clientID, secret)
	if err != nil {
		return err
	}

	// Save profile
	cfg.Profiles[profileName] = config.Profile{
		Environment:  loginEnvironment,
		ClientID:     clientID,
		ClientSecret: secret,
		AccessToken:  token,
		TenantID:     tenantID,
	}

	if cfg.DefaultProfile == "" {
		cfg.DefaultProfile = profileName
	}

	if err := config.Save(cfg); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Logged in. Profile %q saved.\n", profileName)
	if cfg.DefaultProfile == profileName {
		fmt.Fprintf(os.Stderr, "This is the default profile.\n")
	}

	return nil
}

func prompt(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func promptSecret(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr) // newline after hidden input
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
