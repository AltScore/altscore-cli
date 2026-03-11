package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var helpTopics = map[string]string{
	"auth": `# Authentication

altscore uses OAuth2 client credentials for authentication.

## Login

  altscore login --profile <name> --environment <env>

This prompts for client_id and client_secret (or reads them from
ALTSCORE_CLIENT_ID and ALTSCORE_CLIENT_SECRET environment variables),
exchanges them for an access token, and stores everything in the
named profile.

## Token Refresh

Access tokens are cached in the profile. When a request returns
HTTP 401, the CLI automatically re-authenticates using the stored
client credentials and retries the request once. The new token is
saved to the profile.

## Environment Variables

These override values from the resolved profile:

  ALTSCORE_PROFILE         Profile to use (same as --profile)
  ALTSCORE_CLIENT_ID       Override client ID
  ALTSCORE_CLIENT_SECRET   Override client secret
  ALTSCORE_ENVIRONMENT     Override environment

## Exporting Credentials

Export the active profile as a .env file for the AltScore Python SDK:

  altscore env > .env
  altscore env --profile staging > .env.staging

Output variables:

  ALTSCORE_CLIENT_ID       Client credentials ID
  ALTSCORE_CLIENT_SECRET   Client credentials secret
  ALTSCORE_USER_TOKEN      Current access token
  ALTSCORE_ENVIRONMENT     Environment name
  ALTSCORE_TENANT          Tenant ID

The profile name is printed as a comment to stderr (not included in
the .env file). Supports --profile, --environment, and --tenant flags.

## Auth Endpoints

  production:  https://auth.altscore.ai/oauth/token
  staging:     https://auth.stg.altscore.ai/oauth/token
  sandbox:     https://auth.sandbox.altscore.ai/oauth/token`,

	"profiles": `# Named Profiles

Profiles store credentials and settings for different AltScore
environments or tenants, similar to AWS CLI profiles.

## Config File

  ~/.config/altscore/config.toml

## Profile Resolution Order (highest priority first)

  1. --profile flag
  2. ALTSCORE_PROFILE environment variable
  3. default_profile in config.toml
  4. Falls back to "default"

## Managing Profiles

  altscore login --profile staging --environment staging
  altscore login --profile prod --environment production
  altscore profiles list
  altscore profiles show staging
  altscore profiles set-default prod
  altscore profiles delete old-profile

## Per-Command Overrides

  --environment overrides the profile's environment
  --tenant overrides the profile's tenant_id

Example:
  altscore borrowers list --profile prod --tenant other-tenant-id`,

	"filtering": `# Filtering

Use --filter to apply field-based filters to list commands.

## Syntax

  --filter key=value

Multiple filters can be applied:

  altscore borrowers list --filter persona=individual --filter status=active

Filters are passed as query parameters to the API. The exact filter
keys available depend on the resource. Common filters include:

  persona        Borrower persona (individual, company)
  status         Resource status
  label          Text label or name
  created-after  ISO 8601 datetime

## Combining with Pagination

  altscore borrowers list --filter persona=individual --per-page 10 --page 2`,

	"pagination": `# Pagination

List commands support pagination with --per-page and --page.

## Flags

  --per-page N   Number of items per page (default: from config, usually 100)
  --page N       Page number (default: 1)

## Examples

  altscore borrowers list --per-page 10
  altscore borrowers list --per-page 50 --page 3

## Default Per-Page

The default per_page value comes from the config file:

  [defaults]
  per_page = 100

Override it per-command with --per-page.`,

	"output": `# Output Format

All commands output JSON to stdout. Status messages and errors go
to stderr. This makes it easy to pipe output to jq or other tools.

## Examples

  # Pretty-print a single borrower
  altscore borrowers get <id>

  # Extract just the IDs from a list
  altscore borrowers list | jq '.[].id'

  # Count results
  altscore borrowers list | jq 'length'

  # Save to file
  altscore borrowers list > borrowers.json

  # Filter in jq
  altscore borrowers list | jq '[.[] | select(.persona == "individual")]'

## Verbose Mode

  altscore borrowers list --verbose

Prints HTTP method, URL, and response status to stderr without
affecting the JSON output on stdout.`,
}

var topicsCmd = &cobra.Command{
	Use:   "topics [topic]",
	Short: "Detailed help on specific topics (auth, profiles, filtering, etc.)",
	Long: `Show detailed help on a specific topic.

Available topics:
  auth         How authentication and token refresh works
  profiles     How named profiles work and resolution order
  filtering    How --filter flags work across resources
  pagination   How --per-page and --page work
  output       JSON output format, piping to jq`,
	Example: `  altscore topics auth
  altscore topics profiles
  altscore topics filtering`,
	Args:      cobra.MaximumNArgs(1),
	ValidArgs: []string{"auth", "profiles", "filtering", "pagination", "output"},
	RunE:      runHelp,
}

func init() {
	rootCmd.AddCommand(topicsCmd)
}

func runHelp(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}

	topic := args[0]
	content, ok := helpTopics[topic]
	if !ok {
		return fmt.Errorf("unknown help topic %q. Available: auth, profiles, filtering, pagination, output", topic)
	}

	fmt.Println(content)
	return nil
}
