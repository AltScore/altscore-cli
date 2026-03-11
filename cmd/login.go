package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate and create or update a profile",
	Long: `Authenticate with the AltScore API using OAuth2 client credentials.

Walks you through each field with existing values shown as defaults in
brackets. Press Enter to accept a default.

The tenant ID is auto-detected from the API after authentication. You can
confirm or override the detected value when prompted.`,
	Example: `  # Interactive login
  altscore login

  # Login with a named profile
  altscore login --profile prod`,
	RunE: runLogin,
}

func init() {
	rootCmd.AddCommand(loginCmd)
}

func runLogin(cmd *cobra.Command, args []string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("login requires an interactive terminal")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Prompt profile
	defaultProfile := config.ResolveProfile(cfg, flagProfile)
	profileName, err := promptWithDefault("profile", defaultProfile)
	if err != nil {
		return err
	}

	// Load existing profile for defaults
	existing := cfg.Profiles[profileName]

	// Prompt environment
	envDefault := existing.Environment
	if envDefault == "" {
		envDefault = "production"
	}
	environment, err := promptEnvironment(envDefault)
	if err != nil {
		return err
	}

	// Prompt client id
	clientID, err := promptSecretWithDefault("client id", existing.ClientID)
	if err != nil {
		return err
	}
	if clientID == "" {
		return fmt.Errorf("client id is required")
	}

	// Prompt client secret
	secret, err := promptSecretWithDefault("client secret", existing.ClientSecret)
	if err != nil {
		return err
	}
	if secret == "" {
		return fmt.Errorf("client secret is required")
	}

	// Authenticate
	authURL, err := client.ModuleURL(environment, "auth")
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Authenticating against %s...\n", environment)
	token, err := client.Authenticate(authURL, clientID, secret)
	if err != nil {
		return err
	}

	// Auto-detect tenant
	tenantDefault := existing.TenantID
	if detected, detErr := detectTenant(environment, token); detErr == nil && detected != "" {
		tenantDefault = detected
	}

	tenantID, err := promptWithDefault("tenant", tenantDefault)
	if err != nil {
		return err
	}

	// Save profile
	cfg.Profiles[profileName] = config.Profile{
		Environment:  environment,
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
	return nil
}

// detectTenant calls GET /v1/application/tenant on Borrower Central to
// retrieve the tenant ID associated with the authenticated credentials.
func detectTenant(environment, token string) (string, error) {
	bcURL, err := client.ModuleURL(environment, "borrower_central")
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest("GET", bcURL+"/v1/application/tenant", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tenant detection returned HTTP %d", resp.StatusCode)
	}

	var result struct {
		TenantID string `json:"tenantId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.TenantID, nil
}

func promptWithDefault(label, defaultVal string) (string, error) {
	if defaultVal != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, defaultVal)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	val := strings.TrimSpace(line)
	if val == "" {
		return defaultVal, nil
	}
	return val, nil
}

func promptSecretWithDefault(label, existing string) (string, error) {
	if existing != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, redact(existing))
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	val := strings.TrimSpace(string(data))
	if val == "" {
		return existing, nil
	}
	return val, nil
}

func promptEnvironment(defaultVal string) (string, error) {
	for {
		val, err := promptWithDefault("environment", defaultVal)
		if err != nil {
			return "", err
		}
		if _, err := client.GetBaseURLs(val); err == nil {
			return val, nil
		}
		fmt.Fprintln(os.Stderr, "invalid environment. choose: production, staging, sandbox")
	}
}
