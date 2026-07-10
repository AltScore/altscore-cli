package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// apply takes a single agent-friendly spec and reconciles it against the
// tenant: if no ACTIVE workflow shares the spec's alias, it creates one
// (POST /v2/tasks per task + POST /v2/workflows + optional publish); if a
// match exists, it updates in place (fresh /v2/tasks + create-draft +
// lock + autosave + publish, same workflow id and alias retained). After
// either path, every referenced credit-decisioning entity (scorecards,
// rule-trees, evaluation-rules, mapping-tables, and their nested rules)
// is re-stamped to the workflow's alias so the Hub elements panel stays
// in sync. One verb, one validation pipeline, no fork-vs-update branch
// for the caller.
//
// Spec shape (any field omitted falls through to API defaults):
//
//	{
//	  "label":           "Scoring pipeline",
//	  "category":        "EVALUATION",
//	  "description":     "...",
//	  "inputVariables":  {"borrower_id": {"type": "string", "required": true}},
//	  "customVariables": {},
//	  "nodes": [
//	    {"ref": "start", "type": "start", "label": "Start"},
//	    {
//	      "ref":             "fetch",
//	      "label":           "Fetch ECU bureau",
//	      "type":            "altdata-enrichment",
//	      "sourcesConfig":   [{"sourceId":"ECU-PUB-0002","version":"v1"}],
//	      "borrowerIdField": "borrower_id",
//	      "inputMappings":   {"borrower_id": "inputs.borrower_id"}
//	      // ... any other CreateTaskV2 field
//	    },
//	    {"ref": "end", "type": "end", "label": "End"}
//	  ],
//	  "edges": [
//	    {"from": "start", "to": "fetch"},
//	    {"from": "fetch", "to": "end"}
//	  ]
//	}
//
// Behavior:
//  1. POST /v2/tasks for each task-bearing entry in spec.nodes (everything
//     except type=="start"), capturing the returned alias.
//  2. Build the graph: one node per spec.nodes entry (nodeId = ref, taskAlias
//     = server-assigned alias for task-bearing nodes; start nodes are
//     graph-only with no backing task).
//  3. Auto-position nodes left-to-right (start first, then task-bearing
//     nodes in spec order); the Hub UI re-lays them out anyway.
//  4. POST /v2/workflows with label, category, description, inputVariables,
//     customVariables, nodes, edges, status (default DRAFT).
//
// On any task-create failure, the partial state is reported and the workflow
// is not created. Use --rollback-tasks to cascade-delete created tasks on
// failure (best-effort; tasks-v2 has no DELETE today, so this is a no-op
// placeholder for future API support).

type composeSpec struct {
	Label           string         `json:"label"`
	Alias           string         `json:"alias,omitempty"`
	Category        string         `json:"category"`
	// Pointer so an explicit "" (blank the description) is distinguishable from
	// an omitted field (leave the existing description untouched on update).
	Description     *string        `json:"description,omitempty"`
	Status          string         `json:"status,omitempty"`
	InputVariables  map[string]any `json:"inputVariables,omitempty"`
	CustomVariables map[string]any `json:"customVariables,omitempty"`
	Config          map[string]any `json:"config,omitempty"`
	// Nodes is the workflow's graph -- one flat list, one entry per node
	// (start, end, every task type). Apply dispatches each entry by `type`
	// at parse time. This is the only accepted input shape.
	//
	// The legacy two-bucket shape (`tasks[]` + `extraNodes[]`) was removed:
	// it half-worked, with fields like inputMappings / endConfig /
	// htmlSections on extraNodes entries getting silently stripped or only
	// partially honored depending on the field. detectLegacySpecShape()
	// catches that input early and emits a one-shot rewrite suggestion.
	Nodes []map[string]any `json:"nodes,omitempty"`
	Edges []map[string]any `json:"edges"`
	Notes []map[string]any `json:"notes,omitempty"`

	// Tasks and ExtraNodes are INTERNAL buckets used by the downstream
	// build pipeline -- populated by splitting Nodes at parse time.
	// No JSON tags: never read from user input. The internal split is
	// type=="start" -> ExtraNodes (graph-only); everything else
	// (including end) -> Tasks (backing task created). Renaming these
	// to taskNodes/graphOnlyNodes is a future cleanup; the user-facing
	// contract (Nodes) is what matters here.
	Tasks      []map[string]any `json:"-"`
	ExtraNodes []map[string]any `json:"-"`
}

// detectLegacySpecShape rejects specs that use the removed
// `tasks[]` + `extraNodes[]` two-bucket shape. Returns an error with a
// concrete `nodes[]` rewrite when either key is present and non-empty.
// Called pre-unmarshal so the message can cite the user's input verbatim.
func detectLegacySpecShape(body []byte) error {
	var peek map[string]json.RawMessage
	if err := json.Unmarshal(body, &peek); err != nil {
		// Let the caller's main Unmarshal produce the proper parse error.
		return nil
	}
	countArray := func(key string) int {
		raw, ok := peek[key]
		if !ok {
			return 0
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return 0
		}
		return len(arr)
	}
	nTasks := countArray("tasks")
	nExtra := countArray("extraNodes")
	if nTasks == 0 && nExtra == 0 {
		return nil
	}
	var got []string
	if nTasks > 0 {
		got = append(got, fmt.Sprintf("tasks[] (%d entries)", nTasks))
	}
	if nExtra > 0 {
		got = append(got, fmt.Sprintf("extraNodes[] (%d entries)", nExtra))
	}
	return fmt.Errorf(`spec uses the removed two-bucket shape: %s.
the `+"`tasks[]`"+` and `+"`extraNodes[]`"+` keys were removed -- use the flat `+"`nodes[]`"+` shape instead.

how to migrate (per-entry bodies are unchanged, only the wrapping key changes):

  before:
    {
      "tasks":      [{"ref":"fetch","type":"altdata-enrichment","...":"..."},
                     {"ref":"score","type":"scorecard","...":"..."}],
      "extraNodes": [{"ref":"start","type":"start","label":"Start"},
                     {"ref":"end","type":"end","endConfig":{"...":"..."}}]
    }

  after:
    {
      "nodes": [
        {"ref":"start","type":"start","label":"Start"},
        {"ref":"fetch","type":"altdata-enrichment","...":"..."},
        {"ref":"score","type":"scorecard","...":"..."},
        {"ref":"end","type":"end","endConfig":{"...":"..."}}
      ]
    }

order inside nodes[] is cosmetic; edges still drive execution order.
move every entry verbatim -- ref, type, label, inputMappings, endConfig,
htmlSections, sourcesConfig, every field carries over unchanged`,
		strings.Join(got, " and "))
}

func makeWfv2ApplyCmd() *cobra.Command {
	var bodyFlag string
	var dryRun bool
	var diffFlag bool
	var publish bool
	var skipLintOnPublish bool
	var skipRescope bool
	var allowStealOwnership bool
	var verify bool
	var noAutoDefaults bool

	cmd := &cobra.Command{
		Use:     "apply",
		Aliases: []string{"compose"},
		Short:   "Declaratively create-or-update a v2 workflow from a single spec (alias: compose). Use --dry-run or --diff to preview changes before mutating.",
		Long: `Declarative reconciliation of a v2 workflow against the spec. One verb
covers both greenfield create and update-in-place. Same validation pipeline
for both paths -- specs that pass apply against a fresh tenant also pass
when re-applied against a tenant that already has the workflow. Also available
as 'compose' (alias) -- both invocations are identical. Preview changes before
mutating with --dry-run (prints the assembled body) or --diff (shows a per-
section diff against the current tenant state).

The target workflow is resolved by alias:
  - spec.alias if set, otherwise slugifyWorkflowLabel(spec.label).
  - If no ACTIVE workflow has that alias -> create path:
    POST /v2/tasks for every task, POST /v2/workflows, optional publish.
  - If exactly one ACTIVE workflow has that alias -> update path:
    Create fresh tasks (old tasks orphan, that's accepted), open a clean
    draft via create-draft --force-recreate, acquire the lock, autosave the
    new nodes/edges/variables/config, then publish. Same workflow id, same
    alias, version increments, schedules / entity-scope survive.

After either path apply walks the spec's dependency graph and stamps every
referenced credit-decisioning entity (scorecards, rule-trees, evaluation-
rules, mapping-tables, and nested rules within them) to the workflow's
alias -- but only when the entity is currently UNSCOPED or ALREADY scoped
to this workflow. If an entity is owned by ANOTHER workflow, apply refuses
to silently transfer ownership and errors out with a clone-the-entity
suggestion. Each v2 workflow owns its credit-decisioning entities 1:1;
silently re-stamping a cross-owned entity makes it disappear from the
previous owner's Hub element panel. Pass --allow-steal-ownership to
override (rare: workflow rename / identity migration / decommissioning the
old owner). Pass --skip-rescope to disable the entire rescope step (then
a stale scope shows as a hard error in normalize and the agent has to fix
it manually).

DRAFT vs publish: the create path saves the workflow in DRAFT by default,
mirror of the Hub editor's save-then-publish flow. A DRAFT executes its full
graph faithfully (so you can test it by id before publishing) -- it simply
isn't the version the alias serves until published. Pass --publish to publish
immediately; that's the common case when applying from the CLI. The update path
always publishes (autosave -> publish), because the underlying assumption of
"apply" is the spec is the desired state.

Use --dry-run to print what would be sent without making any API calls.
Use --diff to preview structural changes against the current tenant state:
apply fetches the existing workflow (if any) and prints a per-section diff
of metadata, tasks, edges, inputVariables, customVariables, and any entity-
scope conflicts. No API mutations. Pairs well with the UPDATE path -- see
exactly what version-bump will change before pulling the trigger.

Spec format (see file header for full reference):
  - label, alias?, category, description, status (DRAFT default)
  - inputVariables, customVariables
  - nodes: flat list of every graph node. Apply dispatches each entry by
    type at parse time -- 'start' nodes are graph-only, everything else
    (including 'end') gets a backing task created. This is the only
    accepted shape; the legacy 'tasks[]' + 'extraNodes[]' two-bucket
    shape was removed -- apply rejects it with an inline rewrite.
  - edges: list of {from, to, sourceHandle?, label?}`,
		Example: `  altscore workflows-v2 apply --body @scoring-pipeline.json
  altscore workflows-v2 apply --body @spec.json --publish          # create+publish OR update+publish
  altscore workflows-v2 apply --body @spec.json --dry-run
  altscore workflows-v2 apply --body @spec.json --diff              # preview changes vs current tenant state
  altscore workflows-v2 apply --body @spec.json --skip-rescope     # leave entity scopes alone`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Mutually exclusive: --diff, --dry-run, --publish. --diff is a
			// read-only preview; --dry-run prints the assembled body without
			// hitting the API; --publish mutates. Pick one.
			modes := 0
			if diffFlag {
				modes++
			}
			if dryRun {
				modes++
			}
			if publish {
				modes++
			}
			if modes > 1 {
				return fmt.Errorf("--diff, --dry-run, and --publish are mutually exclusive; pick one")
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			// Reject the legacy `tasks[]` + `extraNodes[]` two-bucket shape with
			// an inline rewrite suggestion before parsing into the spec struct.
			// Doing it pre-unmarshal lets us cite the original keys verbatim.
			if err := detectLegacySpecShape(body); err != nil {
				return err
			}
			var spec composeSpec
			if err := json.Unmarshal(body, &spec); err != nil {
				return fmt.Errorf("invalid spec JSON: %w", err)
			}
			if spec.Label == "" {
				return fmt.Errorf("spec.label is required")
			}
			if len(spec.Nodes) == 0 {
				return fmt.Errorf("spec.nodes is required and must contain at least one node (start + at least one task-bearing node)")
			}
			// Split spec.Nodes into the internal Tasks / ExtraNodes buckets
			// the rest of the build pipeline expects. type=="start" is graph-
			// only (ExtraNodes); everything else (including end) gets a
			// backing task created (Tasks).
			for _, n := range spec.Nodes {
				t, _ := n["type"].(string)
				if t == "start" {
					spec.ExtraNodes = append(spec.ExtraNodes, n)
				} else {
					spec.Tasks = append(spec.Tasks, n)
				}
			}
			spec.Nodes = nil
			// BC's category enum is uppercase (ACTION / EVALUATION / CONTACT /
			// OTHER). Help text / dry-run accept any case, then BC rejects
			// lowercase with a 400. Normalize here so users don't have to
			// shout. Status mirrors the same convention (DRAFT/ACTIVE).
			if spec.Category != "" {
				spec.Category = strings.ToUpper(spec.Category)
			}
			if spec.Status != "" {
				spec.Status = strings.ToUpper(spec.Status)
			}

			c, err := loadClient()
			if err != nil {
				return err
			}

			// Determine target alias (predicted before any API call).
			targetAlias := spec.Alias
			if targetAlias == "" {
				targetAlias = slugifyWorkflowLabel(spec.Label)
			}

			// Lookup: is there an existing workflow with this alias on the
			// tenant? Prefer ACTIVE, else fall back to the latest DRAFT so a
			// prior un-published apply is updated in place rather than forked.
			// dry-run still does the lookup so the agent sees which branch will
			// fire when they un-dry the run.
			existing, existingStatus, lookupErr := findWorkflowByAlias(c, targetAlias)
			if lookupErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "# warning: alias lookup for %q failed (%v); falling back to create path\n", targetAlias, lookupErr)
			}

			// Build the workflow body. composeWorkflowBody POSTs new tasks
			// for every node (create AND update paths use fresh tasks; the
			// old tasks orphan on the update path, that's accepted -- no
			// /v2/tasks DELETE exists today). In diff mode, we run compose
			// in dryRun=true so no /v2/tasks POSTs happen; the assembled body
			// is purely for shape-comparison against the current tenant.
			// We also force allowStealOwnership=true in diff mode so cross-
			// owned entities don't abort compose -- they're surfaced in the
			// diff output as "would RE-STAMP" instead, which is the whole
			// point of a preview tool.
			composeAllowSteal := allowStealOwnership
			if diffFlag {
				composeAllowSteal = true
			}

			// Assemble the workflow body. compose in dryRun mode assembles the
			// full graph with spec-local refs standing in for the not-yet-minted
			// server aliases and POSTs nothing. The real (posting) path validates
			// the assembled graph against BC's server-side pre-flight BEFORE it
			// POSTs any /v2/tasks -- a graph BC would reject at create/publish
			// otherwise leaks task versions with no rollback.
			var workflow map[string]any
			switch {
			case diffFlag:
				// Read-only preview: assemble dry, skip the server pre-flight.
				workflow, err = composeWorkflowBody(c, &spec, true, publish, !skipRescope, composeAllowSteal, !noAutoDefaults, nil)
				if err != nil {
					return err
				}
			case dryRun:
				// Dry-run: assemble dry, then run the server pre-flight and print
				// its findings. Advisory here -- dry-run mutates nothing and still
				// prints the assembled body below regardless of findings.
				capture := newComposeCapture()
				workflow, err = composeWorkflowBody(c, &spec, true, publish, !skipRescope, composeAllowSteal, !noAutoDefaults, capture)
				if err != nil {
					return err
				}
				serverPreflightValidate(c, cmd, workflow, capture, false)
			default:
				// Real apply (create OR update): validate before posting anything,
				// abort on server errors.
				workflow, err = applyAssembleValidateAndPost(c, cmd, &spec, publish, skipRescope, allowStealOwnership, noAutoDefaults)
				if err != nil {
					return err
				}
			}

			wfBody, err := json.Marshal(workflow)
			if err != nil {
				return err
			}

			if diffFlag {
				return diffWorkflow(c, cmd, &spec, workflow, existing, targetAlias)
			}

			if dryRun {
				if existing != nil {
					existingID, _ := existing["id"].(string)
					if existingStatus == "DRAFT" {
						fmt.Fprintf(cmd.OutOrStderr(), "# DRY RUN -- apply UPDATE path: would adopt existing DRAFT id=%s alias=%s (lock + autosave%s)\n", existingID, targetAlias, publishSuffix(publish))
					} else {
						fmt.Fprintf(cmd.OutOrStderr(), "# DRY RUN -- apply UPDATE path: would draft + autosave + publish ACTIVE workflow id=%s alias=%s\n", existingID, targetAlias)
					}
				} else {
					fmt.Fprintln(cmd.OutOrStderr(), "# DRY RUN -- apply CREATE path: would POST /v2/workflows with the body below")
				}
				return output.RawJSON(json.RawMessage(wfBody))
			}

			var resultJSON []byte
			var wfID string

			if existing == nil {
				// === CREATE path ===
				data, _, err := c.Do("POST", "borrower_central", "/v2/workflows", json.RawMessage(wfBody))
				if err != nil {
					return fmt.Errorf("create workflow: %w", err)
				}
				printComposeSummary(cmd.ErrOrStderr(), workflow)

				var created map[string]any
				if err := json.Unmarshal(data, &created); err != nil {
					return fmt.Errorf("parse created workflow response: %w", err)
				}
				wfID, _ = created["id"].(string)
				if wfID == "" {
					return fmt.Errorf("apply create: response had no 'id' field")
				}
				resultJSON = data

				if publish {
					if err := lintAndPublish(c, cmd, wfID, skipLintOnPublish); err != nil {
						return err
					}
				}
			} else {
				// === UPDATE path ===
				existingID, _ := existing["id"].(string)
				if existingID == "" {
					return fmt.Errorf("apply update: existing workflow %q has no id field", targetAlias)
				}
				wfID = existingID

				// Pull last-known-version BEFORE creating the draft so the
				// autosave can pass it for optimistic concurrency. Drafting
				// bumps the version; we want the pre-draft number.
				lastKnownVersion := 0
				if v, ok := existing["version"].(float64); ok {
					lastKnownVersion = int(v)
				}

				adoptDraft := existingStatus == "DRAFT"
				fmt.Fprintf(cmd.ErrOrStderr(), "# apply UPDATE path: workflow id=%s alias=%s status=%s lastKnownVersion=%d\n", wfID, targetAlias, existingStatus, lastKnownVersion)
				printComposeSummary(cmd.ErrOrStderr(), workflow)

				// 1) Obtain the draft to autosave onto.
				//    - ACTIVE base: create-draft --force-recreate (clean draft
				//      regardless of whether one already exists). Response is
				//      wrapped: {created, message, workflow: {id, ...}}.
				//    - DRAFT base: the workflow IS already a draft (a prior
				//      un-published apply). Adopt it directly -- create-draft
				//      requires an ACTIVE base and would fail here -- so we lock
				//      and autosave onto this id, exactly as the Hub editor does
				//      when you re-open an unpublished draft.
				var draftID string
				draftVersion := lastKnownVersion
				if adoptDraft {
					draftID = wfID
					fmt.Fprintf(cmd.ErrOrStderr(), "# adopting existing draft %s (version %d)\n", draftID, draftVersion)
				} else {
					draftBody, _ := json.Marshal(map[string]any{"forceRecreate": true})
					draftResp, _, err := c.Do("POST", "borrower_central", "/v2/workflows/"+wfID+"/create-draft", json.RawMessage(draftBody))
					if err != nil {
						return fmt.Errorf("create draft for %s: %w", wfID, err)
					}
					var draftWrap map[string]any
					if err := json.Unmarshal(draftResp, &draftWrap); err != nil {
						return fmt.Errorf("parse create-draft response: %w", err)
					}
					draft, _ := draftWrap["workflow"].(map[string]any)
					if draft == nil {
						// Fallback for an unwrapped shape.
						draft = draftWrap
					}
					draftID, _ = draft["id"].(string)
					if draftID == "" {
						return fmt.Errorf("create-draft for %s returned no workflow.id", wfID)
					}
					if v, ok := draft["version"].(float64); ok {
						draftVersion = int(v)
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "# created draft %s (version %d)\n", draftID, draftVersion)
				}

				// 2) lock acquire (alias-keyed endpoint). acquireWfv2Lock wraps
				// the POST + parse + token extraction.
				clientID := fmt.Sprintf("apply-%d", time.Now().UnixNano())
				lockToken, err := acquireWfv2Lock(c, targetAlias, clientID)
				if err != nil && strings.Contains(err.Error(), "SELF_LOCK_CONFLICT") {
					// A stale lock left by THIS principal -- e.g. a prior apply
					// that died before releasing. Each apply uses a fresh
					// clientId, so it can't reuse its own lock; force-release and
					// retry once. We only do this for a self-conflict: a lock held
					// by a different user surfaces as a non-self conflict and is
					// left untouched so apply never stomps a live Hub editor.
					fmt.Fprintf(cmd.ErrOrStderr(), "# stale self-lock on %s; force-releasing and retrying\n", targetAlias)
					_, _, _ = c.Do("DELETE", "borrower_central", "/v2/workflows/"+targetAlias+"/lock/force", nil)
					lockToken, err = acquireWfv2Lock(c, targetAlias, clientID)
				}
				if err != nil {
					return fmt.Errorf("acquire lock on %s: %w", targetAlias, err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "# acquired lock client-id=%s\n", clientID)
				// Release the lock when apply returns (after autosave + any
				// publish). Each apply uses a fresh clientId, so a lock left
				// dangling from a prior apply makes the NEXT apply fail with a
				// SELF_LOCK_CONFLICT until the 300s TTL expires -- especially on
				// the DRAFT-adopt-without-publish path, which otherwise never
				// clears it. Best-effort: publish may already release it
				// server-side, in which case this DELETE is a harmless no-op.
				defer releaseWfv2Lock(c, targetAlias, lockToken)

				// 3) autosave the assembled body onto the draft
				autosavePayload := map[string]any{
					"label":           workflow["label"],
					"category":        workflow["category"],
					"status":          "DRAFT", // remain DRAFT until publish
					"inputVariables":  workflow["inputVariables"],
					"customVariables": workflow["customVariables"],
					"nodes":           workflow["nodes"],
					"edges":           workflow["edges"],
					"lockToken":       lockToken,
				}
				if draftVersion > 0 {
					autosavePayload["lastKnownVersion"] = draftVersion
				}
				if v, ok := workflow["alias"]; ok {
					autosavePayload["alias"] = v
				}
				if v, ok := workflow["description"]; ok {
					autosavePayload["description"] = v
				}
				if v, ok := workflow["config"]; ok {
					autosavePayload["config"] = v
				}
				if v, ok := workflow["notes"]; ok {
					autosavePayload["notes"] = v
				}
				autosaveBytes, _ := json.Marshal(autosavePayload)
				autosaveRaw := json.RawMessage(autosaveBytes)
				if err := validateWorkflowV2Body(&autosaveRaw); err != nil {
					return fmt.Errorf("autosave body validation failed: %w", err)
				}
				autoResp, _, err := c.Do("PUT", "borrower_central", "/v2/workflows/"+draftID+"/autosave", autosaveRaw)
				if err != nil {
					return fmt.Errorf("autosave draft %s: %w", draftID, err)
				}
				resultJSON = autoResp
				fmt.Fprintf(cmd.ErrOrStderr(), "# autosaved draft %s\n", draftID)

				// 4) publish decision.
				//    - ACTIVE base: always publish. The workflow is already live,
				//      so the spec-as-desired-state contract means republish the
				//      updated graph (unchanged behavior). Pre-publish lint gates.
				//    - DRAFT base (adopt): publish only when --publish is set,
				//      otherwise leave it a draft. The workflow was never live; a
				//      prior apply deliberately (or incidentally) left it in
				//      DRAFT, so re-applying without --publish keeps it editable
				//      rather than surprising the author by going live. This
				//      mirrors the CREATE path (DRAFT unless --publish).
				if !adoptDraft || publish {
					if err := lintAndPublish(c, cmd, draftID, skipLintOnPublish); err != nil {
						return err
					}
				} else {
					fmt.Fprintf(cmd.OutOrStderr(), "# updated DRAFT workflow %s (alias=%s); not published (pass --publish to go live)\n", draftID, targetAlias)
				}
			}

			// === Entity-scope reconciliation ===
			// After either path, walk the spec's referenced credit-decisioning
			// entities and stamp them to targetAlias. Without this, an update
			// from workflow A to workflow A-v2 (or a fresh create that pulls
			// a scorecard scoped to a sibling) leaves nested entities pointing
			// at the old alias and the Hub's elements panel goes empty.
			if !skipRescope {
				if err := reconcileEntityScopes(c, &spec, targetAlias, allowStealOwnership, cmd.ErrOrStderr()); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "# warning: entity-scope reconciliation hit an issue: %v (workflow itself is fine; re-scope failed entities manually with `altscore <resource> update <id> --workflow-alias %s`)\n", err, targetAlias)
				}
			}

			// Read-back verification: confirm the backend actually persisted
			// every field the spec set on each task. Catches silent
			// server-side field drops (e.g. a `contacts` array the task schema
			// doesn't model). Best-effort and non-fatal -- the workflow is
			// already applied; this only warns. Skipped on dry-run/diff (both
			// returned above). Disable with --verify=false.
			if verify {
				verifyAppliedTasks(c, &spec, workflow, cmd.ErrOrStderr())
			}

			if wfID != "" && publish {
				fmt.Fprintf(cmd.OutOrStderr(), "# applied workflow %s (alias=%s)\n", wfID, targetAlias)
			}
			return output.RawJSON(resultJSON)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON spec (or pipe via stdin)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the assembled workflow body without making API calls")
	cmd.Flags().BoolVar(&diffFlag, "diff", false, "preview structural changes against the current tenant state. Fetches the existing workflow (if any), assembles the spec body in memory, and prints a human-readable diff. No API mutations. Mutually exclusive with --dry-run and --publish")
	cmd.Flags().BoolVar(&publish, "publish", false, "publish the workflow after creation (CREATE path only; UPDATE path always publishes the draft it produced)")
	cmd.Flags().BoolVar(&skipLintOnPublish, "skip-lint-on-publish", false, "skip the pre-publish topology lint that refuses to publish on errors")
	cmd.Flags().BoolVar(&skipRescope, "skip-rescope", false, "do not stamp referenced credit-decisioning entities (scorecards, rule-trees, etc.) to the workflow's alias after apply")
	cmd.Flags().BoolVar(&allowStealOwnership, "allow-steal-ownership", false, "permit apply to transfer a credit-decisioning entity's workflowAlias when it is currently owned by ANOTHER workflow. Default: refuse and instruct the spec author to clone the entity with a new code. Use only for rare workflow rename / identity migration / decommissioning scenarios")
	cmd.Flags().BoolVar(&verify, "verify", true, "after writing, read back the persisted tasks and warn (stderr, non-fatal) about any spec-set field the backend dropped or nulled. Pass --verify=false to skip the extra GETs")
	cmd.Flags().BoolVar(&noAutoDefaults, "no-auto-defaults", false, "disable apply's opinionated convenience defaults: (1) end-node borrower_id/billable_id wired to the single customer node's borrower_id, (2) forced end-node PDF generation (pdfConfig.enabled+pdfGenerationRequired=true), (3) deal-contact identity_value back-filled from each contact's identity_key field (default tax_id). Each only fills an absent field; caller-supplied values always win")
	return cmd
}

