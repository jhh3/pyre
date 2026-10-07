package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhh3/outfit/internal/kit"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion     = "outfit.jhh3.dev/workspace/v1alpha2"
	LockAPIVersion = "outfit.jhh3.dev/workspace-lock/v1alpha2"
)

type Manifest struct {
	APIVersion string              `yaml:"apiVersion" json:"apiVersion"`
	Name       string              `yaml:"name" json:"name"`
	Packages   []Package           `yaml:"packages,omitempty" json:"packages,omitempty"`
	Connectors []Connector         `yaml:"connectors" json:"connectors"`
	Profiles   map[string][]string `yaml:"profiles,omitempty" json:"profiles,omitempty"`
	Profile    string              `yaml:"profile,omitempty" json:"profile,omitempty"`
	Skills     *bool               `yaml:"skills,omitempty" json:"skills,omitempty"`
	Links      *bool               `yaml:"links,omitempty" json:"links,omitempty"`
}

type Package struct {
	Key  string `yaml:"key" json:"key"`
	Path string `yaml:"path" json:"path"`
}

type Connector struct {
	Name             string   `yaml:"name" json:"name"`
	Package          string   `yaml:"package" json:"package"`
	Mode             string   `yaml:"mode" json:"mode"`
	CredentialMethod string   `yaml:"credentialMethod,omitempty" json:"credentialMethod,omitempty"`
	Capabilities     []string `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Operations       []string `yaml:"operations,omitempty" json:"operations,omitempty"`
	Skills           *bool    `yaml:"skills,omitempty" json:"skills,omitempty"`
	Links            *bool    `yaml:"links,omitempty" json:"links,omitempty"`
	LinkRules        []string `yaml:"linkRules,omitempty" json:"linkRules,omitempty"`
}

type Lock struct {
	APIVersion      string            `yaml:"apiVersion" json:"apiVersion"`
	WorkspaceDigest string            `yaml:"workspaceDigest" json:"workspaceDigest"`
	Profile         string            `yaml:"profile" json:"profile"`
	Packages        []LockedPackage   `yaml:"packages" json:"packages"`
	Connectors      []LockedConnector `yaml:"connectors" json:"connectors"`
	Tools           []LockedTool      `yaml:"tools,omitempty" json:"tools,omitempty"`
	Skills          []LockedSkill     `yaml:"skills,omitempty" json:"skills,omitempty"`
}

type LockedPackage struct {
	Key     string `yaml:"key" json:"key"`
	Version string `yaml:"version" json:"version"`
	Digest  string `yaml:"digest" json:"digest"`
	Source  string `yaml:"source" json:"source"`
}

type LockedConnector struct {
	Name             string   `yaml:"name" json:"name"`
	Package          string   `yaml:"package" json:"package"`
	Mode             string   `yaml:"mode" json:"mode"`
	CredentialMethod string   `yaml:"credentialMethod" json:"credentialMethod"`
	Capabilities     []string `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Operations       []string `yaml:"operations,omitempty" json:"operations,omitempty"`
	Skills           bool     `yaml:"skills" json:"skills"`
	LinkRules        []string `yaml:"linkRules,omitempty" json:"linkRules,omitempty"`
}

type LockedTool struct {
	ID        string            `yaml:"id" json:"id"`
	Version   string            `yaml:"version" json:"version"`
	Checksums map[string]string `yaml:"checksums" json:"checksums"`
}

type LockedSkill struct {
	Connector   string   `yaml:"connector" json:"connector"`
	Source      string   `yaml:"source" json:"source"`
	Repository  string   `yaml:"repository,omitempty" json:"repository,omitempty"`
	Ref         string   `yaml:"ref,omitempty" json:"ref,omitempty"`
	Digest      string   `yaml:"digest,omitempty" json:"digest,omitempty"`
	Tool        string   `yaml:"tool,omitempty" json:"tool,omitempty"`
	ToolVersion string   `yaml:"toolVersion,omitempty" json:"toolVersion,omitempty"`
	Paths       []string `yaml:"paths" json:"paths"`
}

type Loaded struct {
	Root, Path, LockPath, Digest, LockDigest string
	Manifest                                 Manifest
	Lock                                     Lock
}

func Find(start string) (string, string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", "", err
	}
	for {
		path := filepath.Join(current, ".outfit", "workspace.yaml")
		if _, err := os.Stat(path); err == nil {
			return current, path, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", "", nil
		}
		current = parent
	}
}

