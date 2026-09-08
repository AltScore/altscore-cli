package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Offline structural checks apply runs before the request is sent.

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

// preflightTasks runs cheap validation across every task in the spec during
// assembly, before the apply request is sent -- fully local, except at most one
// read-only backend lookup when a task type is unknown to this build (see
// fetchLiveTaskTypes). A structural mistake caught here never reaches the
// server.
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
	// Spec-level checks: workflow alias + category + inputVariables shape.
	// These fail with opaque backend errors otherwise; surface here.
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
		// Deprecated types are refused BEFORE the validTaskTypes check and
		// never through the warn-and-proceed path below. This half of the
		// check is compiled in, so it needs no backend and holds offline.
		if deprecatedTaskTypes[taskType] {
			return deprecatedTaskTypeError(fmt.Sprintf("node ref=%q", ref), taskType)
		}
		if !validTaskTypes[taskType] {
			if !liveTypesFetched && fetchLiveTaskTypes != nil {
				liveTaskTypes = fetchLiveTaskTypes()
				liveTypesFetched = true
			}
			// The fetch unions the backend's own `deprecated` list into
			// deprecatedTaskTypes, so a type retired after this binary shipped
			// is refused too -- and refused here, not warned about below.
			if deprecatedTaskTypes[taskType] {
				return deprecatedTaskTypeError(fmt.Sprintf("node ref=%q", ref), taskType)
			}
			if liveTaskTypes[taskType] {
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
				// Backend unreachable: warn rather than hard-block, then skip
				// this type's per-type field checks (we have no schema for it).
				warnUnverifiedVocabularyValue(
					fmt.Sprintf("node ref=%q: task type", ref), taskType, "task-type")
				if s := closestTaskType(taskType); s != "" {
					fmt.Fprintf(os.Stderr, "#   (did you mean %q?)\n", s)
				}
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
		case "data-store-write":
			cfg := asMap(task["dataStoreWriteConfig"])
			if t, _ := cfg["tableName"].(string); t == "" {
				return fmt.Errorf("node ref=%q: data-store-write task requires dataStoreWriteConfig.tableName", ref)
			}
		case "data-store-query":
			// Mode-aware, mirroring the Hub plugin's own validator and the
			// runtime: _execute_sql_query reads `sql` and never looks at
			// tableName, while _execute_simple_query requires tableName.
			// Demanding tableName unconditionally rejected the only correct
			// way to author a SQL-mode node.
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
			// Worth checking here rather than leaving it to the backend: BC
			// validates documentExtractionConfig at RUN time only (a
			// half-authored node must stay saveable in the builder), so an
			// apply that ships one of these mistakes returns 201 and only
			// fails when a workflow executes it.
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
			// The document source is runtime resolvable, so it legitimately
			// arrives EITHER as a config value or as an inputMappings entry
			// (either spelling). Count both, or the recommended wiring would
			// be reported as a missing source.
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
			// Same reason as document-extraction above: BC validates
			// spreadsheetExtractionConfig at RUN time only, so an apply that
			// ships a fileless node returns 201 and only fails when a
			// workflow executes it.
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
			// A compute-variables node's inputMappings keys ARE names in that
			// node's own activity context, so a custom variable may declare one
			// as a BARE dependency and read it with inputs.get("<key>").
			// GraphWorkflow._resolve_task_variables resolves each mapping into
			// task_logic.resolvedInputs, StandardActivity.run merges those into
			// the context it hands the method (resolving any entity.* reference
			// against the database on the way), and
			// ComputeVariablesActivity._collect_dependencies tests the declared
			// dependency against that context FIRST.
			//
			// This is the ONLY way to read an entity field -- no dependency
			// namespace addresses one -- and the only way for ONE custom
			// variable to be shared by several nodes that each map a different
			// source to the same key, which is how four copied per-persona
			// expressions become one definition.
			//
			// What does NOT work is a value naming no namespace at all, and
			// that one is worth saying out loud because the check below skips
			// it (`dot <= 0` continues) and it looks deliberate on the page.
			// ScopedWorkflowContext._split_reference returns an empty root_key
			// for a dotless reference and resolve() then returns None without
			// raising, so the key never reaches resolvedInputs and the variable
			// is null on EVERY run with nothing to see. The common way to
			// produce it is the identity projection `k -> k`, which the Hub
			// writes for a dependency it cannot classify -- a typo included.
			if im, _ := task["inputMappings"].(map[string]any); len(im) > 0 {
				keys := make([]string, 0, len(im))
				for k := range im {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					v, _ := im[k].(string)
					vv := strings.TrimSpace(v)
					// `__static__::<json>` is the literal escape: no dot, and
					// `resolve` handles it BEFORE `_split_reference`, so it does
					// resolve. A per-node constant is precisely the shared-variable
					// case this check exists for -- warning on it would be a second
					// false warning in the place the first one was removed from.
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
				dealOp, _ := task["operation"].(string)
				// READ mode authors contact PICKS, not inline contacts: each pick
				// narrows the deal's contacts to one on a dealpick-<id> handle.
				// The write rules below must not run here -- a node switched from
				// write to read KEEPS its authored `contacts`, and judging them by
				// the write rules fails a perfectly valid read node (the Hub's
				// deal plugin validator had the identical hole).
				if dealOp == "read" {
					readCfg := asMap(task["readDealContactsConfig"])
					for pi, p := range asSlice(readCfg["picks"]) {
						pm, ok := p.(map[string]any)
						if !ok {
							return fmt.Errorf("node ref=%q: readDealContactsConfig.picks[%d] must be an object", ref, pi)
						}
						// A deal contact has no priority column, so take orders by
						// createdAt -- NOT the relationships node's highest/lowest.
						if take, ok := pm["take"].(string); ok && take != "" && take != "oldest" && take != "newest" {
							return fmt.Errorf(
								"node ref=%q: readDealContactsConfig.picks[%d].take=%q must be \"oldest\" or \"newest\"",
								ref, pi, take,
							)
						}
					}
					// Zero picks is warn-only in the Hub (the node exposes no
					// contact branch), not a hard compose error. role_key is a
					// tenant vocabulary, validated server-side.
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
			// Dual-mode node. READ (operation:read) pinpoints EXISTING
			// relationships via readRelationshipsConfig.picks (each pick resolves
			// one relationship on a relpick-<id> handle); WRITE (default)
			// bulk-creates N borrower<->contact links from relationshipsConfig.items.
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
				// Zero picks is warn-only in the Hub (node produces no output), not
				// a hard compose error. No inline write items to validate in read
				// mode. Fall through to the shared structural validator below.
				break
			}
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

// checkRefVariableCollisions rejects a spec where a node's ref is also the name
// of a workflow variable. The two are separate scopes at RUNTIME -- a node's
// output is `task_outputs.<ref>.*` while the variable is a bare name at the
// context root -- so the collision is not itself a runtime bug. It is a compose
// bug, because every ref rewriter in apply is an exact string match.
//
// The concrete failure is validateNoResidualSpecRefs, the safety net that flags
// any string exactly equal to a spec-local ref as a missed rewrite. Fields that
// legitimately hold a bare VARIABLE name -- mappingTableConfig.entries[].
// inputVariable, compute-variables selectedVariables[], scorecardConfig.
// totalScoreVariable, and so on -- then read as un-rewritten node refs and abort
// apply.
//
// Excluding those fields one at a time (residualSpecRefExcludedFields) is a
// losing race against the task schema. Keeping the two namespaces disjoint is
// the precondition that makes the exact-match check sound in the first place.
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
