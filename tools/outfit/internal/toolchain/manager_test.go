package toolchain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
)

func testArchive(t *testing.T, body string) []byte {
	return testArchiveBytes(t, []byte(body))
}

func testArchiveBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "nested/acme", Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestToolchainHelperProcess(t *testing.T) {
	if os.Getenv("OUTFIT_TOOLCHAIN_HELPER") != "1" {
		return
	}
	if len(os.Args) == 0 {
		os.Exit(3)
	}
	_, _ = os.Stdout.WriteString("acme 1.2.3\n")
	os.Exit(0)
}

func helperExecutable(t *testing.T) []byte {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func testZip(t *testing.T, name, body string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	file, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestInstallVerifiesAndAtomicallySelectsManagedTool(t *testing.T) {
	t.Setenv("OUTFIT_TOOLCHAIN_HELPER", "1")
	archive := testArchiveBytes(t, helperExecutable(t))
	digest := sha256.Sum256(archive)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/acme.tar.gz" {
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		_, _ = response.Write(archive)
	}))
	defer server.Close()
	manager := Manager{Paths: state.Paths{Root: t.TempDir()}, Client: server.Client(), GOOS: "linux", GOARCH: "amd64"}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	requirement := kit.ToolRequirement{ID: "acme", Display: "Acme", Binary: "acme", Version: "1.2.3", Verify: []string{"-test.run=TestToolchainHelperProcess"}, Source: kit.ToolSource{Kind: "fixed-url", URL: server.URL + "/{{asset}}", Assets: map[string]string{platform: "acme.tar.gz"}, Checksums: map[string]string{platform: hex.EncodeToString(digest[:])}}}
	manager.GOOS, manager.GOARCH = runtime.GOOS, runtime.GOARCH
	receipt, err := manager.Install(context.Background(), requirement)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.VersionText != "acme 1.2.3" || receipt.ActualSHA != receipt.ExpectedSHA {
		t.Fatalf("unexpected receipt: %#v", receipt)
	}
	if info, err := os.Stat(receipt.Path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0) {
		t.Fatalf("managed binary missing or not executable: %v %#v", err, info)
	}
	current, err := manager.Resolve("acme")
	if err != nil || current.Path != receipt.Path {
		t.Fatalf("current selection: %#v %v", current, err)
	}
	listed, err := manager.List()
	if err != nil || len(listed) != 1 || listed[0].ID != "acme" {
		t.Fatalf("tool inventory: %#v %v", listed, err)
	}
}

