// Search-mode meta-tools.
//
// In ModeSearch the router advertises three tools to the MCP client,
// regardless of how many upstream profiles are loaded:
//
//   - nucleus_find_tool(intent, connector?, limit?)   → ranked candidates
//   - nucleus_call(name, arguments)                    → dispatch by name
//   - nucleus_call_plan(steps, parallelism?)           → fan out across profiles
//
// This shrinks the client-visible tool surface from O(profiles × tools)
// (which can be 200+ definitions for a power user) to a constant 3,
// trading one extra round-trip per task for dramatically smaller prompt
// context. The full catalog stays in the gateway and is consulted by a
// lexical ranker on every find_tool call.
//
// The ranker is intentionally simple (token-overlap with field
// boosts). It's deterministic, dependency-free, and good enough for the
// kinds of intents users actually type ("supabase prod sql", "list
// github prs in work account"). A Recommender interface is left as a
// natural extension point if/when an embeddings-based ranker is wanted.
//
// # Fan-out
//
// nucleus_call_plan lets the client issue one tool-call that the gateway
// turns into N parallel calls across profiles, returning a merged
// result. This is the killer feature for the multi-profile shape — a
// query like "compare the users table between prod and staging" becomes
// a single round-trip instead of N. find_tool detects fan-out-shaped
// intents (mentions of multiple aliases, "compare", "between", etc.)
// and emits a `fanout_suggestion` block alongside the ranked tools so
// the LLM has a ready-made plan to invoke.

package router

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/doramirdor/nucleusmcp/internal/audit"
)

// defaultFindToolLimit caps how many candidates nucleus_find_tool returns
// when the caller doesn't specify. Chosen to keep the response small
// enough that returning full JSON schemas is still cheap, while large
// enough that the right tool almost always lands in the top-N.
const defaultFindToolLimit = 8

// maxFindToolLimit guards against pathological inputs (e.g. limit=10000)
// that would defeat the whole point of search mode.
const maxFindToolLimit = 50

// registerSearchTools wires up the two meta-tools against the catalog
// already accumulated by RegisterChild. Called from Finalize.
func (r *Router) registerSearchTools() error {
	if len(r.catalog) == 0 {
		// Still register the tools — they'll just return empty/errors.
		// The alternative (silently doing nothing) leaves the client with
		// zero tools and no way to discover that the gateway is empty,
		// which is a worse failure mode than a tool that says so.
	}

	r.s.AddTool(buildFindToolTool(summarizeCatalog(r.catalog)), r.handleFindTool)
	r.s.AddTool(buildCallTool(), r.handleCall)
	r.s.AddTool(buildCallPlanTool(), r.handleCallPlan)
	return nil
}