// lintAndPublish runs the pre-publish topology lint on a workflow and then
// publishes it. Shared by both the create path (creates workflow + optional
// publish) and the update path (always publishes after autosave). Mirrors
// what compose used to do inline for create + publish.
func lintAndPublish(c *client.Client, cmd *cobra.Command, wfID string, skipLintOnPublish bool) error {
	lintData, _, lerr := c.Do("GET", "borrower_central", "/v2/workflows/"+wfID, nil)
	if lerr == nil {
		var wfFull map[string]any
		if err := json.Unmarshal(lintData, &wfFull); err == nil {
			report := lintWorkflowV2(wfFull)
			errs := []string{}
			for _, issue := range report.Issues {
				if issue.Severity == "error" {
					errs = append(errs, "  - "+issue.Message)
				}
			}
			if len(errs) > 0 {
				if skipLintOnPublish {
					fmt.Fprintf(cmd.OutOrStderr(),
						"# WARNING: pre-publish lint found %d topology error(s) but --skip-lint-on-publish was set; publishing anyway:\n%s\n",
						len(errs), strings.Join(errs, "\n"))
				} else {
					return fmt.Errorf(
						"workflow %s created but pre-publish lint found %d topology error(s); refusing to publish:\n%s\n"+
							"Fix the spec, run 'altscore workflows-v2 publish %s' manually after editing, or pass --skip-lint-on-publish.",
						wfID, len(errs), strings.Join(errs, "\n"), wfID,
					)
				}
			}
		}
	}
	if _, _, err := c.Do("POST", "borrower_central", "/v2/workflows/"+wfID+"/publish", nil); err != nil {
		return fmt.Errorf("workflow %s created but publish failed: %w", wfID, err)
	}
	fmt.Fprintf(cmd.OutOrStderr(), "# published workflow %s\n", wfID)
	return nil
}

// findWorkflowByAlias resolves the workflow apply should reconcile against:
// the ACTIVE version, else the latest DRAFT. Returns the workflow, its status
// ("ACTIVE"|"DRAFT"), or (nil, "", nil) when nothing matches. The DRAFT
// fallback keeps apply an idempotent upsert: a prior apply WITHOUT --publish
// leaves a DRAFT that an ACTIVE-only lookup can't see, so the next apply would
// otherwise fork a duplicate instead of adopting it.
func findWorkflowByAlias(c *client.Client, alias string) (map[string]any, string, error) {
	active, err := queryLatestWorkflowByAliasStatus(c, alias, "ACTIVE")
	if err != nil {
		return nil, "", err
	}
	if active != nil {
		return active, "ACTIVE", nil
	}
	draft, err := queryLatestWorkflowByAliasStatus(c, alias, "DRAFT")
	if err != nil {
		return nil, "", err
	}
	if draft != nil {
		return draft, "DRAFT", nil
	}
	return nil, "", nil
}

