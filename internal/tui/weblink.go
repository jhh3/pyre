package tui

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Links to the web app's inbox (asks.go). The ask signals are OSC 8
// hyperlinks (a terminal that supports them opens them on click, usually
// with ctrl or cmd held while the TUI has the mouse): the header's count
// to <web>/#/inbox, a project header's to #/inbox?project=<id>, a lead's
// chip to #/inbox?session=<lead id>. I (and :inbox) opens the link for
// the selection with this machine's opener, or copies it (OSC 52) when
// there is no display here, e.g. over ssh.

// DefaultWebURL is the web app's address when Options.WebURL is empty.
const DefaultWebURL = "https://pyre-example.ngrok.app"

// errNoDisplay: this machine cannot show a browser (no display, or the
// TUI runs over ssh, where the browser would open on the far machine).
var errNoDisplay = errors.New("no display")

// defaultOpenURL opens u in this machine's browser.
func defaultOpenURL(u string) error {
	if !hasDisplay(os.Getenv) {
		return errNoDisplay
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() //nolint:errcheck // reap the opener; its exit status says nothing useful
	return nil
}

// hasDisplay reports whether a browser opened here would be seen: not
// over ssh, and on Linux/BSD only with an X or Wayland display.
func hasDisplay(getenv func(string) string) bool {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return false
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	}
	return getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
}

// webInboxURL is the web inbox, filtered by key=id when key is set
// ("project", "session").
func (m *Model) webInboxURL(key, id string) string {
	u := strings.TrimRight(m.opts.WebURL, "/") + "/#/inbox"
	if key != "" && id != "" {
		u += "?" + key + "=" + url.QueryEscape(id)
	}
	return u
}

// link makes s an OSC 8 hyperlink to u (zero width: layout is unchanged).
func link(u, s string) string {
	if u == "" || s == "" {
		return s
	}
	return ansi.SetHyperlink(u) + s + ansi.ResetHyperlink()
}

// webInboxTarget is the web inbox link for the selection and what it
// shows: the selected session's lead's asks when that lead has any, else
// the selected (or shown) project's asks, else the whole inbox.
func (m *Model) webInboxTarget() (u, what string) {
	var lead, proj string
	switch m.view {
	case viewSessions, viewGrid:
		if k, ok := groupOfRow(m.sessList.sel); ok && m.view == viewSessions {
			if m.groupMode() == groupProject && k != noGroup && k != pinnedGroup {
				proj = k
			}
			break
		}
		if s, ok := m.selectedSession(); ok {
			if r, ok := m.leadSignals().root[s.Session.SessionID]; ok {
				lead = r
			}
			if s.Session.ProjectID != nil {
				proj = *s.Session.ProjectID
			}
		}
	case viewProjects:
		proj = m.projList.sel
	case viewProject, viewArtifact, viewDoc:
		proj = m.curProject
	}
	if lead != "" {
		if g, ok := m.leadSignal(lead); ok && g.asks > 0 {
			return m.webInboxURL("session", lead), "asks from " + m.sessionTitle(lead)
		}
	}
	if proj != "" && m.projectAsks(proj) > 0 {
		return m.webInboxURL("project", proj), m.projectName(&proj) + " asks"
	}
	return m.webInboxURL("", ""), "web inbox"
}

// openWebInbox is I: open the web inbox for the selection.
func (m *Model) openWebInbox() tea.Cmd {
	u, what := m.webInboxTarget()
	return m.openWebURL(u, what)
}

// openWebURL opens u in the browser, or copies it when this machine has
// no display (or the opener fails), saying which.
func (m *Model) openWebURL(u, what string) tea.Cmd {
	open := m.opts.OpenURL
	if open == nil {
		open = defaultOpenURL
	}
	err := open(u)
	switch {
	case err == nil:
		return m.setToast("opened "+what+" in the browser: "+u, toneGood)
	case errors.Is(err, errNoDisplay):
		return m.copyText(u, "no display here — copied the "+what+" link: "+u)
	}
	return m.copyText(u, "could not open a browser ("+err.Error()+") — copied the "+what+" link: "+u)
}

// asksHeader is the header's "● N asks" (red when any is urgent or due
// within askHotWindow), a link to the web inbox; "" when none is open.
func (m *Model) asksHeader(compact bool) string {
	n, hot := m.openAsks()
	if n == 0 {
		return ""
	}
	t := toneWarn
	if hot {
		t = toneBad
	}
	text := "● " + itoa(n) + " " + pluralWord(n, "ask", "asks")
	if compact {
		text = "● " + itoa(n)
	}
	u := m.webInboxURL("", "")
	return m.mark(zoneDef{kind: zoneLink, name: u}, link(u, m.th.tone(t, text)))
}

// askLink links an ask signal to the web inbox filtered to key=id.
func (m *Model) askLink(key, id, s string) string {
	return link(m.webInboxURL(key, id), s)
}
