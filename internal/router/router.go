// Package router exposes upstream MCP tools to the gateway's own MCP server
// with a namespaced name, and proxies CallTool through to the appropriate
// child.
//
// Naming convention: "<connector>_<alias>_<tool>".
//
//	alias = profile name when no alias is set in .mcp-profiles.toml
//
// Every proxied tool also gets a short prefix injected into its
// description, so an MCP client (e.g. Claude) reading tool metadata
// knows which profile each tool is scoped to without needing a separate
// CLAUDE.md. The prefix has the shape:
//
//	[supabase/atlas project_id=xxx] Execute a SQL query against the project
//
// The proxy itself is transparent: input schemas, descriptions (after
// prefix), and result payloads pass through unchanged.
//
// # Modes
//
// The router has two advertise modes:
//
//   - ModeExposeAll (default, historical): every proxied tool is added
//     directly to the gateway's tool list. The MCP client sees the full
//     catalog at connect time. Best for small numbers of profiles.
//
//   - ModeSearch: only two meta-tools (`nucleus_find_tool`,
//     `nucleus_call`) are advertised. The full catalog is held internally
//     and queried via lexical ranking on `find_tool(intent)`. Drops the
//     client-visible tool count from O(profiles × tools) to O(1), at the
//     cost of one extra round-trip per task. See search.go.
//
// Mode is selected at construction (New / NewWithMode). Profile
// registration is identical in both modes — Finalize must be called after
// all RegisterChild calls so search mode can register its meta-tools.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/doramirdor/nucleusmcp/internal/audit"
	"github.com/doramirdor/nucleusmcp/internal/supervisor"
)

// Mode controls how the router advertises proxied tools to the MCP client.
type Mode int

const (
	// ModeExposeAll registers every proxied tool directly on the gateway
	// server. This is the historical behavior and the default.
	ModeExposeAll Mode = iota

	// ModeSearch advertises only the meta-tools (`nucleus_find_tool` and
	// `nucleus_call`); proxied tools are held in an internal catalog and
	// surfaced on demand. See search.go.
	ModeSearch

	// ModeHybrid advertises one canonical alias per connector directly
	// (the "recommended" path), and exposes the meta-tools for reaching
	// the rest of the catalog. Best when most work goes to one
	// account/profile per service but you occasionally need to dip into
	// another. The canonical alias is chosen per connector by the first
	// matching rule:
	//
	//   1. An explicit "<connector>:<alias>" entry in AlwaysOn.
	//   2. (future) A workspace-resolved primary.
	//   3. The first alias registered for that connector (deterministic
	//      because the resolver iterates in stable order).
	//
	// If a connector has only one alias, ModeHybrid behaves like
	// ModeExposeAll for that connector — nothing to bury.
	ModeHybrid
)

// Router registers proxied tools on a gateway MCP server.
type Router struct {
	s    *mcpserver.MCPServer
	mode Mode

	// catalog accumulates everything RegisterChild sees, regardless of
	// mode — search.go reads it in ModeSearch; ModeExposeAll ignores it.
	catalog []CatalogEntry

	// handlers maps a namespaced tool name to the proxy handler. In
	// ModeExposeAll this is informational; in ModeSearch it's how
	// nucleus_call dispatches.
	handlers map[string]mcpserver.ToolHandlerFunc

	// alwaysOn is the set of explicit "<connector>:<alias>" specs the
	// user pinned for ModeHybrid. Empty means "fall back to the
	// first-alias-per-connector heuristic". Ignored in other modes.
	alwaysOn map[string]bool

	// stickyMu guards sticky.
	stickyMu sync.RWMutex
	// sticky records the last successfully-dispatched alias per
	// connector for the lifetime of the gateway process. The ranker
	// reads it as a small bias when the user's intent is ambiguous
	// between sibling profiles. Cleared when the process exits — we
	// intentionally don't persist this; "I worked on staging this
	// morning" should not silently bias prod work this afternoon.
	sticky map[string]string

	// policy gates write/destructive tool calls before they reach the
	// upstream. Nil means "allow everything" — preserves historical
	// behavior for installs without a policy.toml. See policy.go.
	policy *Policy

	// auditW writes per-call audit entries. Nil means "no audit log"
	// — preserves historical behavior for installs that haven't
	// opted in. Errors writing to the log are logged-and-dropped, never
	// allowed to break a real dispatch.
	auditW *audit.Writer
}

