package codex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Efforts are Codex's reasoning efforts (model_reasoning_effort); a model
// lists the ones it supports.
var Efforts = []string{"low", "medium", "high", "xhigh", "max", "ultra"}

// modelsTTL bounds how often `codex debug models` runs for the probe.
const modelsTTL = time.Hour

type modelCache struct {
	mu     sync.Mutex
	at     time.Time
	models []contracts.ModelChoice
}

var codexModels modelCache

// parseCodexModels reads `codex debug models --bundled` (JSON: models[] with slug,
// display_name, visibility, supported_reasoning_levels[].effort). Hidden
// models are left out.
func parseCodexModels(b []byte) []contracts.ModelChoice {
	var doc struct {
		Models []struct {
			Slug         string `json:"slug"`
			DisplayName  string `json:"display_name"`
			Visibility   string `json:"visibility"`
			DefaultLevel string `json:"default_reasoning_level"`
			Levels       []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	var out []contracts.ModelChoice
	for _, m := range doc.Models {
		if m.Slug == "" || (m.Visibility != "" && m.Visibility != "list") || !contracts.ValidModelName(m.Slug) {
			continue
		}
		c := contracts.ModelChoice{ID: m.Slug, Label: m.DisplayName}
		for _, l := range m.Levels {
			if contracts.ValidEffort(l.Effort) {
				c.Efforts = append(c.Efforts, l.Effort)
			}
		}
		out = append(out, c)
	}
	return out
}

// models lists the Codex models for start forms (cached; a failure keeps
// the last list).
func (a *Adapter) models(ctx context.Context, bin string) []contracts.ModelChoice {
	c := &codexModels
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= modelsTTL {
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		// --bundled: the catalog shipped with the binary. Without it Codex
		// refreshes the catalog with an authenticated request on the
		// owner's own login (which may refresh and rewrite its auth.json).
		cmd := exec.CommandContext(cctx, bin, "debug", "models", "--bundled")
		cmd.Dir = os.TempDir() // never a project directory
		out, err := cmd.Output()
		cancel()
		if ms := parseCodexModels(out); err == nil && len(ms) > 0 {
			c.models = ms
		}
		c.at = time.Now()
	}
	return append([]contracts.ModelChoice(nil), c.models...)
}
