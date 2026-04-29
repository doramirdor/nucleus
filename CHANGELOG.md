# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) (starting at 0.1.0).

## [0.2.0] — 2026-04-29

The "give Nucleus a reason to be on the front page of HN" release. Six wedge features land together: multi-profile fan-out, sticky resolution with `because:` explanations, write-confirmation policy, audit log + `nucleus logs`, idle reaper with transparent respawn, and `nucleus doctor` for first-run UX.

### Added

- **`nucleus_call_plan` meta-tool** — fan one intent out to N proxied tools in parallel, return one merged result. The structural reward for the multi-profile shape: *"compare the users table between prod and staging"* becomes a single tool call instead of N. Bounded parallelism (default 4, cap 16, 32 steps max), 60s per-step timeout, partial failures returned alongside successes. Step validation runs synchronously before any dispatch so a typo in step #5 fails before steps #1–4 mutate state.
- **Fan-out detection in `nucleus_find_tool`** — when an intent contains comparison wording (`compare`, `between`, `across`, `each`, `versus`, …) or names ≥2 known aliases, the response carries a `fanout_suggestion` block with a ready-made step list to feed into `nucleus_call_plan`.
- **Sticky-alias bias** — after each successful dispatch, the gateway remembers the alias actually used per connector and biases ambiguous future ranking toward it. Suppressed when the intent explicitly names another alias (specificity beats recency). Process-lifetime only — never persisted, so "I worked on staging this morning" can't silently bias prod work this afternoon.
- **`because` field on every recommendation** — each `findToolHit` carries a list of contributing-signal strings (`"matched 'sql' in tool name"`, `"matched 'atlas' in alias"`, `"sticky from last call"`). Opaque ranking is bad ranking; this is the single biggest UX upgrade in the project.
- **Policy gate (`~/.nucleusmcp/policy.toml`)** — optional rule file gating writes/destructives across every dispatch path. Two modes per rule: `deny` (block outright) and `confirm` (allow only with the configured phrase under `__nucleus_confirm` in args). Match patterns are `<connector>:<alias>` with `*` wildcards; tool patterns are `*`-only globs. Deny short-circuits over confirm so a confirmed caller can never bypass an explicit deny. Path overridable via `NUCLEUSMCP_POLICY` env var.
- **Audit log (`~/.nucleusmcp/audit.log`)** — JSONL append, 0o600 perms, mutex-serialized concurrent writes, size-based rotation (10 MiB → `.1..5`, ~60 MiB cap). Every dispatch path logs an entry with policy decision, upstream outcome, duration, profile/alias/tool, and `via` (`direct` / `nucleus_call` / `nucleus_call_plan`). **Privacy by default**: arguments redacted to sorted top-level keys + SHA-256 hash so identical calls group without exposing contents; full args only with `NUCLEUSMCP_AUDIT_FULL_ARGS=1`. Result payloads never logged. Audit failures never break dispatch — log to stderr and continue.
- **`nucleus logs` CLI** — tail/filter the audit trail. `--connector` / `--alias` / `--tool` / `--decision` / `--outcome` / `--since 24h` / `--last N` / `--json`. Reads active + rotated files in chronological order so `--since` works across rotations. Pretty-printed by default with `OK` / `BLK` / `ERR` glyphs in a fixed-width column.
- **Idle reaper (`--idle-timeout`)** — children unused past the timeout are closed; the next call respawns transparently via a captured spawn closure (env rebuilt at respawn time so credential rotations apply). 3–5s warm-up cost on the next call, but reclaims memory for power-user installs running a dozen profiles. Default `0` disables reaping.
- **`nucleus doctor` command** — health check covering claude CLI on PATH, `mcp-remote` on PATH, registry reachable, ≥1 profile registered, `policy.toml` parses, audit log dir writable, custom connectors load, plus optional `--probe http://...` for HTTP gateway responsiveness. PASS/WARN/FAIL with `fix:` hints; `--strict` flips warnings into exit-1 for CI.
- **Router `Dispatcher` interface** — `RegisterChild` now goes through `registerDispatcher(d Dispatcher, ...)`, exposing the same policy/audit/sticky wrapper to integration tests without requiring a real subprocess. `*supervisor.Child` still satisfies the interface in production.
- **Integration test suite** — 6 router-level tests covering `find_tool` → `fanout_suggestion` → `call_plan` round-trip, policy denial with audit assertion, confirm-flow retry with phrase, plan-respects-policy mixed-permission split, audit-failure-never-blocks-dispatch, and policy-short-circuits-before-erroring-inner.

### Changed

- **`Child.CallTool` is now the dispatch entry point.** Previously `c.Client.CallTool` was called directly by the router; the wrapper handles transparent respawn after idle reap and updates `lastUsed`. Direct access to `c.Client` still compiles but skips reap/sticky bookkeeping.
- **`makeHandler` renamed to `makeHandlerForDispatcher`** and now takes the `Dispatcher` interface plus connector/profileID/upstreamTool/alias as scalars rather than a `*supervisor.Child`. Same wrapper, broader testability.
- **Search-mode meta-tool count is 3, not 2.** `nucleus_call_plan` joins `nucleus_find_tool` and `nucleus_call`. The startup log line and `clientVisibleToolCount` reflect this.
- **Router `Finalize` registers all three meta-tools in `ModeSearch` and `ModeHybrid`.** `ModeExposeAll` is unchanged — direct tool advertisement only.
- **`nucleus install` next-step messaging** points at `nucleus doctor` and `nucleus logs` so first-run users have a clear path forward.

### Engineering

- 86 tests across 6 packages (audit, connectors, registry, router, supervisor, workspace), all green under `-race`.
- New packages: `internal/audit`, plus `cmd/nucleus/doctor.go` and `cmd/nucleus/logs.go`.
- No new external Go dependencies; existing `pelletier/go-toml/v2` carries the policy file parser.

### Documentation

- README expanded with worked examples for `nucleus_call_plan`, `policy.toml`, `nucleus doctor`, audit log, and idle reaper. New Troubleshooting section leads with `nucleus doctor`.
- Launch material drafted under `docs/launch/`: HN post, X/Twitter thread, demo gif storyboard, post-launch checklist.

### Known gaps (carried forward)

- Mid-session cwd-change hot-swap.
- Native OAuth (still bridges via `mcp-remote` for HTTP connectors — weeks of work, not a launch blocker).

## [0.1.4] — 2026-04-24

### Added
- **Streamable HTTP transport** via `nucleus serve --http <addr>`. Run nucleus as a long-lived local daemon so you can paste its URL (`http://127.0.0.1:8787/mcp` by default) into Claude's **Add custom connector** dialog, which only accepts HTTP(S) endpoints.
- Safety defaults for HTTP mode:
  - Loopback-only binds (127.0.0.1) don't require auth.
  - Non-loopback binds refuse to start without `--token <secret>`, which activates constant-time-compared bearer-token auth on every request.
  - Validation runs *before* upstream children are spawned, so a misconfigured bind fails in <1 s instead of wasting an `npx` spawn.
- `GET /healthz` endpoint for external readiness probes.

### Changed
- Server construction split: `Gateway.Prepare(ctx)` does the workspace resolve + upstream spawns; `Gateway.ServeStdio()` / `ServeHTTP(ctx, opts)` choose the transport. Stdio behavior unchanged.
- Banner re-rendered with canonical tagline **"One connector, many accounts."** (was *"one MCP to recommend them all"*). LOTR flavor moved to the orbital rune text only.
- v0.1.1 / v0.1.2 / v0.1.3 release notes backfilled with the canonical tagline as the opening line.

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
