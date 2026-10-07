package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
)

func TestResolvePinsPackageModeOperationsAndPolicy(t *testing.T) {
	root := t.TempDir()
	manifest := Manifest{APIVersion: APIVersion, Name: "demo", Connectors: []Connector{{Name: "issues", Package: "github", Mode: "rest", Operations: []string{"get_issue"}, LinkRules: []string{"issue"}}}, Profiles: map[string][]string{"dev": {"issues"}}, Profile: "dev"}
	path := filepath.Join(root, ".outfit", "workspace.yaml")
	if err := WriteManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := parsedPackage(t, `apiVersion: outfit.jhh3.dev/connector/v1alpha2
key: github
display: GitHub
version: 1.0.0
credentialMethods:
  - id: no-auth
    label: No auth
    description: Public test endpoint
    recommended: true
    custody: none
modes:
  - id: rest
    label: REST
    description: REST
    recommendation: {rank: 1, reason: Test REST mode}
    executor: {kind: http, baseUrl: "https://api.github.com/"}
    credentialMethods: [no-auth]
    defaultCredentialMethod: no-auth
    operations:
      - name: get_issue
        description: Read issue
        class: read
        http: {method: GET, path: "/issues/:id"}
    verify: get_issue
links:
  - id: issue
    resourceType: issue
    patterns:
      - hosts: [github.com]
        path: '/issues/:id'
        segments: {id: slug}
    lookup: {mode: rest, operation: get_issue, arguments: {id: id}}
    presentation: {canonicalId: '$.id'}
skill: {summary: GitHub}
`)
	locked, err := Resolve(loaded, map[string]kit.Loaded{"github": pkg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(locked.Connectors) != 1 || locked.Connectors[0].CredentialMethod != "no-auth" || locked.Connectors[0].Operations[0] != "get_issue" || locked.Connectors[0].LinkRules[0] != "issue" || locked.Packages[0].Digest != pkg.Digest {
		t.Fatalf("unexpected lock: %#v", locked)
	}
	loaded.Lock = locked
	loaded.LockDigest = "locked"
	policy, err := NewPolicy(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.AllowsOperation("issues", "get_issue") || policy.AllowsOperation("issues", "delete_issue") {
		t.Fatal("operation policy was not exact")
	}
	if !policy.AllowsLink("issues", "issue") || policy.AllowsLink("issues", "pull_request") {
		t.Fatal("link rule policy was not exact")
	}
}

func TestResolvePinsExactDiscoveredMCPTools(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".outfit", "workspace.yaml")
	manifest := Manifest{APIVersion: APIVersion, Name: "demo", Connectors: []Connector{{Name: "remote", Package: "remote-mcp", Mode: "mcp"}}}
	if err := WriteManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := parsedPackage(t, `apiVersion: outfit.jhh3.dev/connector/v1alpha2
key: remote-mcp
display: Remote MCP
version: 1.0.0
credentialMethods:
  - id: no-auth
    label: No auth
    description: Public test endpoint
    recommended: true
    custody: none
modes:
  - id: mcp
    label: MCP
    description: Remote tools
    recommendation: {rank: 1, reason: Test MCP mode}
    executor: {kind: mcp, endpoint: "https://mcp.example.test/mcp"}
    credentialMethods: [no-auth]
    defaultCredentialMethod: no-auth
skill: {summary: Remote tools}
`)
	locked, err := Resolve(loaded, map[string]kit.Loaded{"remote-mcp": pkg}, map[string][]string{"remote": {"read_issue", "create_issue"}})
	if err != nil {
		t.Fatal(err)
	}
	operations := locked.Connectors[0].Operations
	if len(operations) != 2 || operations[0] != "create_issue" || operations[1] != "read_issue" {
		t.Fatalf("dynamic MCP operations were not pinned exactly: %#v", operations)
	}
}

func TestResolvePinsSkillSourceAndManagedToolProvenance(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".outfit", "workspace.yaml")
	manifest := Manifest{APIVersion: APIVersion, Name: "demo", Connectors: []Connector{{Name: "workspace", Package: "gws", Mode: "cli", Capabilities: []string{"gmail"}}}}
	if err := WriteManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := parsedPackage(t, `apiVersion: outfit.jhh3.dev/connector/v1alpha2
key: gws
display: Google Workspace
version: 1.0.0
credentialMethods:
  - {id: managed, label: Managed, description: Managed, recommended: true, custody: outfit}
tools:
  - id: gws
    display: Google Workspace CLI
    binary: gws
    version: 0.22.5
    verify: [--version]
    source:
      kind: fixed-url
      url: https://example.test/gws
      assets: {linux-amd64: gws.tar.gz}
      checksums: {linux-amd64: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}
modes:
  - id: cli
    label: CLI
    description: CLI
    recommendation: {rank: 1, reason: Structured CLI}
    executor: {kind: cli, tool: gws}
    credentialMethods: [managed]
    defaultCredentialMethod: managed
    capabilities: [gmail, drive]
    operations:
      - {name: auth_status, title: Status, description: Status, class: read, cli: {args: [auth, status]}}
skillSources:
  - id: upstream
    kind: github-directory
    repository: googleworkspace/cli
    ref: v0.22.5
    digest: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    paths:
      - {path: skills/gws-shared}
      - {path: skills/gws-gmail, capabilities: [gmail]}
      - {path: skills/gws-drive, capabilities: [drive]}
skill: {summary: Workspace}
`)
	locked, err := Resolve(loaded, map[string]kit.Loaded{"gws": pkg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(locked.Skills) != 1 {
		t.Fatalf("skills = %#v", locked.Skills)
	}
	skill := locked.Skills[0]
	if skill.Repository != "googleworkspace/cli" || skill.Ref != "v0.22.5" || skill.Digest == "" || skill.Tool != "gws" || skill.ToolVersion != "0.22.5" {
		t.Fatalf("skill provenance = %#v", skill)
	}
	if len(skill.Paths) != 2 || skill.Paths[0] != "skills/gws-gmail" || skill.Paths[1] != "skills/gws-shared" {
		t.Fatalf("capability-scoped paths = %#v", skill.Paths)
	}
}

func TestResolveRejectsWildcardOperations(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".outfit", "workspace.yaml")
	manifest := Manifest{APIVersion: APIVersion, Name: "demo", Connectors: []Connector{{Name: "remote", Package: "remote-mcp", Mode: "mcp", Operations: []string{"*"}}}}
	if err := WriteManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := parsedPackage(t, `apiVersion: outfit.jhh3.dev/connector/v1alpha2
key: remote-mcp
display: Remote MCP
version: 1.0.0
credentialMethods:
  - id: no-auth
    label: No auth
    description: Public test endpoint
    recommended: true
    custody: none
modes:
  - id: mcp
    label: MCP
    description: Remote tools
    recommendation: {rank: 1, reason: Test MCP mode}
    executor: {kind: mcp, endpoint: "https://mcp.example.test/mcp"}
    credentialMethods: [no-auth]
    defaultCredentialMethod: no-auth
skill: {summary: Remote tools}
`)
	if _, err := Resolve(loaded, map[string]kit.Loaded{"remote-mcp": pkg}, nil); err == nil {
		t.Fatal("wildcard operation lock was accepted")
	}
}

func TestResolveRejectsUndiscoveredExplicitMCPOperation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".outfit", "workspace.yaml")
	manifest := Manifest{APIVersion: APIVersion, Name: "demo", Connectors: []Connector{{Name: "remote", Package: "remote-mcp", Mode: "mcp", Operations: []string{"invented_tool"}}}}
	if err := WriteManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg := parsedPackage(t, `apiVersion: outfit.jhh3.dev/connector/v1alpha2
key: remote-mcp
display: Remote MCP
version: 1.0.0
credentialMethods:
  - id: no-auth
    label: No auth
    description: Public test endpoint
    recommended: true
    custody: none
modes:
  - id: mcp
    label: MCP
    description: Remote tools
    recommendation: {rank: 1, reason: Test MCP mode}
    executor: {kind: mcp, endpoint: "https://mcp.example.test/mcp"}
    credentialMethods: [no-auth]
    defaultCredentialMethod: no-auth
skill: {summary: Remote tools}
`)
	if _, err := Resolve(loaded, map[string]kit.Loaded{"remote-mcp": pkg}, map[string][]string{"remote": {"real_tool"}}); err == nil || !strings.Contains(err.Error(), "unknown operation invented_tool") {
		t.Fatalf("undiscovered MCP operation was accepted: %v", err)
	}
}

func TestReadRejectsStaleLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".outfit", "workspace.yaml")
	if err := WriteManifest(path, Manifest{APIVersion: APIVersion, Name: "demo", Connectors: []Connector{}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Lock = Lock{APIVersion: LockAPIVersion, WorkspaceDigest: "old"}
	loaded.LockDigest = "locked"
	if _, err := NewPolicy(loaded); err == nil {
		t.Fatal("expected stale lock rejection")
	}
}

func TestApprovalDigestBindsManifestAndExactLockBytes(t *testing.T) {
	first := Loaded{Digest: "manifest", LockDigest: "lock-one"}.ApprovalDigest()
	if first == (Loaded{Digest: "manifest", LockDigest: "lock-two"}).ApprovalDigest() || first == (Loaded{Digest: "other", LockDigest: "lock-one"}).ApprovalDigest() {
		t.Fatal("approval did not bind both reviewed files")
	}
}

func TestNewPolicyRejectsMalformedToolAndSkillPins(t *testing.T) {
	loaded := Loaded{Path: "/tmp/work/.outfit/workspace.yaml", Digest: "manifest", LockDigest: "lock"}
	loaded.Lock = Lock{APIVersion: LockAPIVersion, WorkspaceDigest: "manifest", Packages: []LockedPackage{{Key: "gws", Digest: strings.Repeat("a", 64)}}, Connectors: []LockedConnector{{Name: "work", Package: "gws", Mode: "cli", CredentialMethod: "managed", Operations: []string{"status"}, Skills: true}}, Tools: []LockedTool{{ID: "gws", Version: "1", Checksums: map[string]string{"linux-amd64": "bad"}}}}
	if _, err := NewPolicy(loaded); err == nil || !strings.Contains(err.Error(), "invalid checksum") {
		t.Fatalf("malformed tool pin was accepted: %v", err)
	}
	loaded.Lock.Tools[0].Checksums["linux-amd64"] = strings.Repeat("b", 64)
	loaded.Lock.Skills = []LockedSkill{{Connector: "work", Source: "upstream", Repository: "example/repo", Ref: "", Digest: strings.Repeat("c", 64), Paths: []string{"skills/core"}}}
	if _, err := NewPolicy(loaded); err == nil || !strings.Contains(err.Error(), "invalid ref or digest") {
		t.Fatalf("malformed skill pin was accepted: %v", err)
	}
}

func TestValidateRejectsPackagePathOutsideRepository(t *testing.T) {
	err := Validate(Manifest{APIVersion: APIVersion, Name: "demo", Packages: []Package{{Key: "outside", Path: "../outside"}}, Connectors: []Connector{}})
	if err == nil {
		t.Fatal("outside package path was accepted")
	}
}

func TestInferUsesGitAndDependenciesButOnlyActiveConnections(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("url = git@github.com:acme/repo.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"dependencies":{"@sentry/nextjs":"latest","@linear/sdk":"latest"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Infer(root, []AvailableConnection{{Name: "work-github", Package: "github", Mode: "cli"}, {Name: "sentry", Package: "sentry", Mode: "mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Manifest.Connectors) != 2 || result.Manifest.Connectors[0].Name != "sentry" || result.Manifest.Connectors[1].Name != "work-github" {
		t.Fatalf("unexpected inference: %#v", result.Manifest.Connectors)
	}
}

func TestInferIncludesOnlyReproducibleCustomPackagePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("Uses https://intercom.com/help for support"), 0o600); err != nil {
		t.Fatal(err)
	}
	packageDir := filepath.Join(root, "connectors", "intercom")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "connector.yaml"), []byte(`apiVersion: outfit.jhh3.dev/connector/v1alpha2
key: intercom
display: Custom Intercom
version: 1.0.0
credentialMethods:
  - id: no-auth
    label: No auth
    description: Public test endpoint
    recommended: true
    custody: none
modes:
  - id: rest
    label: REST
    description: REST
    recommendation: {rank: 1, reason: Test REST mode}
    executor: {kind: http, baseUrl: "https://api.intercom.io/"}
    credentialMethods: [no-auth]
    defaultCredentialMethod: no-auth
    operations:
      - name: list_contacts
        description: List contacts
        class: read
        http: {method: GET, path: /contacts}
    verify: list_contacts
skill: {summary: Intercom}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Infer(root, []AvailableConnection{{Name: "project-intercom", Package: "intercom", Mode: "rest", Source: "/outside/connector.yaml"}, {Name: "custom", Package: "intercom", Mode: "rest", Source: filepath.Join(packageDir, "connector.yaml")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Manifest.Connectors) != 1 || result.Manifest.Connectors[0].Name != "custom" || len(result.Manifest.Packages) != 1 || result.Manifest.Packages[0].Path != "connectors/intercom/connector.yaml" {
		t.Fatalf("custom source inference was not reproducible: %#v", result.Manifest)
	}
}

func TestInferReportsAmbiguousActiveConnectionsInsteadOfChoosingOne(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("url = git@github.com:acme/repo.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Infer(root, []AvailableConnection{{Name: "github-personal", Package: "github", Mode: "cli", Source: "builtin"}, {Name: "github-work", Package: "github", Mode: "cli", Source: "builtin"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Manifest.Connectors) != 0 || len(result.Ambiguous["github"]) != 2 || !strings.Contains(FormatInference(result), "AMBIGUOUS") {
		t.Fatalf("ambiguous inference was not fail-closed and explained: %#v", result)
	}
}

func parsedPackage(t *testing.T, raw string) kit.Loaded {
	t.Helper()
	loaded, err := kit.Parse([]byte(raw), "test")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Source = "builtin:test"
	return loaded
}
