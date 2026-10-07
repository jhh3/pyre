package linkcontext

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/jhh3/outfit/internal/state"
)

const preferencesSetting = "links.preferences"

type preferenceState struct {
	Disabled []string        `json:"disabled,omitempty"`
	Modes    map[string]Mode `json:"modes,omitempty"`
}

type Mode string

const (
	ModeHeavy Mode = "heavy"
	ModeLight Mode = "light"
	ModeOff   Mode = "off"
)

func ParseMode(value string) (Mode, error) {
	mode := Mode(value)
	if mode != ModeHeavy && mode != ModeLight && mode != ModeOff {
		return "", fmt.Errorf("link context mode must be heavy, light, or off")
	}
	return mode, nil
}

// Preferences controls automatic pasted-link resolution independently of the
// project workspace policy. Missing entries default on for active connections.
type Preferences struct {
	mu    sync.RWMutex
	modes map[string]Mode
}

func LoadPreferences(ctx context.Context, store *state.Store) (*Preferences, error) {
	var stored preferenceState
	if err := store.ReadSetting(ctx, preferencesSetting, &stored); err != nil && !state.IsNotFound(err) {
		return nil, err
	}
	preferences := &Preferences{modes: map[string]Mode{}}
	for name, mode := range stored.Modes {
		if name != "" && (mode == ModeHeavy || mode == ModeLight || mode == ModeOff) {
			preferences.modes[name] = mode
		}
	}
	for _, name := range stored.Disabled {
		if name != "" {
			preferences.modes[name] = ModeOff
		}
	}
	return preferences, nil
}

func (preferences *Preferences) Enabled(name string) bool {
	return preferences.Mode(name) != ModeOff
}

func (preferences *Preferences) Mode(name string) Mode {
	if preferences == nil {
		return ModeHeavy
	}
	preferences.mu.RLock()
	defer preferences.mu.RUnlock()
	if mode := preferences.modes[name]; mode != "" {
		return mode
	}
	return ModeHeavy
}

func (preferences *Preferences) SetMode(ctx context.Context, store *state.Store, name string, mode Mode) error {
	if _, err := ParseMode(string(mode)); err != nil {
		return err
	}
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	next := make(map[string]Mode, len(preferences.modes)+1)
	for connection, current := range preferences.modes {
		next[connection] = current
	}
	if mode == ModeHeavy {
		delete(next, name)
	} else {
		next[name] = mode
	}
	names := make([]string, 0, len(next))
	for connection := range next {
		names = append(names, connection)
	}
	sort.Strings(names)
	ordered := make(map[string]Mode, len(next))
	for _, connection := range names {
		ordered[connection] = next[connection]
	}
	if err := store.Setting(ctx, preferencesSetting, preferenceState{Modes: ordered}); err != nil {
		return err
	}
	preferences.modes = next
	return nil
}
