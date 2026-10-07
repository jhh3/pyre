# Connector authoring

One `outfit.jhh3.dev/connector/v1alpha2` package defines every product surface. Authors write `connector.yaml`, fixtures, and optional authored/upstream skill references. The compiler derives TUI forms, recommendations, credential bindings, managed tools, operation tools, link recognizers, health, skill guidance, and conformance tests. No provider-specific TUI code is required.

```bash
outfit kit new acme --kind http # or mcp / cli
outfit kit validate ./acme
outfit kit test ./acme
outfit connector install ./acme --trust
```

Or import a provider contract, then review the generated package:

```bash
outfit kit import openapi openapi.yaml --key acme-api --auth-field TOKEN
outfit kit import postman collection.json --key acme-api --base-url https://api.acme.test/
outfit kit import cli help.txt --key acme-cli --binary acme
outfit kit import mcp https://mcp.acme.test/mcp --key acme-mcp --auth-field TOKEN
outfit kit import graphql https://api.acme.test/graphql --key acme-graphql --auth-field TOKEN
```

All network importers default to public addresses. For an intentionally private fixed endpoint, add `--private-network`; the generated package records that capability for review before trust.

Import is deterministic for file inputs. OpenAPI and Postman are constrained to one fixed HTTPS origin. MCP and GraphQL perform live discovery through the same bounded transport model as runtime execution. CLI accepts a captured help/completion file or invokes a PATH-resolved binary with `--help`; it never invokes a shell. Credentials supplied for discovery remain in memory and are not emitted.

Importers deliberately choose conservative defaults: GET, HEAD, and GraphQL queries are reads; DELETE is destructive; other HTTP methods and GraphQL mutations are writes. Unannotated MCP tools remain destructive. The generated README lists skipped or ambiguous contract features, and the package cannot execute before ordinary validation, semantic review, installation, and trust.

HTTP packages use fixed HTTPS origins and typed operations. CLI packages use argv arrays, never shell strings. MCP packages use fixed stdio commands or HTTPS endpoints. URL recognizers use named templates over a closed segment vocabulary, never executable regular expressions.

Operation inputs are a progressive contract rather than duplicated presentation metadata. Authors can declare a complete JSON Schema when provider types, enums, or descriptions matter. Connector Kit otherwise infers missing string properties from `{{arg.name}}` HTTP/CLI templates, HTTP path segments, query/body fields, CLI optional arguments, and pagination cursors. The resulting effective schema powers typed short CLI commands, the generic CLI, JSON/stdin input, the compact read-only MCP dispatcher, generated skills, workflow entry points, validation, and runtime. For example:

```yaml
- name: tickets_list
  title: List tickets
  description: List tickets visible to this account.
  class: read
  http:
    method: GET
    path: /tickets
    query: [status, cursor]
  pagination:
    cursorArgument: cursor
    nextCursorPath: $.meta.next_cursor
    itemsPath: $.tickets
    defaultMaxPages: 10
    defaultMaxItems: 1000
```

That one declaration produces `outfit acme tickets-list --status open --all` and `outfit run acme tickets_list --arg status=open`; the compact `outfit_run` MCP fallback can dispatch the same read without registering another host tool. Connector-specific operation IDs remain accepted, while short commands remove a leading connector prefix and translate underscores to hyphens. Do not hand-author a second agent-only command surface.

Pagination is opt-in at CLI invocation with `--all` or through compact MCP dispatch with `all`, and is always bounded. Declare exact JSON paths when known. Outfit can also infer conventional `cursor`, `after`, `page_token`, GraphQL `pageInfo`, MCP `_meta.nextCursor`, and Stripe `starting_after` shapes. Provider pages remain individually visible in the result envelope.

A resolver can provide one lookup per selectable mode. This keeps one URL/resource definition while letting the runtime use the executor the person actually connected—such as a local REST operation or an imported hosted-MCP tool:

