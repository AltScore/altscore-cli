package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// Assembly: from the parsed spec to the graph and task bodies the server receives.

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
//   - pageBreak: whether to start a new PDF page before this section, which
//     also repeats the report header on it (default false -- a break is opt-in,
//     so a report reads as one continuous document unless asked otherwise)
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
		pageBreak := false
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

// applyAutoEndDefaults injects apply's opinionated end-node defaults into the
// spec in place (gated by --no-auto-defaults). For every end node it:
//   - defaults endConfig.pdfConfig.enabled and pdfGenerationRequired to true so
//     the runtime renders a report unless the author said otherwise (other
//     pdfConfig fields are preserved);
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

		// Default PDF generation on -- per KEY, and only when the key is
		// absent, exactly like the borrower_id/billable_id wiring below and
		// like --no-auto-defaults' own help text promises ("Each only fills an
		// absent field; caller-supplied values always win"). The granularity is
		// the key, not the pdfConfig object: an author who writes
		// `pdfConfig: {"title": "..."}` expressed no opinion on `enabled` and
		// still wants the report, while one who writes `{"enabled": false}` --
		// to make a smoke run side-effect-free, say -- must not get a rendered
		// report anyway.
		endCfg, _ := t["endConfig"].(map[string]any)
		if endCfg == nil {
			endCfg = map[string]any{}
		}
		pdf, _ := endCfg["pdfConfig"].(map[string]any)
		if pdf == nil {
			pdf = map[string]any{}
		}
		if _, has := pdf["enabled"]; !has {
			pdf["enabled"] = true
		}
		// pdfGenerationRequired means "a failed render is fatal", which is
		// incoherent with a report that is switched off. Default it only when
		// the report is actually on, so the default can never author a
		// self-contradictory pdfConfig. An explicit value still wins.
		if enabled, _ := pdf["enabled"].(bool); enabled {
			if _, has := pdf["pdfGenerationRequired"]; !has {
				pdf["pdfGenerationRequired"] = true
			}
		}
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

