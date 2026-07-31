package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// ===================== Shared helpers =====================

// fetchWorkflowV2 fetches the current state of a v2 workflow as a generic map.
func fetchWorkflowV2(c *client.Client, id string) (map[string]any, error) {
	raw, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+id, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch workflow: %w", err)
	}
	var wf map[string]any
	if err := json.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	return wf, nil
}

// acquireWfv2Lock acquires an edit lock on a v2 workflow alias and returns the lockToken.
func acquireWfv2Lock(c *client.Client, alias, clientID string) (string, error) {
	body, _ := json.Marshal(map[string]string{"clientId": clientID})
	raw, _, err := c.Do("POST", "borrower_central", "/v2/workflows/"+alias+"/lock", json.RawMessage(body))
	if err != nil {
		return "", fmt.Errorf("acquire lock: %w", err)
	}
	var ack map[string]any
	if err := json.Unmarshal(raw, &ack); err != nil {
		return "", fmt.Errorf("parse lock response: %w", err)
	}
	tok, _ := ack["lockToken"].(string)
	if tok == "" {
		return "", fmt.Errorf("lock response missing lockToken")
	}
	return tok, nil
}

// releaseWfv2Lock releases a lock; failures are ignored since this is best-effort cleanup.
func releaseWfv2Lock(c *client.Client, alias, token string) {
	body, _ := json.Marshal(map[string]string{"lockToken": token})
	_, _, _ = c.Do("DELETE", "borrower_central", "/v2/workflows/"+alias+"/lock", json.RawMessage(body))
}

// applyLockClientIDPrefix is the clientId every `apply` run stamps on the lock
// it takes. It is also the marker that lets a later run recognise a lock one of
// its own crashed predecessors left behind.
const applyLockClientIDPrefix = "apply-"

// wfv2LockInfo is the subset of GET /v2/workflows/{alias}/lock that callers
// reason about when they are refused a lock.
type wfv2LockInfo struct {
	IsLocked   bool
	ClientID   string
	Email      string
	LockedAt   string
	ExpiresAt  string
	RenewCount int
}

