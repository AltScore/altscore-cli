package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// ExitCodeError lets a RunE bubble an explicit process exit code up to main().
// Cobra still prints the wrapped error via its default Error handling; we just
// surface a non-default code for callers (e.g. 2 for poll timeout).
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitCodeError) Unwrap() error { return e.Err }

// uuidPattern matches RFC 4122 UUIDs (8-4-4-4-12 hex). Used by helpers that
// need to distinguish a workflow UUID from a workflow alias before calling
// alias-only endpoints (e.g. /v2/workflows/{alias}/versions).
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resolveWorkflowAlias accepts either a workflow alias (returned as-is) or a
// workflow UUID (resolved via GET /v2/workflows/{id}). Used to bridge the gap
// between alias-only endpoints (versions, lock) and the UUIDs that compose
// returns from create.
func resolveWorkflowAlias(c *client.Client, aliasOrID string) (string, error) {
	if !uuidPattern.MatchString(aliasOrID) {
		return aliasOrID, nil
	}
	data, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+aliasOrID, nil)
	if err != nil {
		return "", fmt.Errorf("could not resolve workflow %s: %w", aliasOrID, err)
	}
	var wf map[string]any
	if err := json.Unmarshal(data, &wf); err != nil {
		return "", fmt.Errorf("could not parse workflow %s: %w", aliasOrID, err)
	}
	alias, _ := wf["workflowAlias"].(string)
	if alias == "" {
		alias, _ = wf["alias"].(string)
	}
	if alias == "" {
		return "", fmt.Errorf("workflow %s has no workflowAlias field", aliasOrID)
	}
	return alias, nil
}

// ===================== Lifecycle =====================

func makeWfv2PublishCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "publish <id>",
		Short:   "Publish a v2 workflow draft (DRAFT -> ACTIVE)",
		Long:    `Publish a draft workflow. The workflow must be in DRAFT status and pass publish-time validation.`,
		Example: `  altscore workflows-v2 publish <id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/publish", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeWfv2CreateDraftCmd() *cobra.Command {
	var message string
	var forceRecreate bool

	cmd := &cobra.Command{
		Use:   "create-draft <id>",
		Short: "Create or fetch a draft from an ACTIVE v2 workflow",
		Long: `Create a new DRAFT version of an ACTIVE workflow. If a draft already
exists, the existing draft is returned unless --force-recreate is set.`,
		Example: `  altscore workflows-v2 create-draft <id>
  altscore workflows-v2 create-draft <id> --message "Adding scoring step"
  altscore workflows-v2 create-draft <id> --force-recreate`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body := map[string]any{}
			if message != "" {
				body["message"] = message
			}
			if forceRecreate {
				body["forceRecreate"] = true
			}
			raw, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			path := fmt.Sprintf("/v2/workflows/%s/create-draft", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, json.RawMessage(raw))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&message, "message", "", "draft creation message")
	cmd.Flags().BoolVar(&forceRecreate, "force-recreate", false, "discard existing draft and create a new one")
	return cmd
}

func makeWfv2RevertCmd() *cobra.Command {
	var message string
	var mode string

	cmd := &cobra.Command{
		Use:   "revert <alias> <version-id>",
		Short: "Revert a v2 workflow draft to a prior version",
		Long: `Revert a draft to the contents of a prior version. Mode controls whether
