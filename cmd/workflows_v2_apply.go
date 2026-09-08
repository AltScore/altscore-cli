package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// apply takes a single agent-friendly spec and reconciles it against the
// tenant through POST /v2/workflows/apply: if no workflow shares the spec's
// alias, Borrower Central creates one; if one exists, it drafts, autosaves
// and (over an ACTIVE version) publishes in place, same workflow id and
// alias retained. Tasks are identified by (workflowAlias, specRef): an
// unchanged body is left alone, a changed one is version-bumped under the
// same alias, a new ref gets a fresh alias. After the apply, every
// referenced credit-decisioning entity (scorecards, rule-trees,
// evaluation-rules, mapping-tables, and their nested rules) is re-stamped
// to the workflow's alias so the Hub elements panel stays in sync. One
// verb, one validation pipeline, no fork-vs-update branch for the caller.
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
//  1. Parse, normalize and assemble the spec locally, posting nothing:
//     offline structural checks (preflightTasks), per-type normalization,
//     every reference kept in the canonical `task_outputs.<ref>` form.
//  2. Auto-layout the canvas: longest-path columns + barycenter row ordering,
//     the same algorithm as the Hub builder's Align button (see
//     autoLayoutNodes). Skipped by --no-layout, or when the spec pins
//     `position` on any node.
//  3. Send the flat spec ONCE (buildFlatSpecForServer + applyViaServer, see
//     workflows_v2_apply_server.go). The server resolves every ref to a task
//     alias before writing, validates the graph with the oracle publish uses,
//     writes tasks and workflow under its own edit lock, all-or-nothing, and
//     reports what happened per ref.
//
// A rejected spec writes nothing (400 / 422 carrying the server's findings); a
// mid-write failure is unwound server-side. Anything catchable locally belongs
// in preflightTasks or the normalizers, which run before the request.