func Read(start string) (Loaded, error) {
	root, path, err := Find(start)
	if err != nil || path == "" {
		return Loaded{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Loaded{}, err
	}
	var manifest Manifest
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return Loaded{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := Validate(manifest); err != nil {
		return Loaded{}, fmt.Errorf("validate %s: %w", path, err)
	}
	digest := sha256.Sum256(raw)
	loaded := Loaded{Root: root, Path: path, LockPath: filepath.Join(root, ".outfit", "workspace.lock.yaml"), Digest: hex.EncodeToString(digest[:]), Manifest: manifest}
	if locked, err := os.ReadFile(loaded.LockPath); err == nil {
		if err := yaml.Unmarshal(locked, &loaded.Lock); err != nil {
			return Loaded{}, fmt.Errorf("parse %s: %w", loaded.LockPath, err)
		}
		lockDigest := sha256.Sum256(locked)
		loaded.LockDigest = hex.EncodeToString(lockDigest[:])
	} else if !errors.Is(err, os.ErrNotExist) {
		return Loaded{}, err
	}
	return loaded, nil
}

func Validate(value Manifest) error {
	if value.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %s", APIVersion)
	}
	if strings.TrimSpace(value.Name) == "" {
		return errors.New("name is required")
	}
	seen := map[string]bool{}
	for _, item := range value.Connectors {
		if item.Name == "" || item.Package == "" || item.Mode == "" {
			return errors.New("every connector requires name, package, and mode")
		}
		if seen[item.Name] {
			return fmt.Errorf("duplicate connector name %s", item.Name)
		}
		seen[item.Name] = true
		operations := append([]string{}, item.Operations...)
		sort.Strings(operations)
		for index := 1; index < len(operations); index++ {
			if operations[index] == operations[index-1] {
				return fmt.Errorf("connector %s repeats operation %s", item.Name, operations[index])
			}
		}
	}
	if value.Profile != "" {
		members, ok := value.Profiles[value.Profile]
		if !ok {
			return fmt.Errorf("profile %s is not declared", value.Profile)
		}
		for _, name := range members {
			if !seen[name] {
				return fmt.Errorf("profile %s references unknown connector %s", value.Profile, name)
			}
		}
	}
	for _, item := range value.Packages {
		if item.Key == "" || item.Path == "" {
			return errors.New("workspace packages require key and path")
		}
		clean := filepath.Clean(item.Path)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("workspace package %s path must stay inside the repository", item.Key)
		}
		if seen["package\x00"+item.Key] {
			return fmt.Errorf("duplicate workspace package %s", item.Key)
		}
		seen["package\x00"+item.Key] = true
	}
	return nil
}

func (value Manifest) SelectedConnectors() []Connector {
	if value.Profile == "" {
		return append([]Connector{}, value.Connectors...)
	}
	wanted := map[string]bool{}
	for _, name := range value.Profiles[value.Profile] {
		wanted[name] = true
	}
	result := make([]Connector, 0, len(wanted))
	for _, item := range value.Connectors {
		if wanted[item.Name] {
			result = append(result, item)
		}
	}
	return result
}

