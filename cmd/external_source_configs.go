package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// registerExternalSourceConfigs wires the "external-source-configs" command
// group: the standard list/get/create/delete actions via registerResource, plus
// a custom update (the backend uses PUT with partial-merge semantics, not the
// generic builder's PATCH) and the two operational endpoints test-auth and
// test-request.
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
}

// makeExternalSourceConfigUpdateCmd implements `update <id>` against the
// resource's PUT endpoint. The backend applies exclude_unset, so omitted fields
// are left untouched -- callers can send partial bodies safely.
func makeExternalSourceConfigUpdateCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update an external-source-config",
		Long: `Update an external-source-config by ID. Pass a partial JSON body via
--body or stdin. The backend merges the body onto the existing record, so
omitted fields are left untouched.

Request body fields (all optional):
  label: string       Human-readable label
  country: string     Country code
  status: string      "active" | "inactive"
  settings: object    Provider-specific config`,
		Example: `  altscore external-source-configs update <id> --body '{"status":"active"}'
  echo '{"settings":{"apiKey":"..."}}' | altscore external-source-configs update <id>`,
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
