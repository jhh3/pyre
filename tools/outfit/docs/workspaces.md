# Repository workspaces

`.outfit/workspace.yaml` declares the connector environment a repository expects. It contains names and policy, never credentials:

```yaml
apiVersion: outfit.jhh3.dev/workspace/v1alpha2
name: billing-service
skills: true
links: true
packages:
  - key: internal-billing
    path: ./connectors/internal-billing
connectors:
  - name: github-work
    package: github
    mode: cli
    credentialMethod: native-login
    operations: [get_issue, get_pull_request, search_code]
    linkRules: [issue, pull_request]
  - name: billing
    package: internal-billing
    mode: rest
    credentialMethod: managed-token
    operations: [get_customer, list_invoices]
    links: false
profiles:
  default: [github-work, billing]
  github-only: [github-work]
profile: default
```

`outfit lock` resolves selected modes, credential methods, capabilities, operation names, managed tool versions/checksums, and selected skill source refs/digests, then writes `.outfit/workspace.lock.yaml` deterministically. Built-ins use the stable source `builtin`; repository packages retain their checked-in relative path. No secret or provider account ID enters either workspace file.

For live MCP connections, an omitted `operations` list resolves to the exact currently discovered tool names. The lock never stores a wildcard, so later provider tool additions cannot silently broaden the project. A teammate can apply that exact list without rediscovery changing policy; missing locked tools fail closed until the connection is refreshed.

`outfit apply` verifies the lock, installs explicitly trusted repository packages, configures missing local connections, synchronizes allowed skills, and records approval of the exact manifest and lock bytes in local state. Supply missing fields as `--set CONNECTION.FIELD=value`; values go directly to the vault and never enter either workspace file. Every ordinary command refuses a missing, stale, unapproved, or locally drifted workspace.

```bash
outfit lock
outfit apply --trust-packages # prompts for billing.API_TOKEN
outfit workspace status
```

The nearest parent workspace activates automatically when a shell or coding harness enters the repository. Its operation policy applies uniformly to `outfit run`, `outfit write`, `outfit mcp`, generated skills, session context, link enrichment, search, and the local admin inventory.

Switch a declared profile without rebuilding its connector definitions:

```bash
outfit profile use github-only
outfit lock
outfit apply
```

Changing the profile changes the manifest digest, so the newly selected environment requires the same explicit review and local approval.

## Inference

`outfit profile infer` reads a bounded set of project-local signals: `.git/config`, `.mcp.json`, `package.json`, `go.mod`, `pyproject.toml`, `requirements.txt`, `Cargo.toml`, selected provider config files, and `README.md`. It matches only already-active local connections and prints its evidence.

```bash
outfit profile infer
outfit profile infer --json
outfit profile infer --write
```

Inference never activates a connector. `--write` creates a proposal only; `outfit lock` and `outfit apply` remain the review and approval boundaries.