func Resolve(value Loaded, installed map[string]kit.Loaded, discovered map[string][]string) (Lock, error) {
	result := Lock{APIVersion: LockAPIVersion, WorkspaceDigest: value.Digest, Profile: value.Manifest.Profile}
	sources := map[string]string{}
	for _, declared := range value.Manifest.Packages {
		path, err := PackagePath(value.Root, declared.Path)
		if err != nil {
			return Lock{}, fmt.Errorf("package %s: %w", declared.Key, err)
		}
		loaded, err := kit.Load(path)
		if err != nil {
			return Lock{}, fmt.Errorf("load package %s: %w", declared.Key, err)
		}
		if loaded.Package.Key != declared.Key {
			return Lock{}, fmt.Errorf("package %s contains key %s", declared.Key, loaded.Package.Key)
		}
		installed[declared.Key] = loaded
		sources[declared.Key] = filepath.ToSlash(declared.Path)
	}
	packageKeys := map[string]bool{}
	lockedTools := map[string]LockedTool{}
	for _, connector := range value.Manifest.SelectedConnectors() {
		loaded, ok := installed[connector.Package]
		if !ok {
			return Lock{}, fmt.Errorf("connector %s requires package %s, which is neither installed nor declared by path", connector.Name, connector.Package)
		}
		var mode *kit.Mode
		for index := range loaded.Package.Modes {
			if loaded.Package.Modes[index].ID == connector.Mode {
				mode = &loaded.Package.Modes[index]
				break
			}
		}
		if mode == nil {
			return Lock{}, fmt.Errorf("connector %s selects missing mode %s", connector.Name, connector.Mode)
		}
		available := map[string]bool{}
		for _, operation := range mode.Operations {
			available[operation.Name] = true
		}
		if mode.Executor.Kind == "mcp" {
			for _, operation := range discovered[connector.Name] {
				available[operation] = true
			}
		}
		operations := append([]string{}, connector.Operations...)
		if len(operations) == 0 {
			for name := range available {
				operations = append(operations, name)
			}
		}
		if len(operations) == 0 {
			return Lock{}, fmt.Errorf("connector %s exposes no lockable operations; configure it first or list exact operations", connector.Name)
		}
		for _, operation := range operations {
			if operation == "*" {
				return Lock{}, fmt.Errorf("connector %s uses wildcard operations; list exact operation names", connector.Name)
			}
			if !available[operation] {
				return Lock{}, fmt.Errorf("connector %s allows unknown operation %s", connector.Name, operation)
			}
		}
		sort.Strings(operations)
		var linkRules []string
		if enabled(connector.Links, value.Manifest.Links) {
			availableLinks := map[string]bool{}
			for _, resolver := range loaded.Package.Links {
				availableLinks[resolver.ID] = true
			}
			linkRules = append(linkRules, connector.LinkRules...)
			if len(linkRules) == 0 {
				for id := range availableLinks {
					linkRules = append(linkRules, id)
				}
			}
			for _, id := range linkRules {
				if !availableLinks[id] {
					return Lock{}, fmt.Errorf("connector %s allows unknown link rule %s", connector.Name, id)
				}
			}
			sort.Strings(linkRules)
		}
		method := connector.CredentialMethod
		if method == "" {
			method = mode.DefaultCredentialMethod
		}
		if _, ok := kit.FindCredentialMethod(loaded.Package, method); !ok || !stringContains(mode.CredentialMethods, method) {
			return Lock{}, fmt.Errorf("connector %s selects unavailable credential method %s", connector.Name, method)
		}
		capabilities := append([]string{}, connector.Capabilities...)
		if len(capabilities) == 0 {
			capabilities = append(capabilities, mode.Capabilities...)
		}
		availableCapabilities := map[string]bool{}
		for _, capability := range mode.Capabilities {
			availableCapabilities[capability] = true
		}
		for _, capability := range capabilities {
			if !availableCapabilities[capability] {
				return Lock{}, fmt.Errorf("connector %s selects unavailable capability %s", connector.Name, capability)
			}
		}
		sort.Strings(capabilities)
		skillsEnabled := enabled(connector.Skills, value.Manifest.Skills)
		result.Connectors = append(result.Connectors, LockedConnector{Name: connector.Name, Package: connector.Package, Mode: connector.Mode, CredentialMethod: method, Capabilities: capabilities, Operations: operations, Skills: skillsEnabled, LinkRules: linkRules})
		toolID, toolVersion := "", ""
		if mode.Executor.Tool != "" {
			for _, tool := range loaded.Package.Tools {
				if tool.ID == mode.Executor.Tool {
					lockedTools[tool.ID] = LockedTool{ID: tool.ID, Version: tool.Version, Checksums: cloneMap(tool.Source.Checksums)}
					toolID, toolVersion = tool.ID, tool.Version
				}
			}
		}
		if skillsEnabled {
			selected := map[string]bool{}
			for _, capability := range capabilities {
				selected[capability] = true
			}
			for _, source := range loaded.Package.SkillSources {
				var paths []string
				for _, path := range source.Paths {
					include := true
					for _, capability := range path.Capabilities {
						include = include && selected[capability]
					}
					if include {
						paths = append(paths, path.Path)
					}
				}
				if len(paths) > 0 {
					sort.Strings(paths)
					result.Skills = append(result.Skills, LockedSkill{Connector: connector.Name, Source: source.ID, Repository: source.Repository, Ref: source.Ref, Digest: source.Digest, Tool: toolID, ToolVersion: toolVersion, Paths: paths})
				}
			}
		}
		packageKeys[connector.Package] = true
	}
	for _, tool := range lockedTools {
		result.Tools = append(result.Tools, tool)
	}
	for key := range packageKeys {
		loaded := installed[key]
		source := sources[key]
		if source == "" {
			source = loaded.Source
			if strings.HasPrefix(source, "builtin:") {
				source = "builtin"
			} else {
				return Lock{}, fmt.Errorf("custom package %s must be declared in workspace.packages with a repository-relative path", key)
			}
		}
		result.Packages = append(result.Packages, LockedPackage{Key: key, Version: loaded.Package.Version, Digest: loaded.Digest, Source: source})
	}
	sort.Slice(result.Packages, func(i, j int) bool { return result.Packages[i].Key < result.Packages[j].Key })
	sort.Slice(result.Connectors, func(i, j int) bool { return result.Connectors[i].Name < result.Connectors[j].Name })
	sort.Slice(result.Tools, func(i, j int) bool { return result.Tools[i].ID < result.Tools[j].ID })
	sort.Slice(result.Skills, func(i, j int) bool {
		if result.Skills[i].Connector == result.Skills[j].Connector {
			return result.Skills[i].Source < result.Skills[j].Source
		}
		return result.Skills[i].Connector < result.Skills[j].Connector
	})
	return result, nil
}