the revert produces a new draft (default) or replaces the active version.`,
		Example: `  altscore workflows-v2 revert my-wf <version-id>
  altscore workflows-v2 revert my-wf <version-id> --mode publish --message "Rollback bad change"`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body := map[string]any{}
			if message != "" {
				body["message"] = message
			}
			if mode != "" {
				body["mode"] = mode
			}
			raw, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			path := fmt.Sprintf("/v2/workflows/%s/revert/%s", args[0], args[1])
			data, _, err := c.Do("POST", "borrower_central", path, json.RawMessage(raw))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&message, "message", "", "revert message")
	cmd.Flags().StringVar(&mode, "mode", "", `"draft" (default) or "publish"`)
	return cmd
}

func makeWfv2ArchiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "archive <id>",
		Short:   "Archive all versions of a v2 workflow",
		Long:    `Archive every version sharing the workflow's alias. Returns archivedCount.`,
		Example: `  altscore workflows-v2 archive <id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/archive", args[0])
			data, _, err := c.Do("PUT", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeWfv2RestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "restore <id>",
		Short:   "Restore an archived v2 workflow",
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 restore <id>`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/restore", args[0])
			data, _, err := c.Do("PUT", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeWfv2DuplicateCmd() *cobra.Command {
	var newLabel string

	cmd := &cobra.Command{
		Use:   "duplicate <id>",
		Short: "Duplicate a v2 workflow with a new label",
		Long: `Clone a workflow (and its tasks) under a new label. The alias is derived
from the new label; if it collides with an existing alias the API returns 409.`,
		Example: `  altscore workflows-v2 duplicate <id> --new-label "Credit Scoring v2 Copy"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if newLabel == "" {
				return fmt.Errorf("--new-label is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{"newLabel": newLabel})
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			path := fmt.Sprintf("/v2/workflows/%s/duplicate", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, json.RawMessage(body))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&newLabel, "new-label", "", "label for the duplicated workflow (required)")
	return cmd
}

// ===================== Locks =====================

func makeWfv2LockGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "lock",
		Short: "Manage edit locks on a v2 workflow",
		Long: `v2 workflows require an edit lock before mutations. Acquire a lock,
heartbeat it periodically while editing, and release it when done.`,
	}
	group.AddCommand(makeWfv2LockGetCmd())
	group.AddCommand(makeWfv2LockAcquireCmd())
	group.AddCommand(makeWfv2LockHeartbeatCmd())
	group.AddCommand(makeWfv2LockReleaseCmd())
	group.AddCommand(makeWfv2LockForceReleaseCmd())
	return group
}

func makeWfv2LockGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get <alias>",
		Short:   "Inspect lock status for a v2 workflow",
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 lock get my-wf`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/lock", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeWfv2LockAcquireCmd() *cobra.Command {
	var clientID string

	cmd := &cobra.Command{
		Use:     "acquire <alias>",
		Short:   "Acquire an edit lock on a v2 workflow",
		Long:    `Returns lockToken, expiresAt, and ttl. Save the lockToken; you will need it for heartbeat, autosave, and release.`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 lock acquire my-wf --client-id cli-$(uuidgen)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if clientID == "" {
				return fmt.Errorf("--client-id is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{"clientId": clientID})
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			path := fmt.Sprintf("/v2/workflows/%s/lock", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, json.RawMessage(body))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&clientID, "client-id", "", "stable identifier for the lock holder (required)")
	return cmd
}

func makeWfv2LockHeartbeatCmd() *cobra.Command {
	var token string

	cmd := &cobra.Command{
		Use:     "heartbeat <alias>",
		Short:   "Renew lock TTL via lockToken",
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 lock heartbeat my-wf --token $TOKEN`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				return fmt.Errorf("--token is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{"lockToken": token})
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			path := fmt.Sprintf("/v2/workflows/%s/lock/heartbeat", args[0])
			data, _, err := c.Do("PUT", "borrower_central", path, json.RawMessage(body))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&token, "lock-token", "", "lockToken returned from lock acquire (required)")
	cmd.Flags().StringVar(&token, "token", "", "alias for --lock-token")
	cmd.Flags().MarkHidden("token")
	return cmd
}

func makeWfv2LockReleaseCmd() *cobra.Command {
	var token string

	cmd := &cobra.Command{
		Use:   "release <alias>",
		Short: "Release a v2 workflow edit lock you hold",
		Long: `Release an edit lock using the lockToken returned by 'lock acquire'.
Only the holder can release this way -- the token proves ownership.

If you don't have the token (e.g. an automation died without releasing,
or another session lost track of it), use 'workflows-v2 lock force-release
<alias>' instead. That endpoint requires admin permission but doesn't
need the token.

Note: --client-id (used by acquire and the autosave helpers like add-node)
is NOT a substitute for --lock-token here. The release endpoint
authenticates by token, not client id.`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 lock release my-wf --lock-token $TOKEN`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				return fmt.Errorf("--lock-token is required. " +
					"If you've lost the token, use 'altscore workflows-v2 lock force-release <alias>' instead (admin only).")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{"lockToken": token})
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			path := fmt.Sprintf("/v2/workflows/%s/lock", args[0])
			data, _, err := c.Do("DELETE", "borrower_central", path, json.RawMessage(body))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&token, "lock-token", "", "lockToken to release (required)")
	cmd.Flags().StringVar(&token, "token", "", "alias for --lock-token")
	cmd.Flags().MarkHidden("token")
	return cmd
}

func makeWfv2LockForceReleaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "force-release <alias>",
		Short:   "Force-release a stuck lock (admin)",
		Long:    `Use only when the lock holder is gone and a normal release is impossible. Requires admin permission.`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 lock force-release my-wf`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/lock/force", args[0])
			data, _, err := c.Do("DELETE", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// ===================== Editing =====================

func makeWfv2AutosaveCmd() *cobra.Command {
	var bodyFlag string
	var lockToken string
	var lastKnownVersion int

	cmd := &cobra.Command{
		Use:   "autosave <id>",
		Short: "Save partial v2 workflow edits with conflict detection",
		Long: `Partial save. Pass any subset of fields in --body. Provide --lock-token
to assert ownership and --last-known-version for optimistic concurrency.

The API returns 409 if lastKnownVersion is stale.`,
		Example: `  altscore workflows-v2 autosave <id> --lock-token $TOKEN --body '{"label":"Renamed"}'
  altscore workflows-v2 autosave <id> --lock-token $TOKEN --last-known-version 3 --body '{"nodes":[...],"edges":[...]}'`,
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
			if lockToken != "" || lastKnownVersion > 0 {
				var m map[string]any
				if len(body) == 0 {
					m = map[string]any{}
				} else if err := json.Unmarshal(body, &m); err != nil {
					return fmt.Errorf("invalid JSON body: %w", err)
				}
				if lockToken != "" {
					m["lockToken"] = lockToken
				}
				if lastKnownVersion > 0 {
					m["lastKnownVersion"] = lastKnownVersion
				}
				body, err = json.Marshal(m)
				if err != nil {
					return fmt.Errorf("re-encode body: %w", err)
				}
			}
			if err := validateWorkflowV2Body(body); err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/autosave", args[0])
			data, _, err := c.Do("PUT", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	cmd.Flags().StringVar(&lockToken, "lock-token", "", "lockToken from lock acquire")
	cmd.Flags().IntVar(&lastKnownVersion, "last-known-version", 0, "version the client last saw (optimistic concurrency)")
	return cmd
}

func makeWfv2UpdateMappingCmd() *cobra.Command {
	var nodeID string
	var previous string
	var newName string

	cmd := &cobra.Command{
		Use:     "update-mapping <id>",
		Short:   "Update a node's variable mapping",
		Long:    `Rename a variable referenced by a node's input mapping. Use --new "" (empty) to clear the mapping.`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 update-mapping <id> --node-id task_a --previous oldVar --new newVar`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if nodeID == "" || previous == "" {
				return fmt.Errorf("--node-id and --previous are required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			// Backend UpdateMappingWorkflow expects snake_case body keys
			// (node_id / previous_variable_name / new_variable_name) -- the
			// model in app/api/workflows_v2/handler.py:101 doesn't declare
			// camelCase aliases, unlike the rest of v2 which does. Sending
			// camelCase 400s with "field required". Match the model.
			body := map[string]any{
				"node_id":                nodeID,
				"previous_variable_name": previous,
			}
			if cmd.Flags().Changed("new") {
				body["new_variable_name"] = newName
			}
			raw, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			path := fmt.Sprintf("/v2/workflows/%s/update_mapping_workflow", args[0])
			data, _, err := c.Do("PUT", "borrower_central", path, json.RawMessage(raw))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&nodeID, "node-id", "", "node whose mapping is being updated (required)")
	cmd.Flags().StringVar(&previous, "previous", "", "previous variable name (required)")
	cmd.Flags().StringVar(&newName, "new", "", "new variable name (omit to clear)")
	return cmd
}

func makeWfv2ResolveMappingsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "resolve-mappings <id>",
		Short:   "Auto-resolve variable mappings for a v2 workflow",
		Long:    `Smart-mapping pass: attempts to wire up unresolved input mappings based on available task outputs.`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 resolve-mappings <id>`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/resolve-mappings", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// ===================== Versions / executions =====================

func makeWfv2VersionsCmd() *cobra.Command {
	var page int
	var limit int
	var includeChanges bool

	cmd := &cobra.Command{
		Use:   "versions <alias-or-id>",
		Short: "List version history for a v2 workflow",
		Long: `List version history for a v2 workflow.

Accepts either a workflow alias or a UUID. The backend versions endpoint is
alias-only (` + "`/v2/workflows/{alias}/versions`" + `); the CLI auto-resolves
a UUID to its alias via ` + "`GET /v2/workflows/{id}`" + ` first.`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 versions my-wf --include-changes --limit 20`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			alias, err := resolveWorkflowAlias(c, args[0])
			if err != nil {
				return err
			}
			q := url.Values{}
			if page > 0 {
				q.Set("page", strconv.Itoa(page))
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			if includeChanges {
				q.Set("includeChanges", "true")
			}
			path := fmt.Sprintf("/v2/workflows/%s/versions", alias)
			if encoded := q.Encode(); encoded != "" {
				path += "?" + encoded
			}
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().IntVar(&page, "page", 0, "page (1-indexed)")
	cmd.Flags().IntVar(&limit, "limit", 0, "page size")
	cmd.Flags().BoolVar(&includeChanges, "include-changes", false, "include per-version diff summaries")
	return cmd
}

func makeWfv2GetVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get-version <alias> <version>",
		Short:   "Fetch a specific v2 workflow version",
		Long:    `Pass "latest" as <version> to get the most recent published version.`,
		Args:    cobra.ExactArgs(2),
		Example: `  altscore workflows-v2 get-version my-wf latest
  altscore workflows-v2 get-version my-wf 3`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/%s", args[0], args[1])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeWfv2ExecutionsCmd() *cobra.Command {
	var filters []string
	var page int
	var perPage int
	var sortBy string
	var sortDirection string
	var search string

	cmd := &cobra.Command{
		Use:     "executions <id>",
		Short:   "List executions for a v2 workflow",
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 executions <id> --per-page 20 --sort-by createdAt --sort-direction desc`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			q := url.Values{}
			for _, f := range filters {
				k, v, ok := splitFilter(f)
				if !ok {
					return fmt.Errorf("invalid --filter %q (expected key=value)", f)
				}
				q.Set(k, v)
			}
			if search != "" {
				q.Set("search", search)
			}
			if page > 0 {
				q.Set("page", strconv.Itoa(page))
			}
			if perPage > 0 {
				q.Set("per-page", strconv.Itoa(perPage))
			}
			if sortBy != "" {
				q.Set("sort-by", sortBy)
			}
			if sortDirection != "" {
				q.Set("sort-direction", sortDirection)
			}
			path := fmt.Sprintf("/v2/workflows/%s/executions", args[0])
			if encoded := q.Encode(); encoded != "" {
				path += "?" + encoded
			}
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringArrayVar(&filters, "filter", nil, "key=value filter (repeatable)")
	cmd.Flags().StringVar(&search, "search", "", "free-text search")
	cmd.Flags().IntVar(&page, "page", 0, "page (1-indexed)")
	cmd.Flags().IntVar(&perPage, "per-page", 0, "page size")
	cmd.Flags().StringVar(&sortBy, "sort-by", "", "field to sort by")
	cmd.Flags().StringVar(&sortDirection, "sort-direction", "", `"asc" or "desc"`)
	return cmd
}

// splitFilter parses "key=value" into (key, value, true). Returns false if malformed.
func splitFilter(f string) (string, string, bool) {
	for i := 0; i < len(f); i++ {
		if f[i] == '=' {
			return f[:i], f[i+1:], true
		}
	}
	return "", "", false
}

// ===================== Schedules =====================

func makeWfv2ScheduleGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "schedule",
		Short: "Manage v2 workflow schedules",
		Long: `Create, update, delete, and inspect cron-based schedules. Use 'preview'
and 'validate' to dry-run a cron expression before saving it.`,
	}
	group.AddCommand(makeWfv2ScheduleGetCmd())
	group.AddCommand(makeWfv2ScheduleCreateCmd())
	group.AddCommand(makeWfv2ScheduleUpdateCmd())
	group.AddCommand(makeWfv2ScheduleDeleteCmd())
	group.AddCommand(makeWfv2SchedulePreviewCmd())
	group.AddCommand(makeWfv2ScheduleValidateCmd())
	return group
}

func makeWfv2ScheduleGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get <id>",
		Short:   "Get the schedule config for a v2 workflow",
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 schedule get <id>`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/schedules", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeWfv2ScheduleCreateCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "create <id>",
		Short: "Create a schedule for a v2 workflow",
		Long: `Body should contain "schedule" and/or "scheduleBatch" objects with
cron + utcDeltaHours (range -12..14). At least one is required.`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 schedule create <id> --body '{"schedule":{"cron":"0 9 * * *","utcDeltaHours":-5}}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/schedules", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

func makeWfv2ScheduleUpdateCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:     "update <id>",
		Short:   "Update a schedule for a v2 workflow",
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 schedule update <id> --body '{"schedule":{"cron":"*/15 * * * *","utcDeltaHours":0}}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/schedules", args[0])
			data, _, err := c.Do("PUT", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

func makeWfv2ScheduleDeleteCmd() *cobra.Command {
	var deleteIndividual bool
	var deleteBatch bool

	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete the schedule and/or batch schedule for a v2 workflow",
		Long:  `At least one of --individual or --batch must be set.`,
		Args:  cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 schedule delete <id> --individual
  altscore workflows-v2 schedule delete <id> --batch
  altscore workflows-v2 schedule delete <id> --individual --batch`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !deleteIndividual && !deleteBatch {
				return fmt.Errorf("at least one of --individual or --batch is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			body := map[string]bool{}
			if deleteIndividual {
				body["deleteSchedule"] = true
			}
			if deleteBatch {
				body["deleteScheduleBatch"] = true
			}
			raw, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			path := fmt.Sprintf("/v2/workflows/%s/schedules", args[0])
			data, _, err := c.Do("DELETE", "borrower_central", path, json.RawMessage(raw))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().BoolVar(&deleteIndividual, "individual", false, "delete the individual schedule")
	cmd.Flags().BoolVar(&deleteBatch, "batch", false, "delete the batch schedule")
	return cmd
}

func makeWfv2SchedulePreviewCmd() *cobra.Command {
	var cron string
	var utcDelta int
	var count int

	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Preview the next runs for a cron expression",
		Long:  `No workflow context required. Use to dry-run a cron + utcDeltaHours combination.`,
		Example: `  altscore workflows-v2 schedule preview --cron "0 9 * * *" --utc-delta -5
  altscore workflows-v2 schedule preview --cron "*/15 * * * *" --utc-delta 0 --count 10`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cron == "" {
				return fmt.Errorf("--cron is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			body := map[string]any{
				"cron":           cron,
				"utcDeltaHours":  utcDelta,
			}
			if count > 0 {
				body["count"] = count
			}
			raw, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			data, _, err := c.Do("POST", "borrower_central", "/v2/workflows/schedules/preview", json.RawMessage(raw))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&cron, "cron", "", "cron expression (required)")
	cmd.Flags().IntVar(&utcDelta, "utc-delta", 0, "UTC delta hours (-12 to 14)")
	cmd.Flags().IntVar(&count, "count", 0, "number of upcoming runs to return")
	return cmd
}

func makeWfv2ScheduleValidateCmd() *cobra.Command {
	var cron string
	var utcDelta int
	var count int

	cmd := &cobra.Command{
		Use:     "validate",
		Short:   "Validate a cron expression and preview runs",
		Example: `  altscore workflows-v2 schedule validate --cron "0 9 * * *" --utc-delta -5`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cron == "" {
				return fmt.Errorf("--cron is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			body := map[string]any{
				"cron":          cron,
				"utcDeltaHours": utcDelta,
			}
			if count > 0 {
				body["count"] = count
			}
			raw, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			data, _, err := c.Do("POST", "borrower_central", "/v2/workflows/schedules/validate", json.RawMessage(raw))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&cron, "cron", "", "cron expression (required)")
	cmd.Flags().IntVar(&utcDelta, "utc-delta", 0, "UTC delta hours (-12 to 14)")
	cmd.Flags().IntVar(&count, "count", 0, "preview run count")
	return cmd
}

// ===================== Import / export =====================

func makeWfv2ExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <id>",
		Short: "Export a v2 workflow as a JSON bundle",
		Long: `Returns a bundle including the workflow plus its tasks, evaluation rules,
scorecards, rule trees, and mapping tables. Suitable for piping to a file:

  altscore workflows-v2 export <id> > workflow.json`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 export <id> > my-wf.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/export", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeWfv2ImportCmd() *cobra.Command {
	var bodyFlag string
	var newLabel string
	var skipEvaluationRules bool
	var skipScorecards bool
	var skipRuleTrees bool
	var skipMappingTables bool

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a v2 workflow from an export bundle",
		Long: `Pass the exported bundle in --body. Use --new-label to override the label
(the alias is derived from it). Sub-resources can be skipped with --skip-* flags.

The API returns 409 if the alias already exists.`,
		Example: `  altscore workflows-v2 import --body @bundle.json --new-label "Imported v1"
  altscore workflows-v2 import --body @bundle.json --skip-evaluation-rules`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			// Wrap body in {"workflowData": <bundle>, ...overrides} per the API contract.
			var bundle any
			if err := json.Unmarshal(body, &bundle); err != nil {
				return fmt.Errorf("invalid JSON body: %w", err)
			}
			payload := map[string]any{"workflowData": bundle}
			if newLabel != "" {
				payload["newLabel"] = newLabel
			}
			if skipEvaluationRules {
				payload["skipEvaluationRules"] = true
			}
			if skipScorecards {
				payload["skipScorecards"] = true
			}
			if skipRuleTrees {
				payload["skipRuleTrees"] = true
			}
			if skipMappingTables {
				payload["skipMappingTables"] = true
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			data, _, err := c.Do("POST", "borrower_central", "/v2/workflows/import", json.RawMessage(raw))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "exported workflow JSON (or pipe via stdin)")
	cmd.Flags().StringVar(&newLabel, "new-label", "", "override label (alias derived from this)")
	cmd.Flags().BoolVar(&skipEvaluationRules, "skip-evaluation-rules", false, "skip importing evaluation rules")
	cmd.Flags().BoolVar(&skipScorecards, "skip-scorecards", false, "skip importing scorecards")
	cmd.Flags().BoolVar(&skipRuleTrees, "skip-rule-trees", false, "skip importing rule trees")
	cmd.Flags().BoolVar(&skipMappingTables, "skip-mapping-tables", false, "skip importing mapping tables")
	return cmd
}

func makeWfv2ValidateRulesCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:     "validate-rules",
		Short:   "Pre-import check: which rules in a bundle exist on this tenant",
		Long:    `Pass the bundle as --body. Returns missingRules and existingRules so you can decide whether to skip imports.`,
		Example: `  altscore workflows-v2 validate-rules --body @bundle.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			var bundle any
			if err := json.Unmarshal(body, &bundle); err != nil {
				return fmt.Errorf("invalid JSON body: %w", err)
			}
			payload, err := json.Marshal(map[string]any{"workflowData": bundle})
			if err != nil {
				return fmt.Errorf("encode body: %w", err)
			}
			data, _, err := c.Do("POST", "borrower_central", "/v2/workflows/validate-rules", json.RawMessage(payload))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "exported workflow JSON (or pipe via stdin)")
	return cmd
}

// ===================== Execute =====================

type wfv2ExecHeaders struct {
	tags               string
	testTaskID         string
	testTimeoutSeconds int
	storeLogs          string
	executionMode      string
}

func (h wfv2ExecHeaders) asMap() map[string]string {
	out := map[string]string{}
	if h.tags != "" {
		out["X-Tags"] = h.tags
	}
	if h.testTaskID != "" {
		out["X-Test-Task-Id"] = h.testTaskID
	}
	if h.testTimeoutSeconds > 0 {
		out["X-Test-Timeout-Seconds"] = strconv.Itoa(h.testTimeoutSeconds)
	}
	if h.storeLogs != "" {
		out["X-Store-Logs"] = h.storeLogs
	}
	if h.executionMode != "" {
		out["X-Execution-Mode"] = h.executionMode
	}
	return out
}

func bindExecHeaderFlags(cmd *cobra.Command, h *wfv2ExecHeaders) {
	cmd.Flags().StringVar(&h.tags, "tags", "", "comma-separated tags (X-Tags)")
	cmd.Flags().StringVar(&h.testTaskID, "test-task-id", "", "X-Test-Task-Id (required for test mode)")
	cmd.Flags().IntVar(&h.testTimeoutSeconds, "test-timeout-seconds", 0, "X-Test-Timeout-Seconds")
	cmd.Flags().StringVar(&h.storeLogs, "store-logs", "", `X-Store-Logs ("true"/"false")`)
	cmd.Flags().StringVar(&h.executionMode, "execution-mode", "", `X-Execution-Mode ("sync"/"async")`)
}

// wfv2WaitFlags holds the polling configuration shared by execute and
// execute-by-alias. Defaults are 5m total deadline and 2s between polls,
// matching the partner spec.
type wfv2WaitFlags struct {
	wait         bool
	timeout      time.Duration
	pollInterval time.Duration
}

func bindWaitFlags(cmd *cobra.Command, w *wfv2WaitFlags) {
	cmd.Flags().BoolVar(&w.wait, "wait", false, "submit async, then poll until the execution reaches a terminal state")
	cmd.Flags().DurationVar(&w.timeout, "timeout", 5*time.Minute, "total deadline for --wait polling")
	cmd.Flags().DurationVar(&w.pollInterval, "poll-interval", 2*time.Second, "interval between poll calls when --wait is set")
}

// terminalExecutionStatuses are the workflow-execution statuses that end the
// poll loop. Source of truth: ExecutionStatus in
// borrower-central/app/model/workflows_v2/workflow_execution.py.
var terminalExecutionStatuses = map[string]bool{
	"completed": true,
	"failed":    true,
	"cancelled": true,
	"timed_out": true,
}

// failureExecutionStatuses are the subset of terminal statuses that should
// cause the CLI to exit non-zero.
var failureExecutionStatuses = map[string]bool{
	"failed":    true,
	"cancelled": true,
	"timed_out": true,
}

// pollExecutionWait runs the --wait poll loop against
// GET /v2/workflows/{workflowId}/executions/{executionId} until the execution
// reaches a terminal state or the timeout fires.
//
// On --verbose, prints per-node status transitions to stderr as
// "[<ts>] <node_id> <prev_status> -> <new_status>". Returns the final raw
// execution JSON so the caller can pretty-print it to stdout, together with
// the terminal status string. A timeout returns (nil, "", *ExitCodeError{2}).
func pollExecutionWait(c *client.Client, workflowID, executionID string, w wfv2WaitFlags, verbose bool, stderr io.Writer) (json.RawMessage, string, error) {
	if w.pollInterval <= 0 {
		w.pollInterval = 2 * time.Second
	}
	if w.timeout <= 0 {
		w.timeout = 5 * time.Minute
	}

	path := fmt.Sprintf("/v2/workflows/%s/executions/%s", workflowID, executionID)
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	deadline := time.After(w.timeout)
	prevNodeStatuses := map[string]string{}
	transientFailures := 0

	// Do an immediate first poll so very-short runs don't burn pollInterval.
	for {
		data, _, err := c.Do("GET", "borrower_central", path, nil)
		if err != nil {
			// Be defensive about transient HTTP errors -- one retry, then fail.
			transientFailures++
			if transientFailures > 1 {
				return nil, "", fmt.Errorf("poll %s failed: %w", path, err)
			}
			if verbose {
				fmt.Fprintf(stderr, "# poll error (transient, will retry once): %v\n", err)
			}
		} else {
			transientFailures = 0
			status := extractExecutionStatus(data)
			if verbose {
				printNodeStatusTransitions(stderr, data, prevNodeStatuses)
			}
			if terminalExecutionStatuses[status] {
				return data, status, nil
			}
		}

		select {
		case <-deadline:
			return nil, "", &ExitCodeError{
				Code: 2,
				Err:  fmt.Errorf("timed out after %s waiting for execution %s", w.timeout, executionID),
			}
		case <-ticker.C:
		}
	}
}

// extractExecutionStatus pulls the lowercase status string out of a workflow
// execution JSON. Returns "" when the field is missing or not a string so the
// loop keeps polling instead of crashing.
func extractExecutionStatus(data json.RawMessage) string {
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		return ""
	}
	if s, ok := env["status"].(string); ok {
		return s
	}
	// Some wrappers nest the payload under "data".
	if inner, ok := env["data"].(map[string]any); ok {
		if s, ok := inner["status"].(string); ok {
			return s
		}
	}
	return ""
}

// extractNodeStatuses walks the execution JSON and returns a map of
// node_id -> status. Looks at common shapes:
//   - top-level "nodes": [{id|nodeId, status}, ...]
//   - top-level "taskExecutions": [{taskId|nodeId, status}, ...]
//   - "data.nodes" / "data.taskExecutions" for wrapped payloads
//
// Missing or differently-shaped data is silently ignored.
func extractNodeStatuses(data json.RawMessage) map[string]string {
	out := map[string]string{}
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		return out
	}
	collect := func(root map[string]any) {
		for _, key := range []string{"nodes", "taskExecutions", "tasks", "nodeExecutions"} {
			raw, ok := root[key]
			if !ok {
				continue
			}
			arr, ok := raw.([]any)
			if !ok {
				continue
			}
			for _, item := range arr {
				node, ok := item.(map[string]any)
				if !ok {
					continue
				}
				id := ""
				for _, idKey := range []string{"nodeId", "id", "taskId", "taskAlias"} {
					if v, ok := node[idKey].(string); ok && v != "" {
						id = v
						break
					}
				}
				if id == "" {
					continue
				}
				status, _ := node["status"].(string)
				out[id] = status
			}
		}
	}
	collect(env)
	if inner, ok := env["data"].(map[string]any); ok {
		collect(inner)
	}
	return out
}