// queryLatestWorkflowByAliasStatus returns the highest-version workflow matching
// alias+status, or nil when none match. BC handlers have historically ignored
// the query filters, so we also filter client-side after parsing.
func queryLatestWorkflowByAliasStatus(c *client.Client, alias, status string) (map[string]any, error) {
	if alias == "" {
		return nil, nil
	}
	q := url.Values{}
	q.Set("alias", alias)
	q.Set("status", status)
	q.Set("is-latest", "true")
	q.Set("per-page", "10")
	path := "/v2/workflows?" + q.Encode()
	data, _, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, err
	}
	// BC list endpoints return a JSON array at top level.
	var arr []map[string]any
	if jerr := json.Unmarshal(data, &arr); jerr != nil {
		// Some endpoints wrap in {items: [...], total: ...}; tolerate both.
		var wrapped struct {
			Items []map[string]any `json:"items"`
		}
		if werr := json.Unmarshal(data, &wrapped); werr != nil {
			return nil, fmt.Errorf("parse list response: %w", jerr)
		}
		arr = wrapped.Items
	}
	matches := []map[string]any{}
	for _, w := range arr {
		wa, _ := w["alias"].(string)
		if wa == "" {
			wa, _ = w["workflowAlias"].(string)
		}
		st, _ := w["status"].(string)
		if wa == alias && st == status {
			matches = append(matches, w)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > 1 {
		// Pick the highest version.
		best := matches[0]
		bestV := 0
		if v, ok := best["version"].(float64); ok {
			bestV = int(v)
		}
		for _, m := range matches[1:] {
			if v, ok := m["version"].(float64); ok && int(v) > bestV {
				best = m
				bestV = int(v)
			}
		}
		return best, nil
	}
	return matches[0], nil
}

// reconcileEntityScopes walks every credit-decisioning entity reachable from
// the spec's tasks and stamps its `workflowAlias` to targetAlias when the
// entity is currently UNSCOPED (workflowAlias is empty) or ALREADY matches
// the target. When the entity is CROSS-OWNED -- its workflowAlias points at
// a different workflow -- the reconciler refuses to re-stamp unless the
// caller passed --allow-steal-ownership. Each v2 workflow owns its credit-
// decisioning entities 1:1; silently transferring ownership breaks the
// previous owner's Hub element panel (entities only appear in the panel
// when their workflowAlias matches the workflow's alias).
//
// The preflight check in validateEntityWorkflowAliasMatch catches cross-
// ownership before any mutation happens, so this second guard exists only
// for the narrow window where the entity's owner changes between preflight
// and reconcile (concurrent apply on the same tenant, manual update, etc.).
//
// Walks:
//   - scorecard task -> scorecard entity -> nested rules[*].mappingTableCode
//   - rule-tree task -> rule-tree entity -> nested rules[*].ruleCode
//   - evaluate-rules task -> rulesConfig[*].ruleCode
//   - mapping-table task -> mappingTableConfig.entries[*].mappingTableCode
//
// All stamps are PATCH /v1/{resource}/{id} with {"workflowAlias": "<alias>"}.
// Errors are surfaced per-entity to stderr (one log line each) and the rest
// of the walk continues -- partial reconciliation is better than nothing.
func reconcileEntityScopes(c *client.Client, spec *composeSpec, targetAlias string, allowStealOwnership bool, errOut io.Writer) error {
	if c == nil || targetAlias == "" {
		return nil
	}
	// Local memo to avoid re-stamping the same entity twice when multiple
	// tasks reference it (e.g. two scorecard tasks sharing a mapping table).
	stamped := map[string]bool{}

	stamp := func(resource, ref string) {
		if ref == "" {
			return
		}
		entity, _ := lookupEntity(c, resource, ref, false)
		if entity == nil {
			// Best-effort: missing entity already warned by normalize.
			return
		}
		id, _ := entity["id"].(string)
		if id == "" {
			return
		}
		key := resource + "|" + id
		if stamped[key] {
			return
		}
		stamped[key] = true
		actual, _ := entity["workflowAlias"].(string)
		if actual == targetAlias {
			return
		}
		// Cross-owned guard: refuse to steal ownership from another
		// workflow unless the user explicitly opted in. Preflight has
		// already raised this as a hard error in the normal path; this
		// branch only fires if the entity's owner changed between
		// preflight and reconcile.
		if actual != "" && !allowStealOwnership {
			fmt.Fprintf(errOut,
				"# REFUSED to re-scope %s %q (id=%s): currently owned by workflow %q, "+
					"target was %q. Clone the entity with a new code dedicated to %q, or "+
					"re-run apply with --allow-steal-ownership to transfer ownership.\n",
				resource, ref, id, actual, targetAlias, targetAlias)
			return
		}
		patch, _ := json.Marshal(map[string]any{"workflowAlias": targetAlias})
		_, _, err := c.Do("PATCH", "borrower_central", "/v1/"+resource+"/"+id, json.RawMessage(patch))
		if err != nil {
			fmt.Fprintf(errOut, "# warning: could not re-scope %s %s (%s -> %s): %v\n", resource, ref, actual, targetAlias, err)
			return
		}
		fmt.Fprintf(errOut, "# scoped %s %s to %s\n", resource, ref, targetAlias)
		// Refresh memoized entity in lookupEntity's cache so a subsequent
		// validator (or a follow-on apply run) sees the new scope. lookupEntity
		// caches the entity map itself; mutate in place.
		entity["workflowAlias"] = targetAlias
	}

	// Walk tasks in the spec (also covers ExtraNodes-end if the end task
	// somehow ends up holding a credit-decisioning reference, which the
	// schema doesn't allow today but the walk is cheap).
	walkTasks := func(tasks []map[string]any) {
		for _, t := range tasks {
			tt, _ := t["type"].(string)
			switch tt {
			case "scorecard":
				cfg, _ := t["scorecardConfig"].(map[string]any)
				if cfg == nil {
					continue
				}
				code, _ := cfg["scorecardCode"].(string)
				if code == "" {
					code, _ = cfg["scorecardId"].(string)
				}
				stamp("scorecards", code)
				// Nested mapping tables on every rule.
				entity, _ := lookupEntity(c, "scorecards", code, false)
				if entity != nil {
					if rules, ok := entity["rules"].([]any); ok {
						for _, rraw := range rules {
							rm, ok := rraw.(map[string]any)
							if !ok {
								continue
							}
							mt, _ := rm["mappingTableCode"].(string)
							if mt == "" {
								mt, _ = rm["mappingTableId"].(string)
							}
							stamp("mapping-tables", mt)
						}
					}
				}
			case "rule-tree":
				cfg, _ := t["ruleTreeConfig"].(map[string]any)
				if cfg == nil {
					continue
				}
				code, _ := cfg["ruleTreeCode"].(string)
				if code == "" {
					code, _ = cfg["ruleTreeId"].(string)
				}
				stamp("rule-trees", code)
				entity, _ := lookupEntity(c, "rule-trees", code, false)
				if entity != nil {
					if rules, ok := entity["rules"].([]any); ok {
						for _, rraw := range rules {
							rm, ok := rraw.(map[string]any)
							if !ok {
								continue
							}
							rc, _ := rm["ruleCode"].(string)
							if rc == "" {
								rc, _ = rm["ruleId"].(string)
							}
							stamp("evaluation-rules", rc)
						}
					}
				}
			case "evaluate-rules":
				rules, _ := t["rulesConfig"].([]any)
				for _, rraw := range rules {
					rm, ok := rraw.(map[string]any)
					if !ok {
						continue
					}
					rc, _ := rm["ruleCode"].(string)
					if rc == "" {
						rc, _ = rm["ruleId"].(string)
					}
					stamp("evaluation-rules", rc)
				}
			case "mapping-table":
				cfg, _ := t["mappingTableConfig"].(map[string]any)
				if cfg == nil {
					continue
				}
				entries, _ := cfg["entries"].([]any)
				for _, eraw := range entries {
					em, ok := eraw.(map[string]any)
					if !ok {
						continue
					}
					mt, _ := em["mappingTableCode"].(string)
					if mt == "" {
						mt, _ = em["mappingTableId"].(string)
					}
					stamp("mapping-tables", mt)
				}
			}
		}
	}
	walkTasks(spec.Tasks)
	walkTasks(spec.ExtraNodes)
	return nil
}

// localRef returns the spec-local reference for a task or extraNode entry,
// in priority order: explicit `ref`, then `alias` (for tasks) or `nodeId`
// (for nodes), falling back to the supplied default.
func localRef(entry map[string]any, fallback string) string {
	if v, _ := entry["ref"].(string); v != "" {
		return v
	}
	if v, _ := entry["alias"].(string); v != "" {
		return v
	}
	if v, _ := entry["nodeId"].(string); v != "" {
		return v
	}
	return fallback
}

// slugifyWorkflowLabel mirrors borrower-central's
// app/utils/workflow_alias.py:slugify_workflow_label so compose can predict
// the alias the server will assign before any task POST happens. Knowing the
// alias up-front matters because credit-decisioning entities
// (evaluation-rules, rule-trees, mapping-tables, scorecards) only show up in
// a workflow's builder pickers when their workflowAlias matches the
// workflow's alias -- and the alias is server-derived from the label, not
// settable from the body. When a label like "All 5 types" silently slugs to
// "all-5-types", entities stamped with "all-types" become invisible.
//
// Rules (must match BC byte-for-byte):
//   - lowercase + strip
//   - replace any run of non-[a-z0-9] with "-"
//   - collapse repeated "-"
//   - trim leading/trailing "-"
//   - cap at 100 chars
//   - default to "workflow" when empty
func slugifyWorkflowLabel(label string) string {
	s := strings.ToLower(strings.TrimSpace(label))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 100 {
		out = out[:100]
	}
	if out == "" {
		out = "workflow"
	}
	return out
}

// edgeEndpoints reads the source/target ref or nodeId of an edge entry,
// preferring the spec-side `from`/`to` shortcuts.
func edgeEndpoints(e map[string]any) (from, to string) {
	from, _ = e["from"].(string)
	if from == "" {
		from, _ = e["sourceNodeId"].(string)
	}
	to, _ = e["to"].(string)
	if to == "" {
		to, _ = e["targetNodeId"].(string)
	}
	return from, to
}

// (buildEndAutoWiring + endNodeBilliableIDSchema + pdfTitleByType deleted.)
// The compose-time PDF source pre-fill was migrated to runtime in
// borrower-central's end_activity: when endConfig.pdfConfig.enabled=true
// but sourcesConfig is empty, the runtime walks the workflow graph
// upstream from the end node and auto-resolves sections from recognised
// ancestor types (altdata-enrichment, scorecard, mapping-table,
// rule-tree, evaluate-rules). Agents can flip pdfConfig.enabled=true
// without needing to understand the ancestor graph.
//
// inputSchema / inputMappings auto-wiring for the Hub canvas continues
// to be derived client-side by
// altscore-ai-chat/lib/stores/workflow-builder-v2/actions/edge/pdf-data-source-auto-mapping.ts
// when the workflow loads.

// readSpecHTMLSections extracts the spec-only `htmlSections` field from an
// extraNode. Returns nil when the field is absent or shaped wrong (compose
// silently ignores malformed entries; the spec validator would have caught
// truly broken JSON before we got here).
func readSpecHTMLSections(node map[string]any) []map[string]any {
	raw, ok := node["htmlSections"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// htmlSectionVarRegex matches {<word>} placeholders in HTML content -- same
// shape that runtime safe_format() resolves. Compose uses it to discover
// which workflow input/custom variables a section references so it can wire
// them into the end task's inputMappings (otherwise safe_format leaves
// {var} as a literal because the runtime context never resolves it).
var htmlSectionVarRegex = regexp.MustCompile(`\{(\w+)\}`)

// buildHTMLSections turns spec htmlSections into pdfConfig.sourcesConfig
// entries with components shaped as {name:"htmlBlock", content, context}.
// Returns:
//   - sections: ready-to-append entries for endConfig.pdfConfig.sourcesConfig
//   - inputSchema additions: a slot per input/custom variable referenced in
//     any section's content
//   - inputMappings additions: matching wiring (inputs.<name> or
//     custom.<name>) so safe_format resolves at runtime
//
// Each section accepts:
//   - title: PDF heading (default "")
//   - subtitle: PDF subheading (default "")
//   - content: the raw HTML string with {var} interpolation tokens
//   - context: visual treatment "none" | "info" | "warning" | "success" |
//     "danger" (default "none")
//   - pageBreak: whether to start a new PDF page (default true)
func buildHTMLSections(sections []map[string]any, inputVars, customVars map[string]any) (
	built []map[string]any,
	schema map[string]any,
	mappings map[string]any,
) {
	built = make([]map[string]any, 0, len(sections))
	schema = map[string]any{}
	mappings = map[string]any{}

	for _, s := range sections {
		content, _ := s["content"].(string)
		title, _ := s["title"].(string)
		subtitle, _ := s["subtitle"].(string)
		ctx, _ := s["context"].(string)
		if ctx == "" {
			ctx = "none"
		}
		pageBreak := true
		if v, has := s["pageBreak"].(bool); has {
			pageBreak = v
		}

		built = append(built, map[string]any{
			"id":         newUUIDv4(),
			"type":       "htmlBlock",
			"title":      title,
			"subtitle":   subtitle,
			"enabled":    true,
			"page_break": pageBreak,
			"components": []map[string]any{{
				"id":      newUUIDv4(),
				"name":    "htmlBlock",
				"content": content,
				"context": ctx,
			}},
		})

		// Auto-wire workflow-scope variables referenced in the content.
		// Task-output references resolve via end_activity's enriched_context
		// promotion (no wiring needed); only inputs/custom vars need to be
		// pulled into the end task's resolved-context root.
		for _, m := range htmlSectionVarRegex.FindAllStringSubmatch(content, -1) {
			name := m[1]
			if _, has := mappings[name]; has {
				continue
			}
			if _, isInput := inputVars[name]; isInput {
				mappings[name] = "inputs." + name
				schema[name] = inferSchemaForVar(inputVars[name], name)
				continue
			}
			if _, isCustom := customVars[name]; isCustom {
				mappings[name] = "custom." + name
				schema[name] = inferSchemaForVar(customVars[name], name)
				continue
			}
			// Else: assume the name is a task output (alerts, credit_score,
			// decision, ...). end_activity promotes those to root, so no
			// inputMappings entry is needed -- safe_format will find them.
		}
	}
	return built, schema, mappings
}

// inferSchemaForVar derives an inputSchema entry for a workflow-scope
// variable, falling back to type:"string" when the spec doesn't define one
// (or when the spec value isn't shaped as a JSON-Schema-style object).
func inferSchemaForVar(specValue any, name string) map[string]any {
	out := map[string]any{
		"type":  "string",
		"title": humanizeKey(name),
	}
	v, ok := specValue.(map[string]any)
	if !ok {
		return out
	}
	if t, _ := v["type"].(string); t != "" {
		out["type"] = t
	}
	if title, _ := v["title"].(string); title != "" {
		out["title"] = title
	} else if label, _ := v["label"].(string); label != "" {
		out["title"] = label
	}
	return out
}

// printComposeSummary writes a one-line-per-task summary to stderr after
// compose finishes its real (non-dry-run) POSTs. Without this, the only thing
// compose returns to stdout is `{"id": "..."}` of the workflow -- the user
// then has to GET the workflow + each task to discover server-assigned
// aliases. Format mirrors `workflows-v2 lint`'s short report style.
func printComposeSummary(w io.Writer, workflow map[string]any) {
	if w == nil {
		return
	}
	rawNodes, ok := workflow["nodes"].([]map[string]any)
	if !ok {
		// Try the slice-of-any form some assemblies produce.
		alt, _ := workflow["nodes"].([]any)
		for _, item := range alt {
			if m, ok := item.(map[string]any); ok {
				rawNodes = append(rawNodes, m)
			}
		}
	}
	if len(rawNodes) == 0 {
		return
	}
	fmt.Fprintf(w, "# created %d task(s) backing the workflow nodes:\n", len(rawNodes))
	for _, n := range rawNodes {
		typ, _ := n["type"].(string)
		alias, _ := n["taskAlias"].(string)
		label, _ := n["label"].(string)
		if alias == "" {
			continue
		}
		fmt.Fprintf(w, "#   %-20s  alias=%-40s  label=%q\n", typ, alias, label)
	}
}

// postTask creates a task via POST /v2/tasks and returns the server-assigned
// alias and version. Used by the tasks loop and the extraNodes loop. The
// previous two-phase create (postTaskWithMultiDotFallback) is gone:
// CreateTaskV2's strict-vs-lenient distinction was relaxed in the backend --
// the input_mappings validator now just returns its argument unchanged
// (see borrower-central/app/model/workflows_v2/task_schemas.py). Multi-dot
// inputMappings now land at version 1 in a single POST.
//
// In dryRun mode the caller-supplied `ref` is used as the alias placeholder
// (so downstream extraNode positioning matches what real-run produces) and
// the version is always 1.
func postTask(c *client.Client, body map[string]any, ref string, dryRun bool, label string) (alias string, version int, err error) {
	bytes, err := json.Marshal(body)
	if err != nil {
		return "", 0, fmt.Errorf("encode %s: %w", label, err)
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "# Would POST /v2/tasks (%s): %s\n", label, string(bytes))
		if a, _ := body["alias"].(string); a != "" {
			alias = a
		} else {
			alias = ref
		}
		return alias, 1, nil
	}

	data, _, err := c.Do("POST", "borrower_central", "/v2/tasks", json.RawMessage(bytes))
	if err != nil {
		return "", 0, err
	}
	var created map[string]any
	if err := json.Unmarshal(data, &created); err != nil {
		return "", 0, fmt.Errorf("parse %s response: %w", label, err)
	}
	if a, _ := created["alias"].(string); a != "" {
		alias = a
	} else {
		return "", 0, fmt.Errorf("%s: server returned no alias", label)
	}
	if v, ok := created["version"].(float64); ok {
		version = int(v)
	} else {
		version = 1
	}
	return alias, version, nil
}

// reservedMappingScopes are the leading segments in a mapping value that are
// NOT spec-local refs and must not be rewritten.
var reservedMappingScopes = map[string]bool{
	"inputs":               true,
	"custom":               true,
	"system":               true,
	"task_outputs":         true,
	"task_outputs_by_type": true,
	// entity.<root>.<group>.<key>[.<subkey>] is a backend pass-through scope
	// (e.g. entity.borrower.identities.tax_id, entity.deal.deal_fields.x.amount,
	// entity.<alias>:<handle>.identities.cedula for rel/deal-contact branch
	// roots). The CLI has no data-model catalog, so like the other reserved
	// scopes it only recognises the leading segment and never validates deeper.
	"entity": true,
}

// mappingDependencyRef returns the spec-local task ref that an inputMappings
// VALUE depends on, or "" when the value is a literal / refers to a reserved
// namespace / doesn't have a path-like shape. Used by both the topological
// sort (to know which task each mapping depends on) and rewriteRefsInMappings
// (to know whether the head needs rewriting).
//
// Recognised forms:
//
//	"task_outputs.<ref>.<deep>" -> "<ref>"
//	"<ref>.<rest>"              -> "<ref>" (when head is not a reserved scope)
//	"inputs.<name>"             -> ""     (reserved scope, no task dep)
//	"custom.<name>"             -> ""     (reserved scope)
//	"<literal>" (no dot)        -> ""
func mappingDependencyRef(s string) string {
	dot := strings.Index(s, ".")
	if dot <= 0 {
		return ""
	}
	if strings.HasPrefix(s, "task_outputs.") {
		rest := s[len("task_outputs."):]
		innerDot := strings.Index(rest, ".")
		if innerDot <= 0 {
			return ""
		}
		return rest[:innerDot]
	}
	head := s[:dot]
	if reservedMappingScopes[head] {
		return ""
	}
	return head
}

// templateDependencyRefs returns the spec-local refs a task body's template
// strings reference, for use by topologicalTaskOrder. Mirrors
// mappingDependencyRef but scans {{...}} placeholders across known template
// fields per task type. Empty when the task type has no template fields or
// all references are reserved scopes / unknown heads.
func templateDependencyRefs(task map[string]any) []string {
	taskType, _ := task["type"].(string)
	var fields []string
	switch taskType {
	case "http", "webhook":
		for _, f := range []string{"url", "body", "headers"} {
			if s, ok := task[f].(string); ok && s != "" {
				fields = append(fields, s)
			}
		}
	case "end":
		if endCfg, ok := task["endConfig"].(map[string]any); ok {
			if s, ok := endCfg["outputJson"].(string); ok && s != "" {
				fields = append(fields, s)
			}
		}
	case "exception":
		if s, ok := task["errorMessage"].(string); ok && s != "" {
			fields = append(fields, s)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, s := range fields {
		for _, m := range templatePlaceholderRegex.FindAllStringSubmatch(s, -1) {
			inner := strings.TrimSpace(m[1])
			ref := mappingDependencyRef(inner)
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}

// rewriteRefsInMappings replaces leading spec-local refs in mapping values
// with the server-assigned alias from refMap. Spec refs are the
// human-friendly names the spec assigns to each task (`ref` / `alias` /
// `nodeId` fallback); the server picks a slug-NNNNNN alias on create that we
// must substitute in so downstream tasks reference the correct stored alias.
//
// Two shapes are recognised:
//   - long  "task_outputs.<taskRef>.<field>" -> "task_outputs.<server-alias>.<field>"
//   - bare  "<taskRef>.<field>"              -> "<server-alias>.<field>"
//
// Both forms are accepted by the BC runtime resolver -- bare form is the
// implicit task_outputs.<alias> shortcut (PR #1269 / borrower-central feature
// branch feat/workflows-v2-resolver-bare-alias-refs). We no longer rewrite
// bare form to the long form; the server-side resolver expands it.
//
// Reserved scopes (inputs, custom, system, task_outputs, task_outputs_by_type,
// entity) are never treated as refs.
//
// Errors when a mapping value has a path-like shape whose head is neither a
// reserved scope nor a known ref. composeWorkflowBody sorts tasks
// topologically before this runs, so a remaining unknown ref is always either
// a typo or a reference to a task that simply isn't in spec.tasks.
func rewriteRefsInMappings(mappings map[string]any, refMap map[string]string) (map[string]any, error) {
	if len(mappings) == 0 {
		return mappings, nil
	}
	out := map[string]any{}
	for k, v := range mappings {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		// Long form: task_outputs.<ref>.<field>
		if strings.HasPrefix(s, "task_outputs.") {
			rest := s[len("task_outputs."):]
			if dot := strings.Index(rest, "."); dot > 0 {
				ref := rest[:dot]
				if alias, found := refMap[ref]; found {
					s = "task_outputs." + alias + rest[dot:]
				} else if !isServerAlias(ref) {
					return nil, fmt.Errorf(
						"inputMappings[%q]=%q references task_outputs.%s.* but %q is not a known spec ref. "+
							"Known refs: %s. (Reserved scopes: inputs, custom, system, task_outputs, task_outputs_by_type, entity.)",
						k, v, ref, ref, sortedRefMapKeys(refMap))
				}
			}
		} else if dot := strings.Index(s, "."); dot > 0 {
			// Bare <ref>.<rest> -- substitute the head with the server-assigned
			// alias and leave the rest in bare form. BC's resolver treats bare
			// alias.<field> as implicit task_outputs.<alias>.<field>.
			head := s[:dot]
			if !reservedMappingScopes[head] {
				if alias, found := refMap[head]; found {
					s = alias + s[dot:]
				} else if isServerAlias(head) {
					// User supplied a server-style alias directly -- leave it.
					// BC's resolver matches it against task_outputs at runtime.
				} else {
					return nil, fmt.Errorf(
						"inputMappings[%q]=%q has head %q which is neither a reserved namespace "+
							"nor a known spec ref nor a server alias (slug-NNNNNN). "+
							"Known refs: %s. (Reserved scopes: inputs, custom, system, task_outputs, task_outputs_by_type, entity.)",
						k, v, head, sortedRefMapKeys(refMap))
				}
			}
		}
		out[k] = s
	}
	return out, nil
}

// templatePlaceholderRegex matches `{{...}}` substitutions used by the BC
// runtime template engine in http body/headers/url and end outputJson.
// Whitespace around the inner expression is tolerated. The captured group
// is the inner expression (e.g. "task_outputs.fetch.tax_id" or "borrower_id").
var templatePlaceholderRegex = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

// rewriteTaskOutputsRefsInString rewrites `task_outputs.<spec-ref>.` substrings
// in arbitrary strings. Used by composeWorkflowBody to rewrite customVariable
// expressions / returnValues / dependencies arrays after task aliases have
// been assigned. Unknown refs (not in refMap) are left alone so server-style
// aliases and other scopes (inputs, custom, system) pass through unchanged.
//
// The trailing dot in the match prefix is a boundary marker: ref "a" won't
// accidentally match "task_outputs.a-extended." because the char after "a"
// is "-", not ".". Maps are iterated in non-deterministic order; the trailing
// dot also prevents one ref's rewrite from feeding into another (the alias
// "a-server" doesn't contain "task_outputs.a." after rewriting).
func rewriteTaskOutputsRefsInString(s string, refMap map[string]string) string {
	if s == "" || refMap == nil {
		return s
	}
	for ref, alias := range refMap {
		if ref == alias {
			continue
		}
		s = strings.ReplaceAll(s, "task_outputs."+ref+".", "task_outputs."+alias+".")
	}
	return s
}

// rewriteRefsInTemplate rewrites every {{...}} placeholder in s whose inner
// expression looks like a spec-local ref reference. The rewrite mirrors
// rewriteRefsInMappings:
//   - {{task_outputs.<ref>.<deep>}} -> {{task_outputs.<server-alias>.<deep>}}
//   - {{<ref>.<rest>}} (bare)       -> {{<server-alias>.<rest>}} (head substitution only)
//   - {{<reserved-scope>...}}        -> unchanged (inputs/custom/system/...)
//   - {{<server-alias>...}}          -> unchanged (BC resolver accepts bare form)
//
// Returns an error if a placeholder's head is neither a reserved scope nor a
// known spec ref nor a server alias -- a typo or stale reference would
// otherwise silently fail at runtime when the template engine resolves
// against an empty context.
//
// This is the template-string analog of rewriteRefsInMappings. Both share
// mappingDependencyRef + topologicalTaskOrder for ordering, so a task whose
// http url uses {{<downstream-task>.<field>}} gets ordered correctly, the
// rewrite runs after refMap has the downstream alias, and the persisted
// task body has the (now bare-or-canonical) form.
//
// Bare-form output is supported by BC's variable resolver (the resolver
// expands `{{<alias>.<field>}}` to the implicit
// `{{task_outputs.<alias>.<field>}}` at execute time), so we no longer
// re-wrap bare heads with the task_outputs. prefix.
func rewriteRefsInTemplate(s string, refMap map[string]string, localMappings map[string]any) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	var firstErr error
	out := templatePlaceholderRegex.ReplaceAllStringFunc(s, func(match string) string {
		if firstErr != nil {
			return match
		}
		m := templatePlaceholderRegex.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		inner := strings.TrimSpace(m[1])
		// Only rewrite path-like inner expressions; leave plain literals alone.
		dot := strings.Index(inner, ".")
		if dot <= 0 {
			// Bare single-token placeholder, e.g. {{borrower_id}}. By itself
			// the runtime template engine has no scope to resolve it against,
			// so it renders literally and corrupts the JSON. If the task's
			// inputMappings maps this token to a resolvable expression (e.g.
			// "inputs.borrower_id" or "task_outputs.fetch.tax_id"), rewrite to
			// that long form. When the token isn't a known mapping, leave it
			// untouched -- we only rewrite when we can confidently resolve it.
			if resolved, ok := localMappings[inner].(string); ok && resolved != "" {
				return "{{" + resolved + "}}"
			}
			return match
		}
		// task_outputs.<ref>.<deep>
		if strings.HasPrefix(inner, "task_outputs.") {
			rest := inner[len("task_outputs."):]
			innerDot := strings.Index(rest, ".")
			if innerDot <= 0 {
				return match
			}
			ref := rest[:innerDot]
			if alias, found := refMap[ref]; found {
				return "{{task_outputs." + alias + rest[innerDot:] + "}}"
			}
			if !isServerAlias(ref) {
				firstErr = fmt.Errorf(
					"template references task_outputs.%s.* but %q is not a known spec ref. Known refs: %s. (Reserved scopes: inputs, custom, system, task_outputs, task_outputs_by_type, entity.)",
					ref, ref, sortedRefMapKeys(refMap))
				return match
			}
			return match
		}
		// Reserved scope at head -> leave as-is.
		head := inner[:dot]
		if reservedMappingScopes[head] {
			return match
		}
		// Bare <ref>.<rest> -> substitute head, keep bare form. The BC
		// runtime resolver expands `<alias>.<field>` to the implicit
		// `task_outputs.<alias>.<field>`.
		if alias, found := refMap[head]; found {
			return "{{" + alias + inner[dot:] + "}}"
		}
		if isServerAlias(head) {
			// Already a server alias in bare form -- BC handles it directly.
			return match
		}
		// Unknown head -- error so the user sees the typo before runtime
		// silently substitutes nothing.
		firstErr = fmt.Errorf(
			"template uses {{%s.<...>}} but %q is neither a reserved namespace nor a known spec ref nor a server alias. Known refs: %s. (Reserved scopes: inputs, custom, system, task_outputs, task_outputs_by_type, entity.)",
			inner, head, sortedRefMapKeys(refMap))
		return match
	})
	if firstErr != nil {
		return s, firstErr
	}
	return out, nil
}

// rewriteRefsInTaskTemplates applies the template rewrite to every string
// field on a task body that the runtime treats as a {{...}}-substituting
// template. Today that's:
//   - http: url, body, headers
//   - end: endConfig.outputJson
//
// The function mutates `task` in place and returns an error from the first
// failed rewrite, with the field name in the error context.
//
// New task types that introduce template fields should be added here AND in
// mappingDependencyRef -- otherwise topologicalTaskOrder won't see the
// inputMappings-style dep and a forward reference inside a template will
// fail at rewrite time instead of being ordered correctly.
func rewriteRefsInTaskTemplates(task map[string]any, refMap map[string]string) error {
	taskType, _ := task["type"].(string)
	// The task's inputMappings map short tokens (e.g. "borrower_id") to
	// resolvable expressions (e.g. "inputs.borrower_id"). Bare single-token
	// placeholders in a template are rewritten to their mapped long form when
	// a mapping exists. By the time this runs the inputMappings values are
	// already in resolved/server-alias form (rewriteRefsInMappings ran first).
	localMappings, _ := task["inputMappings"].(map[string]any)
	rewriteField := func(fieldPath string, value string) (string, error) {
		out, err := rewriteRefsInTemplate(value, refMap, localMappings)
		if err != nil {
			return "", fmt.Errorf("%s: %w", fieldPath, err)
		}
		return out, nil
	}
	switch taskType {
	case "http", "webhook":
		for _, field := range []string{"url", "body", "headers"} {
			if s, ok := task[field].(string); ok && s != "" {
				out, err := rewriteField(field, s)
				if err != nil {
					return err
				}
				task[field] = out
			}
		}
	case "end":
		endCfg, _ := task["endConfig"].(map[string]any)
		if endCfg != nil {
			if s, ok := endCfg["outputJson"].(string); ok && s != "" {
				out, err := rewriteField("endConfig.outputJson", s)
				if err != nil {
					return err
				}
				endCfg["outputJson"] = out
			}
			// pdfConfig.sourcesConfig[].taskAlias carries a spec-local ref to
			// the data-producing upstream task whose output gets rendered as
			// a PDF section (scorecard, rule-tree, altdata-enrichment, etc.).
			// The runtime end_activity looks the alias up directly against
			// the workflow's task list -- so a spec-local ref like "score"
			// that survives compose persists on the server, the renderer
			// can't find a task with that alias, and the section falls
			// through to an empty render or hits the auto-resolver fallback.
			// Symptom matches the la-fabril / kyc-pf-mx spike: apply needs a
			// manual post-publish fix-up (fetch end task, rewrite section
			// aliases, bump task version, re-publish) for every PDF report
			// the spec defines section entries for.
			if pdfCfg, _ := endCfg["pdfConfig"].(map[string]any); pdfCfg != nil {
				if sources, ok := pdfCfg["sourcesConfig"].([]any); ok {
					for idx, src := range sources {
						sm, _ := src.(map[string]any)
						if sm == nil {
							continue
						}
						alias, _ := sm["taskAlias"].(string)
						if alias == "" {
							continue
						}
						if server, found := refMap[alias]; found {
							sm["taskAlias"] = server
							sources[idx] = sm
						}
					}
					pdfCfg["sourcesConfig"] = sources
				}
			}
		}
	case "exception":
		// errorMessage flows through graph_workflow's _resolve_dict_variables
		// at execute time, so {{task_outputs.<alias>.<deep>}} placeholders
		// resolve from ScopedWorkflowContext just like an http body would.
		// Without this rewrite, a spec-local ref like {{score.total_score}}
		// survives compose unchanged, the runtime resolver rejects `score`
		// (not a known scope), and the persisted exception ships with the
		// raw template literal instead of the resolved value.
		if s, ok := task["errorMessage"].(string); ok && s != "" {
			out, err := rewriteField("errorMessage", s)
			if err != nil {
				return err
			}
			task["errorMessage"] = out
		}
	case "conditional":
		// Leaf ConditionItems with valueType=="variable" carry a deep ref in
		// `value` (e.g. task_outputs.<spec-ref>.<deep>). Without this rewrite
		// the spec-local ref survives compose, the conditional activity tries
		// to resolve <spec-ref> as a scope, and the comparator falls through
		// to its zero-value default -- silently mis-routing every branch.
		branches, _ := task["branches"].([]any)
		for _, b := range branches {
			bm, _ := b.(map[string]any)
			if bm == nil {
				continue
			}
			if err := rewriteRefsInConditionGroup(bm["conditions"], refMap); err != nil {
				return err
			}
		}
	case "child-workflow":
		// inputExpression carries the dispatch expression for batch mode
		// (BC #1247) -- the runtime resolves it against the parent's
		// task_outputs scope at execute time. A spec-local ref like
		// `task_outputs.fetch.cuit_list` survives compose unchanged
		// otherwise, the runtime can't find `fetch` in the scope, and the
		// expression collapses to a single execution with the full parent
		// context. Walk the same rewriter we use on other deep-ref fields.
		if s, ok := task["inputExpression"].(string); ok && s != "" {
			task["inputExpression"] = rewriteTaskOutputsRefsInString(s, refMap)
		}
	}
	return nil
}

// residualSpecRefExcludedFields is the set of task-body field names where a
// string value is allowed to coincidentally equal a spec-local ref without
// it being a missed rewrite. These are user-facing text fields (labels,
// descriptions) and authored literal slots (mapping-table outputs, condition
// literal values, input-variable defaults) -- a word like "score" landing
// there is a normal user choice, not a compose bug.
//
// The list is intentionally conservative: it shouldn't grow much. The point
// of validateNoResidualSpecRefs is to surface unknown ref-bearing paths;
// excluding too much defeats that. New entries here should be cross-checked
// against whether the field is also walked by rewriteRefsInTaskTemplates.
var residualSpecRefExcludedFields = map[string]bool{
	"label":        true,
	"description":  true,
	"title":        true,
	"subtitle":     true,
	"name":         true,
	"code":         true,
	"comment":      true,
	"errorMessage": true, // template, rewritten elsewhere; value may match a ref legitimately
	"outputJson":   true, // template, rewritten elsewhere
	"filePrefix":   true, // PDF metadata
	"brandLogo":    true, // PDF metadata
	"value":        true, // condition literals, mapping entry literals
	"outputValue":  true, // mapping table literals
	"defaultValue": true, // mapping table fallback literals
	"default":      true, // input-variable defaults
	"placeholder":  true,
	"helpText":     true,
	"hint":         true,
	"tooltip":      true,
}

// validateNoResidualSpecRefs walks a composed task body and returns an error
// if any string value at a non-excluded path exactly equals a key in refMap
// whose server-assigned alias is different. A surviving spec-local ref means
// some ref-bearing field on this task type isn't covered by
// rewriteRefsInTaskTemplates -- the existing rewriter is a hardcoded
// per-type switch, so adding a new ref-bearing field (e.g.
// endConfig.pdfConfig.sourcesConfig[].taskAlias, which silently shipped as
// a bug for some time) requires touching the switch. This validator is the
// safety net: when the next ref-bearing field is added to the API but the
// rewriter isn't updated, compose fails loudly with the offending JSON path
// instead of letting the bad task body ship to the server.
//
// Exact-string-match (not substring) keeps the check high-signal:
// "score" inside a label "Final score breakdown" doesn't match the key
// "score". When a legitimate user literal collides with a ref name, the
// agent can either rename the ref or add the field to
// residualSpecRefExcludedFields -- the error message names the field, so
// the remediation is obvious.
func validateNoResidualSpecRefs(body map[string]any, refMap map[string]string, ctx string) error {
	if len(refMap) == 0 {
		return nil
	}
	var walk func(node any, path string) error
	walk = func(node any, path string) error {
		switch v := node.(type) {
		case map[string]any:
			for k, sub := range v {
				childPath := k
				if path != "" {
					childPath = path + "." + k
				}
				if err := walk(sub, childPath); err != nil {
					return err
				}
			}
		case []any:
			for i, sub := range v {
				if err := walk(sub, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		case string:
			if v == "" {
				return nil
			}
			// Last segment of the dotted path is the field name.
			last := path
			if idx := strings.LastIndexByte(path, '.'); idx >= 0 {
				last = path[idx+1:]
			}
			// Strip a trailing array index "[N]" so the field-name check
			// matches whether the value is at parent.field or
			// parent.field[3].
			if bracket := strings.IndexByte(last, '['); bracket >= 0 {
				last = last[:bracket]
			}
			if residualSpecRefExcludedFields[last] {
				return nil
			}
			if server, found := refMap[v]; found && server != v {
				return fmt.Errorf(
					"%s: residual spec-local ref %q at path %q (expected server-assigned alias %q). "+
						"This means rewriteRefsInTaskTemplates doesn't yet walk this field for the task type. "+
						"Add the field to the rewriter (or add %q to residualSpecRefExcludedFields if the "+
						"literal is genuinely user-authored text).",
					ctx, v, path, server, last)
			}
		}
		return nil
	}
	return walk(body, "")
}

// rewriteRefsInConditionGroup walks a ConditionGroup tree (operator + items
// where each item is either a leaf {field, operator, value, valueType} or
// another nested group) and rewrites the `value` of every variable-typed
// leaf via the spec-ref -> server-alias map. Literal-typed values are left
// alone.
func rewriteRefsInConditionGroup(node any, refMap map[string]string) error {
	group, _ := node.(map[string]any)
	if group == nil {
		return nil
	}
	items, _ := group["items"].([]any)
	for _, it := range items {
		im, _ := it.(map[string]any)
		if im == nil {
			continue
		}
		if _, isGroup := im["items"]; isGroup {
			if err := rewriteRefsInConditionGroup(im, refMap); err != nil {
				return err
			}
			continue
		}
		vt, _ := im["valueType"].(string)
		if vt != "variable" {
			continue
		}
		s, _ := im["value"].(string)
		if s == "" {
			continue
		}
		im["value"] = rewriteTaskOutputsRefsInString(s, refMap)
	}
	return nil
}

// sortedRefMapKeys returns refMap's keys sorted -- used in error messages so
// the agent can scan for a near-typo without re-running 'workflows-v2 list'.
func sortedRefMapKeys(refMap map[string]string) string {
	keys := make([]string, 0, len(refMap))
	for k := range refMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// topologicalTaskOrder returns the indices of spec.Tasks sorted so that each
// task's dependencies (incoming edges + tasks referenced in its inputMappings)
// come BEFORE it. Without this, a task whose inputMappings reads from a
// downstream task in the spec produces a bare ref that survives
// rewriteRefsInMappings (refMap doesn't yet have the downstream alias) and
// then fails at runtime.
//
// Stability: independent tasks retain their original spec order.
//
// Returns an error when a cycle prevents a complete ordering. Cycles are
// also rejected by the runtime engine but surfacing here gives a faster,
// clearer message naming the unresolved tasks.
func topologicalTaskOrder(tasks []map[string]any, edges []map[string]any) ([]int, error) {
	n := len(tasks)
	if n == 0 {
		return nil, nil
	}
	refToIdx := map[string]int{}
	refs := make([]string, n)
	for i, t := range tasks {
		ref := localRef(t, fmt.Sprintf("t%d", i))
		refs[i] = ref
		refToIdx[ref] = i
	}

	// deps[i] = task indices that task i depends on.
	deps := make([]map[int]bool, n)
	for i := range deps {
		deps[i] = map[int]bool{}
	}
	addDep := func(consumer int, depRef string) {
		if depRef == "" {
			return
		}
		depIdx, ok := refToIdx[depRef]
		if !ok || depIdx == consumer {
			return
		}
		deps[consumer][depIdx] = true
	}

	// Edge dependencies: A -> B means A is a dep of B.
	for _, e := range edges {
		from, to := edgeEndpoints(e)
		toIdx, hasTo := refToIdx[to]
		if !hasTo {
			continue // edge endpoint isn't a task (likely an extraNode like start/end)
		}
		addDep(toIdx, from)
	}

	// inputMappings dependencies: a value pointing at task_outputs.X or X.Y
	// makes the consumer depend on X. We walk both the top-level map AND
	// the nested scorecardConfig.inputMappings / ruleTreeConfig.inputMappings
	// maps -- the scorecard/rule-tree activities resolve from the nested
	// form, so unrewritten refs there cause the runtime score=0 collapse
	// even when the top-level looks clean.
	collectMappingDeps := func(consumer int, m map[string]any) {
		for _, v := range m {
			s, _ := v.(string)
			addDep(consumer, mappingDependencyRef(s))
		}
	}
	for i, t := range tasks {
		mappings, _ := t["inputMappings"].(map[string]any)
		collectMappingDeps(i, mappings)
		for _, nested := range []string{"scorecardConfig", "ruleTreeConfig"} {
			if cfg, _ := t[nested].(map[string]any); cfg != nil {
				if nm, _ := cfg["inputMappings"].(map[string]any); nm != nil {
					collectMappingDeps(i, nm)
				}
			}
		}
		// mapping-table entries store inputVariable per-entry (not under
		// inputMappings). Mirror the topological-order coverage so a
		// mapping-table that depends on a compute-variables task gets
		// ordered after it.
		if cfg, _ := t["mappingTableConfig"].(map[string]any); cfg != nil {
			if entries, ok := cfg["entries"].([]any); ok {
				for _, e := range entries {
					if em, ok := e.(map[string]any); ok {
						if s, _ := em["inputVariable"].(string); s != "" {
							addDep(i, mappingDependencyRef(s))
						}
					}
				}
			}
		}
		// child-workflow's inputExpression is a bare path (e.g.
		// `task_outputs.fetch.cuit_list`) the runtime resolves at execute
		// time -- without a topo-dep entry the rewrite pass would fire
		// before refMap knew about <fetch>, leaving the spec-local ref
		// to leak into the persisted task.
		if s, _ := t["inputExpression"].(string); s != "" {
			addDep(i, mappingDependencyRef(s))
		}
		// Template-string dependencies (http url/body/headers, end
		// outputJson). A task whose http url uses {{<other-task>.field}}
		// must be ordered AFTER that task so rewriteRefsInTaskTemplates
		// resolves the ref to a server alias.
		for _, ref := range templateDependencyRefs(t) {
			addDep(i, ref)
		}
	}

	// Stable topological sort: scan original order each iteration, emitting
	// any task whose deps are all visited. O(n²) but n is typically <50.
	out := make([]int, 0, n)
	visited := make([]bool, n)
	for {
		progress := false
		for i := 0; i < n; i++ {
			if visited[i] {
				continue
			}
			ready := true
			for d := range deps[i] {
				if !visited[d] {
					ready = false
					break
				}
			}
			if ready {
				out = append(out, i)
				visited[i] = true
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	if len(out) != n {
		unresolved := []string{}
		for i := 0; i < n; i++ {
			if !visited[i] {
				unresolved = append(unresolved, refs[i])
			}
		}
		return nil, fmt.Errorf(
			"cyclic task dependency detected; could not topologically order: %s. "+
				"Each task in inputMappings can only reference upstream tasks (or those that don't depend on it).",
			strings.Join(unresolved, ", "))
	}
	return out, nil
}

// humanizeKey turns "borrower_id" into "Borrower Id", "minScore" into "Min Score",
// etc. Used to auto-generate display titles for input variables when the spec
// doesn't supply one.
func humanizeKey(key string) string {
	if key == "" {
		return ""
	}
	// Split on _ and -
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '_' || r == '-' })
	// Also split camelCase within each part.
	var words []string
	for _, p := range parts {
		buf := []rune{}
		for i, r := range p {
			if i > 0 && r >= 'A' && r <= 'Z' {
				words = append(words, string(buf))
				buf = []rune{r}
			} else {
				buf = append(buf, r)
			}
		}
		if len(buf) > 0 {
			words = append(words, string(buf))
		}
	}
	for i, w := range words {
		if w == "" {
			continue
		}
		runes := []rune(w)
		if runes[0] >= 'a' && runes[0] <= 'z' {
			runes[0] = runes[0] - 32
		}
		words[i] = string(runes)
	}
	return strings.Join(words, " ")
}

// applyAutoEndDefaults injects apply's opinionated end-node defaults into the
// spec in place (gated by --no-auto-defaults). For every end node it:
//   - forces endConfig.pdfConfig.enabled=true and pdfGenerationRequired=true so
//     the runtime always renders a report (other pdfConfig fields are preserved);
//   - wires inputMappings.borrower_id and inputMappings.billable_id to the single
//     customer node's borrower_id output, so the run is attributed to and billed
//     for the deal owner. end_activity defaults billable_id->borrower_id, but we
//     set both explicitly for clarity. Caller-supplied mappings always win.
//
// The borrower_id source uses the customer node's SPEC-LOCAL ref; the task-build
// loop's rewriteRefsInMappings rewrites it to the server alias. When the spec has
// zero or more than one customer node the source is ambiguous, so we skip the
// borrower/billable wiring and warn (PDF forcing still applies).
func applyAutoEndDefaults(spec *composeSpec) {
	var customerRefs []string
	for i, t := range spec.Tasks {
		if tt, _ := t["type"].(string); tt == "customer" {
			customerRefs = append(customerRefs, localRef(t, fmt.Sprintf("n%d", i)))
		}
	}

	for _, t := range spec.Tasks {
		if tt, _ := t["type"].(string); tt != "end" {
			continue
		}

		// Force PDF generation on.
		endCfg, _ := t["endConfig"].(map[string]any)
		if endCfg == nil {
			endCfg = map[string]any{}
		}
		pdf, _ := endCfg["pdfConfig"].(map[string]any)
		if pdf == nil {
			pdf = map[string]any{}
		}
		pdf["enabled"] = true
		pdf["pdfGenerationRequired"] = true
		endCfg["pdfConfig"] = pdf
		t["endConfig"] = endCfg

		// Wire borrower_id / billable_id from the single customer node.
		if len(customerRefs) != 1 {
			fmt.Fprintf(os.Stderr,
				"# warning: skipped auto-wiring end-node borrower_id/billable_id -- found %d customer nodes (need exactly 1). "+
					"Map them explicitly in the end node's inputMappings, or pass --no-auto-defaults.\n",
				len(customerRefs))
			continue
		}
		im, _ := t["inputMappings"].(map[string]any)
		if im == nil {
			im = map[string]any{}
		}
		src := fmt.Sprintf("task_outputs.%s.borrower_id", customerRefs[0])
		if _, has := im["borrower_id"]; !has {
			im["borrower_id"] = src
		}
		if _, has := im["billable_id"]; !has {
			im["billable_id"] = src
		}
		t["inputMappings"] = im
	}
}

// composeWorkflowBody resolves task creates and assembles the final workflow body.
// In dryRun mode, prints what would be POSTed to /v2/tasks but does not call.
//
// Reference resolution: each task/extraNode has a spec-local "ref" (taken from
// the explicit `ref` field, falling back to `alias`/`nodeId`, falling back to a
// generated `t<idx>`). Edges and inputMappings reference tasks by ref. After
// creating a task, the server returns an alias which becomes the canonical
// nodeId + taskAlias and is used to rewrite all references downstream. If the
// caller provided `alias` in a task body, that alias is sent to the API; if
// absent, the server picks one and we use whatever it returns.
// capture, when non-nil, collects the per-node task bodies (keyed by node id)
// and a node-id->ref reverse map as the graph is assembled. It is populated
// only in the dry assembly pass that feeds the server pre-flight validator
// (see applyAssembleValidateAndPost); the real posting pass passes nil.
func composeWorkflowBody(c *client.Client, spec *composeSpec, dryRun bool, publish bool, autoRescopeEntities bool, allowStealOwnership bool, autoDefaults bool, capture *composeCapture) (map[string]any, error) {
	if err := validateEntityTypeVsTaskTypes(spec); err != nil {
		return nil, err
	}

	// Surface the predicted workflow alias up-front. When the spec sets
	// `alias` explicitly, BC (#1291) honors it verbatim and compose (#32)
	// threads it through; otherwise the server slugifies `label`.
	// Credit-decisioning entities scoped via --workflow-alias on create
	// only show up in pickers when their workflowAlias matches THIS alias,
	// so tell the caller exactly what to stamp before they create
	// entities -- not after, when the workflow's pickers come up empty
	// and they have to re-stamp.
	predictedAlias := spec.Alias
	aliasSource := "explicit `alias` in spec"
	if predictedAlias == "" {
		predictedAlias = slugifyWorkflowLabel(spec.Label)
		aliasSource = fmt.Sprintf("server-derived from label %q", spec.Label)
	}
	fmt.Fprintf(os.Stderr, "# Workflow alias will be: %q (%s).\n", predictedAlias, aliasSource)
	fmt.Fprintf(os.Stderr, "# Stamp entities with this alias on create:\n")
	fmt.Fprintf(os.Stderr, "#   altscore evaluation-rules create --workflow-alias %s ...\n", predictedAlias)
	fmt.Fprintf(os.Stderr, "#   altscore rule-trees       create --workflow-alias %s ...\n", predictedAlias)
	fmt.Fprintf(os.Stderr, "#   altscore mapping-tables   create --workflow-alias %s ...\n", predictedAlias)
	fmt.Fprintf(os.Stderr, "#   altscore scorecards       create --workflow-alias %s ...\n", predictedAlias)

	// Pre-flight: validate every task's REQUIRED-field shape locally before
	// posting anything. The previous loop would create tasks 0..N-1 on the
	// server, then fail on N because a label/type was missing or an enum was
	// wrong. Without a tasks-v2 DELETE endpoint there's no cleanup, so we
	// surface those errors before the first HTTP call. Server-side errors
	// (e.g. "headers must be JSON-encoded string") still happen mid-loop, but
	// the cheap-and-obvious mistakes are now blocked client-side.
	fetchLiveTaskTypes = func() map[string]bool { return fetchServerTaskTypes(c) }
	defer func() { fetchLiveTaskTypes = nil }()

	// Same live-fallback for conditional-node operator validation. The
	// compiled-in conditionOperators map is only a mirror of the backend's
	// WORKFLOW_CONDITION_OPERATORS table; wiring this hook lets validation accept
	// operators the backend gained after this binary was built instead of
	// falsely rejecting a valid workflow. Must be wired BEFORE preflightTasks:
	// preflight's structural pass (validateTaskV2Body) already validates
	// conditional-branch operators, so the hook has to be live by then. Reset the
	// memo so each compose re-fetches lazily on the first miss.
	fetchLiveConditionOperators = func() map[string]bool { return fetchServerConditionOperators(c) }
	liveConditionOperators = nil
	liveConditionOperatorsFetched = false
	defer func() { fetchLiveConditionOperators = nil }()

	// Same live-fallback treatment for the three remaining compiled-in
	// vocabularies preflight validates: workflow category, relationship kind,
	// and inputSchema type. Each map is only a mirror of a backend enum
	// (CategoryEnum / relationships kinds / SchemaTypes), so wiring these hooks
	// lets validation accept values the backend gained after this binary was
	// built instead of falsely rejecting a valid spec. Reset each memo so every
	// compose re-fetches lazily on its first miss.
	fetchLiveWorkflowCategories = func() map[string]bool { return fetchServerWorkflowCategories(c) }
	liveWorkflowCategories = nil
	liveWorkflowCategoriesFetched = false
	defer func() { fetchLiveWorkflowCategories = nil }()

	fetchLiveRelationshipKinds = func() map[string]bool { return fetchServerRelationshipKinds(c) }
	liveRelationshipKinds = nil
	liveRelationshipKindsFetched = false
	defer func() { fetchLiveRelationshipKinds = nil }()

	fetchLiveInputSchemaTypes = func() map[string]bool { return fetchServerInputSchemaTypes(c) }
	liveInputSchemaTypes = nil
	liveInputSchemaTypesFetched = false
	defer func() { fetchLiveInputSchemaTypes = nil }()

	if err := preflightTasks(spec); err != nil {
		return nil, err
	}

	// Warn when end-task outputJson templates inline known-object-typed
	// upstream outputs. The runtime template engine substitutes those refs
	// raw, which produces invalid JSON; BC silently falls back to a
	// promoted-scope dump and the user's custom envelope is lost with no
	// error surfaced. See the la-fabril spike report for the original sighting.
	lintOutputJsonObjectRefs(spec)

	// Warn when a spec contains both a rule-tree task and an end task that
	// isn't wired in the canonical "single end node" shape. The canonical
	// pattern collapses conditional + N parallel end nodes into ONE end node
	// fed directly by the rule-tree, with decision_key tracked through
	// inputMappings -- BC's end_activity then auto-records the per-run
	// decision and renders the PDF. Skipping any field is legal (some
	// workflows want multiple ends per branch, no PDF, or no decision
	// recording), so this lint is advisory only.
	lintCanonicalEndNode(spec)

	// Advisory: flag customVariables that are pure pass-through extraction
	// probes (a compute-variables node + a custom var whose expression merely
	// extracts a scoped scalar). The cleaner design wires the scoped value
	// directly into the consuming node's inputMappings. Advisory only --
	// emitted to stderr, never blocks apply (see adviseExtractionProbes).
	if len(spec.CustomVariables) > 0 {
		specNodes := make([]any, len(spec.Nodes))
		for i, n := range spec.Nodes {
			specNodes[i] = n
		}
		adviseExtractionProbes(spec.CustomVariables, specNodes)
	}

	// persona is required by CreateBorrower's Literal["individual","business"]
	// validator on the new-borrower path. It is a property of the workflow's
	// DESIGN (a cedula flow is always "individual", a RUC flow always
	// "business"), not a per-execution choice -- so by default it lives on the
	// entity-write task as a literal (set by normalizeEntityWriteTask) and does
	// NOT surface as a user-facing input. The runtime resolves persona as
	// `context.get("persona") or task.persona`, so a mapped context value wins.
	//
	// Only add inputVariables.persona when the agent opted into a per-execution
	// persona: by declaring it, or by wiring an entity-write task's
	// inputMappings.persona to inputs.*. Caller-supplied inputVariables.persona
	// always wins (we never override).
	personaAsInput := false
	if _, has := spec.InputVariables["persona"]; has {
		personaAsInput = true
	}
	if !personaAsInput {
		for _, t := range spec.Tasks {
			tt, _ := t["type"].(string)
			if tt != "customer" && tt != "deal" && tt != "asset" {
				continue
			}
			if op, _ := t["operation"].(string); op != "write" {
				continue
			}
			if src, _ := asMap(t["inputMappings"])["persona"].(string); strings.HasPrefix(strings.TrimSpace(src), "inputs.") {
				personaAsInput = true
				break
			}
		}
	}
	if personaAsInput {
		if spec.InputVariables == nil {
			spec.InputVariables = map[string]any{}
		}
		if _, has := spec.InputVariables["persona"]; !has {
			spec.InputVariables["persona"] = map[string]any{
				"type":        "string",
				"default":     "individual",
				"required":    false,
				"title":       "Type of customer",
				"description": "Borrower persona at create time. Defaults to 'individual'; pass 'business' when triggering the workflow for a corporate borrower.",
			}
		}
	}

	// Opinionated end-node defaults (gated by --no-auto-defaults): force PDF
	// generation on and wire borrower_id/billable_id to the single customer
	// node. Runs before the task-build loop so the borrower_id mapping uses a
	// spec-local ref that the loop's rewriteRefsInMappings turns into the
	// server alias.
	if autoDefaults {
		applyAutoEndDefaults(spec)
	}

	createdAliases := []string{}
	taskNodes := []map[string]any{}

	// refMap: spec-local reference -> server-assigned alias.
	refMap := map[string]string{}

	// Topologically sort task creation order so cross-task inputMappings
	// always resolve at the time we POST each task. Without this, a task
	// listed in spec.tasks BEFORE the task it references would have a bare
	// "<ref>.<output>" value persist verbatim -- the runtime resolver then
	// fails with "Unknown variable namespace". rewriteRefsInMappings now
	// errors on unresolved refs as a safety net, so this ordering is the
	// difference between "compose works regardless of spec ordering" and
	// "compose silently produces broken workflows when authors list tasks
	// in flow order rather than dependency order".
	order, err := topologicalTaskOrder(spec.Tasks, spec.Edges)
	if err != nil {
		return nil, err
	}

	for _, specIdx := range order {
		i := specIdx
		task := spec.Tasks[specIdx]
		ref := localRef(task, fmt.Sprintf("t%d", i))
		label, _ := task["label"].(string)
		taskType, _ := task["type"].(string)
		if label == "" || taskType == "" {
			return nil, fmt.Errorf("node ref=%q: label and type are required", ref)
		}

		// Carry the spec-local ref as `specRef` on the task body so the
		// server's stable-alias path (BC's CreateTaskV2UC) can look this
		// task up on subsequent applies and version-bump it instead of
		// minting a fresh slug-XXXXXX. predictedAlias scopes the lookup
		// to this workflow: the same ref "score" is legitimately used
		// across many workflows, so (workflowAlias, specRef) is the
		// identity. The fields are advisory on the server side -- a BC
		// without the stable-alias path silently ignores them, so older
		// CLI/server combos keep working.
		task["specRef"] = ref
		task["workflowAlias"] = predictedAlias

		// Strip the spec-only `ref` field before posting; it's not part of the API.
		delete(task, "ref")

		// Rewrite inputMappings using refs resolved so far. Topological
		// ordering above guarantees every dependency is in refMap by the
		// time we get here, so an "unknown ref" error here always means a
		// typo or a reference to a task that simply isn't in spec.tasks.
		if mappings, ok := task["inputMappings"].(map[string]any); ok {
			rewritten, rerr := rewriteRefsInMappings(mappings, refMap)
			if rerr != nil {
				return nil, fmt.Errorf("node ref=%q: %w", ref, rerr)
			}
			task["inputMappings"] = rewritten
		}
		// scorecard and rule-tree activities resolve their per-rule field
		// references from a NESTED inputMappings map (scorecardConfig and
		// ruleTreeConfig respectively -- see graph_workflow's dedicated
		// per-task-type branches in _resolve_task_variables). Compose's
		// rewrite was only walking the top-level map, leaving nested refs
		// like "task_outputs.<spec-ref>.field" untouched at the server
		// after spec-ref-to-alias translation -- so the runtime resolver
		// looked up the spec ref (which doesn't exist in task_outputs)
		// and every rule field came back None. Symptom: scorecard total=0
		// for every input, rule-tree falls through to its default branch
		// regardless of upstream values. Rewrite the nested maps too.
		for _, nested := range []string{"scorecardConfig", "ruleTreeConfig"} {
			cfg, _ := task[nested].(map[string]any)
			if cfg == nil {
				continue
			}
			nestedMappings, ok := cfg["inputMappings"].(map[string]any)
			if !ok || len(nestedMappings) == 0 {
				continue
			}
			rewritten, rerr := rewriteRefsInMappings(nestedMappings, refMap)
			if rerr != nil {
				return nil, fmt.Errorf("node ref=%q %s.inputMappings: %w", ref, nested, rerr)
			}
			cfg["inputMappings"] = rewritten
			task[nested] = cfg
		}
		// Mapping-table tasks store their per-entry input wiring as
		// mappingTableConfig.entries[].inputVariable -- NOT in inputMappings.
		// The runtime mapping_table_activity resolves each entry's input
		// against the workflow context using whatever string is in
		// inputVariable verbatim, so a spec-local ref left as
		// `task_outputs.compute.X` after compose doesn't resolve (the
		// _task_outputs key is the server alias, not the spec ref). The same
		// is true for the top-level inputMappings that
		// normalizeMappingTableTask later mirrors from the entries -- if we
		// rewrite the entries here, the mirror will pick up canonical
		// (server-aliased) values too.
		if cfg, _ := task["mappingTableConfig"].(map[string]any); cfg != nil {
			if entries, ok := cfg["entries"].([]any); ok {
				for idx, e := range entries {
					em, ok := e.(map[string]any)
					if !ok {
						continue
					}
					if s, ok := em["inputVariable"].(string); ok && s != "" {
						em["inputVariable"] = rewriteTaskOutputsRefsInString(s, refMap)
					}
					entries[idx] = em
				}
				cfg["entries"] = entries
				task["mappingTableConfig"] = cfg
			}
		}

		// Rewrite {{...}} template placeholders in known per-type fields
		// (http url/body/headers, end outputJson). Same rules as
		// inputMappings rewrite, just operating on substrings inside
		// template strings. Without this, a `{{<other-task>.field}}` on
		// an http body silently survives compose and fails at execute
		// time -- the runtime template engine substitutes nothing because
		// the bare ref isn't a valid runtime namespace.
		if err := rewriteRefsInTaskTemplates(task, refMap); err != nil {
			return nil, fmt.Errorf("node ref=%q: %w", ref, err)
		}

		// Safety net: after all known rewriters have run, scan the task body
		// for any string value that still exactly equals a spec-local ref.
		// Such a residue means a ref-bearing field exists somewhere in the
		// task schema that no rewriter knows about -- the same class of bug
		// as endConfig.pdfConfig.sourcesConfig[].taskAlias before it was
		// added to the per-type switch above. Failing here surfaces the path
		// before postTask ships a broken body.
		if err := validateNoResidualSpecRefs(task, refMap, fmt.Sprintf("node ref=%q", ref)); err != nil {
			return nil, err
		}

		// Type-specific normalization: enrich altdata-enrichment with inputKeys
		// from source inputFields, validate conditional branches, etc.
		if err := normalizeTaskBody(c, task, &composeNormalizeOpts{
			PredictedAlias:      predictedAlias,
			CustomVariables:     spec.CustomVariables,
			InputVariables:      spec.InputVariables,
			Publish:             publish,
			AutoRescopeEntities: autoRescopeEntities,
			AllowStealOwnership: allowStealOwnership,
			AutoDefaults:        autoDefaults,
		}, dryRun); err != nil {
			return nil, fmt.Errorf("node ref=%q: %w", ref, err)
		}

		serverAlias, version, err := postTask(
			c, task, ref, dryRun,
			fmt.Sprintf("node ref=%q", ref),
		)
		if err != nil {
			return nil, fmt.Errorf("%w (created so far: %v)", err, createdAliases)
		}
		if !dryRun {
			createdAliases = append(createdAliases, serverAlias)
		}

		refMap[ref] = serverAlias

		// Snapshot the task body apply would POST for this node (keyed by the
		// node id it backs) so the server pre-flight validator sees the exact
		// bodies. Only the dry assembly pass sets capture; the real pass leaves
		// it nil.
		if capture != nil {
			if snap, merr := json.Marshal(task); merr == nil {
				capture.tasks[serverAlias] = snap
				capture.refByNodeID[serverAlias] = ref
			}
		}

		// Build the corresponding graph node using the server-assigned alias.
		node := map[string]any{
			"nodeId":      serverAlias,
			"type":        taskType,
			"label":       label,
			"taskAlias":   serverAlias,
			"taskVersion": version,
			"position":    map[string]float64{"x": float64(200 * (i + 1)), "y": 0},
			"data":        map[string]any{},
		}
		if mappings, ok := task["inputMappings"]; ok {
			data := node["data"].(map[string]any)
			data["inputMappings"] = mappings
		}
		taskNodes = append(taskNodes, node)
	}

	// Compose nodes: extraNodes first (typically start/end), then task nodes.
	// EVERY node needs a backing /v2/tasks record -- the Hub creates trivial
	// tasks for start/end too. So we create one task per extraNode unless the
	// caller already supplied taskAlias.
	allNodes := []map[string]any{}
	for i, n := range spec.ExtraNodes {
		ref := localRef(n, fmt.Sprintf("n%d", i))
		nodeType, _ := n["type"].(string)
		label, _ := n["label"].(string)
		if nodeType == "" || label == "" {
			return nil, fmt.Errorf("node ref=%q: type and label are required", ref)
		}

		// Strip the spec-only `ref` field; nodes use `nodeId` (canonical).
		delete(n, "ref")

		taskAlias, _ := n["taskAlias"].(string)
		taskID, _ := n["taskId"].(string)
		if taskAlias == "" && taskID == "" {
			// Auto-create a trivial backing task: just type + label.
			// For `end` nodes we also auto-wire inputSchema / inputMappings
			// from upstream tasks so the PDF report editor (and end_activity's
			// context.get() calls) have the data they expect. Without this,
			// the end task ships with empty schemas and the PDF picker shows
			// nothing -- matching the bug the user hit before manually
			// recreating the end node in the Hub UI.
			//
			// specRef + workflowAlias enable the server's stable-alias path
			// (BC's CreateTaskV2UC.find_by_spec_ref) so successive applies
			// of the same spec version-bump THIS task instead of minting a
			// fresh fin-XXXXXX / start-XXXXXX alias. Same rationale as the
			// spec.Tasks loop above.
			taskBody := map[string]any{
				"label":         label,
				"type":          nodeType,
				"specRef":       ref,
				"workflowAlias": predictedAlias,
			}
			if strings.ToLower(nodeType) == "end" {
				// Data-source ancestors (altdata-enrichment, scorecard,
				// mapping-table, rule-tree, evaluate-rules) used to be
				// walked here and pre-filled into endConfig.pdfConfig
				// .sourcesConfig (the ~180-LOC buildEndAutoWiring). That
				// walker now lives at runtime in borrower-central's
				// end_activity, which auto-resolves sources from the
				// workflow graph whenever pdfConfig.enabled=true but
				// sourcesConfig is empty. Agents can flip enabled=true
				// without also pre-populating the array.
				//
				// inputSchema / inputMappings auto-wiring for the Hub
				// canvas is similarly handled client-side by
				// pdf-data-source-auto-mapping.ts when the workflow is
				// loaded into the builder.
				inSchema := map[string]any{}
				inMappings := map[string]any{}
				var pdfSections []map[string]any

				// Spec extension: per-end-node htmlSections render as
				// PDF sections in the report. Each section interpolates
				// {var} tokens against the end task's resolved context
				// at runtime; compose auto-wires the inputMappings for
				// any input or custom variable referenced in the content
				// so safe_format finds them. Task-output references
				// (e.g. {credit_score}) need no wiring -- end_activity
				// promotes upstream task outputs to root in
				// enriched_context.
				if rawSections := readSpecHTMLSections(n); len(rawSections) > 0 {
					built, htmlSchema, htmlMappings := buildHTMLSections(rawSections, spec.InputVariables, spec.CustomVariables)
					pdfSections = append(pdfSections, built...)
					for k, v := range htmlSchema {
						inSchema[k] = v
					}
					for k, v := range htmlMappings {
						inMappings[k] = v
					}
				}
				// htmlSections is spec sugar, not part of the API node shape.
				delete(n, "htmlSections")

				if len(inSchema) > 0 {
					taskBody["inputSchema"] = inSchema
				}
				if len(inMappings) > 0 {
					taskBody["inputMappings"] = inMappings
				}
				// Build endConfig from per-end-node spec input. Caller-
				// supplied fields under `extraNodes[].endConfig`
				// (decisionConfig, outputJson, pdfConfig title/subtitle/
				// brandLogo, etc.) are preserved verbatim; compose only
				// auto-fills pdfConfig.enabled=true (so the runtime
				// generator turns on) and any htmlSections-derived
				// entries the spec asked for.
				//
				// pdfConfig.enabled=true is what actually flips the PDF
				// generator on. Without it, the runtime end_activity sees
				// endConfig=null and skips report rendering entirely.
				// Same for decisionConfig: if the spec sets `enabled:
				// true, decisionType: "final"`, the runtime records the
				// rule-tree decision via
				// /v1/executions/{id}/decisions; if compose silently
				// stripped that to null, decisions never persist.
				userEndCfg, _ := n["endConfig"].(map[string]any)
				if userEndCfg == nil {
					userEndCfg = map[string]any{}
				}
				userPdfCfg, _ := userEndCfg["pdfConfig"].(map[string]any)
				if userPdfCfg == nil {
					userPdfCfg = map[string]any{}
				}
				// Compose-canonical PDF defaults (overridden by user when
				// supplied via spec). sourcesConfig is left empty -- the
				// runtime auto-resolves ancestor sections when this stays
				// empty AND pdfConfig.enabled=true. htmlSections-derived
				// entries (if any) are added below.
				//
				// Guard against nil here: a Go []map[string]any{} that
				// was never appended-to marshals as JSON null, and BC's
				// Pydantic validator rejects null with
				// "none is not an allowed value". Always emit [] so the
				// payload is valid even when the spec didn't request
				// any pdfConfig.
				if pdfSections == nil {
					pdfSections = []map[string]any{}
				}
				pdfDefaults := map[string]any{
					"brandLogo":             nil,
					"enabled":               true,
					"filePrefix":            "",
					"pdfGenerationRequired": false,
					"sourcesConfig":         pdfSections,
					"title":                 "Report",
					"subtitle":              "",
				}
				for k, v := range userPdfCfg {
					pdfDefaults[k] = v
				}
				endCfgOut := map[string]any{
					"decisionConfig": userEndCfg["decisionConfig"], // may be nil; spec passes through
					"outputJson":     "",
					"pdfConfig":      pdfDefaults,
				}
				if oj, ok := userEndCfg["outputJson"].(string); ok {
					endCfgOut["outputJson"] = oj
				}
				// Carry through any extra fields the user supplied that
				// compose doesn't know about (forward-compat with future
				// endConfig additions).
				for k, v := range userEndCfg {
					if _, handled := endCfgOut[k]; handled {
						continue
					}
					if k == "pdfConfig" {
						continue
					}
					endCfgOut[k] = v
				}
				taskBody["endConfig"] = endCfgOut
				// Strip the spec-only `endConfig` key from the graph node
				// so it doesn't double-write.
				delete(n, "endConfig")

				// Rewrite spec-local refs in endConfig.outputJson so they
				// land canonical (task_outputs.<server-alias>.<deep>). The
				// rewrite for spec.tasks happens at line ~1348; extraNode
				// end tasks bypass that path because they're built here, so
				// without this call an outputJson template like
				// `{{score.total_score}}` survives unchanged and the runtime
				// resolver rejects `score` as an unknown scope.
				if err := rewriteRefsInTaskTemplates(taskBody, refMap); err != nil {
					return nil, fmt.Errorf("node ref=%q: %w", ref, err)
				}
				// Same safety net as the spec.Tasks loop above -- see comment
				// there. extraNode end tasks build their body inline, so any
				// rewrite gap in pdfConfig (or future end-task fields) would
				// otherwise ship verbatim.
				if err := validateNoResidualSpecRefs(taskBody, refMap, fmt.Sprintf("node ref=%q", ref)); err != nil {
					return nil, err
				}
			}

			alias, version, err := postTask(
				c, taskBody, ref, dryRun,
				fmt.Sprintf("node ref=%q (extra-node backing)", ref),
			)
			if err != nil {
				return nil, err
			}
			taskAlias = alias
			n["taskAlias"] = alias
			n["taskVersion"] = version
			if !dryRun {
				createdAliases = append(createdAliases, alias)
			}
			// Snapshot the auto-created backing task body for the server
			// pre-flight validator (dry assembly pass only; see the task loop).
			if capture != nil {
				if snap, merr := json.Marshal(taskBody); merr == nil {
					capture.tasks[alias] = snap
					capture.refByNodeID[alias] = ref
				}
			}
		}

		// nodeId follows the server-assigned task alias (1:1) so edges and
		// downstream references resolve cleanly.
		if taskAlias != "" {
			n["nodeId"] = taskAlias
			refMap[ref] = taskAlias
		} else {
			// taskId-only path (rare): keep whatever nodeId was passed.
			if id, _ := n["nodeId"].(string); id == "" {
				n["nodeId"] = ref
			}
			refMap[ref] = fmt.Sprint(n["nodeId"])
		}

		// Auto-position if missing: spread along the same row
		if _, hasPos := n["position"]; !hasPos {
			x := float64(0)
			if strings.ToLower(nodeType) == "end" {
				x = float64(200 * (len(spec.Tasks) + 1))
			}
			n["position"] = map[string]float64{"x": x, "y": 0}
		}
		if _, hasData := n["data"]; !hasData {
			n["data"] = map[string]any{}
		}
		// Mirror Hub: end nodes carry isEndNode on node.data so the canvas
		// renderer flags them as terminal. inputMappings auto-wiring for the
		// PDF editor's section picker is now derived client-side by the
		// Hub (pdf-data-source-auto-mapping.ts) when the workflow loads.
		if strings.ToLower(nodeType) == "end" {
			data, _ := n["data"].(map[string]any)
			if data == nil {
				data = map[string]any{}
			}
			data["isEndNode"] = true
			n["data"] = data
		}
		allNodes = append(allNodes, n)
	}
	for _, n := range taskNodes {
		allNodes = append(allNodes, n)
	}

	// Edges: support `from`/`to` shortcuts and resolve refs via refMap.
	allEdges := []map[string]any{}
	for i, e := range spec.Edges {
		src, _ := e["sourceNodeId"].(string)
		if src == "" {
			if v, _ := e["from"].(string); v != "" {
				src = v
			}
		}
		tgt, _ := e["targetNodeId"].(string)
		if tgt == "" {
			if v, _ := e["to"].(string); v != "" {
				tgt = v
			}
		}
		if src == "" || tgt == "" {
			return nil, fmt.Errorf("edges[%d]: source/target required (use sourceNodeId+targetNodeId or from+to)", i)
		}
		if alias, ok := refMap[src]; ok {
			src = alias
		}
		if alias, ok := refMap[tgt]; ok {
			tgt = alias
		}
		delete(e, "from")
		delete(e, "to")
		e["sourceNodeId"] = src
		e["targetNodeId"] = tgt
		if _, hasID := e["id"]; !hasID {
			e["id"] = fmt.Sprintf("%s->%s", src, tgt)
		}
		allEdges = append(allEdges, e)
	}

	status := spec.Status
	if status == "" {
		status = "DRAFT"
	}
	inputVars := spec.InputVariables
	if inputVars == nil {
		inputVars = map[string]any{}
	}
	// Hub-created input variables include name + title + description so the
	// builder can render labeled form fields. Auto-fill these from the dict
	// key if the caller didn't supply them or supplied empty strings.
	// `label` (intuitive) maps to `title` (canonical) for caller convenience.
	for key, raw := range inputVars {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := v["name"].(string); name == "" {
			v["name"] = key
		}
		// Allow callers to provide either `title` or `label`; either an absent
		// or empty value triggers the auto-humanized fallback.
		title, _ := v["title"].(string)
		if title == "" {
			if l, _ := v["label"].(string); l != "" {
				title = l
			}
		}
		delete(v, "label")
		if title == "" {
			title = humanizeKey(key)
		}
		v["title"] = title
		if _, has := v["description"]; !has {
			v["description"] = ""
		}
		inputVars[key] = v
	}
	customVars := spec.CustomVariables
	if customVars == nil {
		customVars = map[string]any{}
	}
	// Normalize customVariable expression shape so the runtime
	// compute-variables wrapper produces a non-None value for the common
	// authoring mistake of `expression: "640"` (bare literal). The wrapper
	// at borrower-central/app/temporal/activities/compute_variables_activity.py
	// is `def execute(...): inputs = input; <expression>; return <returnValue
	// or "None">`. With a bare literal expression and no returnValue, the
	// statement is evaluated and discarded, then None is returned.
	//
	// Three resolution paths:
	//   1. expression contains `result =` AND returnValue=="result"  -> shape OK
	//   2. expression has no assignment AND no returnValue           -> set returnValue
	//      to the expression itself so the wrapper's `return <returnValue>`
	//      evaluates the literal/expression directly. Idempotent under
	//      re-compose because the second pass sees a returnValue and skips.
	//   3. variable defined with just `{type, default}` (no expression)
	//      -> nothing to do; BC's graph_workflow seeds custom_variables[<name>]
	//      from `default` at workflow init.
	for name, raw := range customVars {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		expression, _ := v["expression"].(string)
		returnValue, _ := v["returnValue"].(string)
		if expression == "" || returnValue != "" {
			// nothing to mirror, fall through to the ref-rewrite step below
		} else if !strings.Contains(expression, "=") {
			// Bare-literal / single-expression detection: no top-level
			// assignment statement. Match the simplest case (no `=` at all).
			// If the expression has multi-statement bodies the author should
			// declare returnValue explicitly.
			v["returnValue"] = expression
		}

		// Rewrite spec-ref prefixes in customVariable strings so the runtime
		// compute_variables_activity can resolve them against _task_outputs.
		// The dependency list is the lookup key against _task_outputs (see
		// borrower-central/app/temporal/activities/compute_variables_activity.py
		// :_collect_dependencies), AND the same string is used as the dict
		// key into `inputs` inside the expression -- so both must agree on
		// the alias. Symptom when this rewrite is missing: every dependency
		// resolves to None, the compute function falls back to whatever
		// default it has (often 0 or a sentinel like -999999), and every
		// downstream score / decision collapses. Tracked through the v10
		// stress run (Argentine SMB workflow with refs like
		// "task_outputs.enrich.ARG-PUB-0001..." that compose persisted
		// without rewriting `enrich` to the server alias).
		expression, _ = v["expression"].(string)
		returnValue, _ = v["returnValue"].(string)
		if expression != "" {
			v["expression"] = rewriteTaskOutputsRefsInString(expression, refMap)
		}
		if returnValue != "" {
			v["returnValue"] = rewriteTaskOutputsRefsInString(returnValue, refMap)
		}
		if deps, ok := v["dependencies"].([]any); ok {
			for i, d := range deps {
				if ds, ok := d.(string); ok {
					deps[i] = rewriteTaskOutputsRefsInString(ds, refMap)
				}
			}
			v["dependencies"] = deps
		}

		customVars[name] = v
	}
	wf := map[string]any{
		"label":           spec.Label,
		"category":        spec.Category,
		"status":          status,
		"inputVariables":  inputVars,
		"customVariables": customVars,
		"nodes":           allNodes,
		"edges":           allEdges,
	}
	// Honor an explicit `alias` from the spec (BC #1291 made the create
	// endpoint accept it). Without this, BC slugifies `label` -- which is
	// fine, but specs that set alias explicitly were silently ignored by
	// compose for several releases. Threaded as a plain pass-through.
	if spec.Alias != "" {
		wf["alias"] = spec.Alias
	}
	// Send description whenever the spec set the field -- including an explicit
	// "" -- so a workflow's description can be blanked on update. Omitting the
	// field (nil) leaves the existing description untouched.
	if spec.Description != nil {
		wf["description"] = *spec.Description
	}
	if spec.Config != nil {
		wf["config"] = spec.Config
	}
	if spec.Notes != nil {
		wf["notes"] = spec.Notes
	}
	return wf, nil
}

// customerHiddenTypes / dealHiddenTypes mirror the Hub's palette filter at
// altscore-ai-chat/components/workflow-builder-v2/canvas/ComponentsMenu.tsx
// (CUSTOMER_HIDDEN_TYPES / DEAL_HIDDEN_TYPES). A workflow whose
// config.entityType is "customer" cannot include these task types in the
// Hub editor, so a CLI-composed workflow that uses them would render as a
// non-editable graph -- catch it at compose time instead.
var customerHiddenTypes = map[string]bool{
	"deal":  true,
	"asset": true,
}
var dealHiddenTypes = map[string]bool{
	"customer":         true,
	"list-of-similars": true,
}

// validTaskTypes mirrors the backend TaskType enum at
// borrower-central/app/model/workflows_v2/task.py. Sourced once and kept in
// sync as the enum evolves. Used by preflight to reject typos like
// "data-store" (correct: data-store-write/data-store-query) BEFORE creating
// any /v2/tasks rows. Without this check, a typo would orphan all earlier
// tasks in the compose loop (no rollback path exists today).
var validTaskTypes = map[string]bool{
	"http": true, "conditional": true, "comment": true,
	"start": true, "end": true, "wait": true, "webhook": true,
	"create-borrower": true, "update-borrower": true, "update-borrower-name": true,
	"evaluate-rules": true, "altdata-enrichment": true, "create-identity": true,
	"fetch-entity": true, "html-template": true, "fetch-borrower-entities": true,
	"child-workflow": true, "exception": true, "soap": true,
	"mapping-table": true, "scorecard": true, "rule-tree": true,
	"compute-variables": true, "data-store-write": true, "data-store-query": true,
	"customer": true, "deal": true, "credit-line": true,
	"list-of-similars": true, "asset": true, "relationships": true,
	"package-io": true, "sftp": true, "notices": true,
}

// fetchLiveTaskTypes, when set, lazily returns the LIVE backend's task-type
// list so preflight can accept types added to the backend after this binary
// was built (the compiled-in validTaskTypes map above is only a mirror).
// composeWorkflowBody wires it to fetchServerTaskTypes before preflight;
// unit tests leave it nil, keeping preflight fully offline.
var fetchLiveTaskTypes func() map[string]bool

// fetchServerTaskTypes queries GET /v1/meta/workflows-v2-schema?section=taskTypes,
// the machine-readable type list BC derives from its TaskType enum at request
// time. Returns nil on any transport/shape error -- callers fall back to the
// compiled-in mirror, which is exactly the pre-existing behavior.
func fetchServerTaskTypes(c *client.Client) map[string]bool {
	data, _, err := c.Do("GET", "borrower_central", "/v1/meta/workflows-v2-schema?section=taskTypes", nil)
	if err != nil {
		return nil
	}
	var payload struct {
		TaskTypes struct {
			Values []string `json:"values"`
		} `json:"taskTypes"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.TaskTypes.Values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(payload.TaskTypes.Values))
	for _, v := range payload.TaskTypes.Values {
		out[v] = true
	}
	return out
}

// preflightTasks runs cheap validation across every task in the spec before
// composeWorkflowBody starts POSTing -- fully local, except at most one
// read-only backend lookup when a task type is unknown to this build (see
// fetchLiveTaskTypes). Catches the mistakes that would otherwise create
// orphan /v2/tasks rows mid-loop. Without a tasks-v2 DELETE endpoint,
// partial-failure cleanup is impossible; everything we can catch here we
// MUST.
//
// Checks (in order, fail-fast):
//  1. duplicate spec-local refs / explicit aliases
//  2. label + type present
//  3. type is in the backend TaskType enum (with closest-match suggestion)
//  4. http: headers must be a JSON-encoded string
//  5. data-store-write / data-store-query / webhook / comment / exception /
//     child-workflow: per-type required fields
//  6. validateTaskV2Body: type-specific structural checks (conditional
//     branches, scorecard reference, mapping-table entries, rule-tree
//     enums)
//  7. inputMappings values: leading segment must be a runtime namespace OR
//     a known spec-local ref; task_outputs.<X>.<rest> validates <X> too.
//     {{...}} template syntax is skipped (handled by template engine).
//  8. edge endpoints (from/to) must reference a known ref.
//  9. duplicate edges and self-loops are rejected.
func preflightTasks(spec *composeSpec) error {
	// Spec-level checks: workflow category + inputVariables shape. These
	// fail with opaque backend errors otherwise; surface here.
	if err := checkWorkflowCategory(spec.Category); err != nil {
		return err
	}
	for name, def := range spec.InputVariables {
		dm, _ := def.(map[string]any)
		if dm == nil {
			continue
		}
		t, _ := dm["type"].(string)
		if t == "" {
			continue
		}
		if err := checkInputSchemaType(t, fmt.Sprintf("workflow.inputVariables.%s.type", name)); err != nil {
			return err
		}
	}

	// Collect every spec-local ref upfront so we can validate forward
	// references in inputMappings AND detect duplicates that would
	// otherwise silently orphan tasks (the rewriter only records the
	// last ref-to-alias mapping).
	knownRefs := map[string]bool{}
	knownAliases := map[string]bool{}
	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		// 'ref' becomes the server-assigned alias prefix (and thus the
		// nodeId) when no explicit 'alias' is set, so the same URL-safety
		// constraint applies. Reject upper-case / spaces / punctuation
		// upfront -- otherwise it propagates to nodeIds that misbehave in
		// downstream cmds (set-mapping --node-id, lock acquire by alias).
		if !validAliasPattern.MatchString(ref) {
			return fmt.Errorf(
				"node ref %q has invalid characters. Refs become server-assigned aliases (and nodeIds), "+
					"so must be lowercase alphanumeric with internal dashes only "+
					"(regex: ^[a-z0-9][a-z0-9-]*$). Don't use spaces, underscores, slashes, uppercase, or other punctuation.",
				ref,
			)
		}
		if knownRefs[ref] {
			return fmt.Errorf(
				"node duplicate ref %q -- two tasks share the same spec-local key. "+
					"Compose's edge rewriter only records the LAST ref-to-alias mapping, so the earlier "+
					"task ends up with no incident edges (silent orphan). Give each task a unique 'ref'.",
				ref,
			)
		}
		knownRefs[ref] = true
		if alias, _ := task["alias"].(string); alias != "" {
			if !validAliasPattern.MatchString(alias) {
				return fmt.Errorf(
					"node ref=%q: alias %q has invalid characters. Aliases end up in URL paths "+
						"so must be lowercase alphanumeric with internal dashes only "+
						"(regex: ^[a-z0-9][a-z0-9-]*$). "+
						"Don't use spaces, slashes, uppercase, or punctuation.",
					ref, alias,
				)
			}
			if knownAliases[alias] {
				return fmt.Errorf(
					"node ref=%q: duplicate explicit alias %q -- two tasks declare the same alias. "+
						"The second create either version-bumps the first or 409s. "+
						"Either drop the alias on one (compose will pick a unique one) or pick distinct aliases.",
					ref, alias,
				)
			}
			knownAliases[alias] = true
		}
	}
	startCount := 0
	for i, node := range spec.ExtraNodes {
		ref := localRef(node, fmt.Sprintf("n%d", i))
		// Same URL-safety constraint as tasks: refs become server-assigned
		// aliases (and thus nodeIds). Reject upper-case / underscores /
		// spaces / punctuation upfront so the spec fails fast instead of
		// 400'ing at the per-node POST.
		if !validAliasPattern.MatchString(ref) {
			return fmt.Errorf(
				"node ref %q has invalid characters. Refs become server-assigned aliases (and nodeIds), "+
					"so must be lowercase alphanumeric with internal dashes only "+
					"(regex: ^[a-z0-9][a-z0-9-]*$). Don't use spaces, underscores, slashes, uppercase, or other punctuation.",
				ref,
			)
		}
		if knownRefs[ref] {
			return fmt.Errorf(
				"node duplicate ref %q -- collides with another node's 'ref'. "+
					"Give each node a unique 'ref'.",
				ref,
			)
		}
		knownRefs[ref] = true
		// ExtraNodes only ever contains start-typed nodes (the parse-time
		// split puts type=="start" -> ExtraNodes, everything else -> Tasks).
		// Just count starts; no other case can fire by construction.
		nodeType, _ := node["type"].(string)
		if nodeType == "start" {
			startCount++
		}
	}
	if startCount == 0 {
		return fmt.Errorf(
			"spec has no 'start' node. Every workflow needs exactly one start node; " +
				"the engine doesn't know where to begin without it. Add " +
				`{"ref": "start", "type": "start", "label": "Start"} to nodes[].`,
		)
	}
	if startCount > 1 {
		return fmt.Errorf(
			"spec has %d 'start' nodes. Every workflow needs exactly ONE start; "+
				"multiple starts make the engine's traversal non-deterministic. Drop the extras.",
			startCount,
		)
	}
	// End nodes always come through the Tasks bucket post-split (end nodes
	// need an endConfig to emit output, so they go through the full task
	// creation path). Count end nodes there.
	endInTasks := 0
	for _, t := range spec.Tasks {
		if tt, _ := t["type"].(string); tt == "end" {
			endInTasks++
		}
	}
	if endInTasks == 0 {
		// 'end' is conventional but not strictly required; warn-only via
		// stderr, never block. Keep this open for niche use cases (e.g.
		// workflows that terminate via 'exception' branches).
		fmt.Fprintln(os.Stderr,
			"# warning: compose spec has no 'end' node. Most workflows need one for the engine to know where to terminate cleanly.")
	}
	if endInTasks > 1 {
		// A workflow must converge to exactly one end node. Surfaced here in
		// preflight so the apply CREATE path catches it before POSTing (the
		// CREATE path doesn't run validateWorkflowV2Body). Mirror the start-node
		// uniqueness check above.
		return fmt.Errorf(
			"spec has %d 'end' nodes. A workflow must have exactly ONE end node; "+
				"converge all paths (conditional branches, relationship handles) to a single end.",
			endInTasks,
		)
	}

	// Soft advisory: routing tasks (conditional) with branch
	// edges targeting exception tasks usually indicate the agent is treating
	// 'rejected'/'declined'/'manual review' as a failure when they're really
	// valid workflow outcomes that belong on end nodes (one per branch,
	// each with its own endConfig.decisionConfig). See the
	// 'terminationPatterns' schema-guide section. We don't block -- some
	// workflows legitimately fail-fast on a bad-input conditional branch --
	// but we want the agent to see this advisory at compose time, not
	// discover it after deploying a workflow whose executions all show up
	// as failures in metrics.
	refType := map[string]string{}
	for _, t := range spec.Tasks {
		if r := localRef(t, ""); r != "" {
			if tt, _ := t["type"].(string); tt != "" {
				refType[r] = tt
			}
		}
	}
	for _, n := range spec.ExtraNodes {
		if r := localRef(n, ""); r != "" {
			if tt, _ := n["type"].(string); tt != "" {
				refType[r] = tt
			}
		}
	}
	type advise struct{ srcRef, tgtRef, srcType string }
	advisories := []advise{}
	for _, e := range spec.Edges {
		// Mirror the canonical edge normalizer (assembleWorkflowBody at the
		// bottom of this file): specs may use `from`/`to` as shortcuts for
		// `sourceNodeId`/`targetNodeId`. The advisory runs in the preflight
		// pass BEFORE normalization, so without this fallback every edge
		// authored with the documented shortcut form is invisible and the
		// advisory finds nothing.
		src, _ := e["sourceNodeId"].(string)
		if src == "" {
			src, _ = e["from"].(string)
		}
		tgt, _ := e["targetNodeId"].(string)
		if tgt == "" {
			tgt, _ = e["to"].(string)
		}
		if src == "" || tgt == "" {
			continue
		}
		st := refType[src]
		tt := refType[tgt]
		if st == "conditional" && tt == "exception" {
			advisories = append(advisories, advise{src, tgt, st})
		}
	}
	if len(advisories) > 0 {
		fmt.Fprintf(os.Stderr,
			"# advice: spec has %d branch edge(s) from a conditional targeting an exception task.\n"+
				"# advice: exception tasks fail the workflow (isSuccess=false). For VALID decision outcomes\n"+
				"# advice: like 'reject', 'manual_review', 'declined' -- which are expected business results,\n"+
				"# advice: not errors -- prefer a separate end node per branch, each with its own\n"+
				"# advice: endConfig.decisionConfig.enabled=true so the decision is recorded via\n"+
				"# advice: /v1/executions/{id}/decisions. See 'workflows-v2 schema-guide terminationPatterns'.\n"+
				"# advice: Reserve exception tasks for genuine error paths (missing required input, upstream\n"+
				"# advice: HTTP 5xx, unrecoverable state) where the workflow truly could not complete.\n",
			len(advisories))
		for _, a := range advisories {
			fmt.Fprintf(os.Stderr, "# advice:   %s (%s) -> %s (exception)\n", a.srcRef, a.srcType, a.tgtRef)
		}
	}

	// Edge topology: every from/to must reference a known ref; reject
	// duplicate edges and self-loops (almost always bugs). Also reject
	// unknown edge keys -- the most common is 'branchName' (a natural-
	// feeling but unsupported alias for 'sourceHandle' that disappears
	// silently and leaves conditional outgoing edges with sourceHandle:
	// null, breaking the conditional at runtime).
	seenEdges := map[string]bool{}
	for i, edge := range spec.Edges {
		for k := range edge {
			if !validEdgeKeys[k] {
				hint := ""
				if k == "branchName" || k == "branch_name" || k == "branch" {
					hint = " (use 'sourceHandle' instead -- conditional branches are wired by branch_<idx> or 'branch-else')"
				} else if k == "fromHandle" {
					hint = " (did you mean 'sourceHandle'?)"
				} else if k == "toHandle" {
					hint = " (did you mean 'targetHandle'?)"
				}
				return fmt.Errorf("edges[%d]: unknown key %q%s. Valid keys: from, to, sourceNodeId, targetNodeId, sourceHandle, targetHandle, label, id.", i, k, hint)
			}
		}
		from, _ := edge["from"].(string)
		to, _ := edge["to"].(string)
		// Some specs use sourceNodeId/targetNodeId directly with explicit
		// aliases; if those are present and from/to are absent, fall back.
		if from == "" {
			from, _ = edge["sourceNodeId"].(string)
		}
		if to == "" {
			to, _ = edge["targetNodeId"].(string)
		}
		if from == "" || to == "" {
			return fmt.Errorf("edges[%d]: missing 'from'/'to' (or sourceNodeId/targetNodeId)", i)
		}
		// Refs are validated only when they look spec-local (no '-NNNNNN' suffix);
		// explicit-alias edges may target server-style aliases not in knownRefs.
		if !isServerAlias(from) && !knownRefs[from] {
			return fmt.Errorf(
				"edges[%d]: 'from'=%q is not a known ref. Known refs: %s.",
				i, from, strings.Join(sortedKeys(knownRefs), ", "),
			)
		}
		if !isServerAlias(to) && !knownRefs[to] {
			return fmt.Errorf(
				"edges[%d]: 'to'=%q is not a known ref. Known refs: %s.",
				i, to, strings.Join(sortedKeys(knownRefs), ", "),
			)
		}
		if from == to {
			return fmt.Errorf(
				"edges[%d]: self-loop on %q -- a node can't be its own source AND target. "+
					"Almost always a copy-paste bug; if it's intentional, build the cycle through an intermediate node.",
				i, from,
			)
		}
		handle, _ := edge["sourceHandle"].(string)
		key := from + "|" + handle + "->" + to
		if seenEdges[key] {
			return fmt.Errorf(
				"edges[%d]: duplicate edge %s->%s (same sourceHandle %q). "+
					"Drop the duplicate; the workflow graph already has it.",
				i, from, to, handle,
			)
		}
		seenEdges[key] = true
	}

	// Live-backend type list, fetched at most once and only when a type is
	// missing from the compiled-in mirror. Lets an older CLI accept types the
	// backend gained after this binary was built instead of hard-rejecting.
	var liveTaskTypes map[string]bool
	liveTypesFetched := false

	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		label, _ := task["label"].(string)
		taskType, _ := task["type"].(string)
		if label == "" || taskType == "" {
			return fmt.Errorf("node ref=%q: label and type are required (validated before any POST)", ref)
		}
		if !validTaskTypes[taskType] {
			if !liveTypesFetched && fetchLiveTaskTypes != nil {
				liveTaskTypes = fetchLiveTaskTypes()
				liveTypesFetched = true
			}
			if liveTaskTypes[taskType] {
				fmt.Fprintf(os.Stderr,
					"# WARNING: node ref=%q: task type %q is newer than this CLI build "+
						"(absent from its compiled-in list) but IS accepted by the live backend -- proceeding. "+
						"Per-type local validation is skipped for it; update altscore-cli to get it.\n",
					ref, taskType,
				)
			} else {
				suggestion := closestTaskType(taskType)
				suggestionLine := ""
				if suggestion != "" {
					suggestionLine = fmt.Sprintf("Did you mean %q? ", suggestion)
				}
				liveNote := "The live backend type list could not be checked (offline or older backend); validated against this build's compiled-in list only. "
				if len(liveTaskTypes) > 0 {
					liveNote = fmt.Sprintf("The live backend was also consulted and does not list this type either (%d types). ", len(liveTaskTypes))
				}
				return fmt.Errorf(
					"node ref=%q: unknown task type %q. %s%s"+
						"Run 'altscore workflows-v2 schema-guide taskTypes' for the live list, or "+
						"'altscore workflows-v2 schema-guide tasks | jq \".tasks.perType | keys\"' for the active palette. "+
						"Common deprecations: 'data-store' is split into 'data-store-write'/'data-store-query'; "+
						"'pdf-report' is now part of the 'end' task's endConfig.",
					ref, taskType, suggestionLine, liveNote,
				)
			}
		}

		// Per-type required-field checks. These cover the orphan-task class
		// of bug seen in iter-3 smoke tests.
		switch taskType {
		case "http":
			if h, present := task["headers"]; present {
				if _, ok := h.(string); !ok {
					return fmt.Errorf(
						"node ref=%q: http task 'headers' must be a JSON-encoded string, "+
							"not an inline object. Wrap it: \"headers\": \"{\\\"Content-Type\\\":\\\"application/json\\\"}\". "+
							"The runtime fails with an opaque 'str type expected' error otherwise.",
						ref,
					)
				}
			}
			if u, _ := task["url"].(string); u == "" {
				return fmt.Errorf("node ref=%q: http task requires 'url'", ref)
			}
		case "webhook":
			if u, _ := task["url"].(string); u == "" {
				return fmt.Errorf("node ref=%q: webhook task requires 'url'", ref)
			}
			if s, _ := task["secret"].(string); s == "" {
				return fmt.Errorf("node ref=%q: webhook task requires 'secret'", ref)
			}
		case "comment":
			if c, _ := task["comment"].(string); c == "" {
				return fmt.Errorf(
					"node ref=%q: comment task requires a non-empty 'comment' field "+
						"(the canvas annotation body, distinct from 'label' which is the node header).",
					ref,
				)
			}
		case "data-store-write":
			cfg := asMap(task["dataStoreWriteConfig"])
			if t, _ := cfg["tableName"].(string); t == "" {
				return fmt.Errorf("node ref=%q: data-store-write task requires dataStoreWriteConfig.tableName", ref)
			}
		case "data-store-query":
			cfg := asMap(task["dataStoreQueryConfig"])
			if t, _ := cfg["tableName"].(string); t == "" {
				return fmt.Errorf("node ref=%q: data-store-query task requires dataStoreQueryConfig.tableName", ref)
			}
		case "exception":
			// Canonical wire name is 'errorMessage'. Both the BC API
			// schema (CreateTaskV2 / CreateTaskVersionV2 in
			// app/model/workflows_v2/task_schemas.py) and the runtime
			// activity (exception_activity.py) read it. Old specs that
			// ship 'message' are promoted to 'errorMessage' server-side
			// by _promote_legacy_exception_message, so the CLI normalizes
			// outgoing bodies to errorMessage-only without losing legacy
			// specs. Strip the legacy key so the body is unambiguous.
			em, _ := task["errorMessage"].(string)
			m, _ := task["message"].(string)
			if em == "" && m == "" {
				return fmt.Errorf("node ref=%q: exception task requires 'errorMessage' -- the failure message surfaced when this branch fires", ref)
			}
			if em == "" {
				task["errorMessage"] = m
			}
			delete(task, "message")
		case "child-workflow":
			eid, _ := task["executorId"].(string)
			eal, _ := task["executorAlias"].(string)
			if eid == "" && eal == "" {
				return fmt.Errorf("node ref=%q: child-workflow task requires 'executorId' or 'executorAlias'", ref)
			}
			// Server's CreateTaskV2 only declares `executorId`. The runtime
			// resolves it via find_latest_active_by_alias, so passing a
			// workflow alias in executorId works. Normalize the spec-only
			// `executorAlias` key into `executorId` so the persisted task
			// actually carries the executor pointer (otherwise the key is
			// silently dropped and the task body has no executor at all).
			if eid == "" && eal != "" {
				task["executorId"] = eal
			}
			delete(task, "executorAlias")
			// child-workflow auto-detects single vs batch from the resolved
			// type of inputExpression (list -> fan-out, dict -> single). The
			// legacy runInBatch flag and the hardcoded `input_items` context
			// key are no longer read by the runtime. Warn so old specs that
			// relied on the flag get migrated to inputExpression instead of
			// silently downgrading to a single execution.
			if rib, _ := task["runInBatch"].(bool); rib {
				if _, hasExpr := task["inputExpression"].(string); !hasExpr {
					fmt.Fprintf(os.Stderr,
						"# warning: tasks[%d] (ref=%q): child-workflow has runInBatch=true but no inputExpression. "+
							"The runtime ignores runInBatch and dispatches by the type of inputExpression "+
							"(list -> batch, dict -> single). Without inputExpression this task runs once with the full parent context. "+
							"To batch, set inputExpression to an expression that resolves to a list "+
							"(e.g. \"task_outputs.fetch.rows\").\n",
						i, ref)
				}
			}
			if fp, _ := task["failurePolicy"].(string); fp != "" && fp != "fail-fast" && fp != "best-effort" {
				return fmt.Errorf(
					"node ref=%q: child-workflow failurePolicy=%q is invalid. Must be \"fail-fast\" or \"best-effort\" (default: \"best-effort\")",
					ref, fp)
			}
		case "compute-variables":
			// compute-variables expressions live in the customVariables DSL
			// scope; the runtime evaluates them against the workflow's
			// customVariables dict, NOT the task's inputMappings. Wiring
			// inputs via inputMappings on this task type is a no-op:
			// inputMappings populates task_outputs.<this-task>.<field>
			// AFTER the expression runs (which is itself silently
			// useless since the DSL doesn't see them).
			if im, _ := task["inputMappings"].(map[string]any); len(im) > 0 {
				fmt.Fprintf(os.Stderr,
					"# warning: tasks[%d] (ref=%q): compute-variables has inputMappings but those are NOT visible to the expression DSL. "+
						"The DSL evaluates against the workflow's customVariables dict; reference values via the customVariable names directly. "+
						"Wire upstream values into customVariables via the workflow body, not into this task's inputMappings.\n",
					i, ref)
			}
		case "customer", "deal", "asset":
			// sourcesConfig entries control which fields are written/read.
			// Each entry needs at minimum a 'key' AND a 'type' (the
			// data-model type, not the schema type) -- the runtime
			// activity 'Fetch Customer fail 'type'' on missing fields.
			sources := asSlice(task["sourcesConfig"])
			for sci, sc := range sources {
				sm, ok := sc.(map[string]any)
				if !ok {
					return fmt.Errorf("node ref=%q: %s sourcesConfig[%d] must be an object", ref, taskType, sci)
				}
				if k, _ := sm["key"].(string); k == "" {
					return fmt.Errorf("node ref=%q: %s sourcesConfig[%d] missing 'key'", ref, taskType, sci)
				}
				if t, _ := sm["type"].(string); t == "" {
					return fmt.Errorf(
						"node ref=%q: %s sourcesConfig[%d] missing 'type'. "+
							"The runtime activity needs the data-model type per entry "+
							"(e.g. 'identity_key', 'borrower_field') -- a missing 'type' "+
							"surfaces at runtime as an opaque KeyError.",
						ref, taskType, sci,
					)
				} else if t == "deal_contact" || t == "deal_contacts" {
					return fmt.Errorf(
						"node ref=%q: deal_contact/deal_contacts sourcesConfig is no longer supported; "+
							"attach contacts via the inline 'contacts' field (set upsertContacts:true for identity-based upsert)",
						ref,
					)
				}
			}
			// Deal nodes can carry an inline `contacts` list (the field that
			// drives deal-<id> handles). Like relationshipsConfig.upsertContacts,
			// a sibling `upsertContacts` bool lets each row omit borrower_id and
			// resolve/create the borrower by identity instead. When OFF every
			// row needs borrower_id; when ON a row needs borrower_id OR an
			// identity (identity_value, or tax_id / identity_key shorthand) AND
			// persona. Mirrors the relationships preflight below.
			if taskType == "deal" {
				inlineContacts := asSlice(task["contacts"])
				upsertContacts, _ := task["upsertContacts"].(bool)
				for ci, contact := range inlineContacts {
					cm, ok := contact.(map[string]any)
					if !ok {
						return fmt.Errorf("node ref=%q: contacts[%d] must be an object", ref, ci)
					}
					borrowerID, _ := cm["borrower_id"].(string)
					if borrowerID != "" {
						// Existing-borrower path short-circuits; no identity needed.
						continue
					}
					if !upsertContacts {
						return fmt.Errorf(
							"node ref=%q: contacts[%d] missing borrower_id "+
								"(set upsertContacts=true to allow identity-based upsert)",
							ref, ci,
						)
					}
					// upsert path: row must carry an identity to resolve/create.
					identityField := "tax_id"
					if k, _ := cm["identity_key"].(string); k != "" {
						identityField = k
					}
					identityValue, _ := cm["identity_value"].(string)
					if identityValue == "" {
						if v, ok := cm[identityField].(string); ok {
							identityValue = v
						}
					}
					if identityValue == "" {
						return fmt.Errorf(
							"node ref=%q: contacts[%d] missing borrower_id AND missing "+
								"identity_value (or %q shorthand). Provide one so the activity "+
								"can resolve or create the borrower.",
							ref, ci, identityField,
						)
					}
					if persona, _ := cm["persona"].(string); persona == "" {
						return fmt.Errorf(
							"node ref=%q: contacts[%d] resolves by identity but is missing "+
								"persona (\"individual\" or \"business\") -- required to create "+
								"the borrower when the identity doesn't already exist.",
							ref, ci,
						)
					}
				}
			}
		case "relationships":
			// Bulk-create N borrower<->contact links in one activity.
			// borrower_id and items must each come from either inline
			// relationshipsConfig or inputMappings -- empty/missing on both
			// sides would silently create zero rows at runtime.
			cfg := asMap(task["relationshipsConfig"])
			mappings, _ := task["inputMappings"].(map[string]any)
			if mappings == nil {
				mappings = map[string]any{}
			}
			// borrower_id (the anchor borrower) is no longer required here: the
			// backend resolves it from the workflow primary borrower
			// (_primary_borrower_id, set by an upstream customer/create-borrower
			// node or a borrower_id workflow input). An inline/mapped borrower_id
			// still wins. The "needs a borrower" case is surfaced as a workflow-level
			// warning in the Hub, not a hard compose-time error.
			inlineItems := asSlice(cfg["items"])
			_, hasItemsMapping := mappings["items"]
			if len(inlineItems) == 0 && !hasItemsMapping {
				return fmt.Errorf(
					"node ref=%q: relationships task requires either "+
						"relationshipsConfig.items (inline list) or inputMappings.items "+
						"(variable). Both empty would silently create zero relationships.",
					ref,
				)
			}
			// upsertContacts lets items omit contact_id and resolve via identity.
			// When on, every item still
			// needs SOMETHING to identify the contact -- either contact_id or
			// an identity_value (or tax_id / <defaultIdentityKey> as shorthand).
			upsertContacts, _ := cfg["upsertContacts"].(bool)
			defaultIdentityKey, _ := cfg["defaultIdentityKey"].(string)
			legalRepCount := 0
			for ii, item := range inlineItems {
				im, ok := item.(map[string]any)
				if !ok {
					return fmt.Errorf("node ref=%q: relationshipsConfig.items[%d] must be an object", ref, ii)
				}
				contactID, _ := im["contact_id"].(string)
				if contactID == "" {
					if !upsertContacts {
						return fmt.Errorf(
							"node ref=%q: relationshipsConfig.items[%d] missing contact_id "+
								"(set relationshipsConfig.upsertContacts=true to allow identity-based upsert)",
							ref, ii,
						)
					}
					// upsert path: item must carry an identity to resolve.
					identityField := "tax_id"
					if k, _ := im["identity_key"].(string); k != "" {
						identityField = k
					} else if defaultIdentityKey != "" {
						identityField = defaultIdentityKey
					}
					identityValue, _ := im["identity_value"].(string)
					if identityValue == "" {
						if v, ok := im[identityField].(string); ok {
							identityValue = v
						}
					}
					if identityValue == "" {
						return fmt.Errorf(
							"node ref=%q: relationshipsConfig.items[%d] missing contact_id AND missing "+
								"identity_value (or %q shorthand). Provide one so the activity can resolve "+
								"or create the contact.",
							ref, ii, identityField,
						)
					}
					// persona presence is checked at runtime (only required if
					// identity doesn't resolve to an existing borrower).
				}
				if kind, ok := im["relationship"].(string); ok && kind != "" {
					if err := checkRelationshipKind(kind, fmt.Sprintf("node ref=%q: relationshipsConfig.items[%d]", ref, ii)); err != nil {
						return err
					}
				}
				if lr, _ := im["is_legal_representative"].(bool); lr {
					legalRepCount++
				}
			}
			if legalRepCount > 1 {
				return fmt.Errorf(
					"node ref=%q: relationshipsConfig.items has %d entries with "+
						"is_legal_representative=true. The runtime saves them sequentially and each "+
						"True flag flips all others on the same borrower to false -- only one item "+
						"may be the legal representative per batch.",
					ref, legalRepCount,
				)
			}
		}

		// Reuse the type-specific structural validator (conditional
		// branches, scorecard reference model, mapping-table entries,
		// rule-tree enums).
		body, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("node ref=%q: cannot encode for preflight: %w", ref, err)
		}
		// Structural-only: the altdata-enrichment empty-inputKeys check
		// belongs in validateTaskV2Body (used by manual tasks-v2 create),
		// not here. Compose's normalize step fills inputKeys from each
		// source's inputFields automatically; rejecting the spec at preflight
		// would block work that compose can fix on its own.
		if err := validateTaskV2BodyStructural(json.RawMessage(body)); err != nil {
			return fmt.Errorf("node ref=%q: %w", ref, err)
		}

		// inputSchema.<field>.type must be in the JSON-Schema-style enum
		// the runtime accepts. Backend rejects unknown values with a
		// misleading "permitted: 'array'" message; surface the full
		// enum here so the agent picks the right value.
		if is, ok := task["inputSchema"].(map[string]any); ok {
			for fname, fdef := range is {
				fm, _ := fdef.(map[string]any)
				if fm == nil {
					continue
				}
				t, _ := fm["type"].(string)
				if t == "" {
					continue
				}
				if err := checkInputSchemaType(t, fmt.Sprintf("node ref=%q: inputSchema.%s.type", ref, fname)); err != nil {
					return err
				}
			}
		}

		// Conditional task: every branch's condition.field must exist in
		// inputSchema, otherwise the branch silently never matches at
		// runtime. Also: inputMappings keys must match inputSchema keys --
		// stray keys are wired to nothing.
		if taskType == "conditional" {
			schemaFields := map[string]bool{}
			if is, ok := task["inputSchema"].(map[string]any); ok {
				for k := range is {
					schemaFields[k] = true
				}
			}
			if im, ok := task["inputMappings"].(map[string]any); ok {
				strays := []string{}
				for k := range im {
					if !schemaFields[k] {
						strays = append(strays, k)
					}
				}
				if len(strays) > 0 {
					sort.Strings(strays)
					return fmt.Errorf(
						"node ref=%q: conditional inputMappings has key(s) not in inputSchema: %s. "+
							"Every inputMappings key must match an inputSchema key (the schema declares the type, the mapping wires the value). "+
							"Add to inputSchema or remove from inputMappings.",
						ref, strings.Join(strays, ", "),
					)
				}
			}
			branches := asSlice(task["branches"])
			for bi, b := range branches {
				bm, _ := b.(map[string]any)
				if bm == nil {
					continue
				}
				if missing := unknownConditionFields(asMap(bm["conditions"]), schemaFields); len(missing) > 0 {
					return fmt.Errorf(
						"node ref=%q: conditional branch[%d] references field(s) not declared in inputSchema: %s. "+
							"Add them to inputSchema (with type + inputMappings) or fix the typo -- otherwise the branch silently never matches at runtime.",
						ref, bi, strings.Join(missing, ", "),
					)
				}
			}
		}

		// Mapping namespace check: every inputMappings value with a
		// dotted path must lead with a valid runtime namespace OR a
		// known spec-local ref. {{...}} template syntax bypasses the
		// dotted-path resolver, so skip it. For 'task_outputs.<X>.<rest>'
		// also validate <X> is known -- typos like 'task_outputs.producre.v'
		// pass the leading-segment check but break at runtime.
		if im, ok := task["inputMappings"].(map[string]any); ok {
			for k, v := range im {
				s, _ := v.(string)
				if s == "" {
					continue
				}
				if strings.HasPrefix(strings.TrimSpace(s), "{{") {
					continue // template-engine syntax, not a dotted path
				}
				dot := strings.Index(s, ".")
				if dot <= 0 {
					continue
				}
				head := s[:dot]
				if !reservedMappingScopes[head] && !knownRefs[head] {
					return fmt.Errorf(
						"node ref=%q: inputMappings[%q]=%q has unknown leading segment %q. "+
							"Valid namespaces: inputs, custom, system, task_outputs, task_outputs_by_type, entity. "+
							"Or use a spec-local ref (one of: %s) which compose rewrites to task_outputs.<alias>. "+
							"Without one of these the runtime resolver fails with 'Unknown variable namespace' at execution.",
						ref, k, s, head, strings.Join(sortedKeys(knownRefs), ", "),
					)
				}
				if head == "task_outputs" {
					rest := s[dot+1:]
					dot2 := strings.Index(rest, ".")
					if dot2 <= 0 {
						continue
					}
					middle := rest[:dot2]
					if knownRefs[middle] || isServerAlias(middle) {
						continue
					}
					return fmt.Errorf(
						"node ref=%q: inputMappings[%q]=%q references task_outputs.%s.* but %q "+
							"is not a known spec-local ref and doesn't look like a server-assigned alias "+
							"(slug-NNNNNN). Known refs: %s. Likely a typo of one of those.",
						ref, k, s, middle, middle,
						strings.Join(sortedKeys(knownRefs), ", "),
					)
				}
			}
		}
	}

	// Orphan task detection: every task ref must appear as an edge
	// endpoint at least once. Tasks unwired from the graph never run
	// (lint flags them post-publish, but compose should catch earlier
	// so the user doesn't waste a publish round-trip).
	connected := map[string]bool{}
	for _, edge := range spec.Edges {
		if from, _ := edge["from"].(string); from != "" {
			connected[from] = true
		} else if from, _ := edge["sourceNodeId"].(string); from != "" {
			connected[from] = true
		}
		if to, _ := edge["to"].(string); to != "" {
			connected[to] = true
		} else if to, _ := edge["targetNodeId"].(string); to != "" {
			connected[to] = true
		}
	}
	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		taskType, _ := task["type"].(string)
		// Comments are decorative -- they're allowed to be orphaned.
		if taskType == "comment" {
			continue
		}
		if !connected[ref] {
			return fmt.Errorf(
				"tasks[%d] (ref=%q, type=%q) has no incident edges -- it's unreachable. "+
					"Add at least one 'edges' entry connecting %q to the rest of the graph, "+
					"or remove the task. (Comment tasks are allowed to be orphaned; everything else isn't.)",
				i, ref, taskType, ref,
			)
		}
	}

	// DAG topology check: each task's inputMappings can only read from
	// task_outputs.<X> where X is a transitive ancestor in the edge graph.
	// Otherwise the value isn't produced when the consumer runs and
	// surfaces as a runtime KeyError. Built once across the whole spec.
	ancestors := buildAncestors(spec)
	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		im, _ := task["inputMappings"].(map[string]any)
		if im == nil {
			continue
		}
		for k, v := range im {
			s, _ := v.(string)
			if s == "" || strings.HasPrefix(strings.TrimSpace(s), "{{") {
				continue
			}
			head, middle := mappingHeadAndMiddle(s)
			if head != "task_outputs" || middle == "" {
				continue
			}
			// Only validate when the middle segment is a known spec-local
			// ref (server-style aliases reference workflows we don't have
			// the topology for).
			if !knownRefs[middle] || isServerAlias(middle) {
				continue
			}
			if middle == ref {
				return fmt.Errorf(
					"node ref=%q: inputMappings[%q]=%q references its own output -- "+
						"a task cannot consume its own task_outputs.<self>. Did you mean a different ref?",
					ref, k, s,
				)
			}
			if !ancestors[ref][middle] {
				return fmt.Errorf(
					"node ref=%q: inputMappings[%q]=%q references task_outputs.%s.* but "+
						"%q is not an ancestor of %q in the edge graph (it doesn't run before this task). "+
						"At runtime task_outputs.%s won't exist yet -- add an edge from %q to %q (directly or transitively) or remove the mapping.",
					ref, k, s, middle, middle, ref, middle, middle, ref,
				)
			}
		}
	}
	return nil
}

// mappingHeadAndMiddle parses 'task_outputs.<middle>.<rest>' or
// '<head>.<rest>' and returns the leading two dotted segments. Empty
// strings indicate not-applicable.
func mappingHeadAndMiddle(s string) (head, middle string) {
	dot := strings.Index(s, ".")
	if dot <= 0 {
		return "", ""
	}
	head = s[:dot]
	rest := s[dot+1:]
	dot2 := strings.Index(rest, ".")
	if dot2 <= 0 {
		return head, rest
	}
	return head, rest[:dot2]
}

// buildAncestors does one BFS per task ref over the edge graph and
// returns ancestors[ref] = {set of refs that can reach ref through
// edges}. Edges in the spec use ref/from-to so we don't need to wait
// for server-assigned aliases. Used by the DAG mapping check.
func buildAncestors(spec *composeSpec) map[string]map[string]bool {
	// parents[X] = direct predecessors of X
	parents := map[string]map[string]bool{}
	for _, edge := range spec.Edges {
		from, _ := edge["from"].(string)
		to, _ := edge["to"].(string)
		if from == "" {
			from, _ = edge["sourceNodeId"].(string)
		}
		if to == "" {
			to, _ = edge["targetNodeId"].(string)
		}
		if from == "" || to == "" {
			continue
		}
		if parents[to] == nil {
			parents[to] = map[string]bool{}
		}
		parents[to][from] = true
	}
	ancestors := map[string]map[string]bool{}
	var visit func(node string, set map[string]bool)
	visit = func(node string, set map[string]bool) {
		for p := range parents[node] {
			if set[p] {
				continue
			}
			set[p] = true
			visit(p, set)
		}
	}
	for _, task := range spec.Tasks {
		ref := localRef(task, "")
		if ref == "" {
			continue
		}
		set := map[string]bool{}
		visit(ref, set)
		ancestors[ref] = set
	}
	return ancestors
}

// validEdgeKeys is the whitelist for edge object keys in the compose spec.
// 'from'/'to' are spec-local conveniences; 'sourceNodeId'/'targetNodeId' are
// the canonical API names. 'sourceHandle' wires conditional
// branches by handle id; 'branchName' was a common typo that silently
// dropped and broke conditionals at runtime, so we reject it explicitly.
var validEdgeKeys = map[string]bool{
	"from":         true,
	"to":           true,
	"sourceNodeId": true,
	"targetNodeId": true,
	"sourceHandle": true,
	"targetHandle": true,
	"label":        true,
	"id":           true,
}

// validWorkflowCategories mirrors the backend's WorkflowCategory enum.
// Common confusion: CUSTOMER and DEAL are entity TYPES (config.entityType),
// not categories. Keep these distinct in error messages.
var validWorkflowCategories = map[string]bool{
	"ACTION":     true,
	"EVALUATION": true,
	"CONTACT":    true,
	"OTHER":      true,
}

// fetchLiveWorkflowCategories, when set, lazily returns the LIVE backend's
// workflow-category vocabulary so validation can accept categories the backend
// gained after this binary was built (validWorkflowCategories above is only a
// mirror of the CategoryEnum). composeWorkflowBody wires it to
// fetchServerWorkflowCategories before preflight; unit tests leave it nil,
// keeping validation fully offline. Mirrors the fetchLiveTaskTypes hook.
var fetchLiveWorkflowCategories func() map[string]bool

// Live-category list, fetched at most once per compose and only when a category
// is missing from the compiled-in mirror. liveWorkflowCategoriesFetched guards
// the at-most-once semantics even when the fetch returns nil (offline or an
// older backend without the section). Reset when the hook is wired.
var (
	liveWorkflowCategories        map[string]bool
	liveWorkflowCategoriesFetched bool
)

// fetchServerWorkflowCategories queries
// GET /v1/meta/workflows-v2-schema?section=workflowCategories, the sorted string
// list BC derives from its CategoryEnum at request time. Returns nil on any
// transport/shape error (incl. a 404 from an older backend that lacks the
// section) so callers fall back to the compiled-in mirror -- exactly the
// pre-existing behavior. Mirrors fetchServerTaskTypes.
func fetchServerWorkflowCategories(c *client.Client) map[string]bool {
	data, _, err := c.Do("GET", "borrower_central", "/v1/meta/workflows-v2-schema?section=workflowCategories", nil)
	if err != nil {
		return nil
	}
	var payload struct {
		WorkflowCategories struct {
			Values []string `json:"values"`
		} `json:"workflowCategories"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.WorkflowCategories.Values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(payload.WorkflowCategories.Values))
	for _, v := range payload.WorkflowCategories.Values {
		out[strings.ToUpper(v)] = true
	}
	return out
}

// checkWorkflowCategory validates a workflow's category, consulting the live
// backend at most once when the (upper-cased) category is absent from the
// compiled-in mirror. Empty category is always fine (the field is optional).
// Mirrors checkConditionOperator's live-fallback semantics:
//   - compiled-in-known           -> accept (fast path, no fetch)
//   - live-known (newer backend)  -> warn + accept
//   - unknown to a reachable backend -> reject, listing the live vocabulary
//   - backend unreachable (offline / older backend / no hook wired)
//     -> reject against the compiled-in list, exactly as before
func checkWorkflowCategory(category string) error {
	if category == "" {
		return nil
	}
	up := strings.ToUpper(category)
	if validWorkflowCategories[up] {
		return nil
	}
	if !liveWorkflowCategoriesFetched && fetchLiveWorkflowCategories != nil {
		liveWorkflowCategories = fetchLiveWorkflowCategories()
		liveWorkflowCategoriesFetched = true
	}
	if liveWorkflowCategories[up] {
		fmt.Fprintf(os.Stderr,
			"# WARNING: workflow.category=%q is newer than this CLI build "+
				"(absent from its compiled-in list) but IS accepted by the live backend -- proceeding. "+
				"Update altscore-cli to refresh its offline category list.\n",
			category,
		)
		return nil
	}
	if len(liveWorkflowCategories) > 0 {
		return fmt.Errorf(
			"workflow.category=%q is not a valid value. "+
				"The live backend was consulted and does not list it either (%d categories): %v. "+
				"Note: CUSTOMER and DEAL are workflow ENTITY TYPES (config.entityType), not categories.",
			category, len(liveWorkflowCategories), sortedBoolMapKeys(liveWorkflowCategories),
		)
	}
	return fmt.Errorf(
		"workflow.category=%q is not a valid value "+
			"(the live backend could not be checked -- offline or an older backend; "+
			"validated against this build's compiled-in list only). Valid: ACTION, EVALUATION, CONTACT, OTHER. "+
			"Note: CUSTOMER and DEAL are workflow ENTITY TYPES (config.entityType), not categories.",
		category,
	)
}

// validRelKinds mirrors the backend relationships-kind Literal
// (app/model/core/relationships.py). Only a mirror -- checkRelationshipKind
// consults the live backend once before rejecting, so a stale mirror can no
// longer cause a FALSE REJECTION of a valid relationship kind.
var validRelKinds = map[string]bool{
	"shareholder": true, "employee": true, "family": true,
	"other": true, "unspecified": true,
}

// fetchLiveRelationshipKinds, when set, lazily returns the LIVE backend's
// relationship-kind vocabulary. composeWorkflowBody wires it to
// fetchServerRelationshipKinds before preflight; unit tests leave it nil.
var fetchLiveRelationshipKinds func() map[string]bool

// Live relationship-kind list, fetched at most once per compose and only on the
// first miss. liveRelationshipKindsFetched guards at-most-once even when the
// fetch returns nil. Reset when the hook is wired.
var (
	liveRelationshipKinds        map[string]bool
	liveRelationshipKindsFetched bool
)

// fetchServerRelationshipKinds queries
// GET /v1/meta/workflows-v2-schema?section=relationshipKinds, the sorted string
// list BC derives from the relationships-kind Literal at request time. Returns
// nil on any transport/shape error so callers fall back to the compiled-in
// mirror. Mirrors fetchServerTaskTypes.
func fetchServerRelationshipKinds(c *client.Client) map[string]bool {
	data, _, err := c.Do("GET", "borrower_central", "/v1/meta/workflows-v2-schema?section=relationshipKinds", nil)
	if err != nil {
		return nil
	}
	var payload struct {
		RelationshipKinds struct {
			Values []string `json:"values"`
		} `json:"relationshipKinds"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.RelationshipKinds.Values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(payload.RelationshipKinds.Values))
	for _, v := range payload.RelationshipKinds.Values {
		out[v] = true
	}
	return out
}

// checkRelationshipKind validates a relationships item's kind, consulting the
// live backend at most once when the kind is absent from the compiled-in
// mirror. `path` is the caller-formatted field prefix (e.g.
// `node ref="x": relationshipsConfig.items[0]`). Mirrors
// checkConditionOperator's live-fallback semantics.
func checkRelationshipKind(kind, path string) error {
	if validRelKinds[kind] {
		return nil
	}
	if !liveRelationshipKindsFetched && fetchLiveRelationshipKinds != nil {
		liveRelationshipKinds = fetchLiveRelationshipKinds()
		liveRelationshipKindsFetched = true
	}
	if liveRelationshipKinds[kind] {
		fmt.Fprintf(os.Stderr,
			"# WARNING: %s.relationship=%q is newer than this CLI build "+
				"(absent from its compiled-in list) but IS accepted by the live backend -- proceeding. "+
				"Update altscore-cli to refresh its offline relationship-kind list.\n",
			path, kind,
		)
		return nil
	}
	if len(liveRelationshipKinds) > 0 {
		return fmt.Errorf(
			"%s.relationship=%q is not a known relationship kind. "+
				"The live backend was consulted and does not list it either (%d kinds): %v",
			path, kind, len(liveRelationshipKinds), sortedBoolMapKeys(liveRelationshipKinds),
		)
	}
	return fmt.Errorf(
		"%s.relationship=%q not in "+
			"shareholder/employee/family/other/unspecified "+
			"(the live backend could not be checked -- offline or an older backend; "+
			"validated against this build's compiled-in list only)",
		path, kind,
	)
}

// validAliasPattern matches the alias regex the backend treats as URL-safe.
// Lowercase alphanumeric with internal dashes; backend does additional
// length/uniqueness checks but at minimum aliases must match this shape so
// they round-trip through path parameters.
var validAliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// objectTypedOutputsByTaskType lists task-type → field-names whose values
// are objects or arrays at runtime. Template substitution that inlines one
// of these into an outputJson string field produces invalid JSON; BC's
// renderer silently falls back to a promoted-scope dump. Lint these so the
// agent isn't surprised when their custom envelope vanishes.
//
// Surveyed from the v2 task type Pydantic models + activity output shapes.
// Conservative -- list only fields confirmed to produce object/array values.
var objectTypedOutputsByTaskType = map[string]map[string]bool{
	"scorecard":          {"score_breakdown": true},
	"evaluate-rules":     {"alerts": true},
	"altdata-enrichment": {},
	"mapping-table":      {},
}

// outputJsonTemplateRefRegex extracts {{task_outputs.<alias>.<field>}} refs
// from an outputJson template string. The first capture group is the alias,
// the second is the immediate field name (we only check the top-level
// field; deeper paths into nested objects are typically string/scalar leaves).
var outputJsonTemplateRefRegex = regexp.MustCompile(`\{\{\s*task_outputs\.([a-zA-Z0-9_-]+)\.([a-zA-Z0-9_]+)`)

// lintOutputJsonObjectRefs walks every end task's outputJson and warns
// (stderr, non-blocking) when a {{task_outputs.X.Y}} placeholder maps to
// an object/array Y. This catches the bug where a scorecard's
// `score_breakdown` (or evaluate-rules `alerts`) gets inlined into a JSON
// template and silently corrupts the rendered output.
func lintOutputJsonObjectRefs(spec *composeSpec) {
	// Build an alias -> task-type lookup across both Tasks and ExtraNodes
	// so we can resolve each {{task_outputs.X.*}} ref to a type.
	aliasToType := map[string]string{}
	for _, t := range spec.Tasks {
		alias := localRef(t, "")
		ty, _ := t["type"].(string)
		if alias != "" && ty != "" {
			aliasToType[alias] = ty
		}
	}
	for _, t := range spec.ExtraNodes {
		alias := localRef(t, "")
		ty, _ := t["type"].(string)
		if alias != "" && ty != "" {
			aliasToType[alias] = ty
		}
	}

	for _, t := range spec.Tasks {
		ty, _ := t["type"].(string)
		if ty != "end" {
			continue
		}
		endCfg, _ := t["endConfig"].(map[string]any)
		if endCfg == nil {
			continue
		}
		oj, _ := endCfg["outputJson"].(string)
		if oj == "" {
			continue
		}
		for _, m := range outputJsonTemplateRefRegex.FindAllStringSubmatch(oj, -1) {
			refAlias, field := m[1], m[2]
			refType := aliasToType[refAlias]
			objectFields := objectTypedOutputsByTaskType[refType]
			if objectFields == nil || !objectFields[field] {
				continue
			}
			endRef := localRef(t, "<unnamed-end>")
			fmt.Fprintf(os.Stderr,
				"# warning: end task %q outputJson references {{task_outputs.%s.%s}} -- "+
					"that field is an %s output and substituting it inline produces invalid JSON. "+
					"The runtime template engine silently falls back to a promoted-scope dump "+
					"(your custom keys are lost). Two fixes today: (a) drop %s.%s from outputJson and "+
					"rely on the promoted dump, or (b) project the field into a scalar via an upstream "+
					"compute-variables task before the end node references it.\n",
				endRef, refAlias, field, refType, refAlias, field,
			)
		}
	}
}

// lintCanonicalEndNode warns (stderr, non-blocking) when a spec contains
// both a rule-tree task and an end task but the end node isn't wired in the
// canonical "single end node" shape: inputMapping `decision_key` pulled from
// the rule-tree, `decisionConfig.enabled=true`, and `pdfConfig.enabled=true`.
//
// The canonical pattern collapses what used to be a conditional + N parallel
// end nodes (one per outcome) into ONE end node whose `decision_key` tracks
// the rule-tree's own output -- BC's end_activity records the per-run
// decision against the execution and renders the PDF without duplicating
// logic per branch. Skipping any of these three fields is legal (some
// workflows really do want multiple ends per branch, or no PDF, or no
// decision recording), so this lint is advisory only.
func lintCanonicalEndNode(spec *composeSpec) {
	// Collect rule-tree task refs so the warning can name the upstream alias
	// the end node should pull decision_key from.
	var ruleTreeRefs []string
	for _, t := range spec.Tasks {
		if ty, _ := t["type"].(string); ty == "rule-tree" {
			if r := localRef(t, ""); r != "" {
				ruleTreeRefs = append(ruleTreeRefs, r)
			}
		}
	}
	if len(ruleTreeRefs) == 0 {
		return
	}

	for _, t := range spec.Tasks {
		ty, _ := t["type"].(string)
		if ty != "end" {
			continue
		}
		endRef := localRef(t, "<unnamed-end>")

		// 1. inputMappings.decision_key wired (from a rule-tree, ideally)
		inputMappings, _ := t["inputMappings"].(map[string]any)
		hasDecisionKeyMapping := false
		if inputMappings != nil {
			if src, ok := inputMappings["decision_key"].(string); ok && src != "" {
				hasDecisionKeyMapping = true
			}
		}

		// 2. endConfig.decisionConfig.enabled = true
		// 3. endConfig.pdfConfig.enabled = true
		endCfg, _ := t["endConfig"].(map[string]any)
		decisionEnabled := false
		pdfEnabled := false
		if endCfg != nil {
			if dc, ok := endCfg["decisionConfig"].(map[string]any); ok {
				if en, ok := dc["enabled"].(bool); ok && en {
					decisionEnabled = true
				}
			}
			if pc, ok := endCfg["pdfConfig"].(map[string]any); ok {
				if en, ok := pc["enabled"].(bool); ok && en {
					pdfEnabled = true
				}
			}
		}

		if hasDecisionKeyMapping && decisionEnabled && pdfEnabled {
			continue
		}

		var missing []string
		if !hasDecisionKeyMapping {
			missing = append(missing, "inputMappings.decision_key")
		}
		if !decisionEnabled {
			missing = append(missing, "endConfig.decisionConfig.enabled=true")
		}
		if !pdfEnabled {
			missing = append(missing, "endConfig.pdfConfig.enabled=true")
		}

		// Show the first rule-tree as the suggested source. If multiple exist
		// the agent likely knows which one matters; the message lists all so
		// they don't pick blindly.
		sourceHint := fmt.Sprintf("task_outputs.%s.decision_key", ruleTreeRefs[0])
		if len(ruleTreeRefs) > 1 {
			sourceHint = fmt.Sprintf("task_outputs.<one-of:%s>.decision_key", strings.Join(ruleTreeRefs, ","))
		}

		fmt.Fprintf(os.Stderr,
			"# warning: end task %q is not wired as a canonical single-end node "+
				"-- missing: %s. The canonical pattern collapses conditional+N-ends into ONE "+
				"end node fed directly by the rule-tree, where decision_key tracks the "+
				"rule-tree's own output. BC's end_activity then auto-records the per-run "+
				"decision (currentDecision.key) and renders the PDF, so you don't have to "+
				"duplicate outputJson/htmlSections across approve/reject/manual branches. "+
				"Wire it like:\n"+
				"#   inputMappings: { ..., \"decision_key\": %q }\n"+
				"#   endConfig.decisionConfig: { \"enabled\": true, \"decisionType\": \"final\" }\n"+
				"#   endConfig.pdfConfig: { \"enabled\": true, ... }\n"+
				"# (advisory; multiple ends + no-PDF + no-decision shapes are still legal).\n",
			endRef, strings.Join(missing, ", "), sourceHint,
		)
	}
}

// validInputSchemaTypes mirrors the SchemaTypes Pydantic discriminated union
// in borrower-central. The backend's error message lies ("permitted:
// 'array'"); the real enum is below. Used by preflight to reject typos in
// inputSchema.<field>.type before the API round-trip. Only a mirror --
// checkInputSchemaType consults the live backend once before rejecting.
var validInputSchemaTypes = map[string]bool{
	"string":  true,
	"integer": true,
	"number":  true,
	"boolean": true,
	"object":  true,
	"array":   true,
}

// fetchLiveInputSchemaTypes, when set, lazily returns the LIVE backend's
// inputSchema-type vocabulary. composeWorkflowBody wires it to
// fetchServerInputSchemaTypes before preflight; unit tests leave it nil.
var fetchLiveInputSchemaTypes func() map[string]bool

// Live inputSchema-type list, fetched at most once per compose and only on the
// first miss. liveInputSchemaTypesFetched guards at-most-once even when the
// fetch returns nil. Reset when the hook is wired.
var (
	liveInputSchemaTypes        map[string]bool
	liveInputSchemaTypesFetched bool
)

// fetchServerInputSchemaTypes queries
// GET /v1/meta/workflows-v2-schema?section=inputSchemaTypes, the sorted string
// list BC derives from the SchemaTypes discriminated union at request time.
// Returns nil on any transport/shape error so callers fall back to the
// compiled-in mirror. Mirrors fetchServerTaskTypes.
func fetchServerInputSchemaTypes(c *client.Client) map[string]bool {
	data, _, err := c.Do("GET", "borrower_central", "/v1/meta/workflows-v2-schema?section=inputSchemaTypes", nil)
	if err != nil {
		return nil
	}
	var payload struct {
		InputSchemaTypes struct {
			Values []string `json:"values"`
		} `json:"inputSchemaTypes"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.InputSchemaTypes.Values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(payload.InputSchemaTypes.Values))
	for _, v := range payload.InputSchemaTypes.Values {
		out[v] = true
	}
	return out
}

// checkInputSchemaType validates a schema field's type, consulting the live
// backend at most once when the type is absent from the compiled-in mirror.
// `path` is the caller-formatted field path (e.g. `workflow.inputVariables.x.type`
// or `node ref="y": inputSchema.z.type`), so the `=%q` in the message reads as
// `path=type`. Mirrors checkConditionOperator's live-fallback semantics.
func checkInputSchemaType(t, path string) error {
	if validInputSchemaTypes[t] {
		return nil
	}
	if !liveInputSchemaTypesFetched && fetchLiveInputSchemaTypes != nil {
		liveInputSchemaTypes = fetchLiveInputSchemaTypes()
		liveInputSchemaTypesFetched = true
	}
	if liveInputSchemaTypes[t] {
		fmt.Fprintf(os.Stderr,
			"# WARNING: %s=%q is newer than this CLI build "+
				"(absent from its compiled-in list) but IS accepted by the live backend -- proceeding. "+
				"Update altscore-cli to refresh its offline type list.\n",
			path, t,
		)
		return nil
	}
	if len(liveInputSchemaTypes) > 0 {
		return fmt.Errorf(
			"%s=%q is not a valid type. "+
				"The live backend was consulted and does not list it either (%d types): %v.",
			path, t, len(liveInputSchemaTypes), sortedBoolMapKeys(liveInputSchemaTypes),
		)
	}
	return fmt.Errorf(
		"%s=%q is not a valid type "+
			"(the live backend could not be checked -- offline or an older backend; "+
			"validated against this build's compiled-in list only). "+
			"Valid: string, integer, number, boolean, object, array.",
		path, t,
	)
}

// unknownConditionFields walks a ConditionGroup tree and returns every leaf
// 'field' value that isn't in the schemaFields set. Used to validate that
// conditional branches only reference fields the task declares -- otherwise
// the branch silently never matches at runtime.
func unknownConditionFields(group map[string]any, schemaFields map[string]bool) []string {
	if len(group) == 0 || len(schemaFields) == 0 {
		return nil
	}
	var missing []string
	items := asSlice(group["items"])
	for _, it := range items {
		im, _ := it.(map[string]any)
		if im == nil {
			continue
		}
		// Nested ConditionGroup
		if _, isGroup := im["operator"]; isGroup {
			if _, hasItems := im["items"]; hasItems {
				missing = append(missing, unknownConditionFields(im, schemaFields)...)
				continue
			}
		}
		// Leaf ConditionItem -- skip when valueType=variable (those
		// reference RHS fields that may live on a different scope).
		field, _ := im["field"].(string)
		if field == "" {
			continue
		}
		if !schemaFields[field] {
			missing = append(missing, field)
		}
	}
	return missing
}

// isServerAlias reports whether s looks like a server-assigned task alias --
// the trailing 6-hex-after-dash pattern produced by
// borrower-central/app/utils/alias_generator.py::generate_task_alias.
func isServerAlias(s string) bool {
	if len(s) < 8 {
		return false
	}
	dash := strings.LastIndex(s, "-")
	if dash < 1 || len(s)-dash-1 != 6 {
		return false
	}
	for i := dash + 1; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// sortedKeys returns the keys of m in deterministic order.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// closestTaskType returns the canonical TaskType nearest to a given typo by
// Levenshtein distance, or "" if nothing is meaningfully close.
func closestTaskType(input string) string {
	best := ""
	bestDist := -1
	cutoff := len(input) / 2
	if cutoff < 2 {
		cutoff = 2
	}
	for t := range validTaskTypes {
		d := levenshtein(input, t)
		if d <= cutoff && (bestDist == -1 || d < bestDist) {
			best, bestDist = t, d
		}
	}
	return best
}

// levenshtein computes edit distance between two strings.
func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del, ins, sub := prev[j]+1, curr[j-1]+1, prev[j-1]+cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// validateEntityTypeVsTaskTypes rejects compose specs whose task-type set
// would render as a broken palette in the Hub for the declared entityType.
// No-op if entityType is unset (the workflow gets the generic palette).
func validateEntityTypeVsTaskTypes(spec *composeSpec) error {
	cfg := spec.Config
	if cfg == nil {
		return nil
	}
	entityType, _ := cfg["entityType"].(string)
	if entityType == "" {
		return nil
	}
	var hidden map[string]bool
	switch strings.ToLower(entityType) {
	case "customer":
		hidden = customerHiddenTypes
	case "deal":
		hidden = dealHiddenTypes
	default:
		return nil
	}
	violations := []string{}
	for i, task := range spec.Tasks {
		t, _ := task["type"].(string)
		if hidden[t] {
			ref := localRef(task, fmt.Sprintf("t%d", i))
			violations = append(violations, fmt.Sprintf("node ref=%q type=%q", ref, t))
		}
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf(
		"config.entityType=%q hides these task types from the Hub palette, but the spec uses them: %s. "+
			"The workflow would compose successfully but the Hub editor couldn't render it. "+
			"Either change config.entityType, drop the task type, or remove config.entityType to get the generic palette.",
		entityType, strings.Join(violations, ", "),
	)
}
