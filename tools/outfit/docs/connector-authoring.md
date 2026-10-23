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

HTTP modes may declare fixed, non-secret `executor.headers` sent on every request (for example Discord requires `User-Agent: DiscordBot (url, version)`); credentials always go through bindings, and `Authorization`, `Host`, `Cookie`, and `Content-Length` are reserved. An operation can add fixed `queryValues` (such as `with_counts: 'true'`). Body arguments are JSON-coerced by default (`[1,2]`, `true`, and numbers become JSON; large integers keep every digit); list free text and snowflake-style IDs in `stringArguments` so `"42"` or `"true"` stays a string. Values in `bodyValues` act as defaults that a same-named `body` argument overrides, which is how Discord's `allowed_mentions: {parse: []}` stays the default unless a caller passes its own.

Argument-driven request parts are declared, never free-form:

- `argumentHeaders: [{name: X-Audit-Log-Reason, argument: reason, encoding: url}]` sends an optional argument as a header only when given (`url` percent-encodes it; otherwise control characters are rejected). Credential and framing headers (`Authorization`, `Cookie`, `Host`, `Content-Type`, `Content-Length`) cannot be argument headers.
- `itemTemplates: {answers: {poll_media: {text: '{{item}}'}}}` turns plain-string items of a JSON-array argument into objects; object items pass through.
- `relativeTimeArguments: [until]` accepts minutes (`60`), durations (`90m`, `12h`, `7d`), or an RFC 3339 time and sends RFC 3339 UTC; `0`/`none` sends JSON `null`.
- A generic escape hatch opts in with `pathArgument` (plus optional `queryArgument`, `bodyArgument`, and `methodArgument` with a `methods` allow-list). The path must start with `/` and is rejected if it contains a scheme, host, `//`, `?`, `#`, a backslash, whitespace, an empty/`.`/`..` segment, an encoded separator, or the base path itself; each segment is percent-encoded and appended below the fixed `baseUrl`, so the origin, credential, executor headers, and 429 handling never change. `queryArgument` takes a JSON object (array values repeat the key) or `a=1&b=2`; `bodyArgument` is the entire JSON body and cannot be combined with `body` or `bodyValues`. A fixed `method` is not restricted (imported OpenAPI operations may use `HEAD` or `OPTIONS`), but an argument-chosen method must come from GET, POST, PUT, PATCH, or DELETE. A `read` operation using these must be `GET`-only; a method-choosing operation should be `destructive` when its allow-list includes `DELETE`.

```yaml
- {name: api_get, class: read, http: {method: GET, path: /, pathArgument: path, queryArgument: query}}
- name: api_call
  class: destructive
  http: {path: /, methodArgument: method, methods: [POST, PUT, PATCH, DELETE], pathArgument: path, queryArgument: query, bodyArgument: body}
```

A non-2xx HTTP response is a failed call whose message is the provider's declared `errorField`, a top-level `message`, or a Stripe-style `error.message`, with `error.code` (or `error.type`) reported as the envelope's `provider_code`. A CLI that prints its provider's error object and still exits 0 declares it on the executor: `errorField: error` makes any output whose top-level `error` is present a failure, and `errorStatus: {resource_missing: 404, invalid_request_error: 400}` maps the error's code, then its type, to the upstream HTTP status the CLI does not print. An unmapped error still fails, with no status.

Every HTTP call honours `429 Too Many Requests`: Outfit waits the provider's JSON `retry_after`, `Retry-After`, or `X-RateLimit-Reset-After` and retries up to three times. A requested wait above 30 seconds is not slept; the error reports it. A 429 means the provider did not execute the request, so the bounded retry also applies to writes.

A composite read combines several read operations of the same mode into one call without provider-specific code:

```yaml
- name: activity
  class: read
  input: {type: object, properties: {guild: {type: string, description: Guild ID., examples: ['123']}}, required: [guild]}
  compose:
    - {as: channels, operation: channels, fields: [id, name, last_message_id]}
    - {as: threads, operation: active_threads, itemsPath: $.threads, fields: [id, parent_id, last_message_id]}
```

Steps run sequentially through the selected credential and return one object keyed by `as`. Composite arguments a step declares pass through unchanged; `arguments` can map a step argument to another composite argument or to an `=literal`. `fields` projects each item at `itemsPath` (default: the whole result) to compact top-level keys. Steps must be non-composite reads, there are at most eight, and any failing step fails the whole read.

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

Every mode declares an ordered recommendation, at least one credential method, and exactly one default method. Credential fields may be secret or safe settings; an optional ordered `environment` list declares conventional variable names that `outfit` may detect and preload only during explicit setup. Values are never rendered, and generated OAuth fields cannot declare environment sources. Bindings deliver verified fields through `process-env`, `stdin`, `temporary-file`, `temporary-directory`, `unset-env`, HTTP headers/query, or MCP authorization. A binding with `whenArgument: live` applies only when the boolean operation argument `live` is true, and one with `unlessArgument: live` only when it is absent or false, so one connection can hold two credentials and pick one per call (Stripe's test key and optional live key). A selected binding whose secret is not stored fails the call rather than falling back to the other credential; setup and `connector test` repeat the verification read once per stored conditional credential; and a raw `outfit exec` command selects the same binding with `--live`. A mode may cite a checksum-pinned managed tool. Upstream skills require an immutable repository ref, archive digest, declared paths, and optional capability/operation filters.

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