func printNodeStatusTransitions(stderr io.Writer, data json.RawMessage, prev map[string]string) {
	curr := extractNodeStatuses(data)
	if len(curr) == 0 {
		return
	}
	ids := make([]string, 0, len(curr))
	for id := range curr {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ts := time.Now().Format(time.RFC3339)
	for _, id := range ids {
		newStatus := curr[id]
		oldStatus, seen := prev[id]
		if !seen {
			oldStatus = "-"
		}
		if oldStatus != newStatus {
			fmt.Fprintf(stderr, "[%s] %s %s -> %s\n", ts, id, oldStatus, newStatus)
		}
	}
	for id, status := range curr {
		prev[id] = status
	}
}

// extractFailureDetail pulls failedNodeId + a best-effort failure reason out
// of the execution payload. PR1 may be adding "failureReason" as a top-level
// field; until then we also fall back to error.message / error.details.
func extractFailureDetail(data json.RawMessage) (failedNodeID, reason string) {
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		return "", ""
	}
	pick := func(root map[string]any) {
		if failedNodeID == "" {
			if v, ok := root["failedNodeId"].(string); ok {
				failedNodeID = v
			}
		}
		if reason == "" {
			if v, ok := root["failureReason"].(string); ok {
				reason = v
			}
		}
		if reason == "" {
			if errObj, ok := root["error"].(map[string]any); ok {
				if m, ok := errObj["message"].(string); ok {
					reason = m
				}
			}
		}
	}
	pick(env)
	if inner, ok := env["data"].(map[string]any); ok {
		pick(inner)
	}
	return failedNodeID, reason
}

