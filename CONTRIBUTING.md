# Contributing to Nucleus

Thanks for taking the time. This is a small project and contributions don't need to be polished to land — issues, design questions, and WIP PRs are all welcome.

## Dev setup

```bash
git clone https://github.com/doramirdor/nucleusmcp
cd nucleusmcp
make build       # go build → ./bin/nucleus
make test        # go test ./...
make vet         # go vet ./...
make install     # go install into $GOBIN (usually ~/go/bin)
```

Requires **Go 1.23+**. No CGO required; `modernc.org/sqlite` is a pure-Go driver so cross-compilation stays trivial.

## Running the gateway against your own MCP client

```bash
make install
nucleus install              # registers with Claude Code
nucleus add supabase         # or any built-in / custom connector
# restart Claude Code
```

Logs go to stderr — open a Claude Code session from a terminal to see them.

## Adding a new built-in connector

Connectors are pure data (a `manifest.Manifest`) plus optional workspace-autodetect and post-OAuth discoverer hooks. Adding one is a single file:

1. Pick a name and confirm no clash in `internal/connectors/connectors.go`'s `builtins` map.
2. Append a new manifest describing:
   - **Transport + URL** (for HTTP connectors) OR **Spawn** (command + args + env mapping) for stdio/PAT.
   - **Auth** — `AuthPAT` or `AuthOAuth`.
   - **Metadata** fields — anything a profile should store for display / workspace binding.
   - **Autodetect rules** — optional, for per-repo automatic binding.
3. Register the manifest in `builtins`.
4. (Optional) Implement a `DiscovererFunc` in `internal/connectors/discovery.go` that calls a tool on the live MCP client after OAuth to populate metadata. See `discoverSupabaseProjects` for an example.

Every built-in is subject to the same test: `nucleus add <connector>` must complete end-to-end against a real account and produce a working profile.

## Code organization

```
cmd/nucleus           # CLI entrypoint (cobra commands)
internal/server       # gateway orchestration
internal/supervisor   # upstream MCP child lifecycle
internal/router       # tool proxy + namespacing
internal/workspace    # .mcp-profiles.toml + resolver
internal/registry     # SQLite profile store
internal/vault        # OS keychain wrapper + OAuth dirs
internal/connectors   # built-in manifests + discoverers
internal/config       # gateway-level settings (stub for future)
pkg/manifest          # public connector-manifest schema
```

## Style

- Keep log messages on **stderr**. stdout is reserved for the MCP JSON-RPC stream when serving.
- Never log credentials or token bodies. Profile IDs are fine.
- Errors should carry enough context that a user can act on them. Wrap with `fmt.Errorf("... %w", err)`.
- Prefer adding features as new files rather than growing large ones.

## Before opening a PR

```bash
make vet test
```

Both should be clean. CI runs the same on every PR.
