package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/AltScore/altscore-cli/internal/client"
)

// Each v2 workflow owns its credit-decisioning entities 1:1, and an entity shows only in the
// element panel of the workflow its alias names, so ownership is never stolen silently.
func reconcileEntityScopes(c *client.Client, spec *composeSpec, targetAlias string, allowStealOwnership bool, errOut io.Writer) error {
	if c == nil || targetAlias == "" {
		return nil
	}
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
		// lookupEntity caches this map, so mutating in place is what refreshes a
		// later validator or follow-on apply.
		entity["workflowAlias"] = targetAlias
	}

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
