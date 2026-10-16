package hostproc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jhh3/pyre/internal/runtime/termhost"
)

// PrefixKey is the attach client's command prefix (Ctrl-b, as in tmux):
// Ctrl-b d detaches, Ctrl-b Ctrl-b sends one Ctrl-b.
const PrefixKey = 0x02

// detachFilter turns typed bytes into session input, consuming the
// prefix sequences. It keeps state across reads.
type detachFilter struct{ prefixed bool }

// feed returns the bytes to send and whether the user asked to detach.
func (f *detachFilter) feed(in []byte) (out []byte, detach bool) {
	for _, b := range in {
		if f.prefixed {
			f.prefixed = false
			switch b {
			case 'd', 'D':
				return out, true
			case PrefixKey:
				out = append(out, PrefixKey)
			default:
				out = append(out, PrefixKey, b)
			}
			continue
		}
		if b == PrefixKey {
			f.prefixed = true
			continue
		}
		out = append(out, b)
	}
	return out, false
}

// attachMain is the native attach client: it puts this console/terminal
// in raw mode and relays it to the session until Ctrl-b d or the session
// ends. A read-only client sends nothing but the detach keys.
func attachMain(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sock := fs.String("S", "", "socket path")
	name := fs.String("t", "", "session name")
	ro := fs.Bool("r", false, "read-only")
	if err := fs.Parse(args); err != nil || *sock == "" || *name == "" {
		fmt.Fprintln(stderr, "usage: "+termhost.ServeArg+" attach -S socket -t name [-r]")
		return 2
	}
	cols, rows := consoleSize()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := termhost.DialAttach(ctx, termhost.DialOptions{Socket: *sock, Name: *name, ReadOnly: *ro, Cols: cols, Rows: rows})
	cancel()
	if err != nil {
		fmt.Fprintf(stderr, "pyre: attach %s: %v\n", *name, err)
		return 1
	}
	defer conn.Close()
	restore, err := makeRaw()
	if err != nil {
		fmt.Fprintf(stderr, "pyre: attach needs a terminal: %v\n", err)
		return 1
	}
	out := os.Stdout
	finish := func(msg string) {
		_, _ = io.WriteString(out, ResetSequence)
		restore()
		fmt.Fprintf(out, "\r\n[%s]\r\n", msg)
	}

	ended := make(chan string, 1)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					ended <- "terminal closed"
					return
				}
			}
			if err != nil {
				msg := "detached (connection closed)"
				if r := conn.ExitReason(); r != "" {
					msg = r
				} else if errors.Is(err, io.EOF) {
					msg = "session ended"
				}
				ended <- msg
				return
			}
		}
	}()
	detached := make(chan struct{}, 1)
	go func() {
		var f detachFilter
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				b, detach := f.feed(buf[:n])
				if len(b) > 0 && !*ro {
					if _, err := conn.Write(b); err != nil {
						return
					}
				}
				if detach {
					detached <- struct{}{}
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	stopResize := make(chan struct{})
	defer close(stopResize)
	if !*ro {
		go watchResize(stopResize, func(c, r int) { _ = conn.Resize(c, r) })
	}
	select {
	case <-detached:
		finish("detached from " + *name)
	case msg := <-ended:
		finish(msg)
	}
	return 0
}

// consoleSize is the attach client's terminal size (80x24 if unknown).
func consoleSize() (int, int) {
	if c, r, err := termSize(); err == nil && c > 0 && r > 0 {
		return c, r
	}
	return 80, 24
}