// runExecuteWithOptionalWait centralises the post-submit flow shared by
// execute and execute-by-alias. When wait is set it polls; otherwise it just
// prints the submit response (existing behavior).
func runExecuteWithOptionalWait(
	c *client.Client,
	stdout, stderr io.Writer,
	submitResp json.RawMessage,
	wait wfv2WaitFlags,
	verbose bool,
) error {
	if !wait.wait {
		return output.RawJSON(submitResp)
	}

	executionID, workflowID := extractExecutionAndWorkflowIDs(submitResp)
	if executionID == "" || workflowID == "" {
		return fmt.Errorf("--wait: could not find executionId/workflowId in submit response (got %d bytes)", len(submitResp))
	}

	if verbose {
		fmt.Fprintf(stderr, "# polling /v2/workflows/%s/executions/%s every %s (timeout %s)\n",
			workflowID, executionID, wait.pollInterval, wait.timeout)
	}

	finalData, status, err := pollExecutionWait(c, workflowID, executionID, wait, verbose, stderr)
	if err != nil {
		return err
	}

	if err := output.RawJSON(finalData); err != nil {
		return err
	}

	if failureExecutionStatuses[status] {
		failedNode, reason := extractFailureDetail(finalData)
		if failedNode != "" || reason != "" {
			fmt.Fprintf(stderr, "execution %s ended in %s", executionID, status)
			if failedNode != "" {
				fmt.Fprintf(stderr, " at node %s", failedNode)
			}
			if reason != "" {
				fmt.Fprintf(stderr, ": %s", reason)
			}
			fmt.Fprintln(stderr)
		}
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("execution %s ended with status %s", executionID, status)}
	}
	return nil
}