// summarizeCatalog produces the one-line breakdown of "what's loaded"
// that's spliced into the find_tool description, so Claude can see the
// shape of the gateway's catalog at connect time without having to call
// find_tool just to enumerate connectors.
func summarizeCatalog(entries []CatalogEntry) string {
	if len(entries) == 0 {
		return "No profiles currently loaded."
	}
	type agg struct {
		aliases map[string]struct{}
		tools   int
	}
	by := map[string]*agg{}
	for _, e := range entries {
		a, ok := by[e.Connector]
		if !ok {
			a = &agg{aliases: map[string]struct{}{}}
			by[e.Connector] = a
		}
		a.aliases[e.Alias] = struct{}{}
		a.tools++
	}
	names := make([]string, 0, len(by))
	for k := range by {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("Loaded: ")
	for i, n := range names {
		if i > 0 {
			b.WriteString("; ")
		}
		a := by[n]
		aliases := make([]string, 0, len(a.aliases))
		for k := range a.aliases {
			aliases = append(aliases, k)
		}
		sort.Strings(aliases)
		fmt.Fprintf(&b, "%s [%s] (%d tools)",
			n, strings.Join(aliases, ", "), a.tools)
	}
	return b.String()
}

func buildFindToolTool(catalogSummary string) mcp.Tool {
	desc := strings.Join([]string{
		"Search Nucleus's catalog of proxied tools by intent and return ",
		"the top matches as fully-formed tool definitions (name, ",
		"description, JSON schema) ready to invoke via nucleus_call. ",
		"Use this whenever you need to do something against an upstream ",
		"service (Supabase, GitHub, etc.) — call this first to find the ",
		"right tool for the job, then call nucleus_call with the chosen ",
		"name and the tool's arguments.\n\n",
		catalogSummary,
	}, "")

	return mcp.NewTool("nucleus_find_tool",
		mcp.WithDescription(desc),
		mcp.WithString("intent",
			mcp.Required(),
			mcp.Description("Plain-language description of what you want "+
				"to do, e.g. 'run a SQL query on the staging supabase' or "+
				"'list open issues on the work github'.")),
		mcp.WithString("connector",
			mcp.Description("Optional connector filter (e.g. 'supabase', "+
				"'github'). When set, only tools from that connector are "+
				"considered.")),
		mcp.WithNumber("limit",
			mcp.Description(fmt.Sprintf(
				"Max number of candidates to return. Default %d, capped at %d.",
				defaultFindToolLimit, maxFindToolLimit))),
	)
}

func buildCallTool() mcp.Tool {
	// Use raw JSON schema so `arguments` can be a free-form object —
	// mcp-go's WithObject helper insists on a fixed property set, which
	// would force us to lie about the schema.
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {
				"type": "string",
				"description": "Full namespaced tool name returned by nucleus_find_tool, e.g. 'supabase_atlas_execute_sql'."
			},
			"arguments": {
				"type": "object",
				"description": "Arguments object matching the chosen tool's input schema. Pass {} for tools that take no arguments.",
				"additionalProperties": true
			}
		},
		"required": ["name", "arguments"]
	}`)
	return mcp.NewToolWithRawSchema("nucleus_call",
		"Invoke a proxied tool by its full namespaced name. Look up the "+
			"name and its argument schema with nucleus_find_tool first.",
		raw)
}

// handleFindTool runs the lexical ranker over the catalog and returns the
// top-N matching tools, each as a JSON object the client can use directly
// to construct a nucleus_call.
func (r *Router) handleFindTool(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	intent, _ := args["intent"].(string)
	intent = strings.TrimSpace(intent)
	if intent == "" {
		return mcp.NewToolResultError("intent is required and must be non-empty"), nil
	}

	connectorFilter, _ := args["connector"].(string)
	connectorFilter = strings.ToLower(strings.TrimSpace(connectorFilter))

	limit := defaultFindToolLimit
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	if limit > maxFindToolLimit {
		limit = maxFindToolLimit
	}

	results := rankCatalogWithSticky(r.catalog, intent, connectorFilter, limit, r.stickySnapshot())
	fanout := detectFanout(r.catalog, intent, connectorFilter, results)

	payload := struct {
		Query            string             `json:"query"`
		Returned         int                `json:"returned"`
		Total            int                `json:"total_in_catalog"`
		Tools            []findToolHit      `json:"tools"`
		FanoutSuggestion *fanoutSuggestion  `json:"fanout_suggestion,omitempty"`
	}{
		Query:            intent,
		Returned:         len(results),
		Total:            len(r.catalog),
		Tools:            results,
		FanoutSuggestion: fanout,
	}

	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		// Marshaling our own struct failing would mean a bug, not a
		// caller error — surface it loudly.
		return nil, fmt.Errorf("marshal find_tool result: %w", err)
	}
	return mcp.NewToolResultText(string(body)), nil
}

// handleCall is the dispatcher for nucleus_call. It looks the requested
// name up in the handler map populated by RegisterChild and forwards to
// the matching upstream proxy handler.
func (r *Router) handleCall(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return mcp.NewToolResultError("name is required"), nil
	}
	handler, ok := r.handlers[name]
	if !ok {
		// Be helpful: if the user typo'd a connector or alias, hint at
		// what's actually available rather than just saying "not found".
		return mcp.NewToolResultError(fmt.Sprintf(
			"unknown tool %q. Call nucleus_find_tool first to discover "+
				"valid names. %s", name, summarizeCatalog(r.catalog))), nil
	}

	// Re-shape the request as if the client had called the proxied tool
	// directly: the upstream handler expects req.Params.Name to be the
	// upstream's tool name (which makeHandler ignores in favor of the
	// captured upstreamTool — but Arguments needs to be the inner map).
	inner, ok := args["arguments"].(map[string]any)
	if !ok {
		// Arguments missing/null is fine — many tools take no args.
		inner = map[string]any{}
	}

	proxiedReq := mcp.CallToolRequest{}
	proxiedReq.Params.Name = name
	proxiedReq.Params.Arguments = inner

	res, err := handler(withVia(ctx, audit.ViaCall), proxiedReq)
	if err != nil {
		// Wrap the error with the namespaced name so logs/clients can
		// trace which proxied tool blew up.
		return nil, fmt.Errorf("nucleus_call %s: %w", name, err)
	}
	return res, nil
}

// findToolHit is the per-tool record returned in nucleus_find_tool's
// JSON response. Description and InputSchema mirror the proxied tool
// exactly, so Claude has everything it needs to construct an
// nucleus_call without any further round-trip.
//
// Because is the human-readable explanation of *why* this hit ranks
// where it does — one short string per contributing signal ("matched
// 'sql' in tool name", "matched alias 'atlas'", "sticky from last
// call"). It costs nothing in latency, makes the recommender
// auditable, and is the single biggest UX upgrade for users who'd
// otherwise have to trust the ranker as a black box.
type findToolHit struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Score       float64         `json:"score"`
	Connector   string          `json:"connector"`
	Alias       string          `json:"alias"`
	Because     []string        `json:"because,omitempty"`
}

// rankCatalog is the no-sticky entry point used by tests and callers
// who don't have a Router context. It defers to rankCatalogWithSticky
// with an empty sticky map so behavior is identical to the original
// pre-sticky ranker.
func rankCatalog(catalog []CatalogEntry, intent, connectorFilter string, limit int) []findToolHit {
	return rankCatalogWithSticky(catalog, intent, connectorFilter, limit, nil)
}

// rankCatalogWithSticky scores every catalog entry against the intent
// (and optional connector filter) and returns the top `limit` hits
// sorted score-descending. Ties broken by name for determinism. The
// sticky map (connector → last-used alias) provides a small
// disambiguation bias: when two profiles of the same connector tie on
// lexical signal, the one the user last actually worked with wins.
//
// Sticky is suppressed when the user explicitly mentioned an alias by
// name in the intent — they were specific for a reason and we don't
// want sticky overriding their stated choice.
func rankCatalogWithSticky(catalog []CatalogEntry, intent, connectorFilter string, limit int, sticky map[string]string) []findToolHit {
	tokens := tokenize(intent)
	if len(tokens) == 0 {
		return nil
	}

	// Identify which connectors had an alias explicitly mentioned in
	// the intent — sticky doesn't apply to those.
	mentionedConnectors := map[string]bool{}
	if len(sticky) > 0 {
		aliasesByConnector := map[string]map[string]bool{}
		for _, e := range catalog {
			c := strings.ToLower(e.Connector)
			if aliasesByConnector[c] == nil {
				aliasesByConnector[c] = map[string]bool{}
			}
			aliasesByConnector[c][strings.ToLower(e.Alias)] = true
		}
		for _, tok := range tokens {
			for c, aliases := range aliasesByConnector {
				if aliases[tok] {
					mentionedConnectors[c] = true
				}
			}
		}
	}

	type scored struct {
		idx     int
		score   float64
		reasons []string
	}
	scoredAll := make([]scored, 0, len(catalog))

	for i, e := range catalog {
		if connectorFilter != "" && !strings.EqualFold(e.Connector, connectorFilter) {
			continue
		}
		s, reasons := scoreEntryWithReasons(e, tokens)
		if s <= 0 {
			continue
		}
		// Sticky bias: small, additive, only when (a) the connector
		// has a sticky alias, (b) it matches this entry's alias, and
		// (c) the user didn't explicitly mention any alias for this
		// connector. Bias is small enough that lexical signal still
		// dominates — sticky is a tiebreaker, not an override.
		c := strings.ToLower(e.Connector)
		if want, ok := sticky[c]; ok && !mentionedConnectors[c] && strings.EqualFold(want, e.Alias) {
			const wSticky = 1.25
			s += wSticky
			reasons = append(reasons, "sticky from last call")
		}
		scoredAll = append(scoredAll, scored{idx: i, score: s, reasons: reasons})
	}

	sort.Slice(scoredAll, func(i, j int) bool {
		if scoredAll[i].score != scoredAll[j].score {
			return scoredAll[i].score > scoredAll[j].score
		}
		return catalog[scoredAll[i].idx].Name < catalog[scoredAll[j].idx].Name
	})

	if len(scoredAll) > limit {
		scoredAll = scoredAll[:limit]
	}

	out := make([]findToolHit, 0, len(scoredAll))
	for _, s := range scoredAll {
		e := catalog[s.idx]
		// Marshal the inputSchema as raw JSON so the client gets the
		// exact shape mcp-go would have published in expose-all mode.
		// We round-trip through MarshalJSON on the Tool to reuse its
		// RawInputSchema-vs-InputSchema logic.
		schema, err := marshalToolInputSchema(e.Tool)
		if err != nil {
			// Skip rather than fail the whole find_tool — better to
			// return 7 hits than 0 because one tool has a malformed
			// schema upstream.
			continue
		}
		out = append(out, findToolHit{
			Name:        e.Name,
			Description: e.Tool.Description,
			InputSchema: schema,
			Score:       round3(s.score),
			Connector:   e.Connector,
			Alias:       e.Alias,
			Because:     s.reasons,
		})
	}
	return out
}

// stickySnapshot returns a copy of the sticky map so the ranker reads
// from a stable view even if a concurrent dispatch updates it
// mid-rank. Map size is O(connectors) — tiny.
func (r *Router) stickySnapshot() map[string]string {
	r.stickyMu.RLock()
	defer r.stickyMu.RUnlock()
	out := make(map[string]string, len(r.sticky))
	for k, v := range r.sticky {
		out[k] = v
	}
	return out
}

// marshalToolInputSchema extracts just the inputSchema field from the
// Tool's full JSON marshal. Reuses mcp-go's logic for choosing between
// InputSchema and RawInputSchema.
func marshalToolInputSchema(t mcp.Tool) (json.RawMessage, error) {
	full, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var probe struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(full, &probe); err != nil {
		return nil, err
	}
	if len(probe.InputSchema) == 0 {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	return probe.InputSchema, nil
}

// scoreEntry is the legacy entry point — returns score only, drops
// reasons. Kept for any callers (or tests) that don't need the
// explanation; production callers use scoreEntryWithReasons via
// rankCatalogWithSticky.
func scoreEntry(e CatalogEntry, queryTokens []string) float64 {
	s, _ := scoreEntryWithReasons(e, queryTokens)
	return s
}

// scoreEntryWithReasons computes a single relevance score for one
// catalog entry against an already-tokenized query, AND returns the
// list of contributing signals so the caller can show them to the
// user (the `because` field on findToolHit).
//
// Field boosts (rough rationale):
//   - tool name: highest signal — tool names are usually verb-y and
//     specific ("execute_sql", "create_branch")
//   - alias: identifies which profile/account the user wants
//   - connector: identifies the service
//   - description: long, mostly prose, lower signal per match
//
// A token can score in multiple fields; we sum across fields.
// Substring match counts for half — catches "sql" matching
// "execute_sql" without requiring a tokenizer that splits on
// underscores (which we do anyway, but the half-credit makes partial
// matches like "exec" still useful).
//
// Reasons are deduplicated per signal type — five tokens all matching
// the description don't need to produce five identical reasons.
func scoreEntryWithReasons(e CatalogEntry, queryTokens []string) (float64, []string) {
	nameTokens := tokenize(e.UpstreamTool)
	descTokens := tokenize(e.Tool.Description)
	connectorTokens := tokenize(e.Connector)
	aliasTokens := tokenize(e.Alias)

	const (
		wName      = 4.0
		wAlias     = 2.5
		wConnector = 2.0
		wDesc      = 1.0
		wSubstring = 0.5
	)

	var total float64
	loweredName := strings.ToLower(e.UpstreamTool)
	loweredDesc := strings.ToLower(e.Tool.Description)

	// Collect matched tokens per field so the reason strings are
	// concrete ("matched 'sql' in tool name") rather than vague.
	var nameHits, aliasHits, connHits, descHits []string

	for _, q := range queryTokens {
		switch {
		case containsToken(nameTokens, q):
			total += wName
			nameHits = append(nameHits, q)
		case strings.Contains(loweredName, q):
			total += wName * wSubstring
			nameHits = append(nameHits, q)
		}
		if containsToken(aliasTokens, q) {
			total += wAlias
			aliasHits = append(aliasHits, q)
		}
		if containsToken(connectorTokens, q) {
			total += wConnector
			connHits = append(connHits, q)
		}
		switch {
		case containsToken(descTokens, q):
			total += wDesc
			descHits = append(descHits, q)
		case strings.Contains(loweredDesc, q):
			total += wDesc * wSubstring
			descHits = append(descHits, q)
		}
	}

	var reasons []string
	if len(nameHits) > 0 {
		reasons = append(reasons, fmt.Sprintf("matched %s in tool name", quoteJoin(nameHits)))
	}
	if len(aliasHits) > 0 {
		reasons = append(reasons, fmt.Sprintf("matched %s in alias %q", quoteJoin(aliasHits), e.Alias))
	}
	if len(connHits) > 0 {
		reasons = append(reasons, fmt.Sprintf("matched %s in connector %q", quoteJoin(connHits), e.Connector))
	}
	if len(descHits) > 0 {
		reasons = append(reasons, fmt.Sprintf("matched %s in description", quoteJoin(descHits)))
	}
	return total, reasons
}

// quoteJoin renders a list of tokens as a quoted, comma-separated
// list ("'sql', 'execute'") for embedding in `because` strings. Drops
// duplicates while preserving order so the same token doesn't show up
// twice.
func quoteJoin(toks []string) string {
	seen := map[string]bool{}
	parts := make([]string, 0, len(toks))
	for _, t := range toks {
		if seen[t] {
			continue
		}
		seen[t] = true
		parts = append(parts, fmt.Sprintf("%q", t))
	}
	return strings.Join(parts, ", ")
}

// minTokenLen is the cutoff for keeping a token. Tokens shorter than
// this are noise: single letters like "a" / "i" match as substrings of
// almost everything, and two-letter tokens like "on" / "of" / "id" are
// usually stopwords. Three-letter tokens like "sql" / "api" / "url" are
// load-bearing in this domain, so 3 is the sweet spot.
const minTokenLen = 3

// tokenize lowercases and splits on any non-alphanumeric run, then
// drops tokens shorter than minTokenLen. Stable across Unicode locales
// because we use unicode.IsLetter / unicode.IsDigit rather than
// ASCII-only checks.
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 8)
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= minTokenLen {
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return out
}

func containsToken(tokens []string, q string) bool {
	for _, t := range tokens {
		if t == q {
			return true
		}
	}
	return false
}

func round3(f float64) float64 {
	// Three decimal places is plenty for a relevance score, and avoids
	// the JSON marshal printing 17 digits of nonsense precision.
	return float64(int(f*1000+0.5)) / 1000
}

// fanoutKeywords flag intents that ask for the same operation across
// multiple profiles. Keep this list tight: false positives (matching
// every intent that happens to contain "all") would make the suggestion
// noise. These tokens were chosen because they almost always indicate
// *comparison* or *aggregation* across profiles, which is exactly the
// shape nucleus_call_plan was built for.
var fanoutKeywords = map[string]struct{}{
	"compare": {}, "comparison": {}, "diff": {}, "differ": {},
	"between": {}, "across": {}, "each": {}, "every": {},
	"both": {}, "all": {}, "versus": {},
}

// fanoutSuggestion is the optional block returned by nucleus_find_tool
// when the intent looks like it wants to fan out across profiles. The
// LLM can read this and invoke nucleus_call_plan directly with the
// suggested step names, filling in shared arguments once.
type fanoutSuggestion struct {
	// Rationale is a one-line human-readable explanation of why the
	// suggestion fired. Surfaced in the JSON payload so the LLM (and
	// debugging humans) can audit why fan-out was offered.
	Rationale string `json:"rationale"`
	// Tool is the upstream tool name shared across the suggested steps,
	// e.g. "execute_sql". Useful when the LLM wants to communicate
	// "run this same operation everywhere" without re-stating each step.
	Tool string `json:"tool"`
	// Connector is the connector all suggested steps share. Fan-out is
	// always within a single connector — cross-connector "compare github
	// to linear" is meaningful but not what this suggestion serves.
	Connector string `json:"connector"`
	// Steps is the list of namespaced tool names to invoke. The caller
	// constructs the arguments and passes them to nucleus_call_plan.
	Steps []string `json:"steps"`
}

// detectFanout returns a non-nil suggestion when the intent + ranked
// hits indicate the user wants the same operation against multiple
// profiles. Two independent triggers, either is enough:
//
//  1. Intent contains a fan-out keyword AND the top hits include the
//     same upstream tool across 2+ aliases of the same connector.
//  2. Intent explicitly mentions 2+ alias names that exist in the
//     catalog (e.g. "atlas vs default", "prod and staging").
//
// Returning nil is the common case — only emit a suggestion when we're
// confident, otherwise it's noise.
func detectFanout(catalog []CatalogEntry, intent, connectorFilter string, hits []findToolHit) *fanoutSuggestion {
	if len(hits) == 0 {
		return nil
	}
	tokens := tokenize(intent)
	if len(tokens) == 0 {
		return nil
	}

	// Index aliases-per-connector once so both triggers are cheap.
	aliasesByConnector := map[string]map[string]bool{}
	for _, e := range catalog {
		c := strings.ToLower(e.Connector)
		if connectorFilter != "" && c != connectorFilter {
			continue
		}
		if aliasesByConnector[c] == nil {
			aliasesByConnector[c] = map[string]bool{}
		}
		aliasesByConnector[c][strings.ToLower(e.Alias)] = true
	}

	// Trigger 2: explicit alias mentions. Walk tokens, collect any that
	// match a known alias for any connector. Two distinct matches under
	// the same connector → fan out.
	mentionedByConnector := map[string]map[string]bool{}
	for _, tok := range tokens {
		for c, aliases := range aliasesByConnector {
			if aliases[tok] {
				if mentionedByConnector[c] == nil {
					mentionedByConnector[c] = map[string]bool{}
				}
				mentionedByConnector[c][tok] = true
			}
		}
	}

	// Trigger 1: fan-out keyword present.
	hasKeyword := false
	for _, tok := range tokens {
		if _, ok := fanoutKeywords[tok]; ok {
			hasKeyword = true
			break
		}
	}

	// We need a top hit's upstream tool to anchor the suggestion. Pick
	// the top hit and look for sibling aliases offering the same
	// upstream tool — that's the fan-out target.
	top := hits[0]
	siblings := []string{top.Name}
	siblingAliases := map[string]bool{strings.ToLower(top.Alias): true}
	upstreamFromTop := ""
	for _, e := range catalog {
		if e.Name == top.Name {
			upstreamFromTop = e.UpstreamTool
			break
		}
	}
	if upstreamFromTop == "" {
		return nil
	}
	for _, e := range catalog {
		if e.Connector != top.Connector || e.UpstreamTool != upstreamFromTop {
			continue
		}
		al := strings.ToLower(e.Alias)
		if siblingAliases[al] {
			continue
		}
		// If the user explicitly mentioned aliases, only include the
		// mentioned ones — they were specific for a reason.
		if mentioned := mentionedByConnector[strings.ToLower(top.Connector)]; len(mentioned) > 0 {
			if !mentioned[al] && !mentioned[strings.ToLower(top.Alias)] {
				continue
			}
		}
		siblings = append(siblings, e.Name)
		siblingAliases[al] = true
	}

	// Suppress the suggestion in two cases that look like fan-out at
	// first glance but aren't:
	//   - Only one sibling found → there's nothing to fan out to.
	//   - Neither trigger fired → we'd be guessing.
	if len(siblings) < 2 {
		return nil
	}
	mentionsMatched := mentionedByConnector[strings.ToLower(top.Connector)]
	multipleMentions := len(mentionsMatched) >= 2
	if !hasKeyword && !multipleMentions {
		return nil
	}

	// Stable order so tests don't flake and humans can eyeball the
	// suggestion. Within a connector, alphabetical by full name.
	sort.Strings(siblings)

	rationale := buildFanoutRationale(hasKeyword, multipleMentions, mentionsMatched, len(siblings))
	return &fanoutSuggestion{
		Rationale: rationale,
		Tool:      upstreamFromTop,
		Connector: top.Connector,
		Steps:     siblings,
	}
}

func buildFanoutRationale(hasKeyword, multipleMentions bool, mentions map[string]bool, n int) string {
	switch {
	case hasKeyword && multipleMentions:
		names := sortedKeys(mentions)
		return fmt.Sprintf("Intent uses comparison wording and names %d profiles (%s); call nucleus_call_plan to run the same tool against all %d.",
			len(names), strings.Join(names, ", "), n)
	case multipleMentions:
		names := sortedKeys(mentions)
		return fmt.Sprintf("Intent names %d profiles (%s); nucleus_call_plan runs the same tool against all of them in parallel.",
			len(names), strings.Join(names, ", "))
	default:
		return fmt.Sprintf("Intent uses comparison wording; nucleus_call_plan runs the matched tool against all %d profiles in parallel.", n)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// callPlanDefaultParallelism caps how many fan-out steps run
// concurrently when the caller doesn't specify. 4 is enough to overlap
// network/disk on typical "compare prod/staging/dev" plans without
// hammering an upstream that doesn't expect a stampede.
const callPlanDefaultParallelism = 4

// callPlanMaxParallelism guards against pathological inputs. Beyond
// this, the bottleneck is almost always the slowest upstream, not the
// scheduler — more parallelism just risks rate-limit fallout.
const callPlanMaxParallelism = 16

// callPlanMaxSteps caps the number of steps in a single plan. Plans
// larger than this are almost always a planner mistake; fail loudly
// instead of issuing 1000 calls and hoping it works out.
const callPlanMaxSteps = 32

// callPlanStepTimeout bounds how long any single step can run before
// it's reported as a timeout in the merged result. The whole plan is
// not aborted on a single timeout — partial success is more useful
// than failing the whole fan-out for one slow upstream.
const callPlanStepTimeout = 60 * time.Second

func buildCallPlanTool() mcp.Tool {
	// Free-form arguments per step → raw JSON schema, same trick as
	// buildCallTool. Each step's arguments object passes through to the
	// proxied tool unchanged.
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"steps": {
				"type": "array",
				"description": "Ordered list of tool calls to fan out. Each step is a {name, arguments} pair where name is a fully namespaced tool from nucleus_find_tool (e.g. supabase_atlas_execute_sql). Steps run concurrently; results are returned in input order.",
				"items": {
					"type": "object",
					"properties": {
						"name": {
							"type": "string",
							"description": "Full namespaced tool name."
						},
						"arguments": {
							"type": "object",
							"description": "Arguments matching the chosen tool's input schema.",
							"additionalProperties": true
						}
					},
					"required": ["name", "arguments"]
				}
			},
			"parallelism": {
				"type": "number",
				"description": "Optional upper bound on concurrent in-flight steps. Defaults to 4, capped at 16."
			}
		},
		"required": ["steps"]
	}`)
	desc := "Fan out a single intent to multiple proxied tools in parallel and return one merged result, one entry per step. " +
		"Use this when nucleus_find_tool returns a fanout_suggestion, or anytime you'd otherwise call nucleus_call back-to-back " +
		"against several profiles of the same connector (e.g. \"run this query on prod and staging\")."
	return mcp.NewToolWithRawSchema("nucleus_call_plan", desc, raw)
}