// CatalogEntry is one row of the internal tool catalog: a fully proxied
// tool (descriptions already prefixed) plus the metadata needed to rank
// and dispatch it.
type CatalogEntry struct {
	// Name is the namespaced public name, e.g. "supabase_atlas_execute_sql".
	Name string
	// Connector is the connector kind, e.g. "supabase".
	Connector string
	// Alias is the profile alias segment, e.g. "atlas".
	Alias string
	// UpstreamTool is the original tool name on the upstream child.
	UpstreamTool string
	// Tool is the fully proxied tool (description prefixed, name
	// namespaced) — what the client would have seen in ModeExposeAll.
	Tool mcp.Tool
}

// New creates a router in ModeExposeAll bound to the given MCP server.
// Equivalent to NewWithMode(s, ModeExposeAll).
func New(s *mcpserver.MCPServer) *Router {
	return NewWithMode(s, ModeExposeAll)
}

// NewWithMode creates a router in the given mode.
func NewWithMode(s *mcpserver.MCPServer, mode Mode) *Router {
	return &Router{
		s:        s,
		mode:     mode,
		handlers: map[string]mcpserver.ToolHandlerFunc{},
		alwaysOn: map[string]bool{},
		sticky:   map[string]string{},
	}
}

// SetPolicy attaches a policy to the router. Pass nil to disable
// policy enforcement (the default for installs without a policy.toml).
// Safe to call after construction but before serving — the policy
// reference is read inside makeHandler closures, so swapping policies
// mid-flight is racy and not supported.
func (r *Router) SetPolicy(p *Policy) { r.policy = p }

// SetAuditWriter attaches an audit log destination. Pass nil to
// disable. Same lifetime contract as SetPolicy: configure before
// serving; mid-flight swaps are racy.
func (r *Router) SetAuditWriter(w *audit.Writer) { r.auditW = w }

// viaCtxKey is the unexported context key used to thread the Via
// value (direct / nucleus_call / nucleus_call_plan) from the meta-
// tool dispatcher down into the proxy handler. Plumbing it through
// context avoids changing every handler signature *and* keeps
// in-flight metadata out of arguments map (where it'd be visible to
// the upstream).
type viaCtxKey struct{}

// withVia returns ctx tagged with `v` for downstream proxy handlers.
func withVia(ctx context.Context, v audit.Via) context.Context {
	return context.WithValue(ctx, viaCtxKey{}, v)
}

// viaFromCtx pulls the Via tag out, defaulting to ViaDirect when
// nothing's been set (the historical path: client called the tool by
// its namespaced name without going through a meta-tool).
func viaFromCtx(ctx context.Context) audit.Via {
	if v, ok := ctx.Value(viaCtxKey{}).(audit.Via); ok {
		return v
	}
	return audit.ViaDirect
}

// markSticky records `alias` as the most-recently-used alias for
// `connector`. Called after a successful dispatch so future ranking
// can bias toward whatever the user actually worked with last.
func (r *Router) markSticky(connector, alias string) {
	if connector == "" || alias == "" {
		return
	}
	r.stickyMu.Lock()
	r.sticky[strings.ToLower(connector)] = alias
	r.stickyMu.Unlock()
}

// stickyAlias returns the last-used alias for `connector`, or "" if
// nothing's been dispatched there yet.
func (r *Router) stickyAlias(connector string) string {
	r.stickyMu.RLock()
	defer r.stickyMu.RUnlock()
	return r.sticky[strings.ToLower(connector)]
}

// Mode returns the router's advertise mode.
func (r *Router) Mode() Mode { return r.mode }