// extractExecutionAndWorkflowIDs digs the executionId + workflowId out of an
// async-submit response. Tolerates both flat ({executionId, workflowId}) and
// nested-under-"data" shapes.
func extractExecutionAndWorkflowIDs(data json.RawMessage) (string, string) {
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		return "", ""
	}
	exec, wf := "", ""
	pull := func(root map[string]any) {
		if exec == "" {
			for _, k := range []string{"executionId", "execution_id", "id"} {
				if v, ok := root[k].(string); ok && v != "" {
					exec = v
					break
				}
			}
		}
		if wf == "" {
			for _, k := range []string{"workflowId", "workflow_id"} {
				if v, ok := root[k].(string); ok && v != "" {
					wf = v
					break
				}
			}
		}
	}
	pull(env)
	if inner, ok := env["data"].(map[string]any); ok {
		pull(inner)
	}
	return exec, wf
}

func makeWfv2ExecuteCmd() *cobra.Command {
	var bodyFlag string
	var skipStatusCheck bool
	headers := wfv2ExecHeaders{}
	wait := wfv2WaitFlags{}

	cmd := &cobra.Command{
		Use:   "execute <id>",
		Short: "Execute a v2 workflow by ID",
		Long: `Execute a v2 workflow by ID. Pass workflow input as --body.

By default, fetches the workflow first and warns to stderr if it isn't ACTIVE
(DRAFT workflows execute successfully but the engine skips every node, so the
output is empty and the run looks successful while doing nothing useful).
Pass --skip-status-check to opt out of the warning if you're intentionally
exercising a draft.

Use --execution-mode sync (default) or async.

Pass --wait to submit async and poll until the execution reaches a terminal
state. Honors --timeout (default 5m) and --poll-interval (default 2s). On
--verbose prints per-node status transitions to stderr. Exits 0 on completed,
1 on failed/cancelled/timed_out, 2 on the local --timeout firing.`,
		Example: `  altscore workflows-v2 execute <id> --body '{"borrower_id":"abc"}'
  altscore workflows-v2 execute <id> --body '{...}' --execution-mode async --tags smoke
  altscore workflows-v2 execute <id> --body '{...}' --wait --timeout 10m`,
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
			if !skipStatusCheck {
				warnIfNotActive(cmd.OutOrStderr(), c, args[0])
			}
			// --wait implies async submit -- otherwise the sync header would
			// fight the poll loop and we'd race the HTTP timeout. Only flip
			// the mode when the user hasn't explicitly passed one.
			if wait.wait && !cmd.Flags().Changed("execution-mode") {
				headers.executionMode = "async"
			}
			path := fmt.Sprintf("/v2/workflows/%s/execute", args[0])
			data, _, err := c.DoWithHeaders("POST", "borrower_central", path, body, headers.asMap())
			if err != nil {
				return err
			}
			return runExecuteWithOptionalWait(c, cmd.OutOrStdout(), cmd.OutOrStderr(), data, wait, flagVerbose)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	cmd.Flags().BoolVar(&skipStatusCheck, "skip-status-check", false, "skip the pre-flight ACTIVE-status check")
	bindExecHeaderFlags(cmd, &headers)
	bindWaitFlags(cmd, &wait)
	return cmd
}

// warnIfNotActive does a best-effort GET /v2/workflows/{id}; if the workflow's
// status is not ACTIVE, prints a clear warning to stderr explaining what will
// happen at runtime (DRAFT workflows skip every node) and how to fix it. The
// check is best-effort -- if the GET fails for any reason, we silently skip
// the warning rather than block the execute.
func warnIfNotActive(stderr io.Writer, c *client.Client, idOrAlias string) {
	data, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+idOrAlias, nil)
	if err != nil {
		return
	}
	var wf map[string]any
	if err := json.Unmarshal(data, &wf); err != nil {
		return
	}
	status, _ := wf["status"].(string)
	if status == "" || status == "ACTIVE" {
		return
	}
	switch status {
	case "DRAFT":
		fmt.Fprintf(stderr, "# warning: workflow %s is in DRAFT status -- the engine will skip every node and return an empty output. Run 'altscore workflows-v2 publish %s' (or re-apply with --publish) to make it executable.\n", idOrAlias, idOrAlias)
	case "ARCHIVED":
		fmt.Fprintf(stderr, "# warning: workflow %s is ARCHIVED -- restore it via 'altscore workflows-v2 restore %s' before expecting useful output.\n", idOrAlias, idOrAlias)
	default:
		fmt.Fprintf(stderr, "# warning: workflow %s status is %q (not ACTIVE) -- execution may behave unexpectedly.\n", idOrAlias, status)
	}
}