// planStepResult is one row of the merged plan result. Either Result is
// non-nil (success) or Error is non-empty (failure / timeout). Both
// fields are present in JSON so consumers can branch without checking
// types — empty error = success.
type planStepResult struct {
	Index    int             `json:"index"`
	Name     string          `json:"name"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
	DurMS    int64           `json:"duration_ms"`
}

// handleCallPlan dispatches a list of steps in parallel with a bounded
// worker pool. Per-step failures are captured and returned alongside
// successes — the plan as a whole only errors if the input is invalid.
func (r *Router) handleCallPlan(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	rawSteps, ok := args["steps"].([]any)
	if !ok || len(rawSteps) == 0 {
		return mcp.NewToolResultError("steps is required and must be a non-empty array"), nil
	}
	if len(rawSteps) > callPlanMaxSteps {
		return mcp.NewToolResultError(fmt.Sprintf(
			"too many steps: %d > %d. Split into smaller plans or filter the fan-out target list.",
			len(rawSteps), callPlanMaxSteps)), nil
	}

	parallelism := callPlanDefaultParallelism
	if v, ok := args["parallelism"].(float64); ok && v > 0 {
		parallelism = int(v)
	}
	if parallelism > callPlanMaxParallelism {
		parallelism = callPlanMaxParallelism
	}
	if parallelism > len(rawSteps) {
		parallelism = len(rawSteps)
	}

	// Pre-validate every step before we start dispatching. A typo in
	// step #5 should fail the whole plan synchronously, not after we've
	// already mutated state on steps #1-4.
	type step struct {
		name string
		args map[string]any
	}
	steps := make([]step, len(rawSteps))
	for i, raw := range rawSteps {
		obj, ok := raw.(map[string]any)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("steps[%d] must be an object", i)), nil
		}
		name, _ := obj["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return mcp.NewToolResultError(fmt.Sprintf("steps[%d].name is required", i)), nil
		}
		if _, known := r.handlers[name]; !known {
			return mcp.NewToolResultError(fmt.Sprintf(
				"steps[%d]: unknown tool %q. Use nucleus_find_tool to discover valid names.",
				i, name)), nil
		}
		stepArgs, _ := obj["arguments"].(map[string]any)
		if stepArgs == nil {
			stepArgs = map[string]any{}
		}
		steps[i] = step{name: name, args: stepArgs}
	}

	results := make([]planStepResult, len(steps))
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup

	for i, s := range steps {
		i, s := i, s
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			started := time.Now()
			stepCtx, cancel := context.WithTimeout(ctx, callPlanStepTimeout)
			defer cancel()

			handler := r.handlers[s.name]
			proxied := mcp.CallToolRequest{}
			proxied.Params.Name = s.name
			proxied.Params.Arguments = s.args

			res, err := handler(withVia(stepCtx, audit.ViaCallPlan), proxied)
			dur := time.Since(started).Milliseconds()
			row := planStepResult{Index: i, Name: s.name, DurMS: dur}
			switch {
			case err != nil:
				row.Error = err.Error()
			case res != nil && res.IsError:
				row.Result = marshalCallResult(res)
				row.Error = extractErrorText(res)
			case res != nil:
				row.Result = marshalCallResult(res)
			default:
				row.Error = "handler returned nil result with no error"
			}
			results[i] = row
		}()
	}
	wg.Wait()

	successes, failures := 0, 0
	for _, r := range results {
		if r.Error == "" {
			successes++
		} else {
			failures++
		}
	}

	payload := struct {
		Steps       int              `json:"steps"`
		Successes   int              `json:"successes"`
		Failures    int              `json:"failures"`
		Parallelism int              `json:"parallelism"`
		Results     []planStepResult `json:"results"`
	}{
		Steps:       len(steps),
		Successes:   successes,
		Failures:    failures,
		Parallelism: parallelism,
		Results:     results,
	}

	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal call_plan result: %w", err)
	}
	return mcp.NewToolResultText(string(body)), nil
}

// marshalCallResult serializes a tool result to raw JSON for embedding
// in the merged plan response. We re-marshal rather than passing the
// CallToolResult directly so the merged result is one consistent JSON
// document instead of a tree of mcp-go-internal types.
func marshalCallResult(res *mcp.CallToolResult) json.RawMessage {
	if res == nil {
		return nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		// Falling back to a stringified error keeps the merged result
		// valid JSON instead of failing the whole plan because one
		// upstream returned something odd.
		return json.RawMessage(fmt.Sprintf(`{"marshal_error": %q}`, err.Error()))
	}
	return b
}

// extractErrorText pulls the first text-content block from a tool
// result so per-step errors are human-readable in the merged response.
// Used when the upstream signals an error via IsError + a text payload
// rather than a Go error.
func extractErrorText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return "upstream reported error with no text content"
}

// Compile-time guarantee: the meta-tool handlers conform to mcp-go's
// ToolHandlerFunc shape.
var (
	_ mcpserver.ToolHandlerFunc = (*Router)(nil).handleFindTool
	_ mcpserver.ToolHandlerFunc = (*Router)(nil).handleCall
	_ mcpserver.ToolHandlerFunc = (*Router)(nil).handleCallPlan
)
