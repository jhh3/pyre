package skillmanager

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func skillArchive(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, body := range map[string]string{
		"repo/skills/gws-shared/SKILL.md": "---\nname: gws-shared\ndescription: Shared\n---\n\ngws --help\n",
		"repo/skills/gws-gmail/SKILL.md":  "---\nname: gws-gmail\ndescription: Gmail\n---\n\ngws gmail users messages list\n",
		"repo/skills/gws-drive/SKILL.md":  "---\nname: gws-drive\ndescription: Drive\n---\n\ngws drive files list\n",
	} {
		content := []byte(body)
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestSyncInstallsOnlySkillsMatchingGrantedCapabilities(t *testing.T) {
	archive := skillArchive(t)
	digest := sha256.Sum256(archive)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(archive)), Request: request}, nil
	})}
	root, codex := t.TempDir(), t.TempDir()
	legacyOwned := filepath.Join(codex, "cw-workspace-gws-upstream-gws-gmail")
	legacyUser := filepath.Join(codex, "cw-personal")
	for _, directory := range []string{legacyOwned, legacyUser} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(legacyOwned, ".cw-upstream.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := Manager{Paths: state.Paths{Root: root}, Client: client, Roots: map[string]string{"codex": codex}}
	pkg := kit.Package{Key: "google-workspace", Display: "Google Workspace", Skill: kit.Skill{Summary: "Workspace"}, Modes: []kit.Mode{{ID: "cli", Label: "CLI", Capabilities: []string{"gmail", "drive"}}}, SkillSources: []kit.SkillSource{{ID: "gws-upstream", Kind: "github-directory", Repository: "googleworkspace/cli", Ref: "commit", Digest: hex.EncodeToString(digest[:]), Paths: []kit.SkillPath{{Path: "skills/gws-shared"}, {Path: "skills/gws-gmail", Capabilities: []string{"gmail"}}, {Path: "skills/gws-drive", Capabilities: []string{"drive"}}}}}}
	connection := state.Connection{Name: "workspace", ConnectorKey: "google-workspace", SelectedMode: "cli", Status: "active", SafeIdentity: map[string]string{"capabilities": "gmail"}}
	receipts, err := manager.Sync(context.Background(), "codex", []state.Connection{connection}, map[string]kit.Package{"google-workspace": pkg}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 2 {
		t.Fatalf("expected shared + gmail skills, got %#v", receipts)
	}
	for _, receipt := range receipts {
		if strings.Contains(receipt.Path, "drive") {
			t.Fatal("ungranted Drive skill was installed")
		}
		content, err := os.ReadFile(filepath.Join(receipt.InstalledPath, "SKILL.md"))
		if err != nil || !strings.Contains(string(content), "Start with `outfit describe workspace`") || !strings.Contains(string(content), "outfit exec workspace") {
			t.Fatalf("skill was not scoped through outfit: %v %s", err, content)
		}
	}
	inventory, err := manager.Inventory("codex")
	if err != nil || len(inventory) != 3 { // two upstream plus the generated connector skill
		t.Fatalf("inventory: %#v %v", inventory, err)
	}
	if _, err := os.Stat(legacyOwned); !os.IsNotExist(err) {
		t.Fatal("owned legacy upstream skill was not removed")
	}
	if _, err := os.Stat(legacyUser); err != nil {
		t.Fatal("unowned legacy skill was removed")
	}
}

func TestDisabledUpstreamSkillStaysAbsentDuringResync(t *testing.T) {
	archive := skillArchive(t)
	digest := sha256.Sum256(archive)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(archive)), Request: request}, nil
	})}
	root, codex := t.TempDir(), t.TempDir()
	manager := Manager{Paths: state.Paths{Root: root}, Client: client, Roots: map[string]string{"codex": codex}}
	if err := manager.SetEnabled("workspace", "gws-upstream", "skills/gws-gmail", "codex", false); err != nil {
		t.Fatal(err)
	}
	pkg := kit.Package{Key: "google-workspace", Display: "Google Workspace", Skill: kit.Skill{Summary: "Workspace"}, Modes: []kit.Mode{{ID: "cli", Label: "CLI", Capabilities: []string{"gmail"}}}, SkillSources: []kit.SkillSource{{ID: "gws-upstream", Kind: "github-directory", Repository: "googleworkspace/cli", Ref: "commit", Digest: hex.EncodeToString(digest[:]), Paths: []kit.SkillPath{{Path: "skills/gws-shared"}, {Path: "skills/gws-gmail", Capabilities: []string{"gmail"}}}}}}
	connection := state.Connection{Name: "workspace", ConnectorKey: "google-workspace", SelectedMode: "cli", Status: "active", SafeIdentity: map[string]string{"capabilities": "gmail"}}
	receipts, err := manager.Sync(context.Background(), "codex", []state.Connection{connection}, map[string]kit.Package{"google-workspace": pkg}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].Path != "skills/gws-shared" {
		t.Fatalf("disabled skill returned: %#v", receipts)
	}
}

func TestFailedSkillSourceUpdateRetainsInstalledSet(t *testing.T) {
	archive := skillArchive(t)
	digest := sha256.Sum256(archive)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(archive)), Request: request}, nil
	})}
	root, codex := t.TempDir(), t.TempDir()
	manager := Manager{Paths: state.Paths{Root: root}, Client: client, Roots: map[string]string{"codex": codex}}
	pkg := kit.Package{Key: "google-workspace", Display: "Google Workspace", Skill: kit.Skill{Summary: "Workspace"}, Modes: []kit.Mode{{ID: "cli", Label: "CLI", Capabilities: []string{"gmail"}}}, SkillSources: []kit.SkillSource{{ID: "gws-upstream", Kind: "github-directory", Repository: "googleworkspace/cli", Ref: "good", Digest: hex.EncodeToString(digest[:]), Paths: []kit.SkillPath{{Path: "skills/gws-gmail", Capabilities: []string{"gmail"}}}}}}
	connection := state.Connection{Name: "workspace", ConnectorKey: "google-workspace", SelectedMode: "cli", Status: "active", SafeIdentity: map[string]string{"capabilities": "gmail"}}
	receipts, err := manager.Sync(context.Background(), "codex", []state.Connection{connection}, map[string]kit.Package{"google-workspace": pkg}, nil, nil)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("initial sync: %#v %v", receipts, err)
	}
	installed := receipts[0].InstalledPath
	before, err := os.ReadFile(filepath.Join(installed, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	pkg.SkillSources[0].Ref = "bad"
	pkg.SkillSources[0].Digest = strings.Repeat("0", 64)
	if _, err := manager.Sync(context.Background(), "codex", []state.Connection{connection}, map[string]kit.Package{"google-workspace": pkg}, nil, nil); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected rejected update, got %v", err)
	}
	after, err := os.ReadFile(filepath.Join(installed, "SKILL.md"))
	if err != nil {
		t.Fatalf("prior skill was removed: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed update changed the installed skill")
	}
}
