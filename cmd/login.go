package cmd

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const defaultLoginEnvironment = "production"

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate and create or update a profile",
	Long: `Authenticate with the AltScore API using OAuth2 client credentials and
save the result as a named profile.

Asks for three things: the profile alias (skipped when --profile is given),
the client id and the client secret. The tenant is read from the API after
authenticating: the credentials belong to exactly one tenant, so there is
nothing to choose and --tenant is refused here.

The environment is production. Pass --environment staging or
--environment sandbox to log in somewhere else. Logging in again to an
existing profile keeps its environment unless the flag says otherwise, and
offers the stored client id and secret as defaults: press Enter to keep them.`,
	Example: `  # Production; asks for the alias, client id and secret
  altscore login

  # Named profile; asks only for the client id and secret
  altscore login --profile la-fabril

  # Another environment
  altscore login --profile la-fabril-stg --environment staging`,
	RunE: runLogin,
}

func init() {
	rootCmd.AddCommand(loginCmd)
}

func runLogin(cmd *cobra.Command, args []string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("login requires an interactive terminal")
	}
	if flagTenant != "" {
		return fmt.Errorf("--tenant does not apply to login: the tenant comes from the credentials and is read from the API after authenticating")
	}
	// Checked before any prompt, so a typo fails before the secret is typed.
	if flagEnvironment != "" {
		if _, err := client.GetBaseURLs(flagEnvironment); err != nil {
			return err
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	profileName := flagProfile
	if profileName == "" {
		profileName, err = promptProfileAlias()
		if err != nil {
			return err
		}
	}

	existing, updating := cfg.Profiles[profileName]
	environment := loginEnvironment(flagEnvironment, existing.Environment)
	if updating {
		fmt.Fprintf(os.Stderr, "Updating profile %q (%s). Press Enter to keep a stored value.\n", profileName, describeProfileTarget(existing))
	}

	// The client id is an identifier, not a secret (profiles show prints it),
	// so it echoes: a pasted value can be checked before it is sent.
	clientID, err := promptWithDefault("client id", existing.ClientID)
	if err != nil {
		return err
	}
	if clientID == "" {
		return fmt.Errorf("client id is required")
	}

	// A stored secret only belongs to the stored client id.
	secretDefault := ""
	if clientID == existing.ClientID {
		secretDefault = existing.ClientSecret
	}
	secret, err := promptSecretWithDefault("client secret", secretDefault)
	if err != nil {
		return err
	}
	if secret == "" {
		return fmt.Errorf("client secret is required")
	}

	authURL, err := client.ModuleURL(environment, "auth")
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Authenticating against %s...\n", environment)
	token, err := client.Authenticate(authURL, clientID, secret)
	if err != nil {
		return err
	}

	detected, detErr := detectTenant(environment, token)
	if detErr != nil || detected == "" {
		if claim := tenantFromToken(token); claim != "" {
			detected, detErr = claim, nil
		}
	}
	tenantID, warning := resolveLoginTenant(detected, detErr, clientID, existing)
	if warning != "" {
		fmt.Fprintln(os.Stderr, "warning: "+warning)
	}
	if updating && existing.TenantID != "" && tenantID != "" && tenantID != existing.TenantID {
		fmt.Fprintf(os.Stderr, "Profile %q now points at tenant %q (was %q).\n", profileName, tenantID, existing.TenantID)
	}

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

	target := environment
	if tenantID != "" {
		target = fmt.Sprintf("tenant %q on %s", tenantID, environment)
	}
	fmt.Fprintf(os.Stderr, "Logged in to %s. Profile %q saved.\n", target, profileName)
	if cfg.DefaultProfile != profileName {
		arg := profileName
		if strings.ContainsAny(arg, " \t") {
			arg = fmt.Sprintf("%q", arg)
		}
		fmt.Fprintf(os.Stderr, "Use it with --profile %s, or make it the default: altscore profiles set-default %s\n", arg, arg)
	}
	return nil
}

// loginEnvironment picks the environment a login authenticates against: the
// --environment flag, else the profile's current one (so re-logging a staging
// profile stays on staging), else production.
func loginEnvironment(flagValue, existing string) string {
	if flagValue != "" {
		return flagValue
	}
	if existing != "" {
		return existing
	}
	return defaultLoginEnvironment
}

// resolveLoginTenant returns the tenant to store after a successful
// authentication, plus a warning when it could not be read from the API. A
// failed read keeps the stored tenant only when the client id is unchanged,
// since a tenant belongs to the credentials and not to the alias.
func resolveLoginTenant(detected string, detErr error, clientID string, existing config.Profile) (string, string) {
	if detErr == nil && detected != "" {
		return detected, ""
	}
	reason := "the API returned no tenant"
	if detErr != nil {
		reason = detErr.Error()
	}
	if clientID == existing.ClientID && existing.TenantID != "" {
		return existing.TenantID, fmt.Sprintf("could not read the tenant from the API (%s); keeping %q", reason, existing.TenantID)
	}
	return "", fmt.Sprintf("could not read the tenant from the API (%s); the profile is saved without one", reason)
}

// promptProfileAlias has no default on purpose: offering the default profile
// made Enter overwrite its credentials with another tenant's.
func promptProfileAlias() (string, error) {
	for {
		alias, err := promptWithDefault("profile alias", "")
		if err != nil {
			return "", err
		}
		if alias != "" {
			return alias, nil
		}
		fmt.Fprintln(os.Stderr, "a profile alias is required (e.g. the tenant's short name)")
	}
}

func describeProfileTarget(p config.Profile) string {
	env := p.Environment
	if env == "" {
		env = defaultLoginEnvironment
	}
	if p.TenantID == "" {
		return env
	}
	return fmt.Sprintf("tenant %s on %s", p.TenantID, env)
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return parseTenantResponse(body)
}

// tenantFromToken reads the tenantId claim of the access token, the same claim
// the Python SDK reads. It is the fallback when the API read fails. The
// signature is not checked: the token came straight from the auth server and
// the tenant is only stored on the profile.
func tenantFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		TenantID string `json:"tenantId"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.TenantID
}

// parseTenantResponse reads the tenant id out of GET /v1/application/tenant.
// Borrower Central passes Frontegg's tenant record through unchanged, which is
// an ARRAY of tenants (one for client credentials); decoding it as an object
// failed on every login, so the tenant was never detected. A bare object is
// still accepted in case the endpoint starts unwrapping it.
func parseTenantResponse(body []byte) (string, error) {
	type tenant struct {
		TenantID string `json:"tenantId"`
	}
	var list []tenant
	if err := json.Unmarshal(body, &list); err == nil {
		for _, t := range list {
			if t.TenantID != "" {
				return t.TenantID, nil
			}
		}
		return "", nil
	}
	var single tenant
	if err := json.Unmarshal(body, &single); err != nil {
		return "", fmt.Errorf("cannot parse tenant response: %w", err)
	}
	return single.TenantID, nil
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
