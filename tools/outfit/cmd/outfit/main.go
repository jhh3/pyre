package main

import (
	"fmt"
	"os"

	"github.com/jhh3/outfit/internal/app"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := app.Execute(app.BuildInfo{Version: version, Commit: commit, Date: date}); err != nil {
		if !app.IsReportedError(err) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
