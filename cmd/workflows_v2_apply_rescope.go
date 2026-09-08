package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/AltScore/altscore-cli/internal/client"
)

// Post-apply re-scoping of credit-decisioning entities to the workflow alias.

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
