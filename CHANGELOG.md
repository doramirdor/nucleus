# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (starting at 0.1.0).

## [0.1.3] — 2026-04-24

### Changed
- **Product name is now just "Nucleus"** across all prose (README, CONTRIBUTING, server Instructions, demo docs, Go package comments). Previously "NucleusMCP". Tagline tightened to *"one MCP to recommend them all"* — MCP stays as a protocol reference, not as part of the product name.
- Banner (`assets/banner.gif`) re-rendered: title now reads **Nucleus** (no split `NucleusMCP` word), orbital runes simplified.
- Demo GIFs re-recorded against the renamed `nucleus` binary so every on-screen command matches the current CLI.

### Unchanged
- Repo name, Go module path, on-disk storage paths, and OS keychain service string (`nucleusmcp`) all stay the same to avoid breaking existing installs and import paths.

## [0.1.2] — 2026-04-24

### Changed
- **CLI binary renamed `nucleusmcp` → `nucleus`.** The directory at `cmd/nucleusmcp/` moved to `cmd/nucleus/`; the default binary produced by `make install` is now `nucleus`. The MCP server identity advertised to clients (Claude, Cursor, …) is also now `nucleus`. Product name in all prose / docs is now just **Nucleus** (repo name and Go module path stay `nucleusmcp` to avoid breaking import paths and clone URLs).
- Go module path is unchanged (`github.com/doramirdor/nucleusmcp`).
- On-disk storage paths (`~/.nucleusmcp/registry.db`, `~/.nucleusmcp/oauth/…`, `~/.nucleusmcp/connectors/…`) and the OS keychain service string (`nucleusmcp`) are unchanged, so existing profiles and credentials remain accessible after the rename.

### Added
- **Homebrew tap** — `brew install doramirdor/homebrew-tap/nucleus` now works; goreleaser publishes the formula automatically on tag push.
- **`go install`** path documented: `go install github.com/doramirdor/nucleusmcp/cmd/nucleus@latest`.

### Fixed
- `.github/workflows/ci.yml` follows the `cmd/` rename.

### Migration for pre-rename installs
```bash
make install                                          # builds bin/nucleus
claude mcp remove nucleusmcp                          # drop old MCP entry
nucleus install                                       # re-register as "nucleus"
sudo ln -sf "$HOME/go/bin/nucleus" /usr/local/bin/nucleus   # optional
```

## [0.1.1] — 2026-04-23

### Added
- Dynamic server `Instructions` advertised at MCP init: Claude (and any MCP client) now reads the live connector + profile list at connect time, so asking *"what X connections do you have?"* routes through nucleusmcp without the user naming the gateway.
- Tag-triggered release workflow (`.github/workflows/release.yml`). Pushing `vX.Y.Z` produces cross-platform binaries on a GitHub release via GoReleaser.
- First unit-test round: registry (migrations, CRUD, defaults), connectors (built-in lookup, custom save/load roundtrip), router (namespacing + description prefix), workspace parser (both toml forms + ancestor walk).

### Changed
- README: sharpened the "Why" section with the concrete one-connector-per-MCP pain flow; reframed `.mcp-profiles.toml` as optional (the resolver's expose-all fallback covers the default case).
- Demo GIFs re-recorded: clean `$` prompt, scratch-dir sandboxing, no heredoc continuation artifacts.

### Fixed
- Resolver used to error when multiple profiles existed with no binding/autodetect/default; it now exposes every profile as a distinct namespace by default.
- `supabase/config.toml` autodetect with a non-matching `project_id` no longer blocks resolution — falls through to expose-all.

## [0.1.0] — 2026-04-23

### Added
- Gateway with stdio MCP server, per-profile credential injection, and transparent tool proxy
- Profile registry in SQLite at `~/.nucleusmcp/registry.db` with schema migrations
- OS keychain–backed credential vault (macOS Keychain, Linux libsecret, Windows Credential Manager)
- Workspace resolution: `.mcp-profiles.toml` (explicit bindings), autodetect via manifest rules, user-set defaults, and an expose-all fallback
- Multi-profile-per-connector aliases with dedup spawn (same profile under two aliases reuses one child process)
- HTTP / OAuth connectors bridged via `mcp-remote` with per-profile isolated auth directories
- Post-OAuth resource discovery: picker lists projects from the upstream after auth (Supabase)
- Tool description prefix: proxied tools carry `[connector/alias metadata]` and optional user note so MCP clients read profile context natively
- Custom connector support: `nucleus add <name> --transport http <url>` saves a manifest under `~/.nucleusmcp/connectors/`
- Built-in connectors: Supabase (OAuth) and GitHub (PAT)
- CLI: `add`, `remove`, `list`, `info`, `use`, `connectors`, `install`, `serve`

### Known gaps
- Eager spawn at startup — no idle reaper yet
- No mid-session cwd-change hot-swap
- No audit log CLI surface
- `mcp-remote` dependency for HTTP connectors (native OAuth is roadmapped)