func makeWfv2ApplyCmd() *cobra.Command {
	var bodyFlag string
	var dryRun bool
	var diffFlag bool
	var publish bool
	var skipRescope bool
	var allowStealOwnership bool
	var noAutoDefaults bool
	var noLayout bool
	var forceLock bool

	cmd := &cobra.Command{
		Use:     "apply",
		Aliases: []string{"compose"},
		Short:   "Declaratively create-or-update a v2 workflow from a single spec (alias: compose). Use --dry-run or --diff to preview changes before mutating.",
		Long: `Declarative reconciliation of a v2 workflow against the spec. One verb
covers both greenfield create and update-in-place. Same validation pipeline
for both paths -- specs that pass apply against a fresh tenant also pass
when re-applied against a tenant that already has the workflow. Also available
as 'compose' (alias) -- both invocations are identical. Preview changes before
mutating with --dry-run (prints the server's plan) or --diff (shows a per-
section diff against the current tenant state).

The target workflow is resolved by alias:
  - spec.alias if set, otherwise slugifyWorkflowLabel(spec.label).
  - No workflow has that alias -> create path: the server creates the
    tasks and the workflow (DRAFT unless --publish).
  - A workflow has that alias -> update path: the server drafts, autosaves
    and (over an ACTIVE version) publishes in place. Same workflow id, same
    alias, version increments, schedules / entity-scope survive. Tasks are
    matched by ref: an unchanged body is left alone, a changed one is
    version-bumped under its existing alias.
The whole apply is ONE request (POST /v2/workflows/apply) under the server's
edit lock, all-or-nothing: a rejected spec writes nothing.

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
immediately; that's the common case when applying from the CLI. An update over
an ACTIVE workflow always publishes, because the underlying assumption of
"apply" is the spec is the desired state; an update that adopts an existing
DRAFT stays DRAFT unless --publish.

Use --dry-run to get the server's plan (every ref resolved to its task
alias, created / bumped / unchanged per task, validation findings) and the
graph that would be persisted. Nothing is written and no lock is taken.
Use --diff to preview structural changes against the current tenant state:
apply fetches the existing workflow (if any) and prints a per-section diff
of metadata, tasks, edges, inputVariables, customVariables, and any entity-
scope conflicts, keyed by real task alias. No API mutations.

Spec format (see file header for full reference):
  - label, alias?, category, description, status (DRAFT default)
  - inputVariables, customVariables
  - nodes: flat list of every graph node. Apply dispatches each entry by
    type at parse time -- 'start' nodes are graph-only, everything else
    (including 'end') gets a backing task created. This is the only
    accepted shape; the legacy 'tasks[]' + 'extraNodes[]' two-bucket
    shape was removed -- apply rejects it with an inline rewrite.
  - edges: list of {from, to, sourceHandle?, label?}

End-node output (endConfig on the 'end' node):
  - outputJson: free-form JSON string -> the execution's customOutput.
  - standardOutput: the structured v1 white-box decisioning result -> the
    execution's standard 'output' field. Each section binds to ONE variable
    (a {{task_outputs.<ref>.<field>}} template); spec refs are rewritten to
    server aliases and validated on apply. Shape:
      "standardOutput": {
        "enabled": true,
        "score":    {"key":"score","label":"Score",
                     "value":"{{task_outputs.score.total}}","maxValue":1000},
        "scorecard":"{{task_outputs.score.rows}}",
        "metrics":  "{{task_outputs.score.metrics}}",
        "rules":    "{{task_outputs.tree.rules}}",
        "alerts":   "{{task_outputs.kyb.alerts}}",
        "decision": "{{task_outputs.score.decision}}",
        "data":     "{{task_outputs.score.data}}",
        "fields":   {"tax_id":"{{inputs.tax_id}}","note":"approved"}
      }
    score.value/scorecard/metrics/rules/alerts/decision/data each take a single
    variable; score.key/label/maxValue and fields values may be literals.`,
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

			// Determine target alias (predicted before any API call). A spec
			// without one is targeted by the label's slug, which makes the
			// label the workflow's identity: relabel it and apply creates a
			// second workflow instead of updating this one. Warn now; a future
			// release requires `alias`.
			targetAlias := spec.Alias
			if targetAlias == "" {
				targetAlias = slugifyWorkflowLabel(spec.Label)
				fmt.Fprintf(cmd.ErrOrStderr(),
					"# WARNING: spec has no `alias`; targeting %q, derived from the label. "+
						"Add \"alias\": %q to the spec -- a future release will require it. "+
						"Without one, changing the label creates a second workflow instead of updating this one.\n",
					targetAlias, targetAlias)
			}

			// Lookup: is there an existing workflow with this alias on the
			// tenant? Prefer ACTIVE, else fall back to the latest DRAFT so a
			// prior un-published apply is updated in place rather than forked.
			// dry-run still does the lookup so the agent sees which branch will
			// fire when they un-dry the run.
			existing, _, lookupErr := findWorkflowByAlias(c, targetAlias)
			if lookupErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "# warning: alias lookup for %q failed (%v); falling back to create path\n", targetAlias, lookupErr)
			}

			// A declared type binds the value only for variables this apply is ADDING.
			// Needs the existing workflow, so it happens here rather than inside
			// composeWorkflowBody, which never sees it.
			stampEnforceTypeOnNewVariables(spec.CustomVariables, liveCustomVariables(existing), cmd.ErrOrStderr())

			// Assemble the workflow body. composeWorkflowBody POSTs nothing: it
			// builds the graph with spec-local refs standing in for the server
			// aliases and (via capture) records each node's task body; the server
			// resolves refs to aliases and decides per task whether to create,
			// bump or leave it. In diff mode we force allowStealOwnership=true so
			// cross-owned entities don't abort assembly -- they surface in the
			// diff output as "would RE-STAMP" instead, which is the whole point
			// of a preview tool.
			composeAllowSteal := allowStealOwnership
			if diffFlag {
				composeAllowSteal = true
			}

			// Preview modes assemble tolerantly (stubbed lookups); the real
			// apply assembles strictly.
			previewAssembly := diffFlag || dryRun
			capture := newComposeCapture()
			workflow, err := composeWorkflowBody(c, &spec, previewAssembly, publish, !skipRescope, composeAllowSteal, !noAutoDefaults, !noLayout, capture)
			if err != nil {
				return err
			}

			// POST /v2/workflows/apply takes the flat spec and owns ref
			// resolution, validation, task identity, the lock and publish,
			// all-or-nothing. The CLI's job ends at assembly: a spec the server
			// path cannot express (an explicit node alias) is refused here,
			// before any request.
			flat, err := buildFlatSpecForServer(workflow, capture, targetAlias)
			if err != nil {
				return err
			}
			var publishOpt *bool
			if publish {
				yes := true
				publishOpt = &yes
			}
			res, err := applyViaServer(c, cmd, flat, serverApplyOptions{
				DryRun:    diffFlag || dryRun,
				Publish:   publishOpt,
				ForceLock: forceLock,
				ClientID:  fmt.Sprintf("apply-%d", time.Now().UnixNano()),
			})
			if err != nil {
				return err
			}
			return finishServerApply(c, cmd, &spec, res, existing, targetAlias, diffFlag, dryRun, skipRescope, allowStealOwnership)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON spec (or pipe via stdin)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview: assemble the spec, ask the server for its plan (every ref resolved to its task alias, created/bumped/unchanged per task, validation findings) and print the graph that would be persisted. Reads only; nothing is written and no lock is taken")
	cmd.Flags().BoolVar(&diffFlag, "diff", false, "preview structural changes against the current tenant state. Fetches the existing workflow (if any), obtains the server's plan for the spec, and prints a human-readable diff keyed by real task alias. No API mutations. Mutually exclusive with --dry-run and --publish")
	cmd.Flags().BoolVar(&publish, "publish", false, "publish the workflow after creation (CREATE path only; UPDATE path always publishes the draft it produced)")
	cmd.Flags().BoolVar(&skipRescope, "skip-rescope", false, "do not stamp referenced credit-decisioning entities (scorecards, rule-trees, etc.) to the workflow's alias after apply")
	cmd.Flags().BoolVar(&allowStealOwnership, "allow-steal-ownership", false, "permit apply to transfer a credit-decisioning entity's workflowAlias when it is currently owned by ANOTHER workflow. Default: refuse and instruct the spec author to clone the entity with a new code. Use only for rare workflow rename / identity migration / decommissioning scenarios")
	cmd.Flags().BoolVar(&noAutoDefaults, "no-auto-defaults", false, "disable apply's opinionated convenience defaults: (1) end-node borrower_id/billable_id wired to the single customer node's borrower_id, (2) end-node PDF generation (pdfConfig.enabled+pdfGenerationRequired default to true), (3) deal-contact identity_value back-filled from each contact's identity_key field (default tax_id). Each only fills an absent field; caller-supplied values always win -- an explicit pdfConfig.enabled=false keeps the report off")
	cmd.Flags().BoolVar(&forceLock, "force-lock", false, "take the workflow's edit lock even when a live session holds it. The server holds the lock only for the duration of the request; by default a lock held by an open Hub tab makes apply refuse, naming the holder. Forcing discards whatever that session has unsaved")
	cmd.Flags().BoolVar(&noLayout, "no-layout", false, "skip auto-layout of the canvas. By default apply positions nodes in left-to-right columns (the same algorithm as the Hub builder's Align button) so the graph opens readable; with this flag nodes ship on a single row at a 200px pitch and overlap until someone clicks Align. Auto-layout is also skipped when the spec pins `position` on any node")
	return cmd
}
