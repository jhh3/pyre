package skillmanager

import (
	"archive/tar"
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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/outfit/internal/agentguide"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/skillgen"
	"github.com/jhh3/outfit/internal/state"
)

const maxSourceBytes = 64 << 20

type Receipt struct {
	ID            string    `json:"id"`
	Connection    string    `json:"connection"`
	Connector     string    `json:"connector"`
	Host          string    `json:"host"`
	Kind          string    `json:"kind"`
	SourceID      string    `json:"sourceId,omitempty"`
	Source        string    `json:"source"`
	Ref           string    `json:"ref,omitempty"`
	Digest        string    `json:"digest"`
	Path          string    `json:"path"`
	InstalledPath string    `json:"installedPath"`
	Enabled       bool      `json:"enabled"`
	SyncedAt      time.Time `json:"syncedAt"`
}

type Manager struct {
	Paths  state.Paths
	Client *http.Client
	Roots  map[string]string
}

func (manager Manager) VerifySource(ctx context.Context, source kit.SkillSource) (int, error) {
	root, err := manager.fetch(ctx, source)
	if err != nil {
		return 0, err
	}
	for _, declared := range source.Paths {
		path := filepath.Join(root, filepath.FromSlash(declared.Path), "SKILL.md")
		content, err := os.ReadFile(path)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", declared.Path, err)
		}
		text := string(content)
		if !strings.HasPrefix(text, "---\n") || !strings.Contains(text, "\nname:") || !strings.Contains(text, "\ndescription:") {
			return 0, fmt.Errorf("%s has an invalid SKILL.md frontmatter", declared.Path)
		}
	}
	return len(source.Paths), nil
}

type stagedSkill struct {
	destination string
	staged      string
	receipt     Receipt
}

func (manager Manager) Sync(ctx context.Context, host string, connections []state.Connection, packages map[string]kit.Package, tools map[string][]state.Tool, allowed func(string, string) bool) ([]Receipt, error) {
	roots, err := manager.roots(host)
	if err != nil {
		return nil, err
	}
	preferences, err := manager.preferences()
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		if err := os.MkdirAll(root.Path, 0o700); err != nil {
			return nil, err
		}
	}
	var receipts []Receipt
	var staged []stagedSkill
	defer func() {
		for _, item := range staged {
			_ = os.RemoveAll(item.staged)
		}
	}()
	cache := map[string]string{}
	for _, connection := range connections {
		if connection.Status != "active" {
			continue
		}
		pkg, ok := packages[connection.ConnectorKey]
		if !ok {
			continue
		}
		mode, ok := kit.FindMode(pkg, connection.SelectedMode)
		if !ok {
			continue
		}
		capabilities := selectedCapabilities(connection, mode)
		for _, source := range pkg.SkillSources {
			if source.Kind != "github-directory" {
				continue
			}
			cacheKey := source.Repository + "@" + source.Ref + "#" + source.Digest
			cacheRoot := cache[cacheKey]
			if cacheRoot == "" {
				cacheRoot, err = manager.fetch(ctx, source)
				if err != nil {
					return nil, fmt.Errorf("skill source %s: %w", source.ID, err)
				}
				cache[cacheKey] = cacheRoot
			}
			for _, skillPath := range source.Paths {
				if !pathSelected(skillPath, capabilities, connection.Name, allowed) {
					continue
				}
				for _, root := range roots {
					preferenceKey := strings.Join([]string{connection.Name, source.ID, skillPath.Path, root.Host}, "|")
					if enabled, exists := preferences[preferenceKey]; exists && !enabled {
						continue
					}
					id := source.ID + "-" + filepath.Base(skillPath.Path)
					destination := filepath.Join(root.Path, safeName("outfit-"+connection.Name+"-"+id))
					stagedPath, err := os.MkdirTemp(root.Path, ".outfit-skill-stage-")
					if err != nil {
						return nil, err
					}
					if err := copySkill(filepath.Join(cacheRoot, filepath.FromSlash(skillPath.Path)), stagedPath, connection); err != nil {
						_ = os.RemoveAll(stagedPath)
						return nil, err
					}
					receipt := Receipt{ID: id, Connection: connection.Name, Connector: connection.ConnectorKey, Host: root.Host, Kind: source.Kind, SourceID: source.ID, Source: source.Repository, Ref: source.Ref, Digest: source.Digest, Path: skillPath.Path, InstalledPath: destination, Enabled: true, SyncedAt: time.Now().UTC()}
					encoded, _ := json.MarshalIndent(receipt, "", "  ")
					if err := os.WriteFile(filepath.Join(stagedPath, ".outfit-upstream.json"), append(encoded, '\n'), 0o600); err != nil {
						_ = os.RemoveAll(stagedPath)
						return nil, err
					}
					staged = append(staged, stagedSkill{destination: destination, staged: stagedPath, receipt: receipt})
					receipts = append(receipts, receipt)
				}
			}
		}
	}
	rootPaths := make([]string, 0, len(roots))
	for _, root := range roots {
		rootPaths = append(rootPaths, root.Path)
	}
	if _, err := skillgen.SyncRoots(rootPaths, connections, packages, tools, allowed); err != nil {
		return nil, err
	}
	if err := activateSkills(roots, staged); err != nil {
		return nil, err
	}
	sort.Slice(receipts, func(i, j int) bool {
		if receipts[i].Connection == receipts[j].Connection {
			return receipts[i].ID < receipts[j].ID
		}
		return receipts[i].Connection < receipts[j].Connection
	})
	return receipts, nil
}

