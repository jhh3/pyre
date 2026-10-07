package toolchain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/outfit/internal/kit"
)

// ValidateSchemas asks a dynamic CLI to resolve every declared operation that
// belongs to the selected capability set. The combined digest is safe metadata
// that lets connection detail and locks identify the exact command contract
// verified with the pinned binary.
func ValidateSchemas(ctx context.Context, binary string, mode kit.Mode, capabilities []string) (string, error) {
	selected := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		selected[capability] = true
	}
	operations := append([]kit.Operation{}, mode.Operations...)
	sort.Slice(operations, func(i, j int) bool { return operations[i].Name < operations[j].Name })
	hash := sha256.New()
	validated := 0
	for _, operation := range operations {
		if operation.CLI == nil || strings.TrimSpace(operation.CLI.Schema) == "" || !operationSelected(operation, selected) {
			continue
		}
		output, err := resolveSchema(ctx, binary, operation.CLI.Schema)
		if err != nil {
			return "", err
		}
		var schema any
		if err := json.Unmarshal(output, &schema); err != nil {
			return "", fmt.Errorf("schema %s returned invalid JSON: %w", operation.CLI.Schema, err)
		}
		canonical, err := json.Marshal(schema)
		if err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(operation.Name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(canonical)
		_, _ = hash.Write([]byte{0})
		validated++
	}
	if validated == 0 {
		return "", nil
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func resolveSchema(ctx context.Context, binary, schema string) ([]byte, error) {
	var output []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var stderr bytes.Buffer
		command := exec.CommandContext(ctx, binary, "schema", schema)
		command.Stderr = &stderr
		output, err = command.Output()
		if err == nil {
			return output, nil
		}
		exit := new(exec.ExitError)
		if !errors.As(err, &exit) || exit.ExitCode() != 4 || attempt == 2 {
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = strings.TrimSpace(string(output))
			}
			if detail != "" {
				return nil, fmt.Errorf("schema %s: %s", schema, detail)
			}
			return nil, fmt.Errorf("schema %s: %w", schema, err)
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("schema %s: %w", schema, err)
}

func operationSelected(operation kit.Operation, selected map[string]bool) bool {
	if len(operation.Capabilities) == 0 || len(selected) == 0 {
		return true
	}
	for _, capability := range operation.Capabilities {
		if !selected[capability] {
			return false
		}
	}
	return true
}