func cloneMap(value map[string]string) map[string]string {
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func stringContains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func insideRoot(root, target string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(realRoot, realTarget)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("package path escapes the repository")
	}
	return nil
}

func PackagePath(root, declared string) (string, error) {
	target := filepath.Join(root, declared)
	if err := insideRoot(root, target); err != nil {
		return "", err
	}
	return target, nil
}

func enabled(local, global *bool) bool {
	if local != nil {
		return *local
	}
	if global != nil {
		return *global
	}
	return true
}

func WriteLock(path string, value Lock) error {
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(path, encoded, 0o644)
}

func WriteManifest(path string, value Manifest) error {
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(path, encoded, 0o644)
}

func atomicWrite(path string, value []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".outfit-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(value); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

type Policy struct {
	Path, Digest string
	Connectors   map[string]LockedConnector
	Packages     map[string]LockedPackage
}

func NewPolicy(loaded Loaded) (Policy, error) {
	if loaded.Lock.APIVersion != LockAPIVersion || loaded.Lock.WorkspaceDigest != loaded.Digest {
		return Policy{}, errors.New("workspace lock is missing or stale; run outfit lock, review it, then outfit apply")
	}
	if loaded.LockDigest == "" {
		return Policy{}, errors.New("workspace lock is missing; run outfit lock, review it, then outfit apply")
	}
	packages := map[string]bool{}
	for _, item := range loaded.Lock.Packages {
		decodedDigest, digestErr := hex.DecodeString(item.Digest)
		if item.Key == "" || digestErr != nil || len(decodedDigest) != sha256.Size || packages[item.Key] {
			return Policy{}, fmt.Errorf("workspace lock contains an invalid or duplicate package %q", item.Key)
		}
		packages[item.Key] = true
	}
	policy := Policy{Path: loaded.Path, Digest: loaded.ApprovalDigest(), Connectors: map[string]LockedConnector{}, Packages: map[string]LockedPackage{}}
	for _, item := range loaded.Lock.Packages {
		policy.Packages[item.Key] = item
	}
	for _, item := range loaded.Lock.Connectors {
		if item.Name == "" || item.Package == "" || item.Mode == "" || !packages[item.Package] {
			return Policy{}, fmt.Errorf("workspace lock contains an invalid connector %q", item.Name)
		}
		if _, exists := policy.Connectors[item.Name]; exists {
			return Policy{}, fmt.Errorf("workspace lock repeats connector %s", item.Name)
		}
		if len(item.Operations) == 0 {
			return Policy{}, fmt.Errorf("workspace lock connector %s has no operations", item.Name)
		}
		seenOperations := map[string]bool{}
		for _, operation := range item.Operations {
			if operation == "" || operation == "*" || seenOperations[operation] {
				return Policy{}, fmt.Errorf("workspace lock contains an invalid operation for %s", item.Name)
			}
			seenOperations[operation] = true
		}
		seenLinks := map[string]bool{}
		for _, rule := range item.LinkRules {
			if rule == "" || seenLinks[rule] {
				return Policy{}, fmt.Errorf("workspace lock contains an invalid link rule for %s", item.Name)
			}
			seenLinks[rule] = true
		}
		seenCapabilities := map[string]bool{}
		for _, capability := range item.Capabilities {
			if capability == "" || seenCapabilities[capability] {
				return Policy{}, fmt.Errorf("workspace lock contains an invalid capability for %s", item.Name)
			}
			seenCapabilities[capability] = true
		}
		policy.Connectors[item.Name] = item
	}
	tools := map[string]string{}
	for _, item := range loaded.Lock.Tools {
		if _, exists := tools[item.ID]; item.ID == "" || item.Version == "" || exists {
			return Policy{}, fmt.Errorf("workspace lock contains an invalid or duplicate tool %q", item.ID)
		}
		tools[item.ID] = item.Version
		if len(item.Checksums) == 0 {
			return Policy{}, fmt.Errorf("workspace lock tool %s has no platform checksums", item.ID)
		}
		for platform, digest := range item.Checksums {
			decoded, err := hex.DecodeString(digest)
			if platform == "" || err != nil || len(decoded) != sha256.Size {
				return Policy{}, fmt.Errorf("workspace lock tool %s has invalid checksum for %s", item.ID, platform)
			}
		}
	}
	skills := map[string]bool{}
	for _, item := range loaded.Lock.Skills {
		connector, ok := policy.Connectors[item.Connector]
		key := item.Connector + "\x00" + item.Source
		if !ok || !connector.Skills || item.Source == "" || len(item.Paths) == 0 || skills[key] {
			return Policy{}, fmt.Errorf("workspace lock contains an invalid or duplicate skill source %s for %s", item.Source, item.Connector)
		}
		if (item.Tool == "") != (item.ToolVersion == "") {
			return Policy{}, fmt.Errorf("workspace lock skill source %s has incomplete tool provenance", item.Source)
		}
		if item.Tool != "" && tools[item.Tool] != item.ToolVersion {
			return Policy{}, fmt.Errorf("workspace lock skill source %s has invalid tool provenance", item.Source)
		}
		skills[key] = true
		if item.Repository != "" {
			decoded, err := hex.DecodeString(item.Digest)
			if item.Ref == "" || err != nil || len(decoded) != sha256.Size {
				return Policy{}, fmt.Errorf("workspace lock skill source %s has an invalid ref or digest", item.Source)
			}
		}
		seenPaths := map[string]bool{}
		for _, path := range item.Paths {
			if path == "" || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") || seenPaths[path] {
				return Policy{}, fmt.Errorf("workspace lock skill source %s has an invalid path", item.Source)
			}
			seenPaths[path] = true
		}
	}
	return policy, nil
}

