package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"
)

// encodeKey turns a key press into the bytes an xterm-compatible terminal
// would send for it, so keys typed into the embedded terminal reach the
// session (through the tmux client running in the pane's pty) exactly as
// from a real terminal. appCursor is DECCKM (application cursor keys), as
// the client inside the pane last set it.
//
// Shift+Enter and Alt+Enter send ESC CR, the sequence Claude Code and Codex
// read as "new line" (what Claude's /terminal-setup binds), because tmux
// forwards a CSI-u Shift+Enter as plain Enter to a pane that did not ask
// for extended keys.
func encodeKey(k tea.KeyPressMsg, appCursor bool) []byte {
	mod := k.Mod
	ctrl := mod&tea.ModCtrl != 0
	alt := mod&(tea.ModAlt|tea.ModMeta) != 0
	shift := mod&tea.ModShift != 0
	esc := func(b []byte) []byte {
		if alt {
			return append([]byte{0x1b}, b...)
		}
		return b
	}
	// xterm modifier parameter: 1 + shift(1) + alt(2) + ctrl(4).
	mparam := 1
	if shift {
		mparam++
	}
	if alt {
		mparam += 2
	}
	if ctrl {
		mparam += 4
	}
	csi := func(final byte, ss3 bool) []byte {
		if mparam > 1 {
			return []byte("\x1b[1;" + strconv.Itoa(mparam) + string(final))
		}
		if ss3 {
			return []byte{0x1b, 'O', final}
		}
		return []byte{0x1b, '[', final}
	}
	tilde := func(n int) []byte {
		if mparam > 1 {
			return []byte("\x1b[" + strconv.Itoa(n) + ";" + strconv.Itoa(mparam) + "~")
		}
		return []byte("\x1b[" + strconv.Itoa(n) + "~")
	}
	switch k.Code {
	case tea.KeyEnter, tea.KeyKpEnter:
		if shift || alt {
			return []byte("\x1b\r")
		}
		if ctrl {
			return []byte("\n")
		}
		return []byte("\r")
	case tea.KeyTab:
		if shift {
			return []byte("\x1b[Z")
		}
		return esc([]byte("\t"))
	case tea.KeyBackspace:
		if ctrl {
			return esc([]byte{0x08})
		}
		return esc([]byte{0x7f})
	case tea.KeyEscape:
		return esc([]byte{0x1b})
	case tea.KeyUp:
		return csi('A', appCursor)
	case tea.KeyDown:
		return csi('B', appCursor)
	case tea.KeyRight:
		return csi('C', appCursor)
	case tea.KeyLeft:
		return csi('D', appCursor)
	case tea.KeyHome:
		return csi('H', appCursor)
	case tea.KeyEnd:
		return csi('F', appCursor)
	case tea.KeyInsert:
		return tilde(2)
	case tea.KeyDelete:
		return tilde(3)
	case tea.KeyPgUp:
		return tilde(5)
	case tea.KeyPgDown:
		return tilde(6)
	case tea.KeyF1, tea.KeyF2, tea.KeyF3, tea.KeyF4:
		f := byte('P' + (k.Code - tea.KeyF1))
		if mparam > 1 {
			return []byte("\x1b[1;" + strconv.Itoa(mparam) + string(f))
		}
		return []byte{0x1b, 'O', f}
	case tea.KeyF5:
		return tilde(15)
	case tea.KeyF6:
		return tilde(17)
	case tea.KeyF7:
		return tilde(18)
	case tea.KeyF8:
		return tilde(19)
	case tea.KeyF9:
		return tilde(20)
	case tea.KeyF10:
		return tilde(21)
	case tea.KeyF11:
		return tilde(23)
	case tea.KeyF12:
		return tilde(24)
	}
	if ctrl {
		if b, ok := ctrlByte(k.Code); ok {
			return esc([]byte{b})
		}
		return nil
	}
	if k.Text != "" {
		return esc([]byte(k.Text))
	}
	if k.Code == tea.KeySpace {
		return esc([]byte(" "))
	}
	if k.Code > 0 && k.Code < 0x10ffff && k.Code >= ' ' {
		r := k.Code
		if shift && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		return esc([]byte(string(r)))
	}
	return nil
}

// ctrlByte is the C0 control a ctrl+key chord sends (ctrl+a = 0x01 …).
func ctrlByte(r rune) (byte, bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return byte(r-'a') + 1, true
	case r >= 'A' && r <= 'Z':
		return byte(r-'A') + 1, true
	case r == ' ' || r == '@' || r == '2':
		return 0, true
	case r == '[' || r == '3':
		return 0x1b, true
	case r == '\\' || r == '4':
		return 0x1c, true
	case r == ']' || r == '5':
		return 0x1d, true
	case r == '^' || r == '6':
		return 0x1e, true
	case r == '_' || r == '/' || r == '7' || r == '-':
		return 0x1f, true
	case r == '?' || r == '8':
		return 0x7f, true
	}
	return 0, false
}

// bracketPaste wraps pasted text in bracketed-paste markers when the
// application asked for them, and strips any end marker inside the text so
// a paste can never end itself early.
func bracketPaste(text string, bracketed bool) []byte {
	if !bracketed {
		return []byte(text)
	}
	const start, end = "\x1b[200~", "\x1b[201~"
	clean := []byte{}
	for i := 0; i < len(text); {
		if len(text)-i >= len(end) && text[i:i+len(end)] == end {
			i += len(end)
			continue
		}
		clean = append(clean, text[i])
		i++
	}
	return append(append([]byte(start), clean...), end...)
}