func makeWfv2ExecuteByAliasCmd() *cobra.Command {
	var bodyFlag string
	headers := wfv2ExecHeaders{}
	wait := wfv2WaitFlags{}

	cmd := &cobra.Command{
		Use:   "execute-by-alias <alias> <version>",
		Short: "Execute a v2 workflow by alias and version",
		Long: `Pass "latest" as <version> to execute the most recent published version.

Pass --wait to submit async and poll until the execution reaches a terminal
state. Honors --timeout (default 5m) and --poll-interval (default 2s). On
--verbose prints per-node status transitions to stderr. Exits 0 on completed,
1 on failed/cancelled/timed_out, 2 on the local --timeout firing.`,
		Args: cobra.ExactArgs(2),
		Example: `  altscore workflows-v2 execute-by-alias my-wf latest --body '{...}'
  altscore workflows-v2 execute-by-alias my-wf latest --body '{...}' --wait`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			if wait.wait && !cmd.Flags().Changed("execution-mode") {
				headers.executionMode = "async"
			}
			path := fmt.Sprintf("/v2/workflows/%s/%s/execute", args[0], args[1])
			data, _, err := c.DoWithHeaders("POST", "borrower_central", path, body, headers.asMap())
			if err != nil {
				return err
			}
			return runExecuteWithOptionalWait(c, cmd.OutOrStdout(), cmd.OutOrStderr(), data, wait, flagVerbose)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	bindExecHeaderFlags(cmd, &headers)
	bindWaitFlags(cmd, &wait)
	return cmd
}

func makeWfv2ExecuteBatchCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "execute-batch <id>",
		Short: "Batch-execute a v2 workflow",
		Long: `Body fields: inputs[], optional label, description, tags[], costCenter,
businessPriority (0-10), parallelExecutions (default 50), maxRetryAttempts
(default 3), continueOnFailures (default true), testMode + testTaskId.

Body shape note: 'inputs' MUST be an array of objects, one per execution.
A flat sync-shape body like '{"borrower_id":"abc"}' is the wrong shape
for batch -- it'll create an empty/zombie batch that never executes.
Use 'workflows-v2 execute' for single sync runs.`,
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 execute-batch <id> --body '{"inputs":[{"borrower_id":"a"},{"borrower_id":"b"}],"label":"smoke"}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			// Preflight: batch needs inputs:[...]. Without it the API
			// happily creates a no-op batch ID; agents copy-pasting from
			// 'execute' get a silent zombie.
			var parsed map[string]any
			if jerr := json.Unmarshal(body, &parsed); jerr == nil {
				rawInputs, present := parsed["inputs"]
				if !present {
					return fmt.Errorf(
						"execute-batch body has no 'inputs' field. " +
							"Batch executions require inputs:[{...}, {...}], one object per execution. " +
							"For a single sync run, use 'altscore workflows-v2 execute' (which takes a flat body).")
				}
				inputs, ok := rawInputs.([]any)
				if !ok {
					return fmt.Errorf("execute-batch body 'inputs' must be an array of objects, got %T", rawInputs)
				}
				if len(inputs) == 0 {
					return fmt.Errorf("execute-batch body 'inputs' is empty -- nothing to execute")
				}
			}
			path := fmt.Sprintf("/v2/workflows/%s/execute-batch", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

func makeWfv2ExecuteBatchByAliasCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:     "execute-batch-by-alias <alias> <version>",
		Short:   "Batch-execute a v2 workflow by alias and version",
		Args:    cobra.ExactArgs(2),
		Example: `  altscore workflows-v2 execute-batch-by-alias my-wf latest --body '{"inputs":[...]}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/%s/execute-batch", args[0], args[1])
			data, _, err := c.Do("POST", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

func makeWfv2DownloadCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:     "download",
		Short:   "Generate a downloadable execution-output file",
		Long:    `Proxied to the high-memory output service. Pass GenerateOutputRequest as --body.`,
		Example: `  altscore workflows-v2 download --body '{...}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", "borrower_central", "/v2/workflows/download", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

// ===================== Batch control =====================

func makeWfv2BatchGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "batch",
		Short: "Control a running v2 execution batch",
	}
	group.AddCommand(makeWfv2BatchSignalCmd("pause", "Send a pause signal to a batch"))
	group.AddCommand(makeWfv2BatchSignalCmd("continue", "Resume a paused batch"))
	group.AddCommand(makeWfv2BatchSignalCmd("terminate", "Cancel a batch"))
	return group
}