func TestFailedUpdateRetainsPreviousSelection(t *testing.T) {
	root := t.TempDir()
	manager := Manager{Paths: state.Paths{Root: root}, GOOS: "linux", GOARCH: "amd64"}
	directory := filepath.Join(root, "tools", "acme", "1.0.0", "linux-amd64")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "receipt.json"), []byte(`{"id":"acme","version":"1.0.0","platform":"linux-amd64","path":"/old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Use("acme", "1.0.0", "linux-amd64"); err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, "#!/bin/sh\necho broken\n")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { _, _ = response.Write(archive) }))
	defer server.Close()
	manager.Client = server.Client()
	requirement := kit.ToolRequirement{ID: "acme", Display: "Acme", Binary: "acme", Version: "2.0.0", Verify: []string{"--version"}, Source: kit.ToolSource{Kind: "fixed-url", URL: server.URL + "/{{asset}}", Assets: map[string]string{"linux-amd64": "acme.tar.gz"}, Checksums: map[string]string{"linux-amd64": "wrong"}}}
	if _, err := manager.Install(context.Background(), requirement); err == nil {
		t.Fatal("bad checksum was accepted")
	}
	current, err := manager.Resolve("acme")
	if err != nil || current.Version != "1.0.0" {
		t.Fatalf("failed update changed selection: %#v %v", current, err)
	}
}

func TestZipExtractionAndArchiveTraversalRejection(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "tool.zip")
	if err := os.WriteFile(archivePath, testZip(t, "nested/acme.exe", "binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "out", "acme.exe")
	if err := extractBinary(archivePath, "tool.zip", "acme.exe", "acme", destination); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(destination); err != nil || string(body) != "binary" {
		t.Fatalf("zip output = %q, %v", body, err)
	}

	traversalPath := filepath.Join(root, "traversal.zip")
	if err := os.WriteFile(traversalPath, testZip(t, "../acme.exe", "malicious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractBinary(traversalPath, "traversal.zip", "acme.exe", "acme", filepath.Join(root, "blocked", "acme.exe")); err == nil {
		t.Fatal("archive path traversal entry was accepted")
	}
}

func TestAdoptRecordsDigestAndGarbageCollectionPreservesCurrent(t *testing.T) {
	t.Setenv("OUTFIT_TOOLCHAIN_HELPER", "1")
	ctx := context.Background()
	root := t.TempDir()
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	managed := filepath.Join(root, "acme"+extension)
	if err := os.WriteFile(managed, helperExecutable(t), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := Manager{Paths: state.Paths{Root: root}, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	requirement := kit.ToolRequirement{ID: "acme", Display: "Acme", Binary: "acme", Version: "1.0.0", Verify: []string{"-test.run=TestToolchainHelperProcess"}}
	receipt, err := manager.Adopt(ctx, requirement, managed)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Adopted || receipt.ActualSHA == "" || receipt.VersionText != "acme 1.2.3" {
		t.Fatalf("adopt receipt = %#v", receipt)
	}

	staleDirectory := filepath.Join(root, "tools", "acme", "0.9.0", runtime.GOOS+"-"+runtime.GOARCH)
	if err := os.MkdirAll(staleDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := []byte(fmt.Sprintf(`{"id":"acme","version":"0.9.0","platform":%q,"path":"/stale"}`, runtime.GOOS+"-"+runtime.GOARCH))
	if err := os.WriteFile(filepath.Join(staleDirectory, "receipt.json"), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := manager.GC()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].Version != "0.9.0" {
		t.Fatalf("removed = %#v", removed)
	}
	current, err := manager.Resolve("acme")
	if err != nil || current.Version != "1.0.0" || !current.Adopted {
		t.Fatalf("current after GC = %#v, %v", current, err)
	}
}

func TestCatalogEstablishesTwentyFiveToolRegistry(t *testing.T) {
	entries := Catalog()
	if len(entries) != 25 {
		t.Fatalf("expected 25 registry entries, got %d", len(entries))
	}
	for _, id := range []string{"gws", "stripe-cli", "github-cli", "wrangler", "kubectl"} {
		entry, ok := CatalogByID(id)
		if !ok || entry.Display == "" || entry.Binary == "" {
			t.Fatalf("missing complete catalog entry %s: %#v", id, entry)
		}
	}
	if GWSRequirement().Version != "0.22.5" || StripeRequirement().Version != "1.50.4" {
		t.Fatal("reference tools are not pinned")
	}
}

// Manifests declare "stripe"/"gws"; their Windows zips carry stripe.exe and
// gws.exe (win-home: "archive does not contain declared binary stripe").
func TestWindowsExtractionFindsTheExeOfADeclaredBinary(t *testing.T) {
	root := t.TempDir()
	for _, declared := range []string{"stripe", "gws"} {
		archivePath := filepath.Join(root, declared+".zip")
		if err := os.WriteFile(archivePath, testZip(t, declared+".exe", "windows-"+declared), 0o600); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, "out", declared+".exe")
		if err := extractBinary(archivePath, declared+"_windows_x86_64.zip", declared, declared, destination); err != nil {
			t.Fatalf("%s: %v", declared, err)
		}
		if body, err := os.ReadFile(destination); err != nil || string(body) != "windows-"+declared {
			t.Fatalf("%s output = %q, %v", declared, body, err)
		}
		// A non-Windows destination still requires the exact declared name.
		if err := extractBinary(archivePath, declared+".zip", declared, declared, filepath.Join(root, "unix", declared)); err == nil {
			t.Fatalf("%s: unix extraction accepted %s.exe", declared, declared)
		}
	}
}
