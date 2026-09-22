package cmd

import (
	"fmt"
	"io"
	"sort"
)

// Only variables new to the workflow are stamped; a live variable's enforceType is
// carried forward because autosave sends customVariables wholesale.
func stampEnforceTypeOnNewVariables(specVars map[string]any, existing map[string]any, warn io.Writer) {
	if len(specVars) == 0 {
		return
	}
	var stamped []string
	for name, raw := range specVars {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, explicit := v["enforceType"]; explicit {
			continue
		}
		if t, _ := v["type"].(string); t == "" {
			continue
		}
		if live, alreadyLive := existing[name]; alreadyLive {
			if lv, ok := live.(map[string]any); ok {
				if et, has := lv["enforceType"]; has {
					v["enforceType"] = et
				}
			}
			continue
		}
		v["enforceType"] = true
		stamped = append(stamped, name)
	}
	if len(stamped) > 0 && warn != nil {
		sort.Strings(stamped)
		fmt.Fprintf(warn, "# declared types will be ENFORCED on %d new custom variable(s): %v\n",
			len(stamped), stamped)
		fmt.Fprintf(warn, "#   (a value that cannot honour its type raises, and the variable's onError decides)\n")
	}
}

func liveCustomVariables(existing map[string]any) map[string]any {
	if existing == nil {
		return nil
	}
	cv, _ := existing["customVariables"].(map[string]any)
	return cv
}
