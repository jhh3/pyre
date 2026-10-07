package tui

import (
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// harnessChips: "✓claude ✗codex" with the marks toned.
func (m *Model) harnessChips(n contracts.Node) string {
	var parts []string
	online := n.Connection.State == contracts.ConnConnected
	for _, h := range n.Harnesses {
		switch {
		case h.Available && online:
			parts = append(parts, string(h.Harness))
		case h.Available:
			parts = append(parts, m.th.dim.Render(string(h.Harness))) // last known; machine offline
		default:
			parts = append(parts, m.th.tone(toneBad, "✗")+m.th.dim.Render(string(h.Harness)))
		}
	}
	return strings.Join(parts, " ")
}
