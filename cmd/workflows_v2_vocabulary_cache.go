package cmd

import (
	"encoding/json"
	"net/url"
	"os"
	"time"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/config"
)

// Only a fresh entry is authoritative: rejecting a value against a list the backend
// has since extended would be a false rejection, the one failure the live lookup prevents.
const (
	vocabularyCacheFile = "workflows-v2-vocabulary.json"
	vocabularyCacheTTL  = 24 * time.Hour
)

var vocabularyNow = time.Now

type vocabularyCacheEntry struct {
	FetchedAt time.Time       `json:"fetchedAt"`
	Body      json.RawMessage `json:"body"`
}

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
