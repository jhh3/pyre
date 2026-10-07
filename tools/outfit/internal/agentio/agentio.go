package agentio

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/jhh3/outfit/internal/invoke"
	"golang.org/x/term"
)

const (
	defaultSpillBytes = 64 * 1024
	minimumSpillBytes = 4 * 1024
	maximumSpillBytes = 10 * 1024 * 1024
)

// Enabled reports whether output should use Outfit's stable agent envelope.
// Explicit OUTFIT_AGENT wins; otherwise only a real, non-terminal file is
// treated as an agent/captured process. In-memory writers remain human-readable
// unless a caller explicitly opts in, which keeps library and test use clear.
func Enabled(output io.Writer) bool {
	if value := strings.TrimSpace(os.Getenv("OUTFIT_AGENT")); value != "" {
		enabled, known := parseBool(value)
		return known && enabled
	}
	file, ok := output.(*os.File)
	return ok && !term.IsTerminal(int(file.Fd()))
}

func parseBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

// Prepare serializes an envelope and, when allowed, replaces an oversized
// inline payload with a receipt for a private temp file containing the complete
// original envelope.
func Prepare(envelope invoke.Envelope, allowSpill bool) (invoke.Envelope, []byte, error) {
	encoded, err := invoke.Marshal(envelope)
	if err != nil {
		return invoke.Envelope{}, nil, err
	}
	if !allowSpill || len(encoded) <= spillLimit() {
		return envelope, encoded, nil
	}
	path, digest, err := writeSpill(encoded)
	if err != nil {
		return invoke.Envelope{}, nil, err
	}
	compact := envelope
	compact.Data = map[string]any{
		"spilled": true,
		"path":    path,
		"summary": "Complete result written to a private JSON file; read that file before continuing.",
	}
	compact.Meta.Spill = &invoke.Spill{Path: path, Bytes: len(encoded), SHA256: digest}
	encoded, err = invoke.Marshal(compact)
	return compact, encoded, err
}

func Render(output io.Writer, envelope invoke.Envelope, allowSpill bool) error {
	_, encoded, err := Prepare(envelope, allowSpill)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(encoded))
	return err
}

func spillLimit() int {
	value := strings.TrimSpace(os.Getenv("OUTFIT_AGENT_MAX_BYTES"))
	if value == "" {
		return defaultSpillBytes
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultSpillBytes
	}
	if parsed < minimumSpillBytes {
		return minimumSpillBytes
	}
	if parsed > maximumSpillBytes {
		return maximumSpillBytes
	}
	return parsed
}

func writeSpill(encoded []byte) (path, digest string, resultErr error) {
	file, err := os.CreateTemp("", "outfit-result-*.json")
	if err != nil {
		return "", "", err
	}
	path = file.Name()
	defer func() {
		if resultErr != nil {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", "", err
	}
	if _, err := file.Write(encoded); err != nil {
		return "", "", err
	}
	if err := file.Sync(); err != nil {
		return "", "", err
	}
	if err := file.Close(); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(encoded)
	return path, hex.EncodeToString(sum[:]), nil
}

// Requested reports whether the caller explicitly requested JSON anywhere in
// an Outfit command line. It intentionally recognizes only the exact flag.
func Requested(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--json" {
			return true
		}
	}
	return false
}

// ReportedError marks an error whose structured envelope was already emitted.
type ReportedError struct{ Err error }

func (e ReportedError) Error() string { return e.Err.Error() }
func (e ReportedError) Unwrap() error { return e.Err }

func Reported(err error) error {
	if err == nil {
		return nil
	}
	return ReportedError{Err: err}
}

func IsReported(err error) bool {
	var target ReportedError
	return errors.As(err, &target)
}
