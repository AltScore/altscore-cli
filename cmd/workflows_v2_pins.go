package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// config.entityPins (borrower-central #2402, live 2026-10-14): publishing a v2 workflow
// records, per reference its nodes make to a decisioning entity, the exact entity version
// that reference resolved to. A non-test run of that published version reads exactly those
// versions; a DRAFT carries none, a test run ignores them, and a version published before
// pins existed has none and reads the latest entities until it is republished.

type entityPin struct {
	EntityType    string   `json:"entityType"`
	Resource      string   `json:"resource,omitempty"`
	Code          string   `json:"code,omitempty"`
	Version       int      `json:"version"`
	EntityID      string   `json:"entityId"`
	WorkflowAlias string   `json:"workflowAlias,omitempty"`
	ScopeAlias    string   `json:"scopeAlias,omitempty"`
	RefCode       string   `json:"refCode,omitempty"`
	RefID         string   `json:"refId,omitempty"`
	TaskAliases   []string `json:"taskAliases,omitempty"`
}

// The CLI group that reads a pinned body: `altscore <resource> get-version <entityId> <version>`.
var pinEntityResources = map[string]string{
	"mapping_table":   "mapping-tables",
	"rule_tree":       "rule-trees",
	"scorecard":       "scorecards",
	"evaluation_rule": "evaluation-rules",
}

// BC's pin_key: a reference as the runtime makes it. An absent half is "" either way.
func (p entityPin) key() string {
	return strings.Join([]string{p.EntityType, p.ScopeAlias, p.RefCode, p.RefID}, "\x00")
}

// What a reader calls the reference: its code, else the id it was written with.
func (p entityPin) label() string {
	for _, s := range []string{p.Code, p.RefCode, p.RefID, p.EntityID} {
		if s != "" {
			return s
		}
	}
	return "?"
}

// A malformed entry is dropped, never fatal, like BC's parse_pins.
func entityPinsOf(workflow map[string]any) []entityPin {
	config, _ := workflow["config"].(map[string]any)
	raw, _ := config["entityPins"].([]any)
	pins := make([]entityPin, 0, len(raw))
	for _, entry := range raw {
		b, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		var p entityPin
		if err := json.Unmarshal(b, &p); err != nil || p.EntityType == "" || p.EntityID == "" {
			continue
		}
		p.Resource = pinEntityResources[p.EntityType]
		pins = append(pins, p)
	}
	return pins
}