// getWfv2LockStatus reads who currently holds the edit lock on an alias.
func getWfv2LockStatus(c *client.Client, alias string) (*wfv2LockInfo, error) {
	raw, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+alias+"/lock", nil)
	if err != nil {
		return nil, fmt.Errorf("read lock status: %w", err)
	}
	var resp struct {
		IsLocked bool `json:"isLocked"`
		Lock     struct {
			LockedBy struct {
				ClientID string `json:"clientId"`
				Email    string `json:"email"`
			} `json:"lockedBy"`
			LockedAt   string  `json:"lockedAt"`
			ExpiresAt  string  `json:"expiresAt"`
			RenewCount float64 `json:"renewCount"`
		} `json:"lock"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse lock status: %w", err)
	}
	return &wfv2LockInfo{
		IsLocked:   resp.IsLocked,
		ClientID:   resp.Lock.LockedBy.ClientID,
		Email:      resp.Lock.LockedBy.Email,
		LockedAt:   resp.Lock.LockedAt,
		ExpiresAt:  resp.Lock.ExpiresAt,
		RenewCount: int(resp.Lock.RenewCount),
	}, nil
}

// abandonedApplyLockMinAge is how long a lock stamped by apply must have sat
// unrenewed before another apply may reclaim it. apply never heartbeats, so
// renewCount alone cannot separate a crashed predecessor from a run that is
// alive right now -- and two applies racing on one alias (two CI jobs, say)
// would rob each other, the loser having already force-recreated the draft the
// winner is autosaving. An apply run reaches its own autosave in well under
// this, so a lock still this young is treated as live.
const abandonedApplyLockMinAge = 90 * time.Second

// isAbandonedApplyLock reports whether a blocking lock is one a previous apply
// run left behind, and is therefore safe to take.
//
// Three things must hold: apply's own clientId prefix, a renewCount of 0 (any
// renewal means something is keeping it alive -- a Hub tab heartbeats every 3
// minutes), and a lockedAt older than abandonedApplyLockMinAge. Anything else
// may be a live session, and BC reports a Hub tab belonging to the same person
// as SELF_LOCK_CONFLICT, so "self conflict" alone never justified taking it:
// stealing a live tab's lock discards whatever is unsaved on that canvas.
//
// An unparseable or absent lockedAt is treated as live -- refusing costs a
// re-run, stealing costs someone's work.
func isAbandonedApplyLock(info *wfv2LockInfo, now time.Time) bool {
	if info == nil || !info.IsLocked {
		return false
	}
	if !strings.HasPrefix(info.ClientID, applyLockClientIDPrefix) || info.RenewCount != 0 {
		return false
	}
	lockedAt, err := time.Parse(time.RFC3339, info.LockedAt)
	if err != nil {
		return false
	}
	return now.Sub(lockedAt) >= abandonedApplyLockMinAge
}

// isLockConflictErr reports whether an acquire failure was a lock conflict
// (LOCK_CONFLICT or SELF_LOCK_CONFLICT) rather than a transport/auth failure.
func isLockConflictErr(err error) bool {
	// SELF_LOCK_CONFLICT contains LOCK_CONFLICT, so one check covers both.
	return err != nil && strings.Contains(err.Error(), "LOCK_CONFLICT")
}

// lockHolderLabel renders a short "who holds it" description for logs.
func lockHolderLabel(info *wfv2LockInfo) string {
	if info == nil || !info.IsLocked {
		return "holder unknown"
	}
	who := info.Email
	if who == "" {
		who = "unknown user"
	}
	return fmt.Sprintf("%s, client-id=%s, renewCount=%d", who, info.ClientID, info.RenewCount)
}

// describeBlockingLock explains a refusal to take someone else's lock, and how
// to proceed either way.
func describeBlockingLock(alias string, info *wfv2LockInfo) string {
	if info == nil || !info.IsLocked {
		return fmt.Sprintf(
			"could not determine who holds the lock on %s. Re-run to retry, "+
				"or pass --force-lock to take the lock regardless (any unsaved "+
				"work in the holding session is discarded).", alias)
	}
	return fmt.Sprintf(
		"the lock on %s is held by %s and expires %s. That looks like a live "+
			"editing session, not an abandoned one, so apply will not take it: "+
			"close the Hub tab editing this workflow (or let its lock lapse) and "+
			"re-run. Pass --force-lock to take it anyway -- anything unsaved in "+
			"that session is discarded.",
		alias, lockHolderLabel(info), info.ExpiresAt)
}

// acquireApplyLock takes the edit lock for an apply run, recovering only from a
// lock its own crashed predecessor left behind unless forceLock is set.
func acquireApplyLock(c *client.Client, cmd *cobra.Command, alias, clientID string, forceLock bool) (string, error) {
	token, err := acquireWfv2Lock(c, alias, clientID)
	if err == nil {
		return token, nil
	}
	if !isLockConflictErr(err) {
		return "", fmt.Errorf("acquire lock on %s: %w", alias, err)
	}

	info, statusErr := getWfv2LockStatus(c, alias)
	if statusErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "# warning: could not read the lock on %s: %v\n", alias, statusErr)
	}

	switch {
	case forceLock:
		fmt.Fprintf(cmd.ErrOrStderr(), "# --force-lock: taking the lock on %s (%s)\n", alias, lockHolderLabel(info))
	case isAbandonedApplyLock(info, time.Now()):
		fmt.Fprintf(cmd.ErrOrStderr(), "# abandoned apply lock on %s (client-id=%s, never renewed, held since %s); force-releasing and retrying\n", alias, info.ClientID, info.LockedAt)
	default:
		return "", fmt.Errorf("acquire lock on %s: %w\n%s", alias, err, describeBlockingLock(alias, info))
	}

	forcePath := "/v2/workflows/" + alias + "/lock/force"
	_, _, ferr := c.Do("DELETE", "borrower_central", forcePath, nil)
	if ferr != nil {
		return "", fmt.Errorf("force-release lock on %s: %w", alias, ferr)
	}
	token, err = acquireWfv2Lock(c, alias, clientID)
	if err != nil {
		return "", fmt.Errorf("acquire lock on %s after force-release: %w", alias, err)
	}
	return token, nil
}

// mutateAndAutosaveV2 runs the lock-fetch-mutate-autosave-release dance.
//
// If lockToken is provided, the caller manages the lock (no acquire/release).
// Otherwise clientID must be provided; a lock is acquired and released here.
//
// The mutate function is called with the parsed workflow map and may modify it
// in place. Only the editable top-level fields are sent on autosave.
func mutateAndAutosaveV2(
	c *client.Client,
	workflowID, lockToken, clientID string,
	mutate func(wf map[string]any) error,
) (json.RawMessage, error) {
	wf, err := fetchWorkflowV2(c, workflowID)
	if err != nil {
		return nil, err
	}

	alias, _ := wf["alias"].(string)

	if lockToken == "" {
		if clientID == "" {
			return nil, fmt.Errorf("either --lock-token or --client-id is required")
		}
		if alias == "" {
			return nil, fmt.Errorf("workflow has no alias; cannot acquire lock")
		}
		tok, err := acquireWfv2Lock(c, alias, clientID)
		if err != nil {
			return nil, err
		}
		lockToken = tok
		defer releaseWfv2Lock(c, alias, tok)
	}

	if err := mutate(wf); err != nil {
		return nil, err
	}

	saveBody := map[string]any{"lockToken": lockToken}
	for _, k := range []string{
		"label", "description", "nodes", "edges", "notes",
		"inputVariables", "customVariables", "config",
	} {
		if v, ok := wf[k]; ok {
			saveBody[k] = v
		}
	}
	if v, ok := wf["version"]; ok {
		saveBody["lastKnownVersion"] = v
	}

	raw, err := json.Marshal(saveBody)
	if err != nil {
		return nil, fmt.Errorf("encode autosave body: %w", err)
	}
	result, _, err := c.Do("PUT", "borrower_central", "/v2/workflows/"+workflowID+"/autosave", json.RawMessage(raw))
	if err != nil {
		return nil, fmt.Errorf("autosave: %w", err)
	}
	return result, nil
}

// asSlice coerces any (possibly nil) value into []any.
func asSlice(v any) []any {
	if v == nil {
		return []any{}
	}
	if s, ok := v.([]any); ok {
		return s
	}
	return []any{}
}

// publishSuffix renders the publish-or-not tail used in apply's DRAFT-adoption
// dry-run message.
func publishSuffix(publish bool) string {
	if publish {
		return " + publish"
	}
	return " (stays DRAFT; pass --publish to go live)"
}

// isScopedRef reports whether s is a scoped reference (a dotted path like
// inputs.x / task_outputs.a.b, or the __static__:: literal escape). Bare names
// like "total_score" are unscoped and won't resolve at runtime.
func isScopedRef(s string) bool {
	return strings.Contains(s, ".") || strings.HasPrefix(s, "__static__::")
}

// lastDotSegment returns the substring after the final "." (or all of s if none).
func lastDotSegment(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// asMap coerces any (possibly nil) value into map[string]any.
func asMap(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// addLockFlags binds the standard --lock-token / --client-id flags to a command.
func addLockFlags(cmd *cobra.Command, lockToken, clientID *string) {
	cmd.Flags().StringVar(lockToken, "lock-token", "", "lockToken from a prior 'lock acquire' (caller-managed)")
	cmd.Flags().StringVar(clientID, "client-id", "", "client id; if --lock-token is omitted, a lock is acquired and released automatically")
}

// ===================== Helper subcommands =====================

func makeWfv2AddNodeCmd() *cobra.Command {
	var lockToken, clientID string
	var nodeType, nodeID, label, description, dataJSON string
	var taskAlias, taskID string
	var taskVersion int
	var posX, posY float64

	cmd := &cobra.Command{
		Use:   "add-node <workflow-id>",
		Short: "Append a node to a v2 workflow graph",
		Long: `Append a node to nodes[] and autosave. Handles the lock dance for you
unless --lock-token is supplied.

v2 nodes are graph-only: the executable config (url/method for HTTP,
sources_config for AltData, evaluatorAlias for evaluators, etc.) lives
on a Task resource at /v2/tasks. Create the task first with
'altscore tasks-v2 create', then reference it here via --task-alias
(and optionally --task-version) or --task-id.

start/end nodes don't need a task reference; everything else does.

--data accepts arbitrary extra JSON merged into node.data (input_mappings,
custom UI hints, etc.). Do not put task config in --data.`,
		Example: `  # 1. Create the underlying task first
  altscore tasks-v2 create --body '{"alias":"fetch","label":"Fetch","type":"altdata-enrichment","sourcesConfig":[{"sourceId":"ECU-PUB-0002","version":"v1"}]}'

  # 2. Reference it from the workflow node (auto-managed lock)
  altscore workflows-v2 add-node <id> --client-id cli-1 \
    --type altdata-enrichment --node-id fetch --label "Fetch" --task-alias fetch

  # Caller-managed lock with explicit version pin
  altscore workflows-v2 add-node <id> --lock-token "$TOKEN" \
    --type evaluate-rules --node-id score --label "Score" \
    --task-alias scoring --task-version 3

  # Start/end nodes: no task reference needed
  altscore workflows-v2 add-node <id> --lock-token "$TOKEN" \
    --type start --node-id start --label "Start"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if nodeType == "" || nodeID == "" || label == "" {
				return fmt.Errorf("--type, --node-id, and --label are required")
			}
			if taskAlias == "" && taskID == "" {
				return fmt.Errorf(
					"every node needs --task-alias (or --task-id) -- including start/end/conditional. " +
						"The Hub creates a backing task for every node, including trivial ones. " +
						"Create the task first with 'altscore tasks-v2 create' OR scaffold the whole workflow with " +
						"'altscore workflows-v2 apply --body @spec.json'.")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}

			var dataExtra map[string]any
			if dataJSON != "" {
				if err := json.Unmarshal([]byte(dataJSON), &dataExtra); err != nil {
					return fmt.Errorf("invalid --data JSON: %w", err)
				}
			}

			mutate := func(wf map[string]any) error {
				nodes := asSlice(wf["nodes"])
				for _, n := range nodes {
					if nm, ok := n.(map[string]any); ok {
						if id, _ := nm["nodeId"].(string); id == nodeID {
							return fmt.Errorf("nodeId %q already exists", nodeID)
						}
					}
				}

				data := map[string]any{}
				for k, v := range dataExtra {
					data[k] = v
				}

				node := map[string]any{
					"nodeId":   nodeID,
					"type":     nodeType,
					"label":    label,
					"position": map[string]float64{"x": posX, "y": posY},
					"data":     data,
				}
				if description != "" {
					node["description"] = description
				}
				if taskAlias != "" {
					node["taskAlias"] = taskAlias
				}
				if taskID != "" {
					node["taskId"] = taskID
				}
				if cmd.Flags().Changed("task-version") {
					node["taskVersion"] = taskVersion
				}
				wf["nodes"] = append(nodes, node)
				return nil
			}

			result, err := mutateAndAutosaveV2(c, args[0], lockToken, clientID, mutate)
			if err != nil {
				return err
			}
			return output.RawJSON(result)
		},
	}

	cmd.Flags().StringVar(&nodeType, "type", "", "node type (start, end, altdata-enrichment, evaluate-rules, http, conditional, ...) [required]")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "stable nodeId for the new node [required]")
	cmd.Flags().StringVar(&label, "label", "", "display label [required]")
	cmd.Flags().StringVar(&description, "description", "", "node description")
	cmd.Flags().StringVar(&taskAlias, "task-alias", "", "alias of an existing /v2/tasks resource (recommended)")
	cmd.Flags().IntVar(&taskVersion, "task-version", 0, "pin to a specific task version (default: latest)")
	cmd.Flags().StringVar(&taskID, "task-id", "", "task id (use --task-alias unless you have a reason to pin id)")
	cmd.Flags().Float64Var(&posX, "position-x", 0, "canvas x position")
	cmd.Flags().Float64Var(&posY, "position-y", 0, "canvas y position")
	cmd.Flags().StringVar(&dataJSON, "data", "", "extra fields merged into node.data (JSON)")
	addLockFlags(cmd, &lockToken, &clientID)
	return cmd
}

