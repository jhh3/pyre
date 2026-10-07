package cli

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/clientconn"
	"github.com/jhh3/pyre/internal/digest"
)

// EnvDigestSummarizeCmd overrides the command `pyre digest --summarize`
// runs (the prompt arrives on stdin; stdout is the summary).
const EnvDigestSummarizeCmd = "PYRE_DIGEST_SUMMARIZE_CMD"

// DigestParams are get_digest's arguments (and the CLI flags).
type DigestParams struct {
	// Since: "last" (this client's last-seen mark; default), a duration
	// such as 2h, 90m or 1d, or an RFC 3339 time.
	Since string `json:"since,omitempty"`
	// NoTranscripts skips transcript reads (faster; summaries fall back to
	// attention items, artifacts and run states).
	NoTranscripts bool `json:"no_transcripts,omitempty"`
}

// digestStatePath is this client's last-seen file, beside its config.
func (a *App) digestStatePath() string {
	p := a.configFlag
	if p == "" {
		p = clientconn.DefaultConfigPath(a.Getenv)
	}
	return digest.StateFile(p)
}

// Digest builds the "while you were away" digest from existing hub reads.
// It never changes hub state; marking it seen is the caller's choice.
func (o *Ops) Digest(ctx context.Context, p DigestParams, statePath string, now time.Time) (digest.Digest, error) {
	opts, err := digest.Cutoff(p.Since, now, statePath)
	if err != nil {
		return digest.Digest{}, usagef("%v", err)
	}
	opts.NoTranscripts = p.NoTranscripts
	return digest.Fetch(ctx, o.API, opts)
}

func (a *App) addDigestCommands(root *cobra.Command) {
	var p DigestParams
	var noMark, summarize bool
	cmd := &cobra.Command{
		Use:   "digest [--since last|2h|1d] [--summarize]",
		Short: "While you were away: what each agent did, what changed, what is blocked, what needs you",
		Long: `Summarize what happened since you last looked, one line per agent, ranked:
needs you first, then finished work, then still working. Each line names
what changed (files, documents, artifacts, messages) and carries a pyre://
deep link to the exact transcript moment or object.

The summary is extracted deterministically from hub data (events, session
states, attention items, transcripts): the first sentence of the agent's
last message plus counts of turns, tool calls and files touched. No model
is called unless you pass --summarize, which asks a cheap model through
your own harness (default: claude -p --model haiku; override with
` + EnvDigestSummarizeCmd + `, prompt on stdin) and prints its catch-up above
the deterministic digest.

--since last (default) starts at this client's last-seen mark, kept in
digest-state.json beside the client config (12h on first use), and moves
the mark to now afterwards unless --no-mark. An explicit --since never moves
the mark. The TUI's "While you were away" panel shares the mark.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			now := time.Now()
			state := a.digestStatePath()
			d, err := ops.Digest(cmd.Context(), p, state, now)
			if err != nil {
				return err
			}
			if summarize {
				argv := digest.DefaultSummarizeCommand
				if s := strings.TrimSpace(a.Getenv(EnvDigestSummarizeCmd)); s != "" {
					argv = strings.Fields(s)
				}
				if d, err = digest.Summarize(cmd.Context(), d, now, digest.CommandSummarizer(argv, 2*time.Minute)); err != nil {
					a.warnf("pyre: --summarize failed (showing the deterministic digest): %v", err)
				}
			}
			mark := !noMark && (p.Since == "" || strings.EqualFold(p.Since, "last"))
			if err := a.emit(d, func(w io.Writer) { digest.WriteText(w, d, now) }); err != nil {
				return err
			}
			if mark && d.HeadCursor > 0 {
				if err := digest.SaveLastSeen(state, digest.LastSeen{Cursor: d.HeadCursor, At: now}); err != nil {
					a.warnf("pyre: could not record the last-seen mark: %v", err)
				}
			}
			return nil
		}),
	}
	f := cmd.Flags()
	f.StringVar(&p.Since, "since", "last", "last (since you last looked), a duration (2h, 90m, 1d) or an RFC 3339 time")
	f.BoolVar(&noMark, "no-mark", false, "do not move the last-seen mark")
	f.BoolVar(&summarize, "summarize", false, "also ask a cheap model (through your own harness) for a short prose catch-up")
	f.BoolVar(&p.NoTranscripts, "no-transcripts", false, "skip transcript reads (faster; less detail)")
	root.AddCommand(cmd)
}