// The ACTIVE version is the one live runs of the alias execute, so it is what "the pins of
// <alias>" means. GET /{alias} and /{alias}/latest both answer the highest version instead,
// the open DRAFT when there is one.
func resolveActiveWorkflowID(c clientDoer, alias string) (string, error) {
	data, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+url.PathEscape(alias)+"/versions?limit=50", nil)
	if err != nil {
		return "", fmt.Errorf("list versions of %s: %w", alias, err)
	}
	var page struct {
		Versions []struct {
			Status     string `json:"status"`
			WorkflowID string `json:"workflowId"`
			VersionID  string `json:"versionId"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		return "", fmt.Errorf("parse versions of %s: %w", alias, err)
	}
	for _, v := range page.Versions {
		if v.Status != "ACTIVE" {
			continue
		}
		if v.WorkflowID != "" {
			return v.WorkflowID, nil
		}
		if v.VersionID != "" {
			return v.VersionID, nil
		}
	}
	return "", fmt.Errorf("workflow %s has no ACTIVE version, so nothing runs on pins; a DRAFT carries none. Pass a version number to read another version ('workflows-v2 versions %s')", alias, alias)
}

func loadWorkflowVersionForPins(c clientDoer, ref, version string) (map[string]any, error) {
	var path string
	switch {
	case version != "":
		alias := ref
		if uuidPattern.MatchString(ref) {
			wf, err := fetchWorkflowDoc(c, "/v2/workflows/"+ref)
			if err != nil {
				return nil, err
			}
			alias, _ = wf["alias"].(string)
			if alias == "" {
				return nil, fmt.Errorf("workflow %s has no alias", ref)
			}
		}
		path = "/v2/workflows/" + url.PathEscape(alias) + "/" + url.PathEscape(version)
	case uuidPattern.MatchString(ref):
		path = "/v2/workflows/" + ref
	default:
		id, err := resolveActiveWorkflowID(c, ref)
		if err != nil {
			return nil, err
		}
		path = "/v2/workflows/" + id
	}
	return fetchWorkflowDoc(c, path)
}

func fetchWorkflowDoc(c clientDoer, path string) (map[string]any, error) {
	data, _, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, err
	}
	var wf map[string]any
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	return wf, nil
}

// One stderr line that says what the rows mean for THIS version.
func describePinnedVersion(stderr io.Writer, wf map[string]any, pins []entityPin) {
	alias, _ := wf["alias"].(string)
	status, _ := wf["status"].(string)
	id, _ := wf["id"].(string)
	head := fmt.Sprintf("# %s v%v %s (%s): ", orDash(alias), orZero(wf["version"]), orDash(status), id)
	switch {
	case len(pins) > 0:
		fmt.Fprintf(stderr, "%s%d entity pin(s). A non-test run of this version reads exactly these entity versions; "+
			"an entity edited since reaches it only when the workflow is published again. "+
			"Read a pinned body: altscore <resource> get-version <entityId> <version>\n", head, len(pins))
	case status == "DRAFT":
		fmt.Fprintf(stderr, "%sno entity pins: a DRAFT never carries them and reads the latest entities. Publishing pins them.\n", head)
	default:
		fmt.Fprintf(stderr, "%sno entity pins: published before pins existed (2026-10-14), or a pin failure at publish, "+
			"so its runs read the latest entities until it is published again.\n", head)
	}
}

func makeWfv2PinsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pins <alias-or-id> [version]",
		Short: "Show the entity versions a published v2 workflow version runs on (config.entityPins)",
		Long: `Prints config.entityPins of one workflow version, one compact row per reference
its nodes make to a mapping table, rule tree, scorecard or evaluation rule:
  entityType, resource, code, version, entityId, workflowAlias,
  scopeAlias, refCode, refId (the reference as written), taskAliases (the nodes
  that read it)
stderr says what the rows mean for that version.

Which version:
  <id>                  that exact version
  <alias>               the ACTIVE version: the one live runs of the alias execute
  <alias-or-id> <n>     version n ("latest" is the highest version, any status)

Publishing pins each reference to the entity version it resolves to at that
moment. From then on a non-test run of that version reads exactly the pinned
versions, so editing a mapping table, rule, tree or scorecard changes
production only through the next publish. A DRAFT carries no pins and reads
the latest entities, test runs (--test) ignore pins, and a version published
before 2026-10-14 has none and reads the latest entities until republished.

Read a pinned body with 'altscore <resource> get-version <entityId> <version>'.
'workflows-v2 diff <id-a> <id-b>' compares the pins of two versions.`,
		Example: `  altscore workflows-v2 pins my-wf
  altscore workflows-v2 pins my-wf 3
  altscore workflows-v2 pins <workflow-id> | jq -c '.[] | {resource, entityId, version}'`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			version := ""
			if len(args) == 2 {
				version = args[1]
			}
			wf, err := loadWorkflowVersionForPins(c, args[0], version)
			if err != nil {
				return err
			}
			pins := entityPinsOf(wf)
			describePinnedVersion(cmd.ErrOrStderr(), wf, pins)
			return printCompactRows(pins)
		},
	}
}

// ----- diff: the pins of two versions -----

type pinDelta struct {
	EntityType string `json:"entityType"`
	Resource   string `json:"resource,omitempty"`
	Code       string `json:"code,omitempty"`
	ScopeAlias string `json:"scopeAlias,omitempty"`
	RefCode    string `json:"refCode,omitempty"`
	RefID      string `json:"refId,omitempty"`
	EntityIDA  string `json:"entityIdA"`
	VersionA   int    `json:"versionA"`
	EntityIDB  string `json:"entityIdB"`
	VersionB   int    `json:"versionB"`
}

// Pins are compared only when both sides carry some: a DRAFT, or a version published before
// pins existed, reads the latest entities, and listing every pin as removed would say the
// opposite of what changed.
func diffEntityPins(a, b []entityPin) (onlyA, onlyB []entityPin, changed []pinDelta, unchanged int) {
	onlyA, onlyB, changed = []entityPin{}, []entityPin{}, []pinDelta{}
	if len(a) == 0 || len(b) == 0 {
		return
	}
	byKeyB := map[string]entityPin{}
	for _, p := range b {
		byKeyB[p.key()] = p
	}
	seenA := map[string]bool{}
	for _, pa := range a {
		k := pa.key()
		seenA[k] = true
		pb, ok := byKeyB[k]
		if !ok {
			onlyA = append(onlyA, pa)
			continue
		}
		if pa.EntityID == pb.EntityID && pa.Version == pb.Version {
			unchanged++
			continue
		}
		changed = append(changed, pinDelta{
			EntityType: pa.EntityType, Resource: pa.Resource, Code: pa.label(),
			ScopeAlias: pa.ScopeAlias, RefCode: pa.RefCode, RefID: pa.RefID,
			EntityIDA: pa.EntityID, VersionA: pa.Version,
			EntityIDB: pb.EntityID, VersionB: pb.Version,
		})
	}
	for _, pb := range b {
		if !seenA[pb.key()] {
			onlyB = append(onlyB, pb)
		}
	}
	sortPins(onlyA)
	sortPins(onlyB)
	sort.Slice(changed, func(i, j int) bool {
		if changed[i].EntityType != changed[j].EntityType {
			return changed[i].EntityType < changed[j].EntityType
		}
		return changed[i].Code < changed[j].Code
	})
	return
}

func sortPins(ps []entityPin) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].EntityType != ps[j].EntityType {
			return ps[i].EntityType < ps[j].EntityType
		}
		return ps[i].label() < ps[j].label()
	})
}

func renderEntityPins(buf *strings.Builder, r diffReport) {
	nA, nB := len(r.A.pins), len(r.B.pins)
	switch {
	case nA == 0 && nB == 0:
		buf.WriteString("Entity pins: neither side carries any (a DRAFT never does; a version published\n")
		buf.WriteString("before 2026-10-14 reads the latest entities until it is republished).\n\n")
		return
	case nA == 0 || nB == 0:
		tag, other, n := "A", "B", nB
		if nB == 0 {
			tag, other, n = "B", "A", nA
		}
		fmt.Fprintf(buf, "Entity pins: %s carries none and reads the latest entities (a DRAFT, or published\n", tag)
		fmt.Fprintf(buf, "before 2026-10-14); %s carries %d. Run 'workflows-v2 pins' on %s to list them.\n\n", other, n, other)
		return
	}
	if len(r.EntityPinsChanged) == 0 && len(r.EntityPinsOnlyInA) == 0 && len(r.EntityPinsOnlyInB) == 0 {
		fmt.Fprintf(buf, "Entity pins: the same %d entity versions on both sides.\n\n", r.EntityPinsUnchanged)
		return
	}
	fmt.Fprintf(buf, "Entity pins (the entity versions each side's published runs read; %d unchanged):\n", r.EntityPinsUnchanged)
	for _, d := range r.EntityPinsChanged {
		fmt.Fprintf(buf, "  ~ %-16s %-32s v%d -> v%d", d.EntityType, d.Code, d.VersionA, d.VersionB)
		if d.EntityIDA != d.EntityIDB {
			fmt.Fprintf(buf, "  (entity %s -> %s)", d.EntityIDA, d.EntityIDB)
		}
		buf.WriteString("\n")
	}
	for _, block := range []struct {
		sign string
		pins []entityPin
	}{{"-", r.EntityPinsOnlyInA}, {"+", r.EntityPinsOnlyInB}} {
		for _, p := range block.pins {
			fmt.Fprintf(buf, "  %s %-16s %-32s v%d\n", block.sign, p.EntityType, p.label(), p.Version)
		}
	}
	buf.WriteString("  Read either body: altscore <resource> get-version <entityId> <version>\n\n")
}