func (policy Policy) PackageDigest(key string) (string, bool) {
	item, ok := policy.Packages[key]
	return item.Digest, ok
}

func (loaded Loaded) ApprovalDigest() string {
	digest := sha256.Sum256([]byte(loaded.Digest + "\x00" + loaded.LockDigest))
	return hex.EncodeToString(digest[:])
}

func (policy Policy) AllowsConfiguration(name, packageKey, mode string) bool {
	item, ok := policy.Connectors[name]
	return ok && item.Package == packageKey && item.Mode == mode
}

func (policy Policy) Allows(name string) bool {
	_, ok := policy.Connectors[name]
	return ok
}

func (policy Policy) AllowsOperation(name, operation string) bool {
	item, ok := policy.Connectors[name]
	if !ok {
		return false
	}
	for _, allowed := range item.Operations {
		if allowed == operation {
			return true
		}
	}
	return false
}

func (policy Policy) Skills(name string) bool {
	item, ok := policy.Connectors[name]
	return ok && item.Skills
}
func (policy Policy) Links(name string) bool {
	item, ok := policy.Connectors[name]
	return ok && len(item.LinkRules) > 0
}

func (policy Policy) AllowsLink(name, rule string) bool {
	item, ok := policy.Connectors[name]
	if !ok {
		return false
	}
	for _, allowed := range item.LinkRules {
		if allowed == rule {
			return true
		}
	}
	return false
}

func ApprovalKey(root string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(root)))
	return "workspace.approval:" + hex.EncodeToString(digest[:])
}
