# Twitter / X tweet thread — Nucleus

**Post timing:** within 30 minutes of the HN submission, *not* before — you want the HN URL in tweet 8 so the thread doubles as a top-of-funnel for the HN post. If HN doesn't go (under 30 pts at 90 min), still post the thread; it stands alone.

**Author posture:** plain prose, no engagement-bait threads ("a thread 🧵"), no all-lowercase aesthetic. People follow accounts that talk like adults.

**Length per tweet:** target 240 chars to leave room for a quote-retweet without truncation.

---

## Tweet 1 (the hook)

```
Every MCP client today (Claude Code, Cursor, Claude Desktop) treats one connector as one slot.

One Supabase. One GitHub PAT.

Want Claude to peek at prod from a staging conversation? Disconnect, reconnect, restart, lose your context.

I got tired of it. Built Nucleus.
```

Why this works as the opener: pain you actually have, in three lines. The reader doesn't need to know what MCP is — "one Supabase, one GitHub PAT" carries enough shape on its own. No emojis, no "🧵", no "BREAKING".

## Tweet 2 (the shape)

```
Nucleus is a local MCP gateway. It holds N authenticated profiles per service and exposes them all to your client at once:

  supabase_prod_execute_sql
  supabase_staging_execute_sql
  github_work_create_issue
  github_personal_create_issue

One connector slot. Many accounts.
```

Code block in monospace makes it scannable. The naming convention does most of the explaining for you.

## Tweet 3 (the surprise — this is the moment)

```
The thing that surprised me building it:

once you can hold multiple profiles, you can FAN OUT across them.

"Compare the users table between prod and staging" becomes ONE tool call.

Not two sequential round-trips. Not two conversations. One.
```

This is the tweet that gets retweeted. It's the only one with capitalized emphasis — use sparingly so it lands.

## Tweet 4 (the gif placeholder — REPLACE WITH ACTUAL DEMO)

```
[ATTACH: 30-second demo gif of asking Claude to compare prod and staging in one sentence, showing the merged result]

This is `nucleus_call_plan`. The gateway's shape makes it almost free.
```

The gif is the single most important asset in the entire launch. See `demo-gif-storyboard.md` for what to record.

## Tweet 5 (build credibility — what's in the box)

```
What ships:

  - Stdio + HTTP transports (Claude Code CLI and Claude UI both work)
  - OS-keychain credential storage (Keychain / libsecret / Credential Manager)
  - Workspace-aware profile resolution
  - Per-call audit log (JSONL, rotated) + `nucleus logs`
  - policy.toml: deny + require-confirmation rules
  - Idle reaper with transparent respawn
```

This tweet earns the tweet 6 click. Without it, "looks like a toy."

## Tweet 6 (the trust unlock — policy)

```
You can hand Nucleus to a junior on the team:

[[rule]]
match  = "supabase:atlas"
deny   = ["apply_migration", "delete_branch"]
reason = "atlas is the production project — schema changes go through CI"

[[rule]]
match   = "supabase:atlas"
confirm = ["execute_sql"]
phrase  = "I understand atlas is PRODUCTION"
```

The TOML block is the proof. People who read this are operators; they want to see the config, not be told it exists.

## Tweet 7 (transparency — `because:`)

```
Every recommendation explains itself:

  because: ["matched 'sql' in tool name",
            "matched 'atlas' in alias",
            "sticky from last call"]

Opaque ranking is bad ranking. ~30 lines of code, biggest UX win in the project.
```

## Tweet 8 (the call to action + HN link)

```
Open source. Local-only by default. Single Go binary.

  brew install doramirdor/homebrew-tap/nucleus
  go install github.com/doramirdor/nucleusmcp/cmd/nucleus@latest

Repo:  https://github.com/doramirdor/nucleusmcp
HN:    [PASTE HN URL]

Would love your feedback — especially the "this breaks because…" kind.
```

The HN URL goes here so anyone arriving at the tweet thread has a one-click path to comment. Asking for "this breaks because…" feedback (instead of generic "thoughts?") biases responses toward useful bug reports.

---

## Reply hooks for inevitable comments

**"How is this different from MetaMCP / Microsoft mcp-gateway?"**

> Both aggregate multiple MCP servers behind one address — that's commodity. Nucleus's wedge is multiple authenticated profiles of the *same* upstream service, with workspace-aware resolution and per-tool profile context in the description. None of the gateway-shaped projects lead with that.

**"Anthropic is going to build this into the protocol"**

> Hopefully! The MCP roadmap explicitly calls out gateway/proxy patterns as a workstream — they're standardizing the surface, not building a gateway. Fastest path forward is third-party gateways figuring out the UX, then Anthropic codifying what worked.

**"Why Go?"**

> Single static binary, no runtime, brew-installable, and the OS-keychain libs are mature. Mostly: I want `nucleus` to be one file the user drops on PATH and forgets about.

**"Is this safe?"**

> Loopback-only by default. Tokens never written to disk in plaintext (OS keychain). Tokens never logged. Per-profile OAuth dirs at 0o700. Tool args summarized as sorted keys + SHA-256 hash in the audit log unless you explicitly opt into full-args. There's a policy.toml gate for write/destructive tools.

**"Will you build SSO / team-shared profiles?"**

> Not soon. That's a different product (an "Okta for MCP") that sells, doesn't star. Solo dev tooling stays sharp by being useful to one person on one laptop. If org demand is loud enough I'll revisit.

---

## What NOT to do

- Don't use 🧵 / 📌 / 👇. They mark you as a thread-farmer.
- Don't quote-tweet your own thread to "boost" it. That move is dead now.
- Don't tag Anthropic or @AnthropicAI in the post. If they want to share it, they will; tagging looks like begging.
- Don't post the thread before the HN URL exists. The CTA in tweet 8 is half the value.
