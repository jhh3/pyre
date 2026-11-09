package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
)

// Full transcript reads (ReadSessionRequest.Full, additive 2026-10-23).
//
// Adapters cut each entry's text at about 2000 bytes when they read the
// harness's own transcript; nothing is cut when it is written, so the
// whole text is still on the node. A full read replaces the cut text of
// the entries a read returned with their whole text, newest first, within
// contracts.FullEntryMax per entry and contracts.FullReadMax per read, and
// redacts every entry of the response. Every read, default or full, is
// redacted (redactRead).

// transcriptFullReader is an adapter that returns the uncut text of the
// shown entries numbered seqs (claude.Adapter, claude.ExecAdapter,
// codex.Adapter, codex.ExecAdapter).
type transcriptFullReader interface {
	TranscriptFullText(ctx context.Context, h contracts.RunHandle, seqs []int64) (map[int64]string, error)
}

// redactFull masks credentials in a read's text with the shared credential
// patterns and Pyre's own token prefixes.
func redactFull(s string) string { return credentials.RedactTranscript(s) }

// redactRead masks credentials in a read's screen and, unless expandFull
// already did (a full read), its entries. Text without a secret-shaped
// substring is unchanged.
func redactRead(resp *contracts.ReadSessionResponse, entries bool) {
	if entries {
		for i := range resp.Entries {
			resp.Entries[i].Text = redactFull(resp.Entries[i].Text)
		}
	}
	resp.Screen = redactFull(resp.Screen)
}

// cutFull keeps at most max bytes of s (valid UTF-8) and says how much was
// left out and why; true when it cut.
func cutFull(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…[%d bytes truncated: a full read keeps at most %d KB per entry]", len(s)-cut, max>>10), true
}

// expandFull applies a full read to resp (entries from adapter for run h).
func expandFull(ctx context.Context, adapter contracts.RuntimeAdapter, h contracts.RunHandle, resp *contracts.ReadSessionResponse) {
	resp.Full = true
	var seqs []int64
	for _, e := range resp.Entries {
		if e.Truncated {
			seqs = append(seqs, e.Seq)
		}
	}
	var notes []string
	var full map[int64]string
	if len(seqs) > 0 {
		if fr, ok := adapter.(transcriptFullReader); ok {
			var err error
			if full, err = fr.TranscriptFullText(ctx, h, seqs); err != nil {
				notes = append(notes, "full text could not be read ("+redact(err.Error())+"); entries keep the default cut")
			}
		} else {
			notes = append(notes, "the "+adapter.Name()+" adapter does not return full entry text yet; entries keep the default cut")
		}
	}
	var cut, over, missing []string
	budget := contracts.FullReadMax
	for i := len(resp.Entries) - 1; i >= 0; i-- { // newest first: a final report wins the budget
		e := &resp.Entries[i]
		orig := e.Text
		e.Text = redactFull(e.Text)
		if !e.Truncated || full == nil {
			continue
		}
		t, ok := full[e.Seq]
		if !ok || !sameStart(orig, t) {
			missing = append(missing, fmt.Sprint(e.Seq))
			continue
		}
		t, capped := cutFull(redactFull(t), contracts.FullEntryMax)
		b, _ := json.Marshal(t)
		if len(b) > budget {
			over = append(over, fmt.Sprint(e.Seq))
			continue
		}
		budget -= len(b)
		e.Text, e.Truncated = t, capped
		if capped {
			cut = append(cut, fmt.Sprint(e.Seq))
		}
	}
	if len(cut) > 0 {
		notes = append(notes, fmt.Sprintf("seq %s: longer than the %d KB a full read keeps per entry; cut there",
			strings.Join(reverse(cut), ", "), contracts.FullEntryMax>>10))
	}
	if len(over) > 0 {
		notes = append(notes, fmt.Sprintf("seq %s: past the %d KB of full text one read returns; default cut kept (read each alone with seq)",
			strings.Join(reverse(over), ", "), contracts.FullReadMax>>10))
	}
	if len(missing) > 0 {
		notes = append(notes, "seq "+strings.Join(reverse(missing), ", ")+": not found again in the transcript; default cut kept")
	}
	resp.FullNote = strings.Join(notes, "; ")
}

// sameStart: the full text begins as the entry's default cut does (each
// with whitespace folded: Codex folds a cut entry onto one line), so text
// read from another numbering than the read's (a Codex run that stopped
// between the two, read from its rollout) never replaces an entry.
func sameStart(cut, full string) bool {
	fold := func(s string, n int) string {
		if len(s) > n {
			s = s[:n]
		}
		return strings.Join(strings.Fields(s), " ")
	}
	a, b := fold(cut, 512), fold(full, 4096)
	n := min(len(a), len(b), 200)
	return a[:n] == b[:n]
}

func reverse(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}
