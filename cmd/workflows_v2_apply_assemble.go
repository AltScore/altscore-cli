package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

// The {var} shape runtime safe_format() resolves. Compose wires every input/custom variable
// a section names into the end task's inputMappings, or safe_format leaves {var} literal.
var htmlSectionVarRegex = regexp.MustCompile(`\{(\w+)\}`)

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

		// Task-output references need no wiring: end_activity promotes them to root in
		// enriched_context. Only inputs/custom vars have to be pulled into the end task.
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
		}
	}
	return built, schema, mappings
}

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

// Every default fills an ABSENT key only, so a caller value always wins. The borrower_id
// source is a SPEC-LOCAL ref, rewritten to the server alias by the task-build loop.
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

		// Granularity is the KEY, not the pdfConfig object: an author who writes
		// `pdfConfig: {"title": ...}` expressed no opinion on `enabled` and still wants a report.
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
		// "a failed render is fatal" is incoherent with a report that is switched off, so this
		// defaults only when the report is actually on.
		if enabled, _ := pdf["enabled"].(bool); enabled {
			if _, has := pdf["pdfGenerationRequired"]; !has {
				pdf["pdfGenerationRequired"] = true
			}
		}
		endCfg["pdfConfig"] = pdf
		t["endConfig"] = endCfg

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

