---
title: Tool-search mode (reduce client context by recommending tools instead of listing them all)
status: Proposed
date: 2026-04-27
---

# Problem

Today the router eagerly advertises every `<connector>_<alias>_<tool>` to the MCP client (see `internal/router/router.go` `RegisterChild`, and README resolution rule 5: "expose every profile as a separate namespace"). That's the right default for one or two profiles, but as users add more connectors/profiles the tool list grows linearly:

- 4 connectors × 3 profiles × ~20 tools each ≈ **240 tool definitions** dumped into the model's context at session start.
- Every additional profile multiplies this. The cost is paid on every turn — tool defs sit in the system prompt and increase the surface area Claude has to reason over.

This collides with Nucleus's own pitch ("one connector, many accounts") — the more accounts you add, the worse the context tax gets.

# Proposal — tool-search mode

Add an optional gateway mode where Nucleus exposes a small **meta-tool surface** instead of the full proxied tool list. The model asks Nucleus what it needs; Nucleus returns just the matching tools (or invokes them on the model's behalf).

This mirrors a pattern Claude Code itself uses for its own deferred tools (`ToolSearch` with `select:<tool_name>` queries).

## Two shapes worth considering

**A. Search-then-call (lazy materialization)**
- Nucleus advertises one tool: `nucleus_find_tool(intent: string, connector?: string) → [{name, description, schema}]`.
- Once Claude has picked a tool name, it calls a second meta-tool `nucleus_call(name, args)` — Nucleus proxies as today.
- Pros: minimal context (1–2 tool defs). Works with any MCP client that supports tool calls.
- Cons: extra round-trip per task; Claude has to learn the pattern (Instructions string + tool descriptions do the teaching).

**B. Search-and-register (deferred materialization, like ToolSearch)**
- Nucleus advertises `nucleus_find_tool` plus a small "always-on" set the user pins (e.g. `supabase_prod_*`).
- After `find_tool`, matching tools get *added* to the live tool list for the rest of the session via MCP's `notifications/tools/list_changed`.
- Pros: post-search, calls look native (`supabase_staging_execute_sql(...)` directly).
- Cons: requires the client to honor `list_changed` notifications. Claude Code does; not every client does.

Recommendation: ship **A** first (works everywhere), gate **B** behind a capability check.

## Ranking / recommendation

`find_tool(intent)` needs to return the right handful of tools. Options, cheapest first:

1. **Lexical** — BM25/substring over `connector + alias + tool name + description + metadata`. Zero extra deps, deterministic, good-enough for "supabase prod sql" style queries. Probably the v1.
2. **Embeddings** — small local model (bundled ONNX or via `ollama`) for semantic match. Better for "how do I see who's signed up this week" → `posthog_*persons*`. Adds a dep and a cold-start cost.
3. **LLM-routed** — call out to a cheap model. Cleanest results, worst latency/cost story, requires a key. Skip.

Start with (1); leave a `Recommender` interface so (2) can slot in.

## Config surface

```toml
# .mcp-profiles.toml
[gateway]
mode = "search"     # "expose-all" (default today), "search", or "hybrid"
always_on = ["supabase:atlas"]   # in hybrid mode, these are eagerly advertised
```

CLI knob: `nucleus serve --mode search`.

# Tradeoffs

| | expose-all (today) | tool-search |
|---|---|---|
| Context cost at session start | O(profiles × tools) | O(1) |
| First-call latency | 1 hop | 2 hops (find + call) |
| Discovery UX | Claude sees every tool by name | Claude must ask first |
| Risk of "missed" tools | low | depends on recommender quality |
| Client compatibility | any MCP client | any (mode A); list_changed-aware (mode B) |

The biggest risk is recommender quality — if `find_tool("query the users table")` doesn't surface `supabase_prod_execute_sql`, the user gets worse behavior than today's bloated-but-complete list. Mitigation: log every `find_tool` query + result + which tool ultimately got called, so we can tune the ranker against real traces (and let users opt back into `expose-all`).

# Out of scope for v1

- Cross-profile recommendation ("which Supabase has the `users` table?") — would need schema introspection, separate feature.
- Auto-invocation (Nucleus picks *and* calls the tool). Keep the model in the loop.
- Recommender that learns from prior calls in the same session. Nice-to-have; lexical is the floor.

# Open questions

1. Should `find_tool` return full JSON schemas, or just names + a one-line description and require a follow-up `describe_tool`? (Schemas are the bulk of the bytes.)
2. In hybrid mode, what's the right default for `always_on` — workspace's resolved profile? Most-recently-used? Empty?
3. Do we expose the recommender's score so Claude can decide when to ask the user vs pick? Probably yes.

# Next step

If this shape lands, the smallest useful prototype is mode A + lexical ranker, gated behind `--mode search`, leaving today's `expose-all` path untouched as the default. ~1 new file in `internal/router/` for the meta-tools, ~1 for the ranker, plus a flag wire-up in `internal/server/server.go`.