// composeWorkflowBody assembles the workflow body. It POSTs NOTHING: every task
// node's body is recorded on `capture` and the graph is built with PLACEHOLDER
// identifiers -- the spec-local ref (or an explicit `alias` on the body).
// buildFlatSpecForServer turns the result into the flat spec that
// POST /v2/workflows/apply accepts; the server swaps placeholders for the
// server-minted aliases.
//
// dryRun selects PREVIEW behavior (there is no posting here): tolerant
// normalization (offline-friendly source/entity lookups) plus a "# task body
// ..." echo of each assembled body. --dry-run / --diff pass true; the real
// apply assembly passes false for strict normalization.
//
// Reference resolution: each task/extraNode has a spec-local "ref" (taken from
// the explicit `ref` field, falling back to `alias`/`nodeId`, falling back to a
// generated `t<idx>`). Edges and inputMappings reference tasks by ref; assembly
// rewrites them to the placeholder identifier, and the server then rewrites the
// placeholder to the real alias.
//
// capture, when non-nil, collects the per-node task bodies (keyed by placeholder)
// + a placeholder->ref reverse map for readable findings.
func composeWorkflowBody(c *client.Client, spec *composeSpec, dryRun bool, publish bool, autoRescopeEntities bool, allowStealOwnership bool, autoDefaults bool, autoLayout bool, capture *composeCapture) (map[string]any, error) {
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

	// Pre-flight: validate every task's required-field shape locally, so the
	// cheap mistakes never reach the server.
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

	// Advisory: a rule-tree feeding an end node that is not in the canonical
	// single-end shape (see lintCanonicalEndNode).
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

		// Advisory: custom variables whose whole output vocabulary is a small
		// integer code (2/1/0/-1). Apply is where a v1 port LANDS, so this has
		// to fire here and not only on a later `lint` of the saved workflow.
		// It needs no server fetch, unlike the rest of the readability block.
		if f, ok := adviseOrdinalCodeVars(spec.CustomVariables); ok {
			printReadabilityFindings(os.Stderr, []readabilityFinding{f})
		}
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
	// node. Runs before the task-build loop so the borrower_id mapping is a
	// spec-local ref the loop validates like any other.
	if autoDefaults {
		applyAutoEndDefaults(spec)
	}

	// Sample author-pinned positions BEFORE the node loops run: the extraNode
	// loop back-fills `position` onto the spec's own node maps, so asking after
	// assembly always answers "yes" and auto-layout would never fire.
	layoutPinned := specHasPinnedPositions(spec)

	taskNodes := []map[string]any{}

	// refMap: spec-local reference -> placeholder identifier used in the
	// assembled graph. Assembly POSTs nothing, so the placeholder is the ref
	// (or an explicit `alias` on the body), not a server-minted alias; the
	// server maps each ref to its real alias.
	refMap := map[string]string{}

	// registerTask records a fully-assembled task body and returns the
	// placeholder identifier the graph should use for it. Assembly never POSTs:
	// the captured body is what the server receives inline on the node. In
	// preview mode (--dry-run / --diff) it also echoes that body.
	registerTask := func(body map[string]any, ref, label string) string {
		placeholder := ref
		if a, _ := body["alias"].(string); a != "" {
			placeholder = a
		}
		if dryRun {
			if snap, err := json.Marshal(body); err == nil {
				fmt.Fprintf(os.Stderr, "# task body (%s): %s\n", label, string(snap))
			}
		}
		if capture != nil {
			if snap, merr := json.Marshal(body); merr == nil {
				capture.tasks[placeholder] = snap
				capture.refByNodeID[placeholder] = ref
			}
		}
		return placeholder
	}

	// Order tasks by dependency so every ref is in refMap before its consumer
	// is rewritten; a task listed before the task it references would
	// otherwise fail the unknown-ref check.
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

		// Strip the spec-only `ref` field; it is not part of the task body.
		delete(task, "ref")

		// A pinned canvas position is a graph concern, not part of the task
		// body: lift it onto the node below.
		pinnedPos, hasPinnedPos := task["position"]
		delete(task, "position")

		// Rewrite every ref-bearing field (inputMappings, nested scorecard/
		// rule-tree maps, mapping-table entries, {{...}} templates, conditional
		// conditions) and run the residual-ref safety net. refMap is identity
		// (ref->placeholder), so this validates the graph and leaves refs in the
		// canonical form the server substitutes. Topological ordering above
		// guarantees every dependency is already in refMap, so an "unknown ref"
		// here always means a typo or a reference to a task that simply isn't in
		// spec.tasks.
		ctx := fmt.Sprintf("node ref=%q", ref)
		if err := rewriteTaskRefs(task, refMap, ctx); err != nil {
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

		// Record the body and take a placeholder identifier for the graph.
		placeholder := registerTask(task, ref, ctx)
		refMap[ref] = placeholder

		// Positions here are only a fallback for --no-layout / pinned-position
		// specs; autoLayoutNodes overwrites them below on the default path.
		node := map[string]any{
			"nodeId":      placeholder,
			"type":        taskType,
			"label":       label,
			"taskAlias":   placeholder,
			"taskVersion": 1,
			"position":    map[string]float64{"x": float64(200 * (i + 1)), "y": 0},
			"data":        map[string]any{},
		}
		if hasPinnedPos {
			node["position"] = pinnedPos
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
			// context.get() calls) have the data they expect.
			//
			// specRef + workflowAlias let the server version-bump THIS task on
			// re-apply instead of minting a fresh alias.
			taskBody := map[string]any{
				"label":         label,
				"type":          nodeType,
				"specRef":       ref,
				"workflowAlias": predictedAlias,
			}
			if strings.ToLower(nodeType) == "end" {
				// PDF sources are resolved at runtime by end_activity when
				// pdfConfig.enabled is true and sourcesConfig is empty, so
				// nothing is pre-filled here.
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
				// Same generic long-form pass and safety net as rewriteTaskRefs
				// runs for spec.Tasks. extraNode end tasks build their body
				// inline, so a pdfConfig (or future end-task) field would
				// otherwise ship verbatim.
				rewriteTaskOutputsRefsDeep(taskBody, refMap, residualSpecRefExcludedFields)
				if err := validateNoResidualSpecRefs(taskBody, refMap, fmt.Sprintf("node ref=%q", ref)); err != nil {
					return nil, err
				}
			}

			// Record the backing task body and take a placeholder identifier.
			taskAlias = registerTask(taskBody, ref, fmt.Sprintf("node ref=%q (extra-node backing)", ref))
			n["taskAlias"] = taskAlias
			n["taskVersion"] = 1
		}

		// nodeId is the task placeholder (1:1) so edges and downstream
		// references resolve cleanly.
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

	// Lay the canvas out now that nodes and edges are both resolved, with the
	// same algorithm as the Hub's Align button.
	if autoLayout {
		if layoutPinned {
			fmt.Fprintf(os.Stderr, "# Auto-layout skipped: spec pins node positions. Remove them to let the CLI lay the canvas out.\n")
		} else {
			autoLayoutNodes(allNodes, allEdges)
		}
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
		// The dependency list is the lookup key against _task_outputs AND the
		// dict key into `inputs` inside the expression, so both must name the
		// same alias. refMap is the identity map here: the call validates the
		// refs and the server performs the real substitution.
		rewriteCustomVariableRefs(v, refMap)

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
	// An explicit `alias` passes through; without one the server slugifies
	// `label`.
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