// SetAlwaysOn pins specific "<connector>:<alias>" pairs to the
// always-advertised set in ModeHybrid. Pairs that don't match any
// registered profile are kept (logged at Finalize time) so a
// preconfigured list doesn't silently lose entries when a profile is
// temporarily missing. Empty list disables the override and falls back
// to the first-alias-per-connector heuristic.
func (r *Router) SetAlwaysOn(specs []string) {
	r.alwaysOn = map[string]bool{}
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		r.alwaysOn[strings.ToLower(s)] = true
	}
}

// ProfileContext carries profile metadata down from the server so the
// router can splice it into tool descriptions.
type ProfileContext struct {
	// Metadata is the stored profile metadata (e.g. project_id=xxx).
	Metadata map[string]string
	// Note is free-form text the user set (e.g. "PROD — writes require
	// confirmation"). Optional.
	Note string
}

// Dispatcher is the minimal interface makeHandler needs from an
// upstream — just "given a tool call, get me a result." The
// supervisor's *Child satisfies it via Child.CallTool. Extracting
// the interface lets integration tests wire fake dispatchers behind
// the same policy/audit/sticky wrapper the real router uses, without
// constructing a full supervisor.Child (which holds a live mcp-go
// client tied to a subprocess).
type Dispatcher interface {
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
}

// RegisterChild advertises every tool from the child under a namespaced
// name, using the given alias. A short context prefix is injected into
// each tool's description so clients understand which profile owns it.
//
// Passing "" for alias means "use the profile name as the alias" —
// preserves pre-alias behavior.
//
// In ModeExposeAll, each tool is added to the gateway server immediately.
// In ModeSearch, tools are stashed in the internal catalog and the
// gateway-facing meta-tools are added by Finalize.
func (r *Router) RegisterChild(c *supervisor.Child, alias string, pc ProfileContext) error {
	if alias == "" {
		alias = c.Profile
	}
	return r.registerDispatcher(c, c.Connector, c.ProfileID, alias, pc, c.Tools)
}

// registerDispatcher is the underlying registration path used by both
// RegisterChild and integration tests. It accepts any Dispatcher and
// the metadata that would normally come from a supervisor.Child, so
// tests can drive the full proxy wrapper (policy + audit + sticky)
// without spinning up a real subprocess.
func (r *Router) registerDispatcher(
	d Dispatcher,
	connector, profileID, alias string,
	pc ProfileContext,
	tools []mcp.Tool,
) error {
	prefix := buildDescriptionPrefix(connector, alias, pc)
	for _, t := range tools {
		proxied := t
		proxied.Name = NamespacedName(connector, alias, t.Name)
		proxied.Description = prependDescription(prefix, proxied.Description)

		handler := r.makeHandlerForDispatcher(d, connector, profileID, t.Name, alias)
		r.handlers[proxied.Name] = handler

		r.catalog = append(r.catalog, CatalogEntry{
			Name:         proxied.Name,
			Connector:    connector,
			Alias:        alias,
			UpstreamTool: t.Name,
			Tool:         proxied,
		})

		// In ModeExposeAll we eagerly add every tool — preserves the
		// historical behavior exactly. ModeSearch and ModeHybrid defer
		// the AddTool decision to Finalize, where the full catalog is
		// known and the canonical-alias heuristic can run.
		if r.mode == ModeExposeAll {
			r.s.AddTool(proxied, handler)
		}
	}
	return nil
}

// Finalize completes registration after all children have been added.
//
//   - ModeExposeAll: no-op (every tool was added eagerly during
//     RegisterChild).
//   - ModeSearch: register the `nucleus_find_tool` + `nucleus_call`
//     meta-tools against the accumulated catalog.
//   - ModeHybrid: pick a canonical alias per connector (see picker docs
//     on ModeHybrid), advertise its tools directly, AND register the
//     meta-tools so the rest of the catalog stays reachable.
//
// Calling Finalize twice is safe but pointless.
func (r *Router) Finalize() error {
	switch r.mode {
	case ModeSearch:
		return r.registerSearchTools()
	case ModeHybrid:
		r.registerHybridDirectTools()
		return r.registerSearchTools()
	}
	return nil
}