```yaml
links:
  - id: page
    resourceType: notion.page
    patterns:
      - hosts: [app.notion.com]
        path: /p/:slug
        segments: {slug: notion-page}
    lookups:
      - mode: rest
        operation: page
        arguments: {id: slug}
        heavyReads:
          - operation: blocks
            arguments: {id: id}
            recurse: {itemsPath: $.results, idPath: $.id, hasChildrenPath: $.has_children, argument: id, maxDepth: 8, maxCalls: 48}
      - {mode: hosted_mcp, operation: notion-fetch, arguments: {id: $url}}
    presentation: {canonicalId: $.id, title: $.title, summary: $.text}
```

`$url` passes the already validated source URL to tools that natively accept it. An argument source beginning with `=` supplies a typed JSON-coercible literal such as `=true` to a primary or heavy lookup. Validation permits dynamic operation names only for MCP modes; live discovery must still find a read-class tool with that exact name before a link becomes eligible.

Automatic link context has three per-connection modes: `heavy` (the default), `light`, and `off`. Heavy includes the complete bounded primary read payload and can declare additional `heavyReads`. Each additional operation must be read-only and uses arguments already validated by the primary URL lookup. The optional generic recursion contract follows tree-shaped results with explicit item, id, child, depth, and call bounds; it is how the built-in Notion package reads nested page blocks without provider-specific runtime code. Light retains only projected metadata and a short excerpt. Authors should make the primary lookup useful by itself and use heavy reads for discussion, child content, recent events, files, or jobs that materially change the meaning of a linked resource.

Every mode declares an ordered recommendation, at least one credential method, and exactly one default method. Credential fields may be secret or safe settings; an optional ordered `environment` list declares conventional variable names that `outfit` may detect and preload only during explicit setup. Values are never rendered, and generated OAuth fields cannot declare environment sources. Bindings deliver verified fields through `process-env`, `stdin`, `temporary-file`, `temporary-directory`, `unset-env`, HTTP headers/query, or MCP authorization. A mode may cite a checksum-pinned managed tool. Upstream skills require an immutable repository ref, archive digest, declared paths, and optional capability/operation filters.

Trusted native CLIs that split login into a browser approval and a returned completion command can declare that handoff once on their setup command:

```yaml
setup:
  program: provider
  args: [login]
  browserContinuation:
    urlField: browser_url
    verificationCodeField: verification_code
    nextStepField: next_step
    timeoutSeconds: 300
```

The setup command must return a JSON object with those fields. Outfit opens only an HTTPS URL, parses the next step as argv, requires it to name the same pinned executable, and invokes that executable directly without a shell. The resulting credential is still verified through the method's ordinary `verify` command before the connection is committed.

Use Stripe and Google Workspace as executable references:

```bash
outfit connector show stripe
outfit connector show google-workspace
outfit tool catalog
outfit skill catalog
```

Validation rejects absent method/tool references, invalid recommendation ranks, secret argv templates, incompatible bindings, missing platform artifacts, write-class verification, unsafe link lookups, and mutable upstream skill sources.

An installed package records its source, version, digest, trust decision, and requested capabilities. `outfit kit diff OLD NEW` explains changed origins/executables, auth fields, operation classes, and removed modes. A changed package requires `--approve-update`; without `--trust` it remains inspectable but cannot execute.

For a provider MCP execution mode, declare only the endpoint/command and authentication. Setup performs live paginated tool discovery, stores input schemas, treats unannotated tools as destructive, and compiles the resulting surface into the typed `outfit` CLI, generated skills, and the compact read-only local MCP dispatcher.

Every read operation appears in the typed and generic CLI and is eligible for the compact read-only MCP dispatcher. Every mutating operation requires explicit human intent through CLI `--write` or the `outfit write` namespace; local MCP never executes mutations. Use `outfit kit test` to prove the package contract, then inspect the generated human/agent surfaces with `outfit CONNECTOR OPERATION --help`, `outfit describe`, `outfit workflow show`, and the four-tool MCP facade.
