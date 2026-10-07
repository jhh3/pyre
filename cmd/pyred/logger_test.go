package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A20 soak: every pyred log line went to both the rotated log file and
// stderr, so a redirected stderr (launchd StandardErrorPath) grew without
// rotation. Only warnings and errors reach stderr now.
func TestLoggerKeepsInfoOutOfStderr(t *testing.T) {
	state := t.TempDir()
	var stderr bytes.Buffer
	log, closeFn := newLoggerTo(state, "info", &stderr)
	log.Info("synthetic local api request", "path", "/v1/messages.send")
	log.With("component", "local-api").Warn("synthetic warning")
	closeFn()
	file, err := os.ReadFile(filepath.Join(state, "logs", "pyred.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(file), "synthetic local api request") || !strings.Contains(string(file), "synthetic warning") {
		t.Fatalf("log file misses lines:\n%s", file)
	}
	if strings.Contains(stderr.String(), "synthetic local api request") {
		t.Fatalf("INFO line copied to stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "synthetic warning") || !strings.Contains(stderr.String(), "component=local-api") {
		t.Fatalf("warning missing from stderr:\n%s", stderr.String())
	}
}
