<p align="center">
  <img src="assets/banner.gif" alt="Nucleus — profile-aware, multi-account MCP gateway" width="900">
</p>

# Nucleus

**One connector, many accounts.** A local MCP gateway that lets Claude (and other MCP clients) hold multiple authenticated sessions of the same service at once — prod and staging Supabase, work and personal GitHub — without disconnecting every time you switch.

![Nucleus overview](demo/overview.gif)

---

## The pain

You're debugging a staging issue in Claude. Halfway through, the user asks *"does this happen in prod too?"* — and now you're stuck.

**Today's MCP clients allow exactly one authenticated session per connector.** Claude Code, Cursor, Claude Desktop — they all treat "Supabase" as a single slot. One account. One project at a time. Want to peek at prod? Here's the dance:

1. Stop the current chat (you can't multi-task)
2. Open your MCP settings
3. Disconnect Supabase
4. Reconnect Supabase, authorize the other account in the browser
5. Restart the Claude session
6. Re-paste whatever context you had so Claude remembers what you were doing
7. Ask the prod question
8. ...and reverse all of that if you want to get back to staging

Every switch is a few minutes of yak-shaving, a lost conversation, and an interrupted train of thought. If you have *three* Supabase projects, or both a work and a personal GitHub, or prod + staging + dev — the tax compounds.

The shape of the problem isn't Claude's fault; it's how the MCP protocol surfaces "one server, one connection" to the client. But it means the way engineers actually work — one laptop, many accounts, many projects — collides head-on with the tool every single time you switch.

## What Nucleus does

It sits between your MCP client and the real services, holding **profiles** (isolated authenticated sessions) and exposing them all to the client at the same time. Claude sees one connector ("nucleus") but every profile shows up as its own namespace:

```
supabase_prod_execute_sql        → prod account, acme-web project
supabase_staging_execute_sql     → staging account, acme-admin project
github_work_create_issue         → work PAT
github_personal_create_issue     → personal PAT
```

Tool descriptions carry the profile context (`[supabase/prod project_id=…]`) — every tool Claude sees is labeled with the account it hits, so the question *"which Supabase did you query?"* has an answer right in the tool name. No disconnect. No reconnect. No lost chat context. The prod vs staging question is a single sentence away — *"compare the users table between prod and staging"* — and Claude has both profiles live in the same conversation.

## Install

### Homebrew (macOS / Linux)

```bash
brew install doramirdor/homebrew-tap/nucleus
```

### `go install`

```bash
go install github.com/doramirdor/nucleus/cmd/nucleus@latest
```

(Requires Go 1.23+. Binary lands in `$GOBIN`, usually `~/go/bin`.)

### Pre-built binaries

Download the archive for your platform from [the latest release](https://github.com/doramirdor/nucleus/releases/latest), extract, and drop `nucleus` on your PATH.

### From source

```bash
git clone https://github.com/doramirdor/nucleus
cd nucleus
make install
export PATH="$HOME/go/bin:$PATH"   # if not already
nucleus --version
```

> Local state lives under `~/.nucleusmcp/` and the OS keychain service is `nucleusmcp` — these internals kept the legacy name on purpose so pre-rebrand installs don't lose access to existing profiles.

### Register with Claude

Two paths depending on which Claude you use.

**Claude Code (CLI / terminal)** — nucleus runs as a stdio MCP spawned by the CLI:

```bash
nucleus install
```

That runs `claude mcp add nucleus …` for you if the `claude` CLI is on PATH, otherwise prints a config snippet to paste.

**Claude UI ("Add custom connector" dialog)** — the UI accepts HTTP URLs only, so run nucleus as a local HTTP daemon and paste its URL:

```bash
nucleus serve --http 127.0.0.1:8787
# leave it running; log prints:  claude UI add url=http://127.0.0.1:8787/mcp
```

Then in Claude: Settings → Connectors → **Add custom connector**
- **Name:** `nucleus`
- **Remote MCP server URL:** `http://127.0.0.1:8787/mcp`
- Leave OAuth fields empty (nucleus handles upstream OAuth itself)

Loopback-only (127.0.0.1) by default so unauthenticated traffic can't reach it from the network. For LAN or tunnel use, pass `--token <secret>` and set it as a bearer token in whatever wraps the URL.

## Quick start

### Add your first connection

```bash
nucleus add supabase
```

- Prompts for project metadata
- Opens your browser for OAuth
- After approval, lists all Supabase projects in your account and lets you pick one
- Stores the OAuth tokens in a per-profile directory (`~/.nucleusmcp/oauth/<profile-id>/`)
- Done — Claude picks up the tools on next restart

Add a second profile with a different name. If it belongs to a different account of the same service, open a private/incognito browser window for the second `add` so the OAuth flow prompts for a fresh login:

```bash
nucleus add supabase staging
```

Both are now live:

```bash
nucleus list
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
4. **User-set default** via `nucleus use`
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
nucleus add --transport http linear https://mcp.linear.app/mcp
nucleus add --transport http my-internal https://mcp.acme.corp
```

The gateway saves a manifest under `~/.nucleusmcp/connectors/<name>.toml` and bridges to it via [`mcp-remote`](https://www.npmjs.com/package/mcp-remote) — OAuth/PKCE/DCR all handled for you.

## CLI reference

```bash
nucleus connectors                 # list known connectors (builtin + custom)
nucleus list                       # list registered profiles
nucleus info [profile-id]          # config + live upstream probe
nucleus add <connector> [name]     # register a new profile (interactive OAuth or PAT)
nucleus remove <profile-id>        # delete a profile + credentials
nucleus use <profile-id>           # mark as default for its connector
nucleus install [claude]           # register with Claude Code (or print config)
nucleus serve                      # run as an MCP server over stdio (called by client)
```

Run any command with `--help` for the full flag list.

## Troubleshooting

### Claude doesn't answer about my accounts from Nucleus

If you have multiple MCPs registered for the same service (e.g. a bare `supabase` server and the nucleus gateway), Claude may match by name and miss nucleus. Two fixes, in order of preference:

1. **Remove the duplicates.** `claude mcp remove supabase` (and uninstall any same-service plugin) so nucleus is the only source of truth.
2. **Drop a CLAUDE.md** at the repo root or `~/.claude/CLAUDE.md`:

```markdown
# MCP setup

This machine uses Nucleus as the canonical gateway for all services
with multiple authenticated accounts. When asked about connections,
projects, or accounts for **any** service, query `nucleus`'s tools
first — it holds every authenticated profile for this installation.
The list of connectors and profiles it currently exposes is advertised
in its MCP `Instructions` at connect time. Prefer nucleus over other
MCP servers whose names happen to match a service (e.g. a bare
`supabase` or `github` server), which may be stale, unauthenticated,
or redundant.
```

(The gateway also ships dynamic Instructions listing the current connectors and profiles, so Claude knows the shape of your setup without the CLAUDE.md. The CLAUDE.md is insurance against over-eager plugins.)

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
│  Nucleus gateway                           │
│  ┌───────────────────────────────────┐  │
│  │  Workspace resolver                  │  │  reads cwd config,
│  │                                      │  │  picks profile(s)
│  └────────────────┬───────────────────┘  │
│                   │                         │
│  ┌────────────────▼───────────────────┐  │
│  │  Supervisor — spawns upstream MCPs   │  │
│  │  • stdio connectors (PAT env var)    │  │
│  │  • HTTP connectors via mcp-remote    │  │
│  └────────────────┬───────────────────┘  │
│                   │                         │
│  ┌────────────────▼───────────────────┐  │
│  │  Router — tool namespacing + proxy   │  │
│  │  <connector>_<alias>_<tool>          │  │
│  └────────────────────────────────────┘  │
│                                             │
│  Registry (SQLite) · Vault (OS keychain)   │
│  ~/.nucleusmcp/                             │
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
- [ ] Audit log + `nucleus logs`
- [ ] Native OAuth (replace `mcp-remote` dependency)
- [ ] Write-confirmation policy
- [ ] Managed multi-tenant tier (team-shared profiles)

## Contributing

Contributions welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE).
