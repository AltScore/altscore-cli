package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Mirrors the Hub palette filter (ComponentsMenu.tsx CUSTOMER_HIDDEN_TYPES /
// DEAL_HIDDEN_TYPES): a workflow using these renders as a non-editable graph.
var customerHiddenTypes = map[string]bool{
	"deal":  true,
	"asset": true,
}

var dealHiddenTypes = map[string]bool{
	"customer":         true,
	"list-of-similars": true,
}

// Fully local except at most one read-only lookup when a task type is unknown to
// this build (fetchLiveTaskTypes). Fail-fast, so nothing caught here reaches the server.
func preflightTasks(spec *composeSpec) error {
	// These fail with opaque backend errors otherwise.
	if err := checkWorkflowAlias(spec.Alias); err != nil {
		return err
	}
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

	// Collected upfront so forward references in inputMappings can be validated.
	knownRefs := map[string]bool{}
	knownAliases := map[string]bool{}
	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
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
		// ExtraNodes only ever holds start nodes; the parse-time split guarantees it.
		nodeType, _ := node["type"].(string)
		if nodeType == "start" {
			startCount++
		}
	}
	if err := checkRefVariableCollisions(spec, knownRefs); err != nil {
		return err
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
	// End nodes come through the Tasks bucket post-split: they need an endConfig.
	endInTasks := 0
	for _, t := range spec.Tasks {
		if tt, _ := t["type"].(string); tt == "end" {
			endInTasks++
		}
	}
	if endInTasks == 0 {
		// Conventional but not required: some workflows terminate via exception branches.
		fmt.Fprintln(os.Stderr,
			"# warning: compose spec has no 'end' node. Most workflows need one for the engine to know where to terminate cleanly.")
	}
	if endInTasks > 1 {
		// Checked here because the apply CREATE path never runs validateWorkflowV2Body.
		return fmt.Errorf(
			"spec has %d 'end' nodes. A workflow must have exactly ONE end node; "+
				"converge all paths (conditional branches, relationship handles) to a single end.",
			endInTasks,
		)
	}

	// Advisory only: some workflows legitimately fail-fast on a bad-input branch, but
	// a 'rejected' outcome routed to an exception makes every run read as a failure.
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
		// This pass runs BEFORE edge normalization, so the from/to shortcut is resolved here.
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
		if from == "" {
			from, _ = edge["sourceNodeId"].(string)
		}
		if to == "" {
			to, _ = edge["targetNodeId"].(string)
		}
		if from == "" || to == "" {
			return fmt.Errorf("edges[%d]: missing 'from'/'to' (or sourceNodeId/targetNodeId)", i)
		}
		// An explicit-alias edge may target a server-style alias absent from knownRefs.
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

	// Fetched at most once, so an older CLI still accepts types the backend gained later.
	var liveTaskTypes map[string]bool
	liveTypesFetched := false

	for i, task := range spec.Tasks {
		ref := localRef(task, fmt.Sprintf("t%d", i))
		label, _ := task["label"].(string)
		taskType, _ := task["type"].(string)
		if label == "" || taskType == "" {
			return fmt.Errorf("node ref=%q: label and type are required (validated before any POST)", ref)
		}
		// Settled BEFORE the validTaskTypes check: a retired type is deliberately absent
		// from validTaskTypes, so the vocabulary block below would call it unknown.
		if deprecatedTaskTypes[taskType] {
			if deprecatedTaskTypeRefused(taskType, spec.ExistingNodeTypes) {
				return deprecatedTaskTypeError(fmt.Sprintf("node ref=%q", ref), taskType)
			}
			warnDeprecatedTaskTypeCarriedForward(fmt.Sprintf("node ref=%q", ref), taskType)
		} else if !validTaskTypes[taskType] {
			if !liveTypesFetched && fetchLiveTaskTypes != nil {
				liveTaskTypes = fetchLiveTaskTypes()
				liveTypesFetched = true
			}
			// The fetch unions the backend's own deprecated list in, so a type retired after
			// this binary shipped is settled here too.
			if deprecatedTaskTypes[taskType] {
				if deprecatedTaskTypeRefused(taskType, spec.ExistingNodeTypes) {
					return deprecatedTaskTypeError(fmt.Sprintf("node ref=%q", ref), taskType)
				}
				warnDeprecatedTaskTypeCarriedForward(fmt.Sprintf("node ref=%q", ref), taskType)
			} else if liveTaskTypes[taskType] {
				fmt.Fprintf(os.Stderr,
					"# WARNING: node ref=%q: task type %q is newer than this CLI build "+
						"(absent from its compiled-in list) but IS accepted by the live backend -- proceeding. "+
						"Per-type local validation is skipped for it; update altscore-cli to get it.\n",
					ref, taskType,
				)
			} else if len(liveTaskTypes) > 0 {
				suggestion := closestTaskType(taskType)
				suggestionLine := ""
				if suggestion != "" {
					suggestionLine = fmt.Sprintf("Did you mean %q? ", suggestion)
				}
				return fmt.Errorf(
					"node ref=%q: unknown task type %q. %s"+
						"The live backend was also consulted and does not list this type either (%d types). "+
						"Run 'altscore workflows-v2 schema-guide taskTypes' for the live list, or "+
						"'altscore workflows-v2 schema-guide tasks | jq \".tasks.perType | keys\"' for the active palette.",
					ref, taskType, suggestionLine, len(liveTaskTypes),
				)
			} else {
				// Backend unreachable: warn rather than block, and skip the per-type checks.
				warnUnverifiedVocabularyValue(
					fmt.Sprintf("node ref=%q: task type", ref), taskType, "task-type")
				if s := closestTaskType(taskType); s != "" {
					fmt.Fprintf(os.Stderr, "#   (did you mean %q?)\n", s)
				}
			}
		}

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
		case "data-store-write":
			cfg := asMap(task["dataStoreWriteConfig"])
			if t, _ := cfg["tableName"].(string); t == "" {
				return fmt.Errorf("node ref=%q: data-store-write task requires dataStoreWriteConfig.tableName", ref)
			}
		case "data-store-query":
			// _execute_sql_query reads `sql` and never tableName; _execute_simple_query needs
			// tableName, so demanding it unconditionally rejects every correct SQL-mode node.
			cfg := asMap(task["dataStoreQueryConfig"])
			mode, _ := cfg["queryMode"].(string)
			if mode == "" {
				mode = "simple"
			}
			if mode == "sql" {
				if s, _ := cfg["sql"].(string); strings.TrimSpace(s) == "" {
					return fmt.Errorf("node ref=%q: data-store-query task in sql mode requires dataStoreQueryConfig.sql", ref)
				}
			} else if t, _ := cfg["tableName"].(string); t == "" {
				return fmt.Errorf("node ref=%q: data-store-query task in simple mode requires dataStoreQueryConfig.tableName", ref)
			}
		case "document-extraction":
			// BC validates documentExtractionConfig at RUN time only, so an apply that ships
			// one of these mistakes returns 201 and fails only when a workflow executes it.
			cfg := asMap(task["documentExtractionConfig"])
			if len(asMap(cfg["extractionSchema"])) == 0 {
				return fmt.Errorf(
					"node ref=%q: document-extraction task requires a non-empty "+
						"documentExtractionConfig.extractionSchema (a JSON Schema with type 'object' "+
						"and at least one entry in 'properties') -- it is the contract the provider "+
						"is asked to fill, and an empty one fails at run time, not on write",
					ref,
				)
			}
			// The source arrives either as a config value or an inputMappings entry (either
			// spelling), so both have to count.
			mappings, _ := task["inputMappings"].(map[string]any)
			sources := []string{}
			for _, field := range []string{"documentUrl", "documentBase64", "rawText"} {
				if s, _ := cfg[field].(string); s != "" {
					sources = append(sources, field)
					continue
				}
				if _, mapped := mappings[field]; mapped {
					sources = append(sources, field)
					continue
				}
				if _, mapped := mappings[camelToSnake(field)]; mapped {
					sources = append(sources, field)
				}
			}
			if len(sources) == 0 {
				return fmt.Errorf(
					"node ref=%q: document-extraction task requires exactly one document source. "+
						"Set documentUrl, documentBase64 or rawText in documentExtractionConfig, or wire "+
						"one of those keys through inputMappings to an upstream output",
					ref,
				)
			}
			if len(sources) > 1 {
				return fmt.Errorf(
					"node ref=%q: document-extraction task has %d document sources (%s) but they are "+
						"mutually exclusive. Keep one and remove the others, counting both the config "+
						"values and any inputMappings entries",
					ref, len(sources), strings.Join(sources, ", "),
				)
			}
			if p, _ := cfg["provider"].(string); p == "ocr-tools" {
				if field := firstNestedSchemaProperty(asMap(cfg["extractionSchema"])); field != "" {
					return fmt.Errorf(
						"node ref=%q: document-extraction field %q is a nested shape, which provider "+
							"'ocr-tools' cannot extract (its targets are scalars and list[string] only). "+
							"Use provider 'llm' for nested objects or arrays of objects",
						ref, field,
					)
				}
			}
		case "spreadsheet-extraction":
			// Same as document-extraction: BC validates this config at RUN time only.
			cfg := asMap(task["spreadsheetExtractionConfig"])
			mappings, _ := task["inputMappings"].(map[string]any)
			sources := []string{}
			for _, field := range []string{"fileUrl", "fileBase64"} {
				if s, _ := cfg[field].(string); s != "" {
					sources = append(sources, field)
					continue
				}
				if _, mapped := mappings[field]; mapped {
					sources = append(sources, field)
					continue
				}
				if _, mapped := mappings[camelToSnake(field)]; mapped {
					sources = append(sources, field)
				}
			}
			if len(sources) == 0 {
				return fmt.Errorf(
					"node ref=%q: spreadsheet-extraction task requires exactly one file source. "+
						"Set fileUrl or fileBase64 in spreadsheetExtractionConfig, or wire one of "+
						"those keys through inputMappings to an upstream output (an http download "+
						"node returns base64 for a binary body)",
					ref,
				)
			}
			if len(sources) > 1 {
				return fmt.Errorf(
					"node ref=%q: spreadsheet-extraction task has %d file sources (%s) but they are "+
						"mutually exclusive. Keep one and remove the other, counting both the config "+
						"values and any inputMappings entries",
					ref, len(sources), strings.Join(sources, ", "),
				)
			}
		case "exception":
			// Both the BC schema and the runtime read `errorMessage`. Strip the legacy
			// `message` key so the persisted body is unambiguous.
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
			// CreateTaskV2 only declares `executorId`, so a spec-only `executorAlias` is
			// silently dropped and the task ends up with no executor at all.
			if eid == "" && eal != "" {
				task["executorId"] = eal
			}
			delete(task, "executorAlias")
			if rib, _ := task["runInBatch"].(bool); rib {
				if _, hasExpr := task["inputExpression"].(string); !hasExpr {
					fmt.Fprintf(os.Stderr,
						"# warning: tasks[%d] (ref=%q): child-workflow has runInBatch=true but no inputExpression. "+
							"The runtime ignores runInBatch and dispatches by the type of inputExpression "+
							"(list -> batch, dict -> single). Without inputExpression this task runs once and the child receives only this node's inputMappings. "+
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
			dm, _ := task["dispatchMode"].(string)
			if dm != "" && dm != "inline" && dm != "async-batch" {
				return fmt.Errorf(
					"node ref=%q: child-workflow dispatchMode=%q is invalid. Must be \"inline\" or \"async-batch\" (default: \"inline\")",
					ref, dm)
			}
			if irp, _ := task["invalidRowPolicy"].(string); irp != "" && irp != "fail" && irp != "skip" {
				return fmt.Errorf(
					"node ref=%q: child-workflow invalidRowPolicy=%q is invalid. Must be \"fail\" or \"skip\" (default: \"fail\")",
					ref, irp)
			}
			if dm == "async-batch" {
				if _, hasExpr := task["inputExpression"].(string); !hasExpr {
					return fmt.Errorf(
						"node ref=%q: child-workflow dispatchMode=\"async-batch\" requires 'inputExpression'. "+
							"Async dispatch batches over a list; set it to an expression that resolves to one "+
							"(e.g. \"task_outputs.build-rows.items\").",
						ref)
				}
				for _, inert := range []string{"maxConcurrency", "failurePolicy"} {
					if _, has := task[inert]; has {
						fmt.Fprintf(os.Stderr,
							"# warning: tasks[%d] (ref=%q): child-workflow %s is ignored when dispatchMode is "+
								"\"async-batch\" -- the platform batch owns concurrency and per-row failure. "+
								"Drop it, or switch to dispatchMode \"inline\" if you need it.\n",
							i, ref, inert)
					}
				}
			}
			if irp, _ := task["invalidRowPolicy"].(string); irp != "" && dm != "async-batch" {
				fmt.Fprintf(os.Stderr,
					"# warning: tasks[%d] (ref=%q): child-workflow invalidRowPolicy is only read when "+
						"dispatchMode is \"async-batch\"; this node dispatches inline, which does not "+
						"validate rows at all.\n",
					i, ref)
			}
		case "compute-variables":
			// A compute-variables node's inputMappings KEYS are names in its own activity
			// context, so a custom variable may depend on one bare; only the VALUE is checked.
			if im, _ := task["inputMappings"].(map[string]any); len(im) > 0 {
				keys := make([]string, 0, len(im))
				for k := range im {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					v, _ := im[k].(string)
					vv := strings.TrimSpace(v)
					// `__static__::<json>` has no dot but does resolve: `resolve` handles it before
					// `_split_reference` ever runs.
					if v == "" || strings.Contains(v, ".") || strings.HasPrefix(vv, "{{") || strings.HasPrefix(vv, "__static__::") {
						continue
					}
					fmt.Fprintf(os.Stderr,
						"# warning: tasks[%d] (ref=%q): compute-variables inputMappings[%q]=%q names no namespace. "+
							"POST /v2/tasks rejects a mapping value with no path separator, so this fails at task "+
							"creation rather than running -- and if it ever reached the runtime it would resolve to "+
							"null on every run, leaving a dependency %q reading nothing. "+
							"Point it at entity.<root>.<group>.<key>, task_outputs.<alias>.<field>, inputs.<name>, "+
							"or __static__::<json> for a constant.\n",
						i, ref, k, v, k)
				}
			}
		case "customer", "deal", "asset":
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
			if taskType == "deal" {
				dealOp, _ := task["operation"].(string)
				// READ mode authors contact PICKS, not inline contacts: a node switched from write
				// to read KEEPS its authored `contacts`, which the write rules would reject.
				if dealOp == "read" {
					readCfg := asMap(task["readDealContactsConfig"])
					for pi, p := range asSlice(readCfg["picks"]) {
						pm, ok := p.(map[string]any)
						if !ok {
							return fmt.Errorf("node ref=%q: readDealContactsConfig.picks[%d] must be an object", ref, pi)
						}
						// A deal contact has no priority column, so take orders by createdAt.
						if take, ok := pm["take"].(string); ok && take != "" && take != "oldest" && take != "newest" {
							return fmt.Errorf(
								"node ref=%q: readDealContactsConfig.picks[%d].take=%q must be \"oldest\" or \"newest\"",
								ref, pi, take,
							)
						}
					}
					// Zero picks and role_key are deliberately unchecked (Hub warns; role_key is server-side).
					break
				}
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
			// Dual-mode: operation:read resolves picks, WRITE bulk-creates items.
			if op, _ := task["operation"].(string); op == "read" {
				readCfg := asMap(task["readRelationshipsConfig"])
				readKinds := map[string]bool{
					"shareholder": true, "employee": true, "family": true,
					"other": true, "unspecified": true,
				}
				for pi, p := range asSlice(readCfg["picks"]) {
					pm, ok := p.(map[string]any)
					if !ok {
						return fmt.Errorf("node ref=%q: readRelationshipsConfig.picks[%d] must be an object", ref, pi)
					}
					if kind, ok := pm["relationship"].(string); ok && kind != "" && !readKinds[kind] {
						return fmt.Errorf(
							"node ref=%q: readRelationshipsConfig.picks[%d].relationship=%q not in "+
								"shareholder/employee/family/other/unspecified",
							ref, pi, kind,
						)
					}
					if take, ok := pm["take"].(string); ok && take != "" && take != "highest" && take != "lowest" {
						return fmt.Errorf(
							"node ref=%q: readRelationshipsConfig.picks[%d].take=%q must be \"highest\" or \"lowest\"",
							ref, pi, take,
						)
					}
				}
				// Zero picks is warn-only in the Hub, not a hard compose error.
				break
			}
			cfg := asMap(task["relationshipsConfig"])
			mappings, _ := task["inputMappings"].(map[string]any)
			if mappings == nil {
				mappings = map[string]any{}
			}
			// borrower_id is not required here: the backend resolves the workflow primary borrower.
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
					// persona is checked at runtime, only when the identity doesn't already resolve.
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

		body, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("node ref=%q: cannot encode for preflight: %w", ref, err)
		}
		// Structural-only: normalize fills inputKeys later, so the sourcable check here
		// would block work compose can fix. ExistingNodeTypes carries the deprecation gate.
		if err := validateTaskV2BodyStructural(json.RawMessage(body), spec.ExistingNodeTypes); err != nil {
			return fmt.Errorf("node ref=%q: %w", ref, err)
		}

		// The backend rejects an unknown value with a misleading "permitted: 'array'" message.
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
							"Valid namespaces: "+reservedScopesList()+". "+
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

	// Caught here rather than by lint post-publish, which costs a publish round-trip.
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
		if !connected[ref] {
			return fmt.Errorf(
				"tasks[%d] (ref=%q, type=%q) has no incident edges -- it's unreachable. "+
					"Add at least one 'edges' entry connecting %q to the rest of the graph, "+
					"or remove the task. (Standalone annotations belong in the top-level "+
					"'notes' array, not as graph nodes.)",
				i, ref, taskType, ref,
			)
		}
	}

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
			// Server-style aliases reference graphs whose topology we don't have.
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

func buildAncestors(spec *composeSpec) map[string]map[string]bool {
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

// Not a runtime bug -- the two are separate scopes -- but apply rewrites refs by
// exact string match, so a field holding the bare variable name aborts the apply.
func checkRefVariableCollisions(spec *composeSpec, knownRefs map[string]bool) error {
	type collision struct{ name, scope string }
	var found []collision
	for _, v := range []struct {
		vars  map[string]any
		scope string
	}{
		{spec.InputVariables, "inputVariables"},
		{spec.CustomVariables, "customVariables"},
	} {
		for name := range v.vars {
			if knownRefs[name] {
				found = append(found, collision{name, v.scope})
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })
	names := make([]string, 0, len(found))
	for _, f := range found {
		names = append(names, fmt.Sprintf("%q (workflow.%s)", f.name, f.scope))
	}
	first := found[0].name
	return fmt.Errorf(
		"node ref/variable name collision: %s. A node ref and a workflow variable must not share a name. "+
			"They are different scopes at runtime (the node's output is task_outputs.%s.*, the variable is a "+
			"bare %q), but apply rewrites refs by exact string match, so any field holding the variable's bare "+
			"name -- a mapping-table entry's inputVariable, a compute node's selectedVariables, a scorecard's "+
			"totalScoreVariable -- looks like an un-rewritten node ref and is rejected before the apply request "+
			"is sent. Rename one of the two: e.g. ref %q, or a distinct variable name.",
		strings.Join(names, ", "), first, first, first+"-node",
	)
}

// Fields whose runtime value is an object or array: inlining one into an outputJson
// string makes invalid JSON, and BC silently falls back to a promoted-scope dump.
var objectTypedOutputsByTaskType = map[string]map[string]bool{
	"scorecard":          {"score_breakdown": true},
	"evaluate-rules":     {"alerts": true},
	"altdata-enrichment": {},
	"mapping-table":      {},
}

// Only the top-level field is checked; deeper paths are typically scalar leaves.
var outputJsonTemplateRefRegex = regexp.MustCompile(`\{\{\s*task_outputs\.([a-zA-Z0-9_-]+)\.([a-zA-Z0-9_]+)`)

func lintOutputJsonObjectRefs(spec *composeSpec) {
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

// Advisory only: multiple ends per branch, no PDF and no decision recording are
// all legal shapes.
func lintCanonicalEndNode(spec *composeSpec) {
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

		inputMappings, _ := t["inputMappings"].(map[string]any)
		hasDecisionKeyMapping := false
		if inputMappings != nil {
			if src, ok := inputMappings["decision_key"].(string); ok && src != "" {
				hasDecisionKeyMapping = true
			}
		}

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

// A branch referencing an undeclared field silently never matches at runtime.
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
		if _, isGroup := im["operator"]; isGroup {
			if _, hasItems := im["items"]; hasItems {
				missing = append(missing, unknownConditionFields(im, schemaFields)...)
				continue
			}
		}
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

// No-op when entityType is unset: the workflow gets the generic palette.
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