// registerHybridDirectTools advertises the canonical alias per connector
// directly on the gateway server. Selection rules, in order:
//
//  1. Explicit AlwaysOn pin matching "<connector>:<alias>".
//  2. First alias seen for that connector (catalog order is the
//     resolver's deterministic order — usually .mcp-profiles.toml order
//     or alphabetical fallback).
//
// Connectors that only have one alias surface that alias here even
// without a pin, so a single-profile setup behaves like ModeExposeAll
// for that connector with zero extra config.
func (r *Router) registerHybridDirectTools() {
	canonical := r.pickCanonicalAliases()
	for _, e := range r.catalog {
		key := strings.ToLower(e.Connector + ":" + e.Alias)
		if !canonical[key] {
			continue
		}
		r.s.AddTool(e.Tool, r.handlers[e.Name])
	}
}

// pickCanonicalAliases returns the set of "<connector>:<alias>" keys
// that should be eagerly advertised in ModeHybrid. Lower-cased for
// case-insensitive matching against AlwaysOn pins.
func (r *Router) pickCanonicalAliases() map[string]bool {
	out := map[string]bool{}
	// Bucket entries by connector while preserving insertion order.
	order := make([]string, 0, len(r.catalog))
	byConnector := map[string][]string{} // connector → []alias (insertion-ordered, dedup'd)
	seenAlias := map[string]bool{}
	for _, e := range r.catalog {
		ca := strings.ToLower(e.Connector + ":" + e.Alias)
		if seenAlias[ca] {
			continue
		}
		seenAlias[ca] = true
		c := strings.ToLower(e.Connector)
		if _, ok := byConnector[c]; !ok {
			order = append(order, c)
		}
		byConnector[c] = append(byConnector[c], e.Alias)
	}

	for _, c := range order {
		aliases := byConnector[c]

		// Explicit pin wins.
		var pinned string
		for _, a := range aliases {
			if r.alwaysOn[c+":"+strings.ToLower(a)] {
				pinned = a
				break
			}
		}
		if pinned != "" {
			out[c+":"+strings.ToLower(pinned)] = true
			continue
		}
		// Fall back to first-alias-seen for this connector.
		if len(aliases) > 0 {
			out[c+":"+strings.ToLower(aliases[0])] = true
		}
	}
	return out
}

// Catalog returns a defensive copy of the internal tool catalog. Used by
// search.go and tests; not intended as a stable public surface.
func (r *Router) Catalog() []CatalogEntry {
	out := make([]CatalogEntry, len(r.catalog))
	copy(out, r.catalog)
	return out
}

// NamespacedName is the public name for a proxied tool.
func NamespacedName(connector, alias, tool string) string {
	return connector + "_" + alias + "_" + tool
}

// buildDescriptionPrefix renders the profile context into a compact
// bracketed prefix, e.g.
//
//	[supabase/atlas project_id=lcshv...]
//	[supabase/atlas project_id=lcshv...] PROD — writes require confirmation:
func buildDescriptionPrefix(connector, alias string, pc ProfileContext) string {
	parts := []string{connector + "/" + alias}
	keys := make([]string, 0, len(pc.Metadata))
	for k := range pc.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+pc.Metadata[k])
	}
	prefix := "[" + strings.Join(parts, " ") + "]"
	if pc.Note != "" {
		prefix += " " + pc.Note + " —"
	}
	return prefix
}

// prependDescription glues the prefix onto the original upstream
// description. If the upstream shipped no description, just the prefix
// is enough — it tells the client at least which profile owns the tool.
func prependDescription(prefix, original string) string {
	original = strings.TrimSpace(original)
	if original == "" {
		return prefix
	}
	return prefix + " " + original
}

