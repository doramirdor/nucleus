# NucleusMCP

**One connector, many accounts.** A local MCP gateway that lets Claude (and other MCP clients) connect to multiple authenticated accounts of the same service — prod and staging Supabase, work and personal Gmail, two GitHub orgs — without disconnecting, reconnecting, or losing context.

![NucleusMCP overview](demo/overview.gif)

---

## Why

MCP clients today assume one authenticated session per connector. If you have two Supabase projects in two accounts, Claude can only see one at a time. Want to switch? Disconnect, reconnect, re-auth — losing your chat context along the way.

NucleusMCP sits between your MCP client and the real services. It holds **profiles** (isolated authenticated sessions) for each connector and exposes them all to the client at once, with namespaced tool names so there's no ambiguity:

```
supabase_prod_execute_sql        → prod account, acme-web project
supabase_staging_execute_sql     → staging account, acme-admin project
github_work_create_issue         → work PAT
github_personal_create_issue     → personal PAT
```

Tool descriptions carry the profile context automatically, so Claude knows *which* account each tool targets without being told.

## Install

```bash
# Build from source (requires Go 1.23+)
git clone https://github.com/doramirdor/nucleusmcp
cd nucleusmcp
make install

# Make sure Go's bin is on PATH (add to your shell rc if needed)
export PATH="$HOME/go/bin:$PATH"

nucleusmcp --version
```

Register with Claude Code (detects the `claude` CLI and runs `claude mcp add`):

```bash
nucleusmcp install
```

Releases with pre-built binaries (Homebrew tap, Scoop, apt/rpm) are on the roadmap.

## Quick start

### Add your first connection

```bash
nucleusmcp add supabase
```

- Prompts for project metadata
- Opens your browser for OAuth
- After approval, lists all Supabase projects in your account and lets you pick one
- Stores the OAuth tokens in a per-profile directory (`~/.nucleusmcp/oauth/<profile-id>/`)
- Done — Claude picks up the tools on next restart

Add a second profile with a different name (or different account — sign out in your browser between runs for true account isolation):

```bash
nucleusmcp add supabase staging
```

Both are now live:

```bash
nucleusmcp list
```

```
ID                  DEFAULT  AGE  METADATA
supabase:default             3m   project_id=abcdef...
supabase:staging             0s   project_id=qrstuv...
```

### Use in Claude

Open Claude Code from anywhere:

```bash
claude
```

Ask it *"What Supabase connections do you have?"* — Claude sees both profiles as separate tool namespaces (`supabase_default_*` and `supabase_staging_*`) with bracketed profile context in every tool's description.

![Multi-profile demo](demo/multi-profile.gif)

## Concepts

| | |
|---|---|
| **Connector** | A kind of upstream MCP server (Supabase, GitHub, …). Built-in connectors ship with the binary; custom connectors are added by URL. |
| **Profile** | One authenticated session for a connector. A profile has its own credentials (OAuth tokens or PAT) and optional metadata (project_id, github_user, …). |
| **Workspace** | A directory from which `claude` is launched. Optionally has a `.mcp-profiles.toml` with explicit profile bindings and/or a service-specific config (`supabase/config.toml`) that the gateway reads for autodetect. |
| **Alias** | The middle segment of a tool name, e.g. `atlas` in `supabase_atlas_execute_sql`. Defaults to the profile name; override per-binding in `.mcp-profiles.toml`. |

## Resolution order

When you start the gateway in a directory, this is how it picks which profile(s) to expose for each connector:

1. **Explicit `.mcp-profiles.toml`** in cwd or ancestor
2. **Autodetect** via the connector's manifest rule (e.g. reading `project_id` from `supabase/config.toml`)
3. **Only one profile** registered for the connector → use it
4. **User-set default** via `nucleusmcp use`
5. **Fallback**: expose *every* profile as a separate namespace

Whatever rule fires is logged, so you can always see why Claude sees what it sees.

## `.mcp-profiles.toml` (optional)

You don't need this file. With nothing configured, the gateway exposes every profile automatically — each under its own tool namespace.

Drop one at the root of a repo when you want to:

- **Pin** specific profiles to this workspace and hide the others
- **Alias** a profile to a shorter name (`supabase_prod_*` instead of `supabase_acme-prod_*`)
- **Attach a note** that's spliced into every proxied tool's description, so the MCP client reads warnings (`"PRODUCTION — read-only"`) at call time

