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

	stamp := func(resource, code, entityID, scope string) {
		ref := code
		if ref == "" {
			ref = entityID
		}
		if ref == "" {
			return
		}
		entity := lookupEntityRef(c, resource, code, entityID, scope)
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
				code, id := refCodeAndID(cfg, "scorecardCode", "scorecardId")
				entity := lookupEntityRef(c, "scorecards", code, id, targetAlias)
				nestedScope := entityOwnerOr(entity, targetAlias)
				stamp("scorecards", code, id, targetAlias)
				if entity != nil {
					if rules, ok := entity["rules"].([]any); ok {
						for _, rraw := range rules {
							rm, ok := rraw.(map[string]any)
							if !ok {
								continue
							}
							mt, mtID := refCodeAndID(rm, "mappingTableCode", "mappingTableId")
							stamp("mapping-tables", mt, mtID, nestedScope)
						}
					}
				}
			case "rule-tree":
				cfg, _ := t["ruleTreeConfig"].(map[string]any)
				if cfg == nil {
					continue
				}
				code, id := refCodeAndID(cfg, "ruleTreeCode", "ruleTreeId")
				entity := lookupEntityRef(c, "rule-trees", code, id, targetAlias)
				nestedScope := entityOwnerOr(entity, targetAlias)
				stamp("rule-trees", code, id, targetAlias)
				if entity != nil {
					if rules, ok := entity["rules"].([]any); ok {
						for _, rraw := range rules {
							rm, ok := rraw.(map[string]any)
							if !ok {
								continue
							}
							rc, rcID := refCodeAndID(rm, "ruleCode", "ruleId")
							stamp("evaluation-rules", rc, rcID, nestedScope)
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
					rc, rcID := refCodeAndID(rm, "ruleCode", "ruleId")
					stamp("evaluation-rules", rc, rcID, targetAlias)
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
					mt, mtID := refCodeAndID(em, "mappingTableCode", "mappingTableId")
					stamp("mapping-tables", mt, mtID, targetAlias)
				}
			}
		}
	}
	walkTasks(spec.Tasks)
	walkTasks(spec.ExtraNodes)
	return nil
}

// A reference's code and id as written; BC tries the code first, then the id.
func refCodeAndID(m map[string]any, codeKey, idKey string) (string, string) {
	code, _ := m[codeKey].(string)
	id, _ := m[idKey].(string)
	return code, id
}
