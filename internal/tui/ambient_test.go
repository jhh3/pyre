package tui

import "testing"

func TestAmbientLabel(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{"your own login on this node (this node has no Pyre anthropic accounts)", "your own login on mac (this node has no Pyre anthropic accounts)"},
		{"your own login on this node (pyred --default-account ambient)", "your own login on mac (pyred --default-account ambient)"},
		{"credential_kind=ambient: node default login (no Pyre assignment)", "your own login on mac (no Pyre assignment)"},
		{"", "your own login on mac"},
	} {
		if got := ambientLabel("mac", tc.reason); got != tc.want {
			t.Errorf("ambientLabel(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}
