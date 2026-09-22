package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
)

func findWorkflowByAlias(c *client.Client, alias string) (map[string]any, string, error) {
	active, err := queryLatestWorkflowByAliasStatus(c, alias, "ACTIVE")
	if err != nil {
		return nil, "", err
	}
	if active != nil {
		return active, "ACTIVE", nil
	}
	draft, err := queryLatestWorkflowByAliasStatus(c, alias, "DRAFT")
	if err != nil {
		return nil, "", err
	}
	if draft != nil {
		return draft, "DRAFT", nil
	}
	return nil, "", nil
}

// The set the deprecation gate diffs an incoming spec against: a retired type the target
// already holds is a carry-forward, not new authoring.
func workflowNodeTypes(workflow map[string]any) map[string]bool {
	if workflow == nil {
		return nil
	}
	out := map[string]bool{}
	for _, node := range toMapSlice(workflow["nodes"]) {
		if t, _ := node["type"].(string); t != "" {
			out[t] = true
		}
	}
	return out
}

// BC handlers have historically ignored the query filters, hence the client-side match.
func queryLatestWorkflowByAliasStatus(c *client.Client, alias, status string) (map[string]any, error) {
	if alias == "" {
		return nil, nil
	}
	q := url.Values{}
	q.Set("alias", alias)
	q.Set("status", status)
	q.Set("is-latest", "true")
	q.Set("per-page", "10")
	path := "/v2/workflows?" + q.Encode()
	data, _, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, err
	}
	var arr []map[string]any
	if jerr := json.Unmarshal(data, &arr); jerr != nil {
		var wrapped struct {
			Items []map[string]any `json:"items"`
		}
		if werr := json.Unmarshal(data, &wrapped); werr != nil {
			return nil, fmt.Errorf("parse list response: %w", jerr)
		}
		arr = wrapped.Items
	}
	matches := []map[string]any{}
	for _, w := range arr {
		wa, _ := w["alias"].(string)
		if wa == "" {
			wa, _ = w["workflowAlias"].(string)
		}
		st, _ := w["status"].(string)
		if wa == alias && st == status {
			matches = append(matches, w)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > 1 {
		best := matches[0]
		bestV := 0
		if v, ok := best["version"].(float64); ok {
			bestV = int(v)
		}
		for _, m := range matches[1:] {
			if v, ok := m["version"].(float64); ok && int(v) > bestV {
				best = m
				bestV = int(v)
			}
		}
		return best, nil
	}
	return matches[0], nil
}

// Mirrors borrower-central's app/utils/workflow_alias.py:slugify_workflow_label byte for
// byte: an entity whose workflowAlias misses the derived alias never shows in the pickers.
func slugifyWorkflowLabel(label string) string {
	s := strings.ToLower(strings.TrimSpace(foldDiacritics(label)))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 100 {
		out = out[:100]
	}
	if out == "" {
		out = "workflow"
	}
	return out
}

// Letters NFKD leaves alone (ss, ae, o-slash, d-stroke, l-stroke, thorn) are absent on
// purpose: the server does not fold them either, so they still become a hyphen.
var diacriticBase = map[rune]rune{
	'À': 'A', 'Á': 'A', 'Â': 'A', 'Ã': 'A', 'Ä': 'A', 'Å': 'A', 'Ā': 'A', 'Ă': 'A', 'Ą': 'A',
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a', 'ā': 'a', 'ă': 'a', 'ą': 'a',
	'Ç': 'C', 'Ć': 'C', 'Ĉ': 'C', 'Ċ': 'C', 'Č': 'C',
	'ç': 'c', 'ć': 'c', 'ĉ': 'c', 'ċ': 'c', 'č': 'c',
	'Ď': 'D', 'ď': 'd',
	'È': 'E', 'É': 'E', 'Ê': 'E', 'Ë': 'E', 'Ē': 'E', 'Ĕ': 'E', 'Ė': 'E', 'Ę': 'E', 'Ě': 'E',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'Ĝ': 'G', 'Ğ': 'G', 'Ġ': 'G', 'Ģ': 'G', 'ĝ': 'g', 'ğ': 'g', 'ġ': 'g', 'ģ': 'g',
	'Ĥ': 'H', 'ĥ': 'h',
	'Ì': 'I', 'Í': 'I', 'Î': 'I', 'Ï': 'I', 'Ĩ': 'I', 'Ī': 'I', 'Ĭ': 'I', 'Į': 'I', 'İ': 'I',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i', 'ĩ': 'i', 'ī': 'i', 'ĭ': 'i', 'į': 'i',
	'Ĵ': 'J', 'ĵ': 'j',
	'Ķ': 'K', 'ķ': 'k',
	'Ĺ': 'L', 'Ļ': 'L', 'Ľ': 'L', 'ĺ': 'l', 'ļ': 'l', 'ľ': 'l',
	'Ñ': 'N', 'Ń': 'N', 'Ņ': 'N', 'Ň': 'N', 'ñ': 'n', 'ń': 'n', 'ņ': 'n', 'ň': 'n',
	'Ò': 'O', 'Ó': 'O', 'Ô': 'O', 'Õ': 'O', 'Ö': 'O', 'Ō': 'O', 'Ŏ': 'O', 'Ő': 'O',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ō': 'o', 'ŏ': 'o', 'ő': 'o',
	'Ŕ': 'R', 'Ŗ': 'R', 'Ř': 'R', 'ŕ': 'r', 'ŗ': 'r', 'ř': 'r',
	'Ś': 'S', 'Ŝ': 'S', 'Ş': 'S', 'Š': 'S', 'ś': 's', 'ŝ': 's', 'ş': 's', 'š': 's',
	'Ţ': 'T', 'Ť': 'T', 'ţ': 't', 'ť': 't',
	'Ù': 'U', 'Ú': 'U', 'Û': 'U', 'Ü': 'U', 'Ũ': 'U', 'Ū': 'U', 'Ŭ': 'U', 'Ů': 'U', 'Ű': 'U', 'Ų': 'U',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u', 'ũ': 'u', 'ū': 'u', 'ŭ': 'u', 'ů': 'u', 'ű': 'u', 'ų': 'u',
	'Ŵ': 'W', 'ŵ': 'w',
	'Ý': 'Y', 'Ÿ': 'Y', 'Ŷ': 'Y', 'ý': 'y', 'ÿ': 'y', 'ŷ': 'y',
	'Ź': 'Z', 'Ż': 'Z', 'Ž': 'Z', 'ź': 'z', 'ż': 'z', 'ž': 'z',
}

// Stray combining marks (U+0300..U+036F) are dropped too, so a decomposed input folds the
// same as a composed one.
func foldDiacritics(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if base, ok := diacriticBase[r]; ok {
			b.WriteRune(base)
			continue
		}
		if r >= 0x0300 && r <= 0x036F {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
