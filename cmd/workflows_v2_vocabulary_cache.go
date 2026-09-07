package cmd

import (
	"encoding/json"
	"net/url"
	"os"
	"time"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/config"
)

// On-disk cache for the workflows-v2 vocabulary sections apply consults when a
// value is missing from its compiled-in mirror: task types, condition
// operators, workflow categories, relationship kinds and inputSchema types.
// Each is served by GET /v1/meta/workflows-v2-schema?section=<name>, a static
// derivation of the backend's enums that changes only on a backend deploy.
//
// Without the cache every apply that uses a value newer than this binary paid
// one GET per vocabulary, and an offline run could not check the value at all
// (it warned and proceeded). With it the first miss in 24 hours fetches and the
// rest read the file, so a value the backend published yesterday is validated
// strictly even when the backend is unreachable today.
//
// Only a fresh entry is authoritative. A stale one is never used as a fallback:
// rejecting a value against a list the backend has since extended would be a
// false rejection, which is the one failure the live lookup exists to prevent.
// Entries are keyed by the Borrower Central base URL the client will hit, so a
// --base-url override or another environment never reads another backend's
// vocabulary. Every I/O failure is silent and degrades to a plain GET.

const (
	vocabularyCacheFile = "workflows-v2-vocabulary.json"
	vocabularyCacheTTL  = 24 * time.Hour
)

// vocabularyNow is the clock the cache reads; tests move it past the TTL.
var vocabularyNow = time.Now

type vocabularyCacheEntry struct {
	FetchedAt time.Time       `json:"fetchedAt"`
	Body      json.RawMessage `json:"body"`
}

// fetchMetaSection returns the raw body of one schema-guide section, from the
// cache when a fresh entry exists and from the backend otherwise. Returns nil
// when the section cannot be obtained; callers fall back to their compiled-in
// mirror exactly as they did before the cache existed.
func fetchMetaSection(c *client.Client, section string) json.RawMessage {
	base, err := c.ModuleBaseURL("borrower_central")
	if err != nil {
		base = ""
	}
	key := base + "|" + section

	cache := loadVocabularyCache()
	if e, ok := cache[key]; ok && len(e.Body) > 0 && vocabularyNow().Sub(e.FetchedAt) < vocabularyCacheTTL {
		return e.Body
	}

	data, _, derr := c.Do("GET", "borrower_central", "/v1/meta/workflows-v2-schema?section="+url.QueryEscape(section), nil)
	if derr != nil || len(data) == 0 {
		return nil
	}
	cache[key] = vocabularyCacheEntry{FetchedAt: vocabularyNow(), Body: data}
	saveVocabularyCache(cache)
	return data
}

func loadVocabularyCache() map[string]vocabularyCacheEntry {
	out := map[string]vocabularyCacheEntry{}
	p, err := config.StateFilePath(vocabularyCacheFile)
	if err != nil {
		return out
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return out
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]vocabularyCacheEntry{}
	}
	return out
}

func saveVocabularyCache(cache map[string]vocabularyCacheEntry) {
	p, err := config.StateFilePath(vocabularyCacheFile)
	if err != nil {
		return
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
