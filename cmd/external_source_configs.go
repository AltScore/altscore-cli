package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// registerExternalSourceConfigs wires the "external-source-configs" command
// group: the standard list/get/create/delete actions via registerResource, plus
// a custom update (the backend uses PUT with partial-merge semantics, not the
// generic builder's PATCH), the two operational endpoints test-auth and
// test-request, and the network pair (network-options, set-network).
func registerExternalSourceConfigs() {
	group := registerResource(ResourceDef{
		Name:     "external-source-configs",
		Singular: "external-source-config",
		BasePath: "/v1/external-source-configs",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "delete"},
		Description: `Manage external source configs (tenant-level wrappers around
AltData sources that add a label, status, and source-specific settings).

These configs back the "External Sources" page in the Hub settings. Each config
references an AltData sourceId and adds a human-readable label,
an active/inactive status, and a free-form settings object the runtime gateway
consumes (auth credentials, endpoint overrides, header maps, etc.).`,
		CreateSchema: `  sourceId: string    [required] AltData source ID this config wraps
  label: string       [required] Human-readable label
  country: string     Country code (optional)
  status: string      "active" | "inactive" (default: "inactive")
  settings: object    Provider-specific config (default: {})`,
		ResponseSchema: `  id, sourceId, label, country, status, settings,
  createdAt, updatedAt`,
	})

	group.AddCommand(makeExternalSourceConfigUpdateCmd())
	group.AddCommand(makeExternalSourceConfigTestAuthCmd())
	group.AddCommand(makeExternalSourceConfigTestRequestCmd())
	group.AddCommand(makeExternalSourceConfigNetworkOptionsCmd())
	group.AddCommand(makeExternalSourceConfigSetNetworkCmd())
}

