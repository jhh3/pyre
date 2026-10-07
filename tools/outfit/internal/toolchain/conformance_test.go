package toolchain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
)

func TestValidateSchemasRetriesTransientDiscoveryFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper is Unix-only")
	}
	directory := t.TempDir()
	counter := filepath.Join(directory, "attempts")
	binary := filepath.Join(directory, "schema-helper")
	body := "#!/bin/sh\ncount=0\n[ ! -f '" + counter + "' ] || count=$(cat '" + counter + "')\ncount=$((count+1))\nprintf '%s' \"$count\" > '" + counter + "'\nif [ \"$count\" -lt 3 ]; then echo '{\"error\":\"discovery unavailable\"}'; exit 4; fi\nprintf '{\"path\":\"%s\"}' \"$2\"\n"
	if err := os.WriteFile(binary, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	mode := kit.Mode{Operations: []kit.Operation{{Name: "drive_get", CLI: &kit.CLIOperation{Schema: "drive.files.get"}}}}
	digest, err := ValidateSchemas(context.Background(), binary, mode, nil)
	if err != nil || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest = %q, err = %v", digest, err)
	}
	if attempts, err := os.ReadFile(counter); err != nil || string(attempts) != "3" {
		t.Fatalf("attempts = %q, err = %v", attempts, err)
	}
}

func TestValidateSchemasHonorsCapabilitiesAndIsDeterministic(t *testing.T) {
	if os.Getenv("OUTFIT_SCHEMA_HELPER") == "1" {
		if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "schema" || os.Args[len(os.Args)-1] == "drive.files.list" {
			os.Exit(3)
		}
		fmt.Printf(`{"path":%q}`, os.Args[len(os.Args)-1])
		os.Exit(0)
	}
	binary := helperBinary(t)
	mode := kit.Mode{Operations: []kit.Operation{
		{Name: "gmail_list", Capabilities: []string{"gmail"}, CLI: &kit.CLIOperation{Schema: "gmail.users.messages.list"}},
		{Name: "drive_list", Capabilities: []string{"drive"}, CLI: &kit.CLIOperation{Schema: "drive.files.list"}},
		{Name: "local_status", CLI: &kit.CLIOperation{Args: []string{"status"}}},
	}}
	digest, err := ValidateSchemas(context.Background(), binary, mode, []string{"gmail"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("digest = %q", digest)
	}
	second, err := ValidateSchemas(context.Background(), binary, mode, []string{"gmail"})
	if err != nil || second != digest {
		t.Fatalf("second digest = %q, %v", second, err)
	}
}

func helperBinary(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("OUTFIT_SCHEMA_HELPER") == "1" {
		return path
	}
	// Re-exec the current test binary through a tiny wrapper that selects this
	// test's helper branch while preserving the provider-style argv contract.
	directory := t.TempDir()
	wrapper := directory + "/schema-helper"
	if os.PathSeparator == '\\' {
		wrapper += ".cmd"
		body := "@set OUTFIT_SCHEMA_HELPER=1\r\n@\"" + path + "\" -test.run=TestValidateSchemasHonorsCapabilitiesAndIsDeterministic -- %*\r\n"
		if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return wrapper
	}
	body := "#!/bin/sh\nOUTFIT_SCHEMA_HELPER=1 exec " + shellQuote(path) + " -test.run=TestValidateSchemasHonorsCapabilitiesAndIsDeterministic -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
