package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// The `decisions` command group is a thin facade over /v1/data-models with
// entityType=decision baked in. Decision keys are not a separate resource on
// the backend -- they're stored as data-model records, the same registry that
// holds borrower fields, identities, and steps. But agents reading workflow
// specs don't expect to find "approve" / "reject" under data-models, and
// evaluation rules' `decisionKey` field has no validation: writing
// decisionKey:"REJECTED" when the tenant only has lowercase `reject`
// registered fails silently at execution time. Surfacing decisions as their
// own group makes the registry obvious and the lookup one command instead of
// two clicks of "did you mean entity-type or entityType, and what's a valid
// value anyway".
//
// Decision keys are referenced from:
//   - evaluation-rules.decisionKey (rule-tree task uses the first matching
//     rule's decisionKey as its outputVariable)
//   - executions/{id}/decisions (final or preliminary decisions emitted by
//     a workflow run, validated against the same registry at write time)

func makeDecisionsGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "decisions",
		Short: "Manage decision keys (workflow outcomes)",
		Long: `Manage tenant-level decision keys -- the outcomes a workflow can emit.

Decision keys live as data-model records with entityType=decision. They define
the vocabulary for evaluation-rules' decisionKey field, rule-tree task outputs,
and execution decisions (POST /v1/executions/{id}/decisions). A workflow that
writes a decisionKey not registered here is rejected at runtime.

This command group is a thin facade over /v1/data-models with entityType
filtered to "decision". Use 'data-models guide decision' for the full schema
and 'altscore executions get-decision <execution-id>' to read the decision a
specific execution emitted.`,
	}
	group.AddCommand(makeDecisionsListCmd())
	group.AddCommand(makeDecisionsGetCmd())
	group.AddCommand(makeDecisionsCreateCmd())
	group.AddCommand(makeDecisionsDeleteCmd())
	return group
}

func makeDecisionsListCmd() *cobra.Command {
	var perPage int
	var page int
	var search string
	var sortBy string
	var sortDirection string
	var includeTests bool
	var testOnly bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List decision keys configured for the tenant",
		Long: `List the tenant's decision keys (data-model records with entityType=decision).

Use --search for free-text search, --include-tests / --test-only for test-mode
records. Sort and pagination flags work the same as 'altscore data-models list'.`,
		Example: `  altscore decisions list
  altscore decisions list --search approve
  altscore decisions list --include-tests`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			params := []string{"entity-type=decision"}
			if includeTests && testOnly {
				return fmt.Errorf("--include-tests and --test-only are mutually exclusive")
			}
			if includeTests {
				params = append(params, "include-tests=true")
			}
			if testOnly {
				params = append(params, "test-only=true")
			}
			if perPage > 0 {
				params = append(params, fmt.Sprintf("per-page=%d", perPage))
			} else if c.Config.Defaults.PerPage > 0 {
				params = append(params, fmt.Sprintf("per-page=%d", c.Config.Defaults.PerPage))
			}
			if page > 0 {
				params = append(params, fmt.Sprintf("page=%d", page))
			}
			if search != "" {
				params = append(params, "search="+search)
			}
			if sortBy != "" {
				params = append(params, "sort-by="+sortBy)
			}
			if sortDirection != "" {
				params = append(params, "sort-direction="+sortDirection)
			}
			path := "/v1/data-models?" + strings.Join(params, "&")
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().IntVar(&perPage, "per-page", 0, "items per page (default: from config)")
	cmd.Flags().IntVar(&page, "page", 0, "page number")
	cmd.Flags().StringVar(&search, "search", "", "free-text search")
	cmd.Flags().StringVar(&sortBy, "sort-by", "", "sort field")
	cmd.Flags().StringVar(&sortDirection, "sort-direction", "", "asc | desc")
	cmd.Flags().BoolVar(&includeTests, "include-tests", false, "include test records")
	cmd.Flags().BoolVar(&testOnly, "test-only", false, "return only test records")
	return cmd
}

func makeDecisionsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get <id>",
		Short:   "Get a decision data-model record by ID",
		Long:    `Retrieve a single decision data-model record. Same as 'data-models get'; provided here for symmetry with 'list' / 'create' / 'delete'.`,
		Example: `  altscore decisions get <id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", "borrower_central", "/v1/data-models/"+args[0], nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeDecisionsCreateCmd() *cobra.Command {
	var key string
	var label string
	var bodyFlag string
	var isTest bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Register a new decision key",
		Long: `Create a decision data-model record. POST /v1/data-models with entityType=decision injected.

Decision keys are referenced verbatim from evaluation-rules.decisionKey and
from /v1/executions/{id}/decisions, so they are case-sensitive. Pick a stable
convention (e.g. all lowercase with hyphens or underscores) and stick to it.

The flag form (--key --label) is the easy path. For optional fields like
metadata or path, pass --body with a full JSON object; --key/--label/--is-test
override matching keys in the body when set.`,
		Example: `  # Quick create with the two required fields
  altscore decisions create --key approve --label "Approve"

  # With metadata via body
  altscore decisions create --body '{"key":"manual-review","label":"Manual review","metadata":{"color":"yellow"}}'

  # Mark as test
  altscore decisions create --key sandbox-approve --label "Sandbox approve" --is-test`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body := json.RawMessage("{}")
			if bodyFlag != "" || !isStdinTerminal() {
				rb, err := readBody(bodyFlag)
				if err != nil {
					return err
				}
				body = rb
			}

			body, err = jsonSetString(body, "entityType", "decision")
			if err != nil {
				return err
			}
			if key != "" {
				body, err = jsonForceSetString(body, "key", key)
				if err != nil {
					return err
				}
			}
			if label != "" {
				body, err = jsonForceSetString(body, "label", label)
				if err != nil {
					return err
				}
			}
			if isTest {
				body, err = jsonSetBool(body, "isTest", true)
				if err != nil {
					return err
				}
			}

			if !bodyHasKey(body, "key") || !bodyHasKey(body, "label") {
				return fmt.Errorf("decisions create requires --key and --label (or both fields in --body)")
			}

			data, _, err := c.Do("POST", "borrower_central", "/v1/data-models", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "decision key (e.g. approve, reject, manual-review)")
	cmd.Flags().StringVar(&label, "label", "", "human-readable label")
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body for optional fields (metadata, path)")
	cmd.Flags().BoolVar(&isTest, "is-test", false, "mark as a test record")
	return cmd
}

func makeDecisionsDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <id>",
		Short:   "Delete a decision key",
		Long:    `Delete a decision data-model record by ID. Same as 'data-models delete <id>'.`,
		Example: `  altscore decisions delete <id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			_, status, err := c.Do("DELETE", "borrower_central", "/v1/data-models/"+args[0], nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Deleted (HTTP %d).\n", status)
			return nil
		},
	}
}

// jsonForceSetString unconditionally writes a key into a JSON object body,
// overwriting any existing value. Used for explicit flags (--key, --label)
// that must take precedence over body values.
func jsonForceSetString(raw json.RawMessage, key, value string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("body must be a JSON object: %w", err)
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	obj[key] = encoded
	return json.Marshal(obj)
}

// isStdinTerminal returns true if stdin is connected to a tty (i.e. not piped).
// Used to decide whether to attempt reading a body when --body is not set.
func isStdinTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return true
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