func makeWfv2RemoveNodeCmd() *cobra.Command {
	var lockToken, clientID string
	var nodeID string
	var keepEdges bool

	cmd := &cobra.Command{
		Use:     "remove-node <workflow-id>",
		Short:   "Remove a node (and by default its edges) from a v2 workflow",
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 remove-node <id> --node-id fetch --client-id cli-1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if nodeID == "" {
				return fmt.Errorf("--node-id is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			mutate := func(wf map[string]any) error {
				nodes := asSlice(wf["nodes"])
				kept := make([]any, 0, len(nodes))
				found := false
				for _, n := range nodes {
					nm, _ := n.(map[string]any)
					if id, _ := nm["nodeId"].(string); id == nodeID {
						found = true
						continue
					}
					kept = append(kept, n)
				}
				if !found {
					return fmt.Errorf("nodeId %q not found", nodeID)
				}
				wf["nodes"] = kept

				if !keepEdges {
					edges := asSlice(wf["edges"])
					keptE := make([]any, 0, len(edges))
					for _, e := range edges {
						em, _ := e.(map[string]any)
						src, _ := em["sourceNodeId"].(string)
						tgt, _ := em["targetNodeId"].(string)
						if src == nodeID || tgt == nodeID {
							continue
						}
						keptE = append(keptE, e)
					}
					wf["edges"] = keptE
				}
				return nil
			}
			result, err := mutateAndAutosaveV2(c, args[0], lockToken, clientID, mutate)
			if err != nil {
				return err
			}
			return output.RawJSON(result)
		},
	}

	cmd.Flags().StringVar(&nodeID, "node-id", "", "id of node to remove [required]")
	cmd.Flags().BoolVar(&keepEdges, "keep-edges", false, "do not also remove edges referencing this node")
	addLockFlags(cmd, &lockToken, &clientID)
	return cmd
}

