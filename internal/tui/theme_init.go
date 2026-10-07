package tui

import (
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

// themeOverride reports a PYRE_THEME=light|dark override.
func themeOverride() (dark, forced bool) {
	switch strings.ToLower(os.Getenv("PYRE_THEME")) {
	case "light":
		return false, true
	case "dark":
		return true, true
	}
	return true, false
}

// capProfile applies NoColor: emphasis is kept, color is not.
func capProfile(p colorprofile.Profile, noColor bool) colorprofile.Profile {
	if noColor && p > colorprofile.ASCII {
		return colorprofile.ASCII
	}
	return p
}

// initialTheme guesses the profile from the output and environment and
// assumes a dark background until the terminal answers.
func initialTheme(opts Options) theme {
	dark, _ := themeOverride()
	p := colorprofile.Detect(opts.Output, os.Environ())
	return newTheme(capProfile(p, opts.NoColor), dark)
}

// onThemeMsg rebuilds the theme when Bubble Tea reports the terminal's
// color profile or background color.
func (m *Model) onThemeMsg(msg tea.Msg) {
	switch msg := msg.(type) {
	case tea.ColorProfileMsg:
		m.th = newTheme(capProfile(msg.Profile, m.opts.NoColor), m.th.dark)
	case tea.BackgroundColorMsg:
		if _, forced := themeOverride(); !forced && msg.Color != nil {
			m.th = newTheme(m.th.profile, msg.IsDark())
		}
	}
}
