// Package hostproc is the termhost host process (`<pyred> __pyre_termhost
// serve -S sock`) and the native attach client (`__pyre_termhost attach`):
// sessions on pseudo-terminals (ConPTY on Windows, a Unix PTY elsewhere),
// the VT emulator behind captures and repaints (charmbracelet/x/vt, as in
// the TUI), and the console handling of the attach client.
//
// It is kept out of package termhost (the light client pyred uses on every
// hook invocation) because the emulator stack has a heavy package init.
// Importing this package registers the `__pyre_termhost` entry: pyred
// imports it only on Windows or with the pyre_termhost build tag
// (cmd/pyred/termhost.go), and tests that start a real host import it so
// the test binary serves the entry.
package hostproc

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/jhh3/pyre/internal/runtime/termhost"
	"github.com/jhh3/pyre/internal/runtime/termhost/internal/wire"
)

func init() {
	termhost.RegisterHostEntry()
	if len(os.Args) > 1 && os.Args[1] == termhost.ServeArg {
		os.Exit(Main(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
}

// Main implements `__pyre_termhost serve|attach ...`.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: "+termhost.ServeArg+" serve -S socket | attach -S socket -t name [-r]")
		return 2
	}
	switch args[0] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(stderr)
		sock := fs.String("S", "", "socket path")
		logPath := fs.String("log", "", "append the host log to this file (default: stderr)")
		envFile := fs.String("env-file", "", "JSON array of KEY=VALUE: the sessions' base environment (read, then deleted)")
		hide := fs.Bool("hide-console", false, "Windows: detach from the console window Task Scheduler opens")
		if err := fs.Parse(args[1:]); err != nil || *sock == "" {
			return 2
		}
		var env []string
		if *envFile != "" {
			// Read and delete it first, whatever happens next.
			b, err := os.ReadFile(*envFile)
			_ = os.Remove(*envFile)
			if err == nil {
				err = json.Unmarshal(b, &env)
			}
			if err != nil {
				fmt.Fprintf(stderr, "termhost: environment file: %v\n", err)
				env = nil
			}
		}
		if *hide {
			detachConsole()
		}
		if *logPath != "" {
			f, err := termhost.OpenLog(*logPath)
			if err != nil {
				fmt.Fprintf(stderr, "termhost: open log: %v\n", err)
				return 1
			}
			defer f.Close()
			redirectStderr(f)
			stderr = f
		}
		lg := log.New(stderr, "", log.LstdFlags)
		srv := &Server{Socket: *sock, Logf: lg.Printf}
		if env != nil {
			srv.Env = wire.HostBaseEnv(env)
		}
		if err := srv.Listen(); err != nil {
			if err == ErrAlreadyRunning {
				return 0
			}
			lg.Printf("termhost: %v", err)
			return 1
		}
		if err := srv.Serve(); err != nil {
			lg.Printf("termhost: %v", err)
			return 1
		}
		lg.Printf("termhost: stopped")
		return 0
	case "attach":
		return attachMain(args[1:], stderr)
	}
	fmt.Fprintf(stderr, "%s: unknown command %q\n", termhost.ServeArg, args[0])
	return 2
}