func makeWfv2AddEdgeCmd() *cobra.Command {
	var lockToken, clientID string
	var edgeID, source, target, label, sourceHandle, targetHandle string

	cmd := &cobra.Command{
		Use:   "add-edge <workflow-id>",
		Short: "Append an edge to a v2 workflow",
		Long:  `Append an edge from --source to --target. --id auto-generates as "{source}->{target}" if omitted.`,
		Example: `  altscore workflows-v2 add-edge <id> --source fetch --target score --client-id cli-1
  altscore workflows-v2 add-edge <id> --source score --target route --label "Success" --source-handle out_success --client-id cli-1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if source == "" || target == "" {
				return fmt.Errorf("--source and --target are required")
			}
			if edgeID == "" {
				edgeID = fmt.Sprintf("%s->%s", source, target)
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			mutate := func(wf map[string]any) error {
				edges := asSlice(wf["edges"])
				for _, e := range edges {
					em, _ := e.(map[string]any)
					if id, _ := em["id"].(string); id == edgeID {
						return fmt.Errorf("edge id %q already exists", edgeID)
					}
				}
				edge := map[string]any{
					"id":           edgeID,
					"sourceNodeId": source,
					"targetNodeId": target,
				}
				if label != "" {
					edge["label"] = label
				}
				if sourceHandle != "" {
					edge["sourceHandle"] = sourceHandle
				}
				if targetHandle != "" {
					edge["targetHandle"] = targetHandle
				}
				wf["edges"] = append(edges, edge)
				return nil
			}
			result, err := mutateAndAutosaveV2(c, args[0], lockToken, clientID, mutate)
			if err != nil {
				return err
			}
			return output.RawJSON(result)
		},
	}

	cmd.Flags().StringVar(&edgeID, "id", "", "edge id (default: source->target)")
	cmd.Flags().StringVar(&source, "source", "", "source node id [required]")
	cmd.Flags().StringVar(&target, "target", "", "target node id [required]")
	cmd.Flags().StringVar(&label, "label", "", "edge label")
	cmd.Flags().StringVar(&sourceHandle, "source-handle", "", "source handle (e.g. out_success)")
	cmd.Flags().StringVar(&targetHandle, "target-handle", "", "target handle")
	addLockFlags(cmd, &lockToken, &clientID)
	return cmd
}

func makeWfv2RemoveEdgeCmd() *cobra.Command {
	var lockToken, clientID string
	var edgeID, source, target string

	cmd := &cobra.Command{
		Use:   "remove-edge <workflow-id>",
		Short: "Remove an edge from a v2 workflow",
		Long:  `Identify the edge by --id, or by both --source and --target. The first match is removed.`,
		Example: `  altscore workflows-v2 remove-edge <id> --id fetch->score --client-id cli-1
  altscore workflows-v2 remove-edge <id> --source fetch --target score --client-id cli-1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if edgeID == "" && (source == "" || target == "") {
				return fmt.Errorf("either --id, or both --source and --target, are required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			mutate := func(wf map[string]any) error {
				edges := asSlice(wf["edges"])
				kept := make([]any, 0, len(edges))
				removed := false
				for _, e := range edges {
					em, _ := e.(map[string]any)
					if !removed {
						if edgeID != "" {
							if id, _ := em["id"].(string); id == edgeID {
								removed = true
								continue
							}
						} else {
							src, _ := em["sourceNodeId"].(string)
							tgt, _ := em["targetNodeId"].(string)
							if src == source && tgt == target {
								removed = true
								continue
							}
						}
					}
					kept = append(kept, e)
				}
				if !removed {
					return fmt.Errorf("no matching edge found")
				}
				wf["edges"] = kept
				return nil
			}
			result, err := mutateAndAutosaveV2(c, args[0], lockToken, clientID, mutate)
			if err != nil {
				return err
			}
			return output.RawJSON(result)
		},
	}

	cmd.Flags().StringVar(&edgeID, "id", "", "edge id to remove")
	cmd.Flags().StringVar(&source, "source", "", "source node id (use with --target)")
	cmd.Flags().StringVar(&target, "target", "", "target node id (use with --source)")
	addLockFlags(cmd, &lockToken, &clientID)
	return cmd
}