// makeHandler returns a ToolHandler that forwards CallTool to the upstream
// child, preserving arguments and remapping the tool name back to its
// upstream form.
//
// Two cross-cutting concerns are handled here so every dispatch path
// (direct tool, nucleus_call, nucleus_call_plan step) gets them for
// free:
//
//   - Policy gating. If the gateway has a policy attached and a rule
//     matches this profile/tool, the call is blocked or required to
//     carry a confirmation phrase before it reaches the upstream.
//   - Sticky resolution. On successful dispatch (no Go error, no
//     IsError result), record this profile's alias as the most-recent
//     for its connector so the ranker can bias toward it next time.
func (r *Router) makeHandlerForDispatcher(
	d Dispatcher,
	connector, profileID, upstreamTool, alias string,
) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started := time.Now()
		args := req.GetArguments()

		if r.policy != nil {
			if decision := r.policy.Check(connector, alias, upstreamTool, args); !decision.Allowed {
				// Map the policy's coarse "blocked" message to the
				// finer-grained audit Decision so logs can distinguish
				// "outright denied" from "needs confirmation" without
				// re-parsing the message text.
				dec := audit.DecisionDenied
				if strings.HasPrefix(decision.Message, "CONFIRMATION REQUIRED") {
					dec = audit.DecisionConfirmRequired
				} else if strings.HasPrefix(decision.Message, "CONFIRMATION MISMATCH") {
					dec = audit.DecisionConfirmMismatch
				}
				r.writeAudit(audit.Entry{
					Connector:  connector,
					Alias:      alias,
					ProfileID:  profileID,
					Tool:       upstreamTool,
					Via:        viaFromCtx(ctx),
					Decision:   dec,
					Outcome:    audit.OutcomeBlocked,
					DurationMS: time.Since(started).Milliseconds(),
					Reason:     decision.Message,
					Args:       args,
				})
				// Policy denials surface as IsError tool results rather
				// than Go errors so the LLM sees a structured message
				// it can act on (e.g. re-call with the confirmation
				// phrase) instead of a transport-level failure.
				return mcp.NewToolResultError(decision.Message), nil
			}
		}
		upstream := req
		upstream.Params.Name = upstreamTool

		// Goes through the Dispatcher interface — for the production
		// path that's Child.CallTool, which handles transparent
		// respawn if the supervisor's reaper closed this child
		// between calls. Tests can supply any stand-in dispatcher.
		res, err := d.CallTool(ctx, upstream)
		dur := time.Since(started).Milliseconds()
		entry := audit.Entry{
			Connector:  connector,
			Alias:      alias,
			ProfileID:  profileID,
			Tool:       upstreamTool,
			Via:        viaFromCtx(ctx),
			Decision:   audit.DecisionAllowed,
			DurationMS: dur,
			Args:       args,
		}
		if err != nil {
			entry.Outcome = audit.OutcomeTransportErr
			entry.Reason = err.Error()
			r.writeAudit(entry)
			return nil, fmt.Errorf("proxy %s/%s: %w", profileID, upstreamTool, err)
		}
		// Don't sticky on upstream-reported errors — sticky is a "you
		// just did real work here" signal, and a 400 from the upstream
		// is not real work.
		if res != nil && !res.IsError {
			entry.Outcome = audit.OutcomeOK
			r.markSticky(connector, alias)
		} else {
			entry.Outcome = audit.OutcomeUpstreamError
			if res != nil {
				for _, item := range res.Content {
					if tc, ok := item.(mcp.TextContent); ok {
						entry.Reason = tc.Text
						break
					}
				}
			}
		}
		r.writeAudit(entry)
		return res, nil
	}
}

// writeAudit drops an entry into the audit log, summarizing args
// per the writer's privacy posture. Failures here MUST NEVER leak to
// the dispatch path — a broken audit pipeline cannot be allowed to
// block tool calls. We log the error to slog (which goes to stderr,
// not the JSON-RPC stream) and move on.
func (r *Router) writeAudit(e audit.Entry) {
	if r.auditW == nil {
		return
	}
	keys, hash := audit.SummarizeArgs(e.Args)
	e.ArgsKeys = keys
	e.ArgsHash = hash
	if err := r.auditW.Write(e); err != nil {
		slog.Error("audit write failed", "err", err,
			"connector", e.Connector, "alias", e.Alias, "tool", e.Tool)
	}
}
