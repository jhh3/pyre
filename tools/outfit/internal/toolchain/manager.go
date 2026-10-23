package toolchain

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
)

const maxArtifactBytes = 512 << 20

type Receipt struct {
	ID          string    `json:"id"`
	Display     string    `json:"display"`
	Binary      string    `json:"binary"`
	Version     string    `json:"version"`
	Platform    string    `json:"platform"`
	Source      string    `json:"source"`
	Asset       string    `json:"asset,omitempty"`
	ExpectedSHA string    `json:"expectedSha256,omitempty"`
	ActualSHA   string    `json:"actualSha256"`
	Path        string    `json:"path"`
	VersionText string    `json:"versionText"`
	Adopted     bool      `json:"adopted"`
	InstalledAt time.Time `json:"installedAt"`
}

type Manager struct {
	Paths      state.Paths
	Client     *http.Client
	GOOS       string
	GOARCH     string
	BeforeSwap func() error
}

func (manager Manager) platform() string {
	goos, arch := manager.GOOS, manager.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	return goos + "-" + arch
}

func (manager Manager) root() string { return filepath.Join(manager.Paths.Root, "tools") }

func (manager Manager) Install(ctx context.Context, requirement kit.ToolRequirement) (Receipt, error) {
	if requirement.Source.Kind == "adopt-only" {
		return Receipt{}, fmt.Errorf("%s is adopt-only; use outfit tool adopt", requirement.ID)
	}
	platform := manager.platform()
	asset := requirement.Source.Assets[platform]
	expected := strings.ToLower(requirement.Source.Checksums[platform])
	if asset == "" || expected == "" {
		return Receipt{}, fmt.Errorf("%s %s has no verified artifact for %s", requirement.ID, requirement.Version, platform)
	}
	source, err := artifactURL(requirement, asset)
	if err != nil {
		return Receipt{}, err
	}
	client := manager.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return Receipt{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Receipt{}, fmt.Errorf("download %s: %w", requirement.ID, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Receipt{}, fmt.Errorf("download %s: %s", requirement.ID, response.Status)
	}
	partialRoot := filepath.Join(manager.Paths.Root, "downloads", "partial")
	if err := os.MkdirAll(partialRoot, 0o700); err != nil {
		return Receipt{}, err
	}
	partial, err := os.CreateTemp(partialRoot, requirement.ID+"-*")
	if err != nil {
		return Receipt{}, err
	}
	partialPath := partial.Name()
	defer os.Remove(partialPath)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(partial, hash), io.LimitReader(response.Body, maxArtifactBytes+1))
	closeErr := partial.Close()
	if copyErr != nil {
		return Receipt{}, copyErr
	}
	if closeErr != nil {
		return Receipt{}, closeErr
	}
	if written > maxArtifactBytes {
		return Receipt{}, errors.New("tool artifact exceeds the 512 MiB limit")
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return Receipt{}, fmt.Errorf("%s checksum mismatch: expected %s, got %s", requirement.ID, expected, actual)
	}
	temporary, err := os.MkdirTemp(manager.root(), "."+requirement.ID+"-install-*")
	if err != nil {
		if mkErr := os.MkdirAll(manager.root(), 0o700); mkErr != nil {
			return Receipt{}, mkErr
		}
		temporary, err = os.MkdirTemp(manager.root(), "."+requirement.ID+"-install-*")
	}
	if err != nil {
		return Receipt{}, err
	}
	defer os.RemoveAll(temporary)
	binaryPath := filepath.Join(temporary, requirement.Binary)
	if strings.HasPrefix(platform, "windows-") && !strings.HasSuffix(strings.ToLower(binaryPath), ".exe") {
		binaryPath += ".exe"
	}
	if err := extractBinary(partialPath, asset, requirement.Source.BinaryPath, requirement.Binary, binaryPath); err != nil {
		return Receipt{}, err
	}
	if err := os.Chmod(binaryPath, 0o700); err != nil {
		return Receipt{}, err
	}
	versionText, err := verifyBinary(ctx, binaryPath, requirement.Verify)
	if err != nil {
		return Receipt{}, fmt.Errorf("verify %s: %w", requirement.ID, err)
	}
	receipt := Receipt{ID: requirement.ID, Display: requirement.Display, Binary: requirement.Binary, Version: requirement.Version, Platform: platform, Source: source, Asset: asset, ExpectedSHA: expected, ActualSHA: actual, Path: filepath.Join(manager.root(), requirement.ID, requirement.Version, platform, filepath.Base(binaryPath)), VersionText: versionText, InstalledAt: time.Now().UTC()}
	receiptBytes, _ := json.MarshalIndent(receipt, "", "  ")
	if err := os.WriteFile(filepath.Join(temporary, "receipt.json"), append(receiptBytes, '\n'), 0o600); err != nil {
		return Receipt{}, err
	}
	if manager.BeforeSwap != nil {
		if err := manager.BeforeSwap(); err != nil {
			return Receipt{}, err
		}
	}
	final := filepath.Dir(receipt.Path)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return Receipt{}, err
	}
	if _, err := os.Stat(final); err == nil {
		stored, readErr := readReceipt(filepath.Join(final, "receipt.json"))
		if readErr == nil && stored.ActualSHA == receipt.ActualSHA {
			if err := manager.Use(requirement.ID, requirement.Version, platform); err != nil {
				return Receipt{}, err
			}
			return stored, nil
		}
		return Receipt{}, fmt.Errorf("tool destination %s already exists with different contents", final)
	}
	if err := os.Rename(temporary, final); err != nil {
		return Receipt{}, err
	}
	if err := manager.Use(requirement.ID, requirement.Version, platform); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (manager Manager) Adopt(ctx context.Context, requirement kit.ToolRequirement, path string) (Receipt, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Receipt{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() {
		return Receipt{}, fmt.Errorf("adopt %s: executable not found", path)
	}
	versionText, err := verifyBinary(ctx, absolute, requirement.Verify)
	if err != nil {
		return Receipt{}, err
	}
	file, err := os.Open(absolute)
	if err != nil {
		return Receipt{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return Receipt{}, copyErr
	}
	if closeErr != nil {
		return Receipt{}, closeErr
	}
	receipt := Receipt{ID: requirement.ID, Display: requirement.Display, Binary: requirement.Binary, Version: requirement.Version, Platform: manager.platform(), Source: absolute, ActualSHA: hex.EncodeToString(hash.Sum(nil)), Path: absolute, VersionText: versionText, Adopted: true, InstalledAt: time.Now().UTC()}
	directory := filepath.Join(manager.root(), requirement.ID, requirement.Version, receipt.Platform)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Receipt{}, err
	}
	encoded, _ := json.MarshalIndent(receipt, "", "  ")
	if err := os.WriteFile(filepath.Join(directory, "receipt.json"), append(encoded, '\n'), 0o600); err != nil {
		return Receipt{}, err
	}
	if err := manager.Use(requirement.ID, requirement.Version, receipt.Platform); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (manager Manager) Use(id, version, platform string) error {
	selection := struct {
		Version  string `json:"version"`
		Platform string `json:"platform"`
	}{Version: version, Platform: platform}
	encoded, _ := json.MarshalIndent(selection, "", "  ")
	directory := filepath.Join(manager.root(), id)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".current-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(directory, "current.json"))
}

func (manager Manager) Resolve(id string) (Receipt, error) {
	var selection struct {
		Version  string `json:"version"`
		Platform string `json:"platform"`
	}
	bytes, err := os.ReadFile(filepath.Join(manager.root(), id, "current.json"))
	if err != nil {
		return Receipt{}, err
	}
	if err := json.Unmarshal(bytes, &selection); err != nil {
		return Receipt{}, err
	}
	return readReceipt(filepath.Join(manager.root(), id, selection.Version, selection.Platform, "receipt.json"))
}

func (manager Manager) ResolveRequirement(requirement kit.ToolRequirement) (string, error) {
	receipt, err := manager.Resolve(requirement.ID)
	if err != nil {
		return "", fmt.Errorf("managed tool %s is not installed; run outfit tool install %s", requirement.ID, requirement.ID)
	}
	if receipt.Version != requirement.Version {
		return "", fmt.Errorf("managed tool %s is %s but this connector requires %s; run outfit tool use %s %s", requirement.ID, receipt.Version, requirement.Version, requirement.ID, requirement.Version)
	}
	if expected := requirement.Source.Checksums[receipt.Platform]; expected != "" && !receipt.Adopted && !strings.EqualFold(expected, receipt.ActualSHA) {
		return "", fmt.Errorf("managed tool %s receipt does not match the connector checksum", requirement.ID)
	}
	if _, err := os.Stat(receipt.Path); err != nil {
		return "", fmt.Errorf("managed tool %s binary is missing; run outfit tool repair %s", requirement.ID, requirement.ID)
	}
	return receipt.Path, nil
}

func (manager Manager) List() ([]Receipt, error) {
	paths, err := filepath.Glob(filepath.Join(manager.root(), "*", "*", "*", "receipt.json"))
	if err != nil {
		return nil, err
	}
	result := make([]Receipt, 0, len(paths))
	for _, path := range paths {
		receipt, err := readReceipt(path)
		if err != nil {
			return nil, err
		}
		result = append(result, receipt)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ID == result[j].ID {
			return result[i].Version < result[j].Version
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (manager Manager) Remove(id, version, platform string) error {
	if current, err := manager.Resolve(id); err == nil && current.Version == version && current.Platform == platform {
		return fmt.Errorf("%s %s is active; select another version before removing it", id, version)
	}
	target := filepath.Join(manager.root(), id, version, platform)
	relative, err := filepath.Rel(manager.root(), target)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("invalid managed tool removal target")
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	_ = os.Remove(filepath.Dir(target))
	return nil
}

func (manager Manager) GC() ([]Receipt, error) {
	receipts, err := manager.List()
	if err != nil {
		return nil, err
	}
	var removed []Receipt
	for _, receipt := range receipts {
		current, currentErr := manager.Resolve(receipt.ID)
		if currentErr == nil && current.Version == receipt.Version && current.Platform == receipt.Platform {
			continue
		}
		if err := manager.Remove(receipt.ID, receipt.Version, receipt.Platform); err != nil {
			return removed, err
		}
		removed = append(removed, receipt)
	}
	return removed, nil
}

func artifactURL(requirement kit.ToolRequirement, asset string) (string, error) {
	switch requirement.Source.Kind {
	case "github-release":
		tag := requirement.Source.Tag
		if tag == "" {
			tag = "v" + requirement.Version
		}
		tag = strings.ReplaceAll(tag, "{{version}}", requirement.Version)
		return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", requirement.Source.Repository, tag, asset), nil
	case "fixed-url":
		return strings.ReplaceAll(requirement.Source.URL, "{{asset}}", asset), nil
	default:
		return "", fmt.Errorf("unsupported managed tool source %q", requirement.Source.Kind)
	}
}

func verifyBinary(ctx context.Context, path string, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("version verification command is empty")
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Env = append(os.Environ(), "NO_COLOR=1")
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return strings.TrimSpace(string(output)), nil
}

// extractBinary copies the declared binary out of the downloaded artifact.
// A Windows destination (ending in .exe) also accepts the declared path with
// .exe appended: manifests name the binary once ("stripe", "gws") while
// Windows archives carry "stripe.exe" and "gws.exe".
func extractBinary(archivePath, asset, declaredPath, binary, destination string) error {
	wanted := declaredPath
	if wanted == "" {
		wanted = binary
	}
	candidates := []string{wanted}
	if strings.HasSuffix(strings.ToLower(destination), ".exe") && !strings.HasSuffix(strings.ToLower(wanted), ".exe") {
		candidates = append(candidates, wanted+".exe")
	}
	matches := func(name string) bool {
		for _, candidate := range candidates {
			if archiveMatch(name, candidate) {
				return true
			}
		}
		return false
	}
	if strings.HasSuffix(asset, ".tar.gz") || strings.HasSuffix(asset, ".tgz") {
		file, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer file.Close()
		compressed, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer compressed.Close()
		reader := tar.NewReader(compressed)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if header.Typeflag != tar.TypeReg || !matches(header.Name) {
				continue
			}
			return writeExtracted(destination, reader)
		}
		return fmt.Errorf("archive does not contain declared binary %s", wanted)
	}
	if strings.HasSuffix(asset, ".zip") {
		reader, err := zip.OpenReader(archivePath)
		if err != nil {
			return err
		}
		defer reader.Close()
		for _, file := range reader.File {
			if file.FileInfo().IsDir() || !matches(file.Name) {
				continue
			}
			opened, err := file.Open()
			if err != nil {
				return err
			}
			err = writeExtracted(destination, opened)
			opened.Close()
			return err
		}
		return fmt.Errorf("archive does not contain declared binary %s", wanted)
	}
	return copyFile(archivePath, destination)
}

func archiveMatch(name, wanted string) bool {
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return false
	}
	wanted = filepath.ToSlash(filepath.Clean(wanted))
	return clean == wanted || (filepath.Base(clean) == filepath.Base(wanted) && !strings.Contains(wanted, "/"))
}

func writeExtracted(destination string, source io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, io.LimitReader(source, maxArtifactBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func copyFile(source, destination string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	return writeExtracted(destination, file)
}

func readReceipt(path string) (Receipt, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return Receipt{}, err
	}
	var receipt Receipt
	if err := json.Unmarshal(bytes, &receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}