func makeWfv2BatchSignalCmd(verb, short string) *cobra.Command {
	return &cobra.Command{
		Use:     fmt.Sprintf("%s <batch-id>", verb),
		Short:   short,
		Args:    cobra.ExactArgs(1),
		Example: fmt.Sprintf(`  altscore workflows-v2 batch %s <batch-id>`, verb),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v2/workflows/%s/%s", args[0], verb)
			data, _, err := c.Do("POST", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// ===================== Sources =====================

func makeWfv2SourcesStatusCmd() *cobra.Command {
	var status string
	var country string
	var search string
	var page int
	var perPage int

	cmd := &cobra.Command{
		Use:     "sources-status",
		Short:   "List AltData source status (used by v2 enrichment tasks)",
		Example: `  altscore workflows-v2 sources-status --country ECU --status active`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			q := url.Values{}
			if status != "" {
				q.Set("status", status)
			}
			if country != "" {
				q.Set("country", country)
			}
			if search != "" {
				q.Set("search", search)
			}
			if page > 0 {
				q.Set("page", strconv.Itoa(page))
			}
			if perPage > 0 {
				q.Set("per-page", strconv.Itoa(perPage))
			}
			path := "/v2/workflows/sources-status"
			if encoded := q.Encode(); encoded != "" {
				path += "?" + encoded
			}
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&status, "status", "", "filter by status")
	cmd.Flags().StringVar(&country, "country", "", "filter by country")
	cmd.Flags().StringVar(&search, "search", "", "free-text search")
	cmd.Flags().IntVar(&page, "page", 0, "page (1-indexed)")
	cmd.Flags().IntVar(&perPage, "per-page", 0, "page size")
	return cmd
}

func makeWfv2ExternalSourcesStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "external-sources-status",
		Short:   "List direct-provider data sources",
		Example: `  altscore workflows-v2 external-sources-status`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", "borrower_central", "/v2/workflows/external-sources-status", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// ===================== AI =====================

func makeWfv2AIGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "ai",
		Short: "AI helpers for v2 workflow editing",
	}
	group.AddCommand(makeWfv2AISuggestMappingsCmd())
	return group
}

func makeWfv2AISuggestMappingsCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "suggest-mappings",
		Short: "Ask the LLM to suggest variable mappings",
		Long: `Body fields:
  fields: [{name, type?, description?, context?}]
  availableOutputs: [{source, type, label?, description?, taskAlias, outputName, nodeLabel?}]

Returns a list of suggested mappings. May return 503 if the LLM is not configured.`,
		Example: `  altscore workflows-v2 ai suggest-mappings --body '{"fields":[...],"availableOutputs":[...]}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", "borrower_central", "/v2/workflows/ai/suggest-mappings", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

// ===================== Schema guide =====================

func makeWfv2SchemaGuideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema-guide [section]",
		Short: "Canonical reference for v2 workflow shape (nodes, edges, tasks, examples, ...)",
		Long: `Fetch the canonical reference for v2 workflow construction from the
backend (/v1/meta/workflows-v2-schema).

Available sections (run with no arg to see the full structure):
  architecture                          tasks-first overview
  endpoints                             /v2 routes the CLI wraps
  nodes                                 graph node shape (camelCase fields)
  edges                                 graph edge shape
  variables                             input/custom/system/task_outputs scopes
  mappings                              inputMappings rules + multi-dot syntax
  tasks                                 per-type config (perType + deprecatedTypes)
  composeSpec                           the spec format used by 'workflows-v2 apply'
  conditions                            ConditionGroup operator vocabulary
  creditDecisioningEntities             /v1/{evaluation-rules,mapping-tables,scorecards,rule-trees}
  examples                              full minimal_shell + scoring_pipeline templates
  gotchas                               common apply mistakes + fixes
  gotchas_about_branches_and_inputkeys  conditional pitfalls
  preflightChecks                       validation order before apply persists`,
		Example: `  altscore workflows-v2 schema-guide
  altscore workflows-v2 schema-guide tasks
  altscore workflows-v2 schema-guide composeSpec
  altscore workflows-v2 schema-guide tasks | jq '.tasks.perType | keys'`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := "/v1/meta/workflows-v2-schema"
			if len(args) > 0 {
				path += "?section=" + url.QueryEscape(args[0])
			}
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
