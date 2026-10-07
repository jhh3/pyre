// Package extensions inventories the skills, plugins, MCP servers, hooks,
// slash commands, agents and prompts that already exist for Claude Code and
// Codex on a node, without moving or rewriting anything.
//
// Sources scanned (all read-only):
//
//	Claude  <claude home>/skills/*, commands/*, agents/*,
//	        settings.json + settings.local.json (hooks, enabledPlugins),
//	        plugins/installed_plugins.json (+ each plugin's skills/),
//	        ~/.claude.json mcpServers (user scope and per-project scope),
//	        <project>/.claude/{skills,commands,agents,settings*.json},
//	        <project>/.mcp.json
//	Codex   <codex home>/skills/* (".system" = provider_default layer),
//	        prompts/*, hooks.json, config.toml (mcp_servers, plugins, hooks),
//	        plugins/cache/<marketplace>/<plugin>/<version>,
//	        <project>/.codex/{skills,hooks.json}
//
// Every item becomes an Item with a stable Identity used for duplicate
// detection (A15): hooks are identified by harness+event+matcher+command
// digest, MCP servers by harness+name with a definition digest for
// equivalence. Commands, arguments, environment and header values never leave
// the node: they are kept in unexported/json:"-" fields and represented only by
// digests.
//
// Items carry the configuration layer they belong to. User customization is
// LayerAdoptedUser and Managed=false; directories that carry a Pyre ownership
// marker (MarkerFile) or that the Outfit integration reports through
// ManagedSource are Managed=true. Adoption is a reference, never a move:
// Preview reports "these N items will be referenced, nothing moved".
package extensions
