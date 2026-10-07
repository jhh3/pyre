package tui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// tone is a semantic color class. Every toned value is always accompanied
// by a symbol and a text label, so meaning never depends on color.
type tone int

const (
	toneNone tone = iota
	toneGood
	toneWarn
	toneBad
	toneMuted
	toneAccent
)

// theme is Pyre's small design system: one accent, four semantic tones
// (green working, amber needs-you, red failed, grey stale/offline), two
// text weights (secondary and faint) and a subtle selection background.
//
// Colors are chosen per color profile and per terminal background (both
// reported by Bubble Tea at startup) so they stay legible on dark and light
// terminals: tuned values for truecolor/256-color terminals, the terminal's
// own palette in 16-color mode, attributes only (bold, faint, reverse)
// under NO_COLOR, and no styling at all when the output is not a terminal.
// Text markers (›, status symbols, [tab]) carry meaning in every mode.
type theme struct {
	profile colorprofile.Profile
	dark    bool
	plain   bool // no styling at all (not a terminal)
	mono    bool // attributes only (NO_COLOR)

	title   lipgloss.Style // primary emphasis (bold)
	bold    lipgloss.Style
	dim     lipgloss.Style // secondary text
	faint   lipgloss.Style // tertiary: rules, separators, column headers
	label   lipgloss.Style // key/value labels
	accent  lipgloss.Style
	brand   lipgloss.Style
	key     lipgloss.Style // key names in hints
	heading lipgloss.Style // section headings
	marker  lipgloss.Style // selection marker ›
	match   lipgloss.Style // fuzzy-match highlight
	tabOn   lipgloss.Style
	tabOff  lipgloss.Style
	selText lipgloss.Style // selected row's primary text
	box     lipgloss.Style // overlay borders
	term    lipgloss.Style // terminal snapshot text

	selBg string // raw SGR that starts the selection background ("" = none)

	// track is the background under usage bars' empty cells (hasTrack:
	// 256-color and truecolor only), so partial-block bars read smooth.
	track    color.Color
	hasTrack bool

	tones map[tone]lipgloss.Style
}

// pal is one color in each profile for dark and light backgrounds.
type pal struct {
	dTC, d256, dANSI string
	lTC, l256, lANSI string
}

var (
	palAccent = pal{"#7aa2f7", "111", "12", "#2f62c4", "26", "4"}
	palGood   = pal{"#8cc97c", "114", "2", "#2a7d3a", "28", "2"}
	palWarn   = pal{"#e7b45a", "179", "3", "#9a5d00", "130", "3"}
	palBad    = pal{"#ef7078", "204", "1", "#c4353d", "160", "1"}
	palMuted  = pal{"#8b919d", "246", "8", "#6b707b", "242", "8"}
	palFaint  = pal{"#4f5562", "239", "8", "#b9bcc4", "250", "7"}
	palSelBg  = pal{"#2b3141", "236", "0", "#e6ebf4", "255", "7"}
	palMatch  = pal{"#f0c674", "222", "11", "#a14a00", "130", "3"}
	palTrack  = pal{"#3b4252", "238", "0", "#d5dae3", "253", "7"}
)

func (t theme) color(p pal) color.Color {
	c := lipgloss.Complete(t.profile)
	if t.dark {
		return c(lipgloss.Color(p.dANSI), lipgloss.Color(p.d256), lipgloss.Color(p.dTC))
	}
	return c(lipgloss.Color(p.lANSI), lipgloss.Color(p.l256), lipgloss.Color(p.lTC))
}

func newTheme(profile colorprofile.Profile, dark bool) theme {
	t := theme{profile: profile, dark: dark}
	t.plain = profile <= colorprofile.NoTTY
	t.mono = profile == colorprofile.ASCII
	s := lipgloss.NewStyle
	fg := func(p pal) lipgloss.Style { return s().Foreground(t.color(p)) }
	t.title = s().Bold(true)
	t.bold = s().Bold(true)
	t.heading = s().Bold(true)
	if t.mono || profile == colorprofile.ANSI || t.plain {
		// 16-color palettes vary too much for grey text: use faint.
		t.dim = s().Faint(true)
		t.faint = s().Faint(true)
		t.label = s().Faint(true)
	} else {
		t.dim = fg(palMuted)
		t.faint = fg(palFaint)
		t.label = fg(palMuted)
	}
	t.term = t.dim
	if t.mono || t.plain {
		t.accent = s().Bold(true)
		t.brand = s().Bold(true)
		t.key = s().Bold(true)
		t.marker = s().Bold(true)
		t.match = s().Underline(true)
		t.tabOn = s().Reverse(true)
		t.tabOff = s()
		t.selText = s().Bold(true)
		t.tones = map[tone]lipgloss.Style{
			toneNone: s(), toneGood: s(), toneWarn: s().Bold(true), toneBad: s().Bold(true),
			toneMuted: s().Faint(true), toneAccent: s(),
		}
	} else {
		t.accent = fg(palAccent)
		t.brand = fg(palAccent).Bold(true)
		t.key = fg(palAccent).Bold(true)
		t.marker = fg(palAccent).Bold(true)
		t.match = fg(palMatch).Bold(true)
		t.tabOn = fg(palAccent).Bold(true)
		t.tabOff = t.dim
		t.selText = s().Bold(true)
		t.tones = map[tone]lipgloss.Style{
			toneNone:   s(),
			toneGood:   fg(palGood),
			toneWarn:   fg(palWarn),
			toneBad:    fg(palBad),
			toneMuted:  t.dim,
			toneAccent: fg(palAccent),
		}
		if profile == colorprofile.ANSI {
			t.tabOn = t.tabOn.Underline(true)
		} else {
			t.track, t.hasTrack = t.color(palTrack), true
		}
		if profile != colorprofile.ANSI || t.dark {
			// A subtle background; in 16-color mode only on dark terminals,
			// where palette "black" is usually a slightly raised grey.
			t.selBg = sgrPrefix(s().Background(t.color(palSelBg)))
		}
	}
	t.box = t.faint
	return t
}

// sgrPrefix extracts the opening escape sequence a style emits.
func sgrPrefix(st lipgloss.Style) string {
	out := st.Render("x")
	if i := strings.Index(out, "x"); i > 0 {
		return out[:i]
	}
	return ""
}
