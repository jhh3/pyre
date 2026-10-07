package digest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Summarizer is the optional model hook: it turns the deterministic digest
// text into a short prose catch-up. It is never used unless asked for
// (`pyre digest --summarize`); the deterministic digest is always the
// source of truth and is printed as well.
type Summarizer func(ctx context.Context, prompt string) (string, error)

// DefaultSummarizeCommand asks a cheap model through the owner's existing
// Claude Code install (their own account and settings; no Pyre credential
// is involved). Override with PYRE_DIGEST_SUMMARIZE_CMD, e.g.
// "codex exec --model gpt-5-mini -" (the prompt arrives on stdin).
var DefaultSummarizeCommand = []string{"claude", "-p", "--model", "haiku", "--output-format", "text"}

// CommandSummarizer runs argv with the prompt on stdin and returns its
// stdout, bounded by timeout.
func CommandSummarizer(argv []string, timeout time.Duration) Summarizer {
	return func(ctx context.Context, prompt string) (string, error) {
		if len(argv) == 0 {
			return "", fmt.Errorf("no summarize command")
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
		cmd.Stdin = strings.NewReader(prompt)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if len(msg) > 300 {
				msg = msg[:300] + "…"
			}
			if msg != "" {
				return "", fmt.Errorf("%s: %v: %s", argv[0], err, msg)
			}
			return "", fmt.Errorf("%s: %v", argv[0], err)
		}
		return strings.TrimSpace(out.String()), nil
	}
}

// SummaryPrompt is what the summarizer receives: instructions plus the
// deterministic digest text (titles, first sentences, counts, links — no
// full transcripts).
func SummaryPrompt(d Digest, now time.Time) string {
	var b strings.Builder
	b.WriteString("You are catching the owner of a fleet of coding agents up on what happened while they were away.\n")
	b.WriteString("Below is a deterministic digest. Write at most 5 short lines: first what needs the owner, then what was finished, then what is still running. ")
	b.WriteString("Name agents by title. Do not invent facts; do not repeat links. Plain text, no markdown headings.\n\n")
	WriteText(&b, d, now)
	return b.String()
}

// Summarize fills d.Summary using s. On failure d is returned unchanged
// with the error, so callers still print the deterministic digest.
func Summarize(ctx context.Context, d Digest, now time.Time, s Summarizer) (Digest, error) {
	if s == nil || d.Empty() {
		return d, nil
	}
	out, err := s(ctx, SummaryPrompt(d, now))
	if err != nil {
		return d, err
	}
	d.Summary = out
	return d, nil
}