// makeExternalSourceConfigUpdateCmd implements `update <id>` against the
// resource's PUT endpoint. The backend applies exclude_unset to the TOP-LEVEL
// fields only: `settings`, when sent, replaces the stored settings whole. So a
// partial settings object wipes every key it leaves out (URL, auth, ...); to
// change the network alone use set-network, which has its own endpoint.
func makeExternalSourceConfigUpdateCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update an external-source-config",
		Long: `Update an external-source-config by ID. Pass a JSON body via --body or
stdin. Omitted top-level fields are left untouched, but settings is NOT merged:
when sent, it REPLACES the stored settings whole, so send the complete object
(read it first with get). To change only the outbound network (AltScore proxy
by country), use set-network instead.

Request body fields (all optional):
  label: string       Human-readable label
  country: string     Country code
  status: string      "active" | "inactive"
  settings: object    Provider-specific config, complete (replaces the stored one)`,
		Example: `  altscore external-source-configs update <id> --body '{"status":"active"}'
  altscore external-source-configs get <id> | jq '{settings: (.settings + {request_timeout: 45})}' \
    | altscore external-source-configs update <id>`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			data, _, err := c.Do("PUT", "borrower_central", "/v1/external-source-configs/"+args[0], body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

// makeExternalSourceConfigTestAuthCmd hits POST /{id}/test-auth, which exercises
// the configured auth flow against the live provider and returns the gateway's
// diagnostic response.
func makeExternalSourceConfigTestAuthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test-auth <id>",
		Short: "Test the configured auth for an external-source-config",
		Long: `Run a live auth check against the provider using the config's stored
credentials. Returns the gateway's diagnostic response (success flag, status
code, message, masked details).`,
		Example: `  altscore external-source-configs test-auth <id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := "/v1/external-source-configs/" + args[0] + "/test-auth"
			data, _, err := c.Do("POST", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// makeExternalSourceConfigTestRequestCmd hits POST /{id}/test-request with the
// caller-supplied input keys, exercising the full request pipeline.
func makeExternalSourceConfigTestRequestCmd() *cobra.Command {
	var inputKeysFlag string

	cmd := &cobra.Command{
		Use:   "test-request <id>",
		Short: "Run a sample request through an external-source-config",
		Long: `Execute a sample request against the provider using the config and the
caller-supplied input keys. Returns the gateway's full response envelope.

Pass the input keys as a JSON object via --input-keys or via stdin. The body
sent to the backend is { "inputKeys": <your-object> }.`,
		Example: `  altscore external-source-configs test-request <id> --input-keys '{"taxId":"..."}'
  echo '{"taxId":"..."}' | altscore external-source-configs test-request <id>`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			// Reuse the same JSON-from-flag-or-stdin reader; here the user
			// supplies the inputKeys object directly and we wrap it.
			inputKeys, err := readBody(inputKeysFlag)
			if err != nil {
				return fmt.Errorf("provide inputKeys via --input-keys or stdin: %w", err)
			}

			wrapped, err := json.Marshal(map[string]json.RawMessage{"inputKeys": inputKeys})
			if err != nil {
				return err
			}

			path := "/v1/external-source-configs/" + args[0] + "/test-request"
			data, _, err := c.Do("POST", "borrower_central", path, wrapped)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&inputKeysFlag, "input-keys", "", "inputKeys JSON object (or pipe via stdin)")
	return cmd
}

// makeExternalSourceConfigNetworkOptionsCmd lists what settings.network accepts:
// the modes and the countries AltScore's proxy can leave from.
func makeExternalSourceConfigNetworkOptionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "network-options",
		Short: "List the outbound network modes and proxy countries a source can use",
		Long: `List what settings.network accepts: the modes ("direct", "altscore_proxy")
and the countries AltScore's proxy can leave from (ISO codes, plus ANY).`,
		Example: `  altscore external-source-configs network-options`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", "borrower_central", "/v1/external-source-configs/network-options", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// networkBody builds the PUT /{id}/network body from set-network's flags.
func networkBody(direct bool, country string) ([]byte, error) {
	country = strings.TrimSpace(country)
	switch {
	case direct && country != "":
		return nil, errors.New("--direct and --country are mutually exclusive")
	case direct:
		return json.Marshal(map[string]string{"mode": "direct"})
	case country != "":
		return json.Marshal(map[string]string{"mode": "altscore_proxy", "country": strings.ToUpper(country)})
	default:
		return nil, errors.New("pass --country <code> to go out through AltScore's proxy, or --direct to stop")
	}
}

func setExternalSourceNetwork(c *client.Client, id string, body []byte) ([]byte, error) {
	data, _, err := c.Do("PUT", "borrower_central", "/v1/external-source-configs/"+id+"/network", body)
	return data, err
}

// makeExternalSourceConfigSetNetworkCmd implements `set-network <id>` against
// PUT /{id}/network, which changes settings.network and keeps every other
// setting as stored (unlike update, which replaces settings whole).
func makeExternalSourceConfigSetNetworkCmd() *cobra.Command {
	var country string
	var direct bool

	cmd := &cobra.Command{
		Use:   "set-network <id>",
		Short: "Route an external-source-config through AltScore's proxy by country, or direct",
		Long: `Set how the source reaches its provider, leaving every other setting as stored.

--country <code>  every request of each call (OAuth token, request, retry)
                  goes out through AltScore's proxy with an IP from that
                  country. Codes: see network-options (e.g. EC, MX, ANY).
                  Each call through the proxy is billed as usage (TOOL-0009),
                  test-request included.
--direct          back to our own egress (the default for a new source).

An unsupported country is refused (400 INVALID_SOURCE_NETWORK) and nothing is saved.`,
		Example: `  altscore external-source-configs set-network <id> --country EC
  altscore external-source-configs set-network <id> --direct`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := networkBody(direct, country)
			if err != nil {
				return err
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, err := setExternalSourceNetwork(c, args[0], body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&country, "country", "", "Exit country of AltScore's proxy (ISO code or ANY)")
	cmd.Flags().BoolVar(&direct, "direct", false, "Stop using the proxy: go out direct")
	return cmd
}