func makeWfv2SetVariableCmd() *cobra.Command {
	var lockToken, clientID string
	var name, scope, varType, defaultJSON, description string
	var required bool

	cmd := &cobra.Command{
		Use:   "set-variable <workflow-id>",
		Short: "Add or update an input or custom variable on a v2 workflow",
		Long: `Set inputVariables[name] (scope=input) or customVariables[name] (scope=custom).

For input scope, the value is a SchemaTypes object: {type, default?, description?, ...}.
For custom scope, the value is the raw default expression or value.

--type: string | integer | number | boolean | object | array | secret
--default: JSON-encoded default value (e.g. '"alice"' for a string, '750' for a number)`,
		Example: `  altscore workflows-v2 set-variable <id> --client-id cli-1 \
    --scope input --name borrower_id --type string --required

  altscore workflows-v2 set-variable <id> --client-id cli-1 \
    --scope input --name min_score --type integer --default 700

  altscore workflows-v2 set-variable <id> --client-id cli-1 \
    --scope custom --name normalized_score --default '"{{task_outputs.score.value}}"'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if scope != "input" && scope != "custom" {
				return fmt.Errorf(`--scope must be "input" or "custom"`)
			}
			c, err := loadClient()
			if err != nil {
				return err
			}

			var defaultVal any
			hasDefault := cmd.Flags().Changed("default")
			if hasDefault {
				if err := json.Unmarshal([]byte(defaultJSON), &defaultVal); err != nil {
					return fmt.Errorf("invalid --default JSON: %w", err)
				}
			}

			mutate := func(wf map[string]any) error {
				if scope == "input" {
					inputs := asMap(wf["inputVariables"])
					schema := map[string]any{}
					if varType != "" {
						schema["type"] = varType
					} else {
						existing := asMap(inputs[name])
						if t, ok := existing["type"]; ok {
							schema["type"] = t
						} else {
							schema["type"] = "string"
						}
					}
					if hasDefault {
						schema["default"] = defaultVal
					}
					if description != "" {
						schema["description"] = description
					}
					if required {
						schema["required"] = true
					}
					inputs[name] = schema
					wf["inputVariables"] = inputs
				} else {
					customs := asMap(wf["customVariables"])
					if hasDefault {
						customs[name] = defaultVal
					} else {
						customs[name] = nil
					}
					wf["customVariables"] = customs
				}
				return nil
			}
			result, err := mutateAndAutosaveV2(c, args[0], lockToken, clientID, mutate)
			if err != nil {
				return err
			}
			return output.RawJSON(result)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "variable name [required]")
	cmd.Flags().StringVar(&scope, "scope", "input", `"input" or "custom"`)
	cmd.Flags().StringVar(&varType, "type", "", "input scope: string|integer|number|boolean|object|array|secret")
	cmd.Flags().StringVar(&defaultJSON, "default", "", "default value (JSON-encoded)")
	cmd.Flags().StringVar(&description, "description", "", "input scope: human-readable description")
	cmd.Flags().BoolVar(&required, "required", false, "input scope: mark as required")
	addLockFlags(cmd, &lockToken, &clientID)
	return cmd
}

func makeWfv2UnsetVariableCmd() *cobra.Command {
	var lockToken, clientID string
	var name, scope string

	cmd := &cobra.Command{
		Use:     "unset-variable <workflow-id>",
		Short:   "Remove an input or custom variable from a v2 workflow",
		Args:    cobra.ExactArgs(1),
		Example: `  altscore workflows-v2 unset-variable <id> --client-id cli-1 --scope input --name old_var`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if scope != "input" && scope != "custom" {
				return fmt.Errorf(`--scope must be "input" or "custom"`)
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			mutate := func(wf map[string]any) error {
				key := "inputVariables"
				if scope == "custom" {
					key = "customVariables"
				}
				m := asMap(wf[key])
				if _, ok := m[name]; !ok {
					return fmt.Errorf("variable %q not found in %s scope", name, scope)
				}
				delete(m, name)
				wf[key] = m
				return nil
			}
			result, err := mutateAndAutosaveV2(c, args[0], lockToken, clientID, mutate)
			if err != nil {
				return err
			}
			return output.RawJSON(result)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "variable name [required]")
	cmd.Flags().StringVar(&scope, "scope", "input", `"input" or "custom"`)
	addLockFlags(cmd, &lockToken, &clientID)
	return cmd
}

func makeWfv2SetMappingCmd() *cobra.Command {
	var lockToken, clientID string
	var nodeID, inputName, expression string
	var clear bool

	cmd := &cobra.Command{
		Use:   "set-mapping <workflow-id>",
		Short: "Set or clear an input mapping on a node",
		Long: `Set node.data.input_mappings[--input-name] = --expression on a node.

Expression syntax (variable system scopes):
  inputs.<name>
  task_outputs.<taskAlias>.<field>
  custom.<name>
  system.<key>
  task_outputs_by_type.<taskType>[<idx>].<field>
  entity.<root>.<group>.<key>[.<subkey>]

Use --clear to remove the mapping instead.`,
		Example: `  altscore workflows-v2 set-mapping <id> --client-id cli-1 \
    --node-id score --input-name borrower_id --expression "inputs.borrower_id"

  altscore workflows-v2 set-mapping <id> --client-id cli-1 \
    --node-id score --input-name credit_data --expression "task_outputs.fetch.result"

  altscore workflows-v2 set-mapping <id> --client-id cli-1 \
    --node-id score --input-name old_input --clear`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if nodeID == "" || inputName == "" {
				return fmt.Errorf("--node-id and --input-name are required")
			}
			if !clear && expression == "" {
				return fmt.Errorf("--expression is required (or use --clear to remove)")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			mutate := func(wf map[string]any) error {
				nodes := asSlice(wf["nodes"])
				for i, n := range nodes {
					nm, _ := n.(map[string]any)
					if id, _ := nm["nodeId"].(string); id == nodeID {
						data := asMap(nm["data"])
						mappings := asMap(data["inputMappings"])
						if clear {
							if _, ok := mappings[inputName]; !ok {
								return fmt.Errorf("mapping %q not found on node %q", inputName, nodeID)
							}
							delete(mappings, inputName)
						} else {
							mappings[inputName] = expression
						}
						data["inputMappings"] = mappings
						nm["data"] = data
						nodes[i] = nm
						wf["nodes"] = nodes
						return nil
					}
				}
				return fmt.Errorf("nodeId %q not found", nodeID)
			}
			result, err := mutateAndAutosaveV2(c, args[0], lockToken, clientID, mutate)
			if err != nil {
				return err
			}
			return output.RawJSON(result)
		},
	}

	cmd.Flags().StringVar(&nodeID, "node-id", "", "target node id [required]")
	cmd.Flags().StringVar(&inputName, "input-name", "", "input field name on the node [required]")
	cmd.Flags().StringVar(&expression, "expression", "", "mapping expression (see --help)")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the mapping instead of setting it")
	addLockFlags(cmd, &lockToken, &clientID)
	return cmd
}
