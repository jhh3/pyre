package terminal

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Fitted streams (contracts.TerminalOpenCommand.Fit): a watcher must never
// change the agent's window. tmux ignores an `-f ignore-size` client for
// sizing while another client is attached, but still sizes the window to
// it when it is the only kind attached (verified on tmux 3.4), so the PTY
// is made exactly the window's size: whatever tmux decides, the window
// keeps its size. The node re-reads the size every FitEvery while the
// stream is open and follows it (a human resized the window).

// fitFormat asks tmux for the window size, its status line and version.
const fitFormat = "#{window_width} #{window_height} #{status} #{version}"

// tmuxFit is a parsed local tmux attach argv.
type tmuxFit struct {
	globals []string // tmux binary and global flags (-u, -S socket, ...)
	target  string
}

// parseFitAttach recognises the run's local attach argv:
// `tmux [global flags] [cmd ... ;] attach-session -t T [-r]` (what the tmux
// runtime builds, set-option prefix included).
func parseFitAttach(argv []string) (tmuxFit, bool) {
	if len(argv) < 3 || !strings.Contains(filepath.Base(argv[0]), "tmux") {
		return tmuxFit{}, false
	}
	i := 1
	for ; i < len(argv) && strings.HasPrefix(argv[i], "-"); i++ {
		switch argv[i] {
		case "-S", "-L", "-f", "-c", "-T":
			i++ // global flag with a value
		}
	}
	if i >= len(argv) {
		return tmuxFit{}, false
	}
	f := tmuxFit{globals: append([]string(nil), argv[:i]...)}
	// Commands are separated by ";" words; find the attach.
	for start := i; start < len(argv); {
		end := start
		for end < len(argv) && argv[end] != ";" {
			end++
		}
		switch argv[start] {
		case "attach-session", "attach", "a":
			for j := start + 1; j < end-1; j++ {
				if argv[j] == "-t" {
					f.target = argv[j+1]
				}
			}
			return f, f.target != ""
		}
		start = end + 1
	}
	return tmuxFit{}, false
}

func (f tmuxFit) probeArgv() []string {
	return append(append([]string(nil), f.globals...), "display-message", "-p", "-t", f.target, fitFormat)
}

// attachArgv is the fitted client: read-only, ignore-size when the server
// knows the flag. Never -d (that would detach the human).
func (f tmuxFit) attachArgv(ignoreSize bool) []string {
	argv := append(append([]string(nil), f.globals...), "attach-session", "-t", f.target, "-r")
	if ignoreSize {
		argv = append(argv, "-f", "ignore-size")
	}
	return argv
}

// probe reads the window's size (status rows included: the PTY size that
// matches the window) and whether ignore-size exists (tmux >= 3.2).
func (f tmuxFit) probe(ctx context.Context, env []string) (cols, rows int, ignoreSize bool, err error) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	argv := f.probeArgv()
	cmd := exec.CommandContext(pctx, argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, false, contracts.Errorf(contracts.ErrUnavailable, "cannot read the window size (%v); not attaching a watcher that could resize it", err)
	}
	return parseFitSize(string(out))
}

// parseFitSize reads fitFormat's answer.
func parseFitSize(out string) (cols, rows int, ignoreSize bool, err error) {
	fs := strings.Fields(strings.TrimSpace(out))
	if len(fs) < 2 {
		return 0, 0, false, contracts.Errorf(contracts.ErrUnavailable, "unexpected window size %q", strings.TrimSpace(out))
	}
	cols, err1 := strconv.Atoi(fs[0])
	rows, err2 := strconv.Atoi(fs[1])
	if err1 != nil || err2 != nil || cols < 2 || rows < 1 || cols > MaxDim || rows > MaxDim {
		return 0, 0, false, contracts.Errorf(contracts.ErrUnavailable, "unexpected window size %q", strings.TrimSpace(out))
	}
	if len(fs) > 2 {
		switch st := fs[2]; st {
		case "off":
		case "on":
			rows++
		default:
			if n, err := strconv.Atoi(st); err == nil && n > 0 && n <= 5 {
				rows += n
			}
		}
	}
	if len(fs) > 3 {
		ignoreSize = tmuxAtLeast(fs[3], 3, 2)
	}
	return min(cols, MaxDim), min(rows, MaxDim), ignoreSize, nil
}

// tmuxAtLeast compares a tmux version ("3.4", "3.2a", "next-3.6").
func tmuxAtLeast(v string, major, minor int) bool {
	v = strings.TrimPrefix(v, "next-")
	maj, rest, _ := strings.Cut(v, ".")
	a, err := strconv.Atoi(maj)
	if err != nil {
		return false
	}
	digits := strings.TrimRightFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	b, _ := strconv.Atoi(digits)
	return a > major || a == major && b >= minor
}

// fitPrepare turns a fitted request's attach argv into the fitted client
// and the PTY size to start it at.
func (b *NodeBridge) fitPrepare(ctx context.Context, tc contracts.TerminalOpenCommand, argv []string) (tmuxFit, []string, int, int, error) {
	if !tc.ReadOnly {
		return tmuxFit{}, nil, 0, 0, contracts.Errorf(contracts.ErrInvalid, "fit is only for read-only streams")
	}
	f, ok := parseFitAttach(argv)
	if !ok {
		return tmuxFit{}, nil, 0, 0, contracts.Errorf(contracts.ErrUnsupported, "run's attach is not a recognisable tmux attach; cannot fit it to the window").WithRef(contracts.KindRun, tc.RunID)
	}
	cols, rows, ignore, err := f.probe(ctx, b.env())
	if err != nil {
		return tmuxFit{}, nil, 0, 0, err
	}
	return f, f.attachArgv(ignore), cols, rows, nil
}

func fitFrame(cols, rows int) Frame { return Frame{Type: FrameResize, Cols: cols, Rows: rows} }
