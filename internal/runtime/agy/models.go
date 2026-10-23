package agy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Efforts are the reasoning efforts `agy --effort` takes (agy 1.4.2 help).
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// modelCache keeps `agy models` (a network call) for ModelsTTL.
type modelCache struct {
	mu     sync.Mutex
	at     time.Time
	busy   bool // a refresh is running
	models []contracts.ModelChoice
}

// parseModels reads `agy models` output: a "Fetching available models..."
// line, then "<id>\t<label>" per model.
func parseModels(out string) []contracts.ModelChoice {
	var ms []contracts.ModelChoice
	for _, l := range strings.Split(out, "\n") {
		id, label, ok := strings.Cut(strings.TrimSpace(l), "\t")
		id, label = strings.TrimSpace(id), strings.TrimSpace(label)
		if !ok || !contracts.ValidModelName(id) {
			continue
		}
		ms = append(ms, contracts.ModelChoice{ID: id, Label: label})
	}
	return ms
}

// models returns the node's agy models as last fetched and, when the list
// is older than ModelsTTL, refreshes it in the background (`agy models` is
// a network call; the probe runs on the launch path). A failed refresh
// keeps the last list. The default (settings.json "model", a display name)
// is marked.
func (a *Adapter) models(bin string) []contracts.ModelChoice {
	c := &a.modelCache
	c.mu.Lock()
	if !c.busy && (c.at.IsZero() || a.o.Now().Sub(c.at) >= a.o.ModelsTTL) {
		c.busy = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "models")
			cmd.Dir = os.TempDir()
			out, err := cmd.Output()
			ms := parseModels(string(out))
			c.mu.Lock()
			if err == nil && len(ms) > 0 {
				c.models = ms
			}
			c.at, c.busy = a.o.Now(), false
			c.mu.Unlock()
		}()
	}
	cur := append([]contracts.ModelChoice(nil), c.models...)
	c.mu.Unlock()
	def := a.defaultModelLabel()
	for i := range cur {
		cur[i].Default = def != "" && strings.EqualFold(cur[i].Label, def)
	}
	return cur
}

// defaultModelLabel is agy's configured default model (settings.json
// "model", a display name such as "Gemini 4 Argon (Medium)").
func (a *Adapter) defaultModelLabel() string {
	b, err := os.ReadFile(filepath.Join(a.o.AgyHome, "settings.json"))
	if err != nil {
		return ""
	}
	var s struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(b, &s)
	return s.Model
}