```toml
# Single profile per connector
[supabase]
profile = "atlas"

# Or multiple, with aliases and Claude-visible notes
[[supabase]]
profile = "atlas"
alias   = "prod"
note    = "PRODUCTION — read-only unless explicitly asked"

[[supabase]]
profile = "default"
alias   = "staging"
note    = "staging"

# Mixing connectors is fine
[github]
profile = "work"
```

## Custom connectors

Any HTTP MCP server works, not just the built-ins:

```bash
nucleusmcp add --transport http linear https://mcp.linear.app/mcp
nucleusmcp add --transport http my-internal https://mcp.acme.corp
```

The gateway saves a manifest under `~/.nucleusmcp/connectors/<name>.toml` and bridges to it via [`mcp-remote`](https://www.npmjs.com/package/mcp-remote) — OAuth/PKCE/DCR all handled for you.

## CLI reference

```bash
nucleusmcp connectors                 # list known connectors (builtin + custom)
nucleusmcp list                       # list profiles (connections)
nucleusmcp info [profile-id]          # config + live upstream probe
nucleusmcp add <connector> [name]     # register a new profile (interactive OAuth or PAT)
nucleusmcp remove <profile-id>        # delete a profile + credentials
nucleusmcp use <profile-id>           # mark as default for its connector
nucleusmcp install [claude]           # register with Claude Code (or print config)
nucleusmcp serve                      # run as an MCP server over stdio (called by client)
```

Run any command with `--help` for the full flag list.

## Security

- **Credentials never touch disk in plaintext.** PATs go into the OS keychain (Keychain on macOS, libsecret on Linux, Credential Manager on Windows). OAuth tokens live in per-profile directories managed by `mcp-remote` with `0700` perms.
- **Tokens are never logged.** Log output (which goes to stderr so it can't contaminate the MCP JSON-RPC stream on stdout) includes profile IDs and status — never credential values.
- **Profile isolation.** Each profile has its own OAuth auth directory keyed by ID. Two profiles of the same Supabase account still get separate cached tokens.

Not yet shipped: write-confirmation policy enforcement, audit log, process sandboxing. Track on the roadmap.

## Architecture

```
MCP Client (Claude, Cursor, ...)
        │  MCP protocol (stdio)
        ▼
┌───────────────────────────────────────────┐
│  NucleusMCP gateway                       │
│  ┌─────────────────────────────────────┐  │
│  │  Workspace resolver                 │  │  reads cwd config,
│  │                                     │  │  picks profile(s)
│  └────────────────┬────────────────────┘  │
│                   │                        │
│  ┌────────────────▼────────────────────┐  │
│  │  Supervisor — spawns upstream MCPs  │  │
│  │  • stdio connectors (PAT env var)   │  │
│  │  • HTTP connectors via mcp-remote   │  │
│  └────────────────┬────────────────────┘  │
│                   │                        │
│  ┌────────────────▼────────────────────┐  │
│  │  Router — tool namespacing + proxy  │  │
│  │  <connector>_<alias>_<tool>         │  │
│  └─────────────────────────────────────┘  │
│                                            │
│  Registry (SQLite)  ·  Vault (keychain)   │
│  ~/.nucleusmcp/                            │
└───────────────────────────────────────────┘
        │                           │
        ▼ stdio                     ▼ HTTP + OAuth (via mcp-remote)
  local MCP (GitHub, ...)   hosted MCP (Supabase, Linear, ...)
```

## Roadmap

- [x] Stdio MCP proxy with per-profile credentials
- [x] SQLite profile registry + OS keychain vault
- [x] Workspace resolution (`.mcp-profiles.toml` + autodetect)
- [x] Multi-profile aliases + dedup spawning
- [x] HTTP/OAuth connectors via `mcp-remote`
- [x] Post-OAuth resource discovery (Supabase project picker)
- [x] Tool description prefix for client context
- [ ] Idle reaper / on-demand spawn (today: eager at startup)
- [ ] Mid-session hot-swap on cwd change
- [ ] Audit log + `nucleusmcp logs`
- [ ] Native OAuth (replace `mcp-remote` dependency)
- [ ] Write-confirmation policy
- [ ] Managed multi-tenant tier (team-shared profiles)

## Contributing

Contributions welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE).