// POSTs NOTHING: each task body is recorded on `capture` and the graph carries PLACEHOLDER
// ids the server swaps for the aliases it mints. dryRun picks PREVIEW assembly, not "do not write".
func composeWorkflowBody(c *client.Client, spec *composeSpec, dryRun bool, publish bool, autoRescopeEntities bool, allowStealOwnership bool, autoDefaults bool, autoLayout bool, capture *composeCapture) (map[string]any, error) {
	if err := validateEntityTypeVsTaskTypes(spec); err != nil {
		return nil, err
	}

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

	fetchLiveTaskTypes = func() map[string]bool { return fetchServerTaskTypes(c) }
	defer func() { fetchLiveTaskTypes = nil }()

	// Must be wired BEFORE preflightTasks: its structural pass already validates
	// conditional-branch operators, so the hook has to be live by then.
	fetchLiveConditionOperators = func() map[string]bool { return fetchServerConditionOperators(c) }
	liveConditionOperators = nil
	liveConditionOperatorsFetched = false
	defer func() { fetchLiveConditionOperators = nil }()

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

	lintOutputJsonObjectRefs(spec)

	lintCanonicalEndNode(spec)

	if len(spec.CustomVariables) > 0 {
		specNodes := make([]any, len(spec.Nodes))
		for i, n := range spec.Nodes {
			specNodes[i] = n
		}
		adviseExtractionProbes(spec.CustomVariables, specNodes)

		// Apply is where a v1 port LANDS, so this fires here and not only on a later lint.
		if f, ok := adviseOrdinalCodeVars(spec.CustomVariables); ok {
			printReadabilityFindings(os.Stderr, []readabilityFinding{f})
		}
	}

	if f, ok := adviseDiacritics(humanStringsFromSpec(spec)); ok {
		printReadabilityFindings(os.Stderr, []readabilityFinding{f})
	}

	// persona is a property of the workflow's DESIGN (a cedula flow is always "individual"), so
	// it lives on the entity-write task as a literal unless the author opted into it per run.
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

	// Runs before the task-build loop so the borrower_id mapping is a spec-local ref the loop
	// validates like any other.
	if autoDefaults {
		applyAutoEndDefaults(spec)
	}

	// Sampled BEFORE the node loops: the extraNode loop back-fills `position` onto the spec's
	// own maps, so asking afterwards always answers yes and auto-layout would never fire.
	layoutPinned := specHasPinnedPositions(spec)

	taskNodes := []map[string]any{}

	refMap := map[string]string{}

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

	// Dependency order, so every ref is in refMap before its consumer is rewritten.
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

		// (workflowAlias, specRef) is the task identity the server version-bumps on: the same ref
		// "score" is legitimately used across many workflows.
		task["specRef"] = ref
		task["workflowAlias"] = predictedAlias

		delete(task, "ref")

		// A pinned canvas position is a graph concern, not part of the task body.
		pinnedPos, hasPinnedPos := task["position"]
		delete(task, "position")

		ctx := fmt.Sprintf("node ref=%q", ref)
		// refMap is the identity map here: this validates the graph and leaves refs in the
		// canonical form the server substitutes.
		if err := rewriteTaskRefs(task, refMap, ctx); err != nil {
			return nil, err
		}

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

		placeholder := registerTask(task, ref, ctx)
		refMap[ref] = placeholder

		// These positions are only a fallback for --no-layout and pinned specs.
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

	// EVERY node needs a backing /v2/tasks record -- the Hub creates trivial ones for start and
	// end too -- so an extraNode without a taskAlias gets one minted here.
	allNodes := []map[string]any{}
	for i, n := range spec.ExtraNodes {
		ref := localRef(n, fmt.Sprintf("n%d", i))
		nodeType, _ := n["type"].(string)
		label, _ := n["label"].(string)
		if nodeType == "" || label == "" {
			return nil, fmt.Errorf("node ref=%q: type and label are required", ref)
		}

		delete(n, "ref")

		taskAlias, _ := n["taskAlias"].(string)
		taskID, _ := n["taskId"].(string)
		if taskAlias == "" && taskID == "" {
			taskBody := map[string]any{
				"label":         label,
				"type":          nodeType,
				"specRef":       ref,
				"workflowAlias": predictedAlias,
			}
			if strings.ToLower(nodeType) == "end" {
				// PDF sources are resolved at runtime by end_activity when pdfConfig.enabled is
				// true and sourcesConfig is empty, so nothing is pre-filled here.
				inSchema := map[string]any{}
				inMappings := map[string]any{}
				var pdfSections []map[string]any

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
				// Spec sugar, not part of the API node shape.
				delete(n, "htmlSections")

				if len(inSchema) > 0 {
					taskBody["inputSchema"] = inSchema
				}
				if len(inMappings) > 0 {
					taskBody["inputMappings"] = inMappings
				}
				// pdfConfig.enabled=true is what flips the PDF generator on: without it end_activity
				// sees endConfig=null and skips rendering. Caller fields pass through verbatim.
				userEndCfg, _ := n["endConfig"].(map[string]any)
				if userEndCfg == nil {
					userEndCfg = map[string]any{}
				}
				userPdfCfg, _ := userEndCfg["pdfConfig"].(map[string]any)
				if userPdfCfg == nil {
					userPdfCfg = map[string]any{}
				}
				// sourcesConfig must be [] and never nil: a Go slice that was never appended to
				// marshals as JSON null, which BC's validator rejects.
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
					"decisionConfig": userEndCfg["decisionConfig"],
					"outputJson":     "",
					"pdfConfig":      pdfDefaults,
				}
				if oj, ok := userEndCfg["outputJson"].(string); ok {
					endCfgOut["outputJson"] = oj
				}
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
				delete(n, "endConfig")

				// extraNode end tasks are built here and bypass the spec.Tasks rewrite, so an
				// outputJson template would otherwise ship with its spec-local refs intact.
				if err := rewriteRefsInTaskTemplates(taskBody, refMap); err != nil {
					return nil, fmt.Errorf("node ref=%q: %w", ref, err)
				}
				rewriteTaskOutputsRefsDeep(taskBody, refMap, residualSpecRefExcludedFields)
				if err := validateNoResidualSpecRefs(taskBody, refMap, fmt.Sprintf("node ref=%q", ref)); err != nil {
					return nil, err
				}
			}

			taskAlias = registerTask(taskBody, ref, fmt.Sprintf("node ref=%q (extra-node backing)", ref))
			n["taskAlias"] = taskAlias
			n["taskVersion"] = 1
		}

		// nodeId is the task placeholder (1:1) so edges and downstream references resolve.
		if taskAlias != "" {
			n["nodeId"] = taskAlias
			refMap[ref] = taskAlias
		} else {
			if id, _ := n["nodeId"].(string); id == "" {
				n["nodeId"] = ref
			}
			refMap[ref] = fmt.Sprint(n["nodeId"])
		}

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
		// The Hub's canvas renderer flags a terminal node off node.data.isEndNode.
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
	// The Hub renders labeled form fields from name + title + description; `label` is accepted
	// as a caller-friendly spelling of `title`.
	for key, raw := range inputVars {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := v["name"].(string); name == "" {
			v["name"] = key
		}
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
	// The compute-variables wrapper is `<expression>; return <returnValue or None>`, so a bare
	// literal expression with no returnValue is evaluated, discarded, and None comes back.
	for name, raw := range customVars {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		expression, _ := v["expression"].(string)
		returnValue, _ := v["returnValue"].(string)
		if expression == "" || returnValue != "" {
			// nothing to mirror; no `=` at all is the only shape treated as a bare
			// literal, since a multi-statement body must declare returnValue itself.
		} else if !strings.Contains(expression, "=") {
			v["returnValue"] = expression
		}

		// The dependency list is both the _task_outputs lookup key and the dict key inside the
		// expression, so the two must name the same alias.
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
	if spec.Alias != "" {
		wf["alias"] = spec.Alias
	}
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
