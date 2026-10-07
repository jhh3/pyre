// Package configuration is Pyre's single configuration merge and
// materialization implementation (contracts: ConfigLayer, LayerOrder). No
// other package merges harness configuration.
//
// # Layers
//
// Values, skills, plugins, MCP servers, hooks and connection references are
// merged in contracts.LayerOrder:
//
//	provider_default  documented harness defaults (provenance only, never written)
//	adopted_user      the user's own files, read in native order:
//	                  ~/.claude/settings.json < settings.local.json < <project>/.claude/settings*.json,
//	                  ~/.codex/config.toml < <project>/.codex/config.toml, plus inventoried extensions
//	managed_global    the global Profile (<state>/env/profiles/global/<name>.json)
//	managed_project   the project Profile (<state>/env/profiles/project/<id>/<name>.json)
//	session           explicit session overrides
//
// Each ResolvedValue records the winning layer and the lower layers it
// overrode. Sensitive keys/values are shown as "[redacted]"; profiles and
// session overrides that contain credential-like material are rejected
// (secrets come from connection grants, resolved separately).
//
// # Materialization (never clobber user config)
//
// Claude: managed and session values and managed hooks go into one
// per-generation settings overlay passed with --settings; the Claude adapter
// merges that file into the run's own single --settings file, after its
// runtime hooks (Claude Code takes one --settings). MCP servers go into a
// per-generation --mcp-config file; project/session skills into a
// per-generation plugin passed with --plugin-dir. ~/.claude/settings.json is
// not edited unless a global profile explicitly lists a key in Persist; then
// only that key is written, with an ownership record, a backup and a diff.
//
// Codex: values, MCP servers, plugins and hooks become -c key=value
// overlays. One app-server serves every run on a CODEX_HOME, so the Codex
// adapter applies them as per-thread config of the run's thread
// (thread/start, re-sent on every thread/resume), never as app-server flags.
// ~/.codex/config.toml is never rewritten. Keys whose segments contain '.'
// cannot be addressed by -c and are returned as conflicts.
//
// Scopes: a project profile overrides the global profile. A same-name MCP
// server in a higher layer replaces the lower definition (one registration);
// two declarations in one layer are a conflict.
//
// Global managed skills are installed into <home>/skills/<name> with an
// ownership marker (extensions.MarkerFile); a same-name user directory is a
// conflict, never overwritten.
//
// Duplicate registration is prevented by identity: a managed or runtime hook
// identical (event+matcher+command) to a user hook, or an MCP server with the
// same name and definition as a user registration, is reused instead of being
// registered again. Same name with a different definition is a conflict.
//
// # Generations, plans, drift
//
// A resolution becomes an immutable EnvironmentGeneration stored under
// <state>/env/generations/<id>/ (record.json, overlay files, home content
// for rollback). Identical resolutions share one generation (by digest).
// Running runs keep the generation (and overlay files) they were launched
// with; Prune never removes a generation referenced by a live run.
//
// Profiles: <state>/env/profiles holds the active profiles that launches
// resolve; env.profile.set writes a pending desired profile to
// <state>/env/desired, which plans prefer. Apply and Rollback write the
// generation's recorded profile snapshot into the active store, so a desired
// change reaches new runs only through a reviewed apply and a rollback
// changes what new runs launch with.
//
// Plan produces a ChangePlan (diffs, backups, conflicts, restart-required
// sessions) and stores it; Apply requires the plan's ResultDigest, refuses
// if conflicts exist or files changed since the plan, applies each file
// atomically (temp+rename) and undoes all completed steps on failure.
// Rollback re-materializes a previous generation's managed items, subject to
// the same ownership checks. Drift compares live files with the current
// generations: managed modifications ("modified"/"missing"), user additions
// ("unmanaged_addition", preserved), lost ownership or same-name user
// registrations ("conflict") and runtime version changes ("version_mismatch").
//
// # Daemon integration
//
//	m, _ := configuration.New(configuration.Options{StateDir: ..., NodeID: ..., Runs: ..., Grants: ..., Versions: updatesMgr, Reporter: outbox})
//	gen, overlay, err := m.Resolve(ctx, configuration.Scope{ProjectID: ..., SessionID: ..., Harness: ..., ConfigHome: grant.ProfileDir, AdapterHooks: ..., AdapterMCP: ...})
//	// launch: args = overlay.ExtraArgs + adapter args; env += overlay.Env; ResolvedLaunch.EnvGeneration = gen.GenerationID
//	for name, h := range m.Handlers() { dispatcher.Register(name, h) } // env.plan/apply/drift/rollback/resolve
package configuration
