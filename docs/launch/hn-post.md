# HN launch post — Nucleus

**Status:** draft, not posted. Pick a Tuesday or Wednesday morning (US Pacific 8–9am, US Eastern 11am–noon — that lands on the front page when the most engineers are at their desks). Avoid Mondays (Slack reset) and Thursdays/Fridays (drop-off).

---

## Title (HN, 80-char cap)

**Recommended:**

> Show HN: Nucleus — one MCP connector that holds many authenticated accounts

Backups in priority order:

- `Show HN: I asked Claude to diff my prod and staging databases in one sentence`
- `Show HN: A local MCP gateway so Claude can hold prod and staging at the same time`
- `Show HN: Multi-account MCP gateway for Claude Code, Cursor, Claude Desktop`

Title rules I'm following:
- "Show HN:" prefix mandatory for tools.
- No emojis. HN strips them anyway and they read amateur.
- Lead with the noun ("Nucleus" / "MCP gateway") so a skimmer with no MCP context still gets the shape in two seconds.
- Avoid "AI" in the title — HN crowd has filter fatigue. "MCP" earns more clicks because it's specific.

## Body (under 1500 chars — HN truncates aggressively in the feed)

```
Hi HN — I built Nucleus because every MCP client today (Claude Code, Cursor, Claude Desktop) treats one connector as one slot. One Supabase connection, one GitHub PAT. The moment I wanted Claude to peek at prod from a staging conversation, the dance was: stop the chat, open MCP settings, disconnect, reconnect with the other account, restart, re-paste context. Every switch was minutes of yak-shaving and a lost train of thought.

Nucleus is a local MCP gateway. It sits between your client and the upstream servers, holds N authenticated profiles per service, and exposes them all simultaneously as namespaced tools (supabase_prod_execute_sql, supabase_staging_execute_sql, github_work_*, github_personal_*). Tool descriptions carry the profile context so Claude always knows which account it's about to hit.

The thing that surprised me about building it: once you can hold multiple profiles, you can fan out across them. "Compare the users table between prod and staging" becomes one tool call (`nucleus_call_plan`) instead of two sequential round-trips. That's not possible with vanilla MCP — it's the structural reward for the multi-profile shape.

What's in the box:
- Stdio + HTTP transports (works in Claude Code's CLI and Claude UI's "Add custom connector")
- OS-keychain credential storage (macOS Keychain / libsecret / Credential Manager)
- Workspace-aware profile resolution (.mcp-profiles.toml, autodetect from supabase/config.toml, etc.)
- Per-call audit log (JSONL, rotated) + nucleus logs to tail it
- Optional policy.toml (deny / require-confirmation) gating destructives
- Idle reaper with transparent respawn

Repo: https://github.com/doramirdor/nucleusmcp
brew install doramirdor/homebrew-tap/nucleus
go install github.com/doramirdor/nucleusmcp/cmd/nucleus@latest

Happy to answer questions about the architecture, the wedge vs other MCP gateways, or where this is heading.
```

Why this body:
- Pain → solution → surprise (fan-out) → bullets → install. That order survives a bored reader skimming on mobile.
- The "surprise" paragraph is the ammunition for engaged comments. People reply to the *interesting* part, not the bullet list.
- Install commands at the bottom because the click-to-clone action lives in the comments, not the post.

## First-comment explainer (post immediately, pinned by activity)

Post this **as a self-comment within 30 seconds of the submission going live**. It anchors the discussion before drive-by commenters set the tone, and earns you "OP is here, engaged" goodwill.

```
Author here — happy to answer questions. A few things I learned building this that didn't fit in the post:

1. Multi-account is the wedge, fan-out is the moat. Plenty of MCP gateways aggregate servers (MetaMCP, Microsoft mcp-gateway, etc.) — that's commodity by now. The thing that other gateways structurally can't ship without copying this architecture is one tool call that hits N profiles in parallel. "compare prod vs staging" went from two sequential conversations to a single sentence.

2. Anthropic is paving the road for gateways, not eating them. The MCP roadmap explicitly calls out "Gateway and Proxy Patterns" as a workstream they're standardizing — auth propagation, session semantics, what gateways can see. That's tailwind. Configuration Portability is the watch-item; if servers become trivially shareable across clients, namespacing-as-glue gets less load-bearing.

3. The recommender explains itself. Every result includes a `because` field ("matched 'sql' in tool name", "sticky from last call"). Took ~30 lines and is the single biggest UX upgrade — opaque ranking is bad ranking.

4. Sticky resolution is per-process, not persisted. "I worked on staging this morning" should not silently bias prod work this afternoon. Hard rule.

5. PII posture for the audit log: arguments are summarized as sorted keys + SHA-256 hash by default. Same args twice → same hash, so you can spot retries / loops without exposing contents. Verbatim args only when NUCLEUSMCP_AUDIT_FULL_ARGS=1.

What I deliberately skipped: SSO/SCIM/team-shared profiles. That's a different product (an Okta for MCP) and it sells, doesn't star. Solo devs don't need it; orgs already have other ways. The wedge stays sharp by being useful to one person on one laptop.
```

## When *not* to post

- During a major Anthropic launch day (your post will be drowned).
- During a major outage (tone-deaf).
- Friday after 2pm Pacific (engineers checked out, weekend graveyard).
- Mondays before noon Pacific (catching-up-on-Slack window).

## After-post checklist

- [ ] Copy the exact submission URL (used by tweet thread + the canonical share link).
- [ ] Post the first-comment explainer within 30 seconds.
- [ ] Reply to *every* comment within the first 90 minutes — engagement signals dominate the ranking algorithm in the first hour.
- [ ] Don't ask for upvotes. HN smells it instantly.
- [ ] Don't update the title or body after posting (HN penalizes edits).
- [ ] If the post stalls below 30 points after 90 minutes, it's not going to make the front page; pivot to the tweet thread and try again in 2 weeks with a different framing.

## Targeted follow-ups (post-HN, regardless of outcome)

1. **`r/ClaudeAI`** — repost the body lightly rephrased ("I built…" framing instead of "Show HN:"); link the GitHub.
2. **`r/programming`** — only if HN broke 100 points; otherwise the cross-post smells of attention-shopping.
3. **Anthropic Discord** — drop a link in #show-and-tell with one sentence. Not a sales pitch.
4. **MCP awesome-list PRs** — submit to `e2b-dev/awesome-mcp-gateways` and `punkpeye/awesome-mcp-servers` on the same day. These are how new MCP users discover tools.