func activateSkills(roots []hostRoot, staged []stagedSkill) error {
	type activatedSkill struct {
		destination string
		backup      string
	}
	desired := map[string]bool{}
	var activated []activatedSkill
	rollback := func() {
		for index := len(activated) - 1; index >= 0; index-- {
			item := activated[index]
			_ = os.RemoveAll(item.destination)
			if item.backup != "" {
				_ = os.Rename(item.backup, item.destination)
			}
		}
	}
	for _, item := range staged {
		desired[item.destination] = true
		backup := ""
		if _, err := os.Stat(item.destination); err == nil {
			holder, holderErr := os.MkdirTemp(filepath.Dir(item.destination), ".outfit-skill-old-")
			if holderErr != nil {
				rollback()
				return holderErr
			}
			if err := os.Remove(holder); err != nil {
				rollback()
				return err
			}
			backup = holder
			if err := os.Rename(item.destination, backup); err != nil {
				rollback()
				return err
			}
		}
		if err := os.Rename(item.staged, item.destination); err != nil {
			if backup != "" {
				_ = os.Rename(backup, item.destination)
			}
			rollback()
			return err
		}
		activated = append(activated, activatedSkill{destination: item.destination, backup: backup})
	}
	for _, item := range activated {
		if item.backup != "" {
			_ = os.RemoveAll(item.backup)
		}
	}
	for _, root := range roots {
		legacyEntries, _ := filepath.Glob(filepath.Join(root.Path, "cw-*"))
		for _, entry := range legacyEntries {
			if _, err := os.Stat(filepath.Join(entry, ".cw-upstream.json")); err == nil {
				if err := os.RemoveAll(entry); err != nil {
					return err
				}
			}
		}
		entries, _ := filepath.Glob(filepath.Join(root.Path, "outfit-*"))
		for _, entry := range entries {
			if desired[entry] {
				continue
			}
			if _, err := os.Stat(filepath.Join(entry, ".outfit-upstream.json")); err == nil {
				if err := os.RemoveAll(entry); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (manager Manager) Inventory(host string) ([]Receipt, error) {
	roots, err := manager.roots(host)
	if err != nil {
		return nil, err
	}
	var result []Receipt
	for _, root := range roots {
		markers, _ := filepath.Glob(filepath.Join(root.Path, "outfit-*", ".outfit-upstream.json"))
		for _, marker := range markers {
			bytes, err := os.ReadFile(marker)
			if err != nil {
				return nil, err
			}
			var receipt Receipt
			if err := json.Unmarshal(bytes, &receipt); err != nil {
				return nil, err
			}
			result = append(result, receipt)
		}
		generated, _ := filepath.Glob(filepath.Join(root.Path, "outfit-*", ".outfit-generated"))
		for _, marker := range generated {
			name := strings.TrimPrefix(filepath.Base(filepath.Dir(marker)), "outfit-")
			result = append(result, Receipt{ID: "generated", Connection: name, Host: root.Host, Kind: "generated", Source: "connector package", InstalledPath: filepath.Dir(marker), Enabled: true})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Host == result[j].Host && result[i].Connection == result[j].Connection {
			return result[i].ID < result[j].ID
		}
		if result[i].Host == result[j].Host {
			return result[i].Connection < result[j].Connection
		}
		return result[i].Host < result[j].Host
	})
	return result, nil
}

func (manager Manager) SetEnabled(connection, source, path, host string, enabled bool) error {
	preferences, err := manager.preferences()
	if err != nil {
		return err
	}
	preferences[strings.Join([]string{connection, source, path, host}, "|")] = enabled
	encoded, _ := json.MarshalIndent(preferences, "", "  ")
	directory := filepath.Join(manager.Paths.Root, "skills")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "preferences.json"), append(encoded, '\n'), 0o600)
}

func (manager Manager) ExportPreferences() (map[string]bool, error) { return manager.preferences() }

func (manager Manager) ImportPreferences(preferences map[string]bool) error {
	encoded, _ := json.MarshalIndent(preferences, "", "  ")
	directory := filepath.Join(manager.Paths.Root, "skills")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "preferences.json"), append(encoded, '\n'), 0o600)
}

