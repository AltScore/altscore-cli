package cmd

import (
	"fmt"
	"io"
	"sort"
)

// stampEnforceTypeOnNewVariables marks a custom variable's declared `type` as binding
// for variables this apply is ADDING, and carries the live `enforceType` forward for
// variables the workflow already has.
//
// Only variables new to this workflow are stamped: many live variables carry a
// declared type that was never enforced and is wrong, and those stay inert. A live
// variable's `enforceType` is carried forward because autosave sends customVariables
// wholesale, so a spec that omits the field would otherwise strip it; carrying it
// makes a round-trip a no-op.
//
// An explicit `enforceType` in the spec always wins, including `false` -- an author who
// has said what they want is not second-guessed.
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

// liveCustomVariables pulls the customVariables map off a workflow fetched from the API.
func liveCustomVariables(existing map[string]any) map[string]any {
	if existing == nil {
		return nil
	}
	cv, _ := existing["customVariables"].(map[string]any)
	return cv
}