func (manager Manager) preferences() (map[string]bool, error) {
	result := map[string]bool{}
	bytes, err := os.ReadFile(filepath.Join(manager.Paths.Root, "skills", "preferences.json"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(bytes, &result); err != nil {
		return nil, err
	}
	return result, nil
}

type hostRoot struct{ Host, Path string }

func (manager Manager) roots(host string) ([]hostRoot, error) {
	if root := manager.Roots[host]; root != "" {
		return []hostRoot{{Host: host, Path: root}}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var roots []hostRoot
	if host == "both" || host == "codex" {
		root := manager.Roots["codex"]
		if root == "" {
			root = os.Getenv("CODEX_HOME")
			if root == "" {
				root = filepath.Join(home, ".codex")
			}
			root = filepath.Join(root, "skills")
		}
		roots = append(roots, hostRoot{Host: "codex", Path: root})
	}
	if host == "both" || host == "claude" {
		root := manager.Roots["claude"]
		if root == "" {
			root = os.Getenv("CLAUDE_CONFIG_DIR")
			if root == "" {
				root = filepath.Join(home, ".claude")
			}
			root = filepath.Join(root, "skills")
		}
		roots = append(roots, hostRoot{Host: "claude", Path: root})
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("host must be codex, claude, or both")
	}
	return roots, nil
}

func (manager Manager) fetch(ctx context.Context, source kit.SkillSource) (string, error) {
	key := sha256.Sum256([]byte(source.Repository + "\x00" + source.Ref + "\x00" + source.Digest))
	cacheRoot := filepath.Join(manager.Paths.Root, "skill-sources", hex.EncodeToString(key[:]))
	complete := filepath.Join(cacheRoot, ".complete")
	if _, err := os.Stat(complete); err == nil {
		return filepath.Join(cacheRoot, "tree"), nil
	}
	client := manager.Client
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	url := fmt.Sprintf("https://github.com/%s/archive/%s.tar.gz", source.Repository, source.Ref)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("download returned %s", response.Status)
	}
	partialRoot := filepath.Join(manager.Paths.Root, "downloads", "partial")
	if err := os.MkdirAll(partialRoot, 0o700); err != nil {
		return "", err
	}
	archive, err := os.CreateTemp(partialRoot, "skills-*.tar.gz")
	if err != nil {
		return "", err
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(response.Body, maxSourceBytes+1))
	closeErr := archive.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if written > maxSourceBytes {
		return "", errors.New("skill source exceeds 64 MiB")
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, source.Digest) {
		return "", fmt.Errorf("source digest mismatch: expected %s, got %s", source.Digest, actual)
	}
	temporary, err := os.MkdirTemp(filepath.Dir(cacheRoot), ".skills-*")
	if err != nil {
		if mkErr := os.MkdirAll(filepath.Dir(cacheRoot), 0o700); mkErr != nil {
			return "", mkErr
		}
		temporary, err = os.MkdirTemp(filepath.Dir(cacheRoot), ".skills-*")
	}
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	if err := extractSkillTree(archivePath, filepath.Join(temporary, "tree")); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(temporary, ".complete"), []byte(actual+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, cacheRoot); err != nil {
		if _, statErr := os.Stat(complete); statErr != nil {
			return "", err
		}
	}
	return filepath.Join(cacheRoot, "tree"), nil
}

func extractSkillTree(path, destination string) error {
	file, err := os.Open(path)
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
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		parts := strings.SplitN(name, "/", 2)
		if len(parts) != 2 || strings.HasPrefix(parts[1], "../") || strings.HasPrefix(parts[1], "/") {
			continue
		}
		relative := parts[1]
		if relative != "skills" && !strings.HasPrefix(relative, "skills/") {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size > 4<<20 {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(reader, 4<<20))
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
}

func copySkill(source, destination string, connection state.Connection) error {
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("skill path %s is missing", source)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == "." {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("skill source contains a symlink")
		}
		bytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if relative == "SKILL.md" {
			bytes = scopeSkill(bytes, connection)
		}
		return os.WriteFile(target, bytes, 0o600)
	})
}

func scopeSkill(content []byte, connection state.Connection) []byte {
	note := "\n" + agentguide.ScopedUpstreamNotice(connection.Name) + "\n"
	text := string(content)
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---\n"); end >= 0 {
			position := 4 + end + len("\n---\n")
			return []byte(text[:position] + note + text[position:])
		}
	}
	return []byte(note + "\n" + text)
}

func selectedCapabilities(connection state.Connection, mode kit.Mode) map[string]bool {
	result := map[string]bool{}
	value := connection.SafeIdentity["capabilities"]
	if value == "" {
		for _, capability := range mode.Capabilities {
			result[capability] = true
		}
		return result
	}
	for _, capability := range strings.Split(value, ",") {
		result[strings.TrimSpace(capability)] = true
	}
	return result
}

func pathSelected(path kit.SkillPath, capabilities map[string]bool, connection string, allowed func(string, string) bool) bool {
	for _, capability := range path.Capabilities {
		if !capabilities[capability] {
			return false
		}
	}
	for _, operation := range path.Operations {
		if allowed != nil && !allowed(connection, operation) {
			return false
		}
	}
	return true
}

func safeName(value string) string {
	var result strings.Builder
	for _, char := range strings.ToLower(value) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			result.WriteRune(char)
		} else {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}
