// Package server wires the gateway together: one MCP server facing the
// client (Claude, Cursor, ...), a supervisor of upstream MCP children, and
// a router that proxies tools between them.
//
// Two transports are available for the client-facing side:
//   - Stdio (default): the gateway is spawned per session by the MCP
//     client via `claude mcp add …` / Cursor's mcpServers config.
//   - HTTP (streamable): the gateway runs as a long-lived daemon the
//     client connects to over HTTP, so it can be registered in the
//     Claude UI's "Add custom connector" dialog.
//
// The MCP server is constructed inside Prepare (not New) so that the
// Instructions it returns at init time can include the live list of
// connectors and profiles this installation has just resolved.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/doramirdor/nucleusmcp/internal/audit"
	"github.com/doramirdor/nucleusmcp/internal/connectors"
	"github.com/doramirdor/nucleusmcp/internal/registry"
	"github.com/doramirdor/nucleusmcp/internal/router"
	"github.com/doramirdor/nucleusmcp/internal/supervisor"
	"github.com/doramirdor/nucleusmcp/internal/vault"
	"github.com/doramirdor/nucleusmcp/internal/workspace"
)

// serverName is the identity the gateway advertises over MCP
// (what Claude shows in `mcp list`). The CLI binary is named `nucleus`,
// so the server identity matches. Note that on-disk storage paths and
// the OS keychain service remain "nucleusmcp" for compatibility with
// pre-rename installs — see internal/registry + internal/vault.
const serverName = "nucleus"

// Gateway is the top-level orchestrator.
type Gateway struct {
	reg     *registry.Registry
	vlt     *vault.Vault
	version string

	// routerMode controls whether tools are advertised eagerly
	// (ModeExposeAll, default), lazily via meta-tools (ModeSearch), or
	// recommended canonical-per-connector + meta-tools (ModeHybrid).
	// Set with WithRouterMode before Prepare.
	routerMode router.Mode

	// alwaysOn is the explicit "<connector>:<alias>" pin list for
	// ModeHybrid. Empty falls back to the heuristic.
	alwaysOn []string

	// idleTimeout is how long a child must be unused before the
	// reaper closes it. Zero disables idle reaping (the historical
	// behavior — children live for the gateway lifetime).
	idleTimeout time.Duration

	// constructed in Prepare, after we know the resolutions
	server  *mcpserver.MCPServer
	sup     *supervisor.Supervisor
	router  *router.Router
	auditW  *audit.Writer
}

// New builds a Gateway. Call Prepare then ServeStdio / ServeHTTP.
func New(reg *registry.Registry, vlt *vault.Vault, version string) *Gateway {
	return &Gateway{reg: reg, vlt: vlt, version: version}
}

// WithRouterMode selects how the gateway advertises proxied tools to the
// MCP client. Default is router.ModeExposeAll. Must be called before
// Prepare; calling it after has no effect.
func (g *Gateway) WithRouterMode(m router.Mode) *Gateway {
	g.routerMode = m
	return g
}

// WithAlwaysOn pins specific "<connector>:<alias>" entries to the
// always-advertised set in ModeHybrid (ignored in other modes). Empty
// list falls back to the first-alias-per-connector heuristic. Must be
// called before Prepare.
func (g *Gateway) WithAlwaysOn(specs []string) *Gateway {
	g.alwaysOn = append(g.alwaysOn[:0:0], specs...)
	return g
}

// WithIdleTimeout sets how long a child can sit unused before the
// reaper closes it. Zero (the default) disables reaping — children
// live for the gateway lifetime. Reaped children are transparently
// re-spawned on the next call, at the cost of a 3-5s warm-up. Must
// be called before Prepare.
func (g *Gateway) WithIdleTimeout(d time.Duration) *Gateway {
	g.idleTimeout = d
	return g
}

// Prepare runs workspace resolution, spawns the chosen profiles' upstream
// MCPs, and builds the client-facing MCPServer (with Instructions
// reflecting the live resolutions). Call exactly once, before a Serve*
// method. On error, nothing is left running.
func (g *Gateway) Prepare(ctx context.Context) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}

	wsConfig, err := workspace.FindAndLoad(cwd)
	if err != nil {
		return fmt.Errorf("workspace config: %w", err)
	}
	if wsConfig.Path != "" {
		slog.Info("workspace config loaded",
			"path", wsConfig.Path,
			"connectors_bound", len(wsConfig.Bindings))
	}

	resolver := workspace.NewResolver(g.reg, wsConfig, cwd)
	resolutions, skips, err := resolver.Resolve()
	if err != nil {
		return fmt.Errorf("resolve profiles: %w", err)
	}

	for _, skip := range skips {
		slog.Warn("skipping connector",
			"connector", skip.Connector, "reason", skip.Reason)
	}

	g.server = mcpserver.NewMCPServer(
		serverName,
		g.version,
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithInstructions(buildInstructions(resolutions, g.routerMode)),
	)
	g.sup = supervisor.New(serverName, g.version)
	g.router = router.NewWithMode(g.server, g.routerMode)
	if len(g.alwaysOn) > 0 {
		g.router.SetAlwaysOn(g.alwaysOn)
	}

	// Policy is optional: a missing policy.toml is the common case
	// (allow everything, the historical default). When present, it
	// gates writes/destructives across every dispatch path.
	// Audit log: same posture as policy. A failure here logs and
	// proceeds without audit rather than blocking startup; an
	// install where the home dir is read-only should still get
	// usable tools.
	if w, err := audit.Open(audit.Options{}); err != nil {
		slog.Error("audit log open failed — proceeding without audit",
			"err", err)
	} else if w != nil {
		g.auditW = w
		g.router.SetAuditWriter(w)
		slog.Info("audit log enabled", "path", w.Path())
	}

	if pol, err := loadPolicy(); err != nil {
		// Don't refuse to start on a malformed policy — the user
		// would lose access to their tools entirely. Log loudly and
		// proceed with no policy (most permissive option) so the
		// install is at least usable while the file gets fixed.
		slog.Error("policy load failed — proceeding without policy enforcement",
			"err", err)
	} else if pol != nil {
		slog.Info("policy loaded", "rules", len(pol.Rules))
		g.router.SetPolicy(pol)
	}

	if len(resolutions) == 0 {
		slog.Warn("no profiles resolved — gateway will expose zero tools",
			"hint", "run `nucleus add <connector>` or add .mcp-profiles.toml")
	}

	// Dedupe spawn by profile ID — the same profile bound under two
	// aliases should run one child, not two.
	spawned := make(map[string]*supervisor.Child)

	for _, res := range resolutions {
		m, ok := connectors.Get(res.Connector)
		if !ok {
			slog.Warn("unknown connector (no manifest)",
				"connector", res.Connector)
			continue
		}
		slog.Info("resolved profile",
			"connector", res.Connector,
			"profile", res.Profile.Name,
			"alias", res.Alias,
			"source", res.Source,
			"hint", res.Hint)

		child, ok := spawned[res.Profile.ID]
		if !ok {
			child, err = g.sup.SpawnProfile(ctx, m, res.Profile, g.vlt)
			if err != nil {
				slog.Error("spawn failed — skipping binding",
					"profile", res.Profile.ID, "alias", res.Alias, "err", err)
				continue
			}
			spawned[res.Profile.ID] = child
			slog.Info("spawned child",
				"profile", res.Profile.ID, "tools", len(child.Tools))
		}

		pc := router.ProfileContext{
			Metadata: res.Profile.Metadata,
			Note:     res.Note,
		}
		if err := g.router.RegisterChild(child, res.Alias, pc); err != nil {
			slog.Error("register failed",
				"profile", res.Profile.ID, "alias", res.Alias, "err", err)
			continue
		}
		slog.Info("alias ready",
			"profile", res.Profile.ID, "alias", res.Alias, "tools", len(child.Tools))
	}

	if err := g.router.Finalize(); err != nil {
		return fmt.Errorf("finalize router: %w", err)
	}

	// Start the idle reaper if configured. The reaper goroutine runs
	// for the lifetime of the supervisor; it's stopped automatically
	// during Shutdown.
	if g.idleTimeout > 0 {
		g.sup.StartReaper(g.idleTimeout, 0) // 0 → default tick (30s)
		slog.Info("idle reaper enabled",
			"idle_timeout", g.idleTimeout.String())
	}

	slog.Info("gateway prepared",
		"active_profiles", len(spawned),
		"active_aliases", len(resolutions),
		"router_mode", routerModeName(g.routerMode),
		"client_visible_tools", clientVisibleToolCount(g.routerMode, g.router),
		"cwd", cwd)
	return nil
}

// routerModeName returns a stable, log-friendly string for a Mode.
func routerModeName(m router.Mode) string {
	switch m {
	case router.ModeSearch:
		return "search"
	case router.ModeHybrid:
		return "hybrid"
	case router.ModeExposeAll:
		return "expose-all"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// clientVisibleToolCount reports how many tools the MCP client will see
// at connect time — useful for confirming the chosen mode is actually
// shrinking the surface as advertised. Caveat: in hybrid mode this
// over-counts slightly because the canonical-alias heuristic isn't
// re-run here, but it's an upper bound (it counts all distinct
// connectors), which is fine for a startup log line.
func clientVisibleToolCount(m router.Mode, r *router.Router) int {
	switch m {
	case router.ModeSearch:
		// nucleus_find_tool + nucleus_call + nucleus_call_plan.
		return 3
	case router.ModeHybrid:
		// Approx: one canonical alias × (avg tools/alias) + 2 meta-tools.
		// Compute exactly by counting distinct connectors and summing
		// the first-alias-per-connector tool count.
		seen := map[string]string{}
		for _, e := range r.Catalog() {
			if _, ok := seen[e.Connector]; !ok {
				seen[e.Connector] = e.Alias
			}
		}
		var n int
		for _, e := range r.Catalog() {
			if seen[e.Connector] == e.Alias {
				n++
			}
		}
		// +3 for find_tool, call, call_plan.
		return n + 3
	}
	return len(r.Catalog())
}

// loadPolicy reads the user's policy.toml from the standard location.
// The "no file present" case is not an error — installs without a
// policy.toml are the common case and "no policy = allow everything."
//
// Resolution order (first match wins):
//
//  1. NUCLEUSMCP_POLICY env var (absolute path) — escape hatch for
//     CI/test environments and admin overrides.
//  2. ~/.nucleusmcp/policy.toml — the standard location.
//
// Workspace-level policy (.mcp-profiles.toml [policy] section) is NOT
// loaded here; that's intentional follow-on work — see roadmap.
func loadPolicy() (*router.Policy, error) {
	if env := strings.TrimSpace(os.Getenv("NUCLEUSMCP_POLICY")); env != "" {
		return router.LoadPolicy(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home dir → can't locate the standard policy file. Treat
		// as "no policy" rather than failing — same shape as missing
		// file.
		return nil, nil
	}
	return router.LoadPolicy(filepath.Join(home, ".nucleusmcp", "policy.toml"))
}

// ServeStdio runs the prepared gateway on stdio. Blocks until the client
// closes stdin or ctx is canceled.
func (g *Gateway) ServeStdio() error {
	if g.server == nil {
		return errors.New("gateway not prepared; call Prepare first")
	}
	slog.Info("gateway listening on stdio")
	return mcpserver.ServeStdio(g.server)
}

// HTTPOptions configures ServeHTTP.
type HTTPOptions struct {
	// Addr is the bind address, e.g. "127.0.0.1:8787" or ":8787".
	// Empty defaults to 127.0.0.1:8787 for safety (loopback only).
	Addr string

	// Token, if non-empty, is required as the bearer token in the
	// Authorization header on every request. Empty disables auth — safe
	// only on a loopback bind.
	Token string
}

// Validate enforces safety invariants for HTTPOptions. Call this before
// Prepare so a misconfigured bind doesn't waste the 3–5 seconds spent
// spawning upstream MCP children.
func (o HTTPOptions) Validate() error {
	addr := o.Addr
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	if !isLoopbackBind(addr) && o.Token == "" {
		return fmt.Errorf(
			"refusing to serve on %s without --token: non-loopback bind "+
				"exposes all profile tools to the network. "+
				"Bind to 127.0.0.1 or supply --token.",
			addr)
	}
	return nil
}

// ServeHTTP runs the prepared gateway as a streamable-HTTP MCP server.
// The endpoint path is /mcp. Blocks until ctx is canceled or the server
// errors.
//
// Safety defaults: if Addr is empty we bind loopback-only. If the caller
// binds to a non-loopback address and hasn't set a Token, we refuse to
// start.
func (g *Gateway) ServeHTTP(ctx context.Context, opts HTTPOptions) error {
	if g.server == nil {
		return errors.New("gateway not prepared; call Prepare first")
	}
	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	if !isLoopbackBind(addr) && opts.Token == "" {
		return fmt.Errorf(
			"refusing to serve on %s without --token: non-loopback bind exposes all profile tools to the network; "+
				"bind to 127.0.0.1 or supply --token",
			addr)
	}

	// Token auth: wrap the streamable-HTTP handler in a middleware that
	// checks Authorization: Bearer <token>. Loopback without a token is
	// allowed (matches the default for most dev tooling — e.g. Jupyter).
	baseHandler := mcpserver.NewStreamableHTTPServer(g.server)
	var handler http.Handler = baseHandler
	if opts.Token != "" {
		handler = bearerAuth(opts.Token, baseHandler)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("gateway listening on http",
		"addr", addr, "endpoint", "/mcp", "auth", opts.Token != "")
	slog.Info("claude UI add",
		"url", fmt.Sprintf("http://%s/mcp", advertiseAddr(addr)))

	// Graceful shutdown on ctx cancel.
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// Shutdown terminates upstream children and flushes the audit log.
// Safe to defer — both calls are nil-safe and idempotent.
func (g *Gateway) Shutdown() {
	if g.sup != nil {
		g.sup.Shutdown()
	}
	if g.auditW != nil {
		// Close flushes any buffered writes and releases the file
		// handle. If close fails (rare; usually only on a full disk
		// at fsync time), there's nowhere helpful to surface it
		// since we're already on the way out.
		_ = g.auditW.Close()
	}
}

// bearerAuth wraps next with a simple Authorization: Bearer <token> check.
// Constant-time comparison to avoid timing-side-channel token extraction.
func bearerAuth(token string, next http.Handler) http.Handler {
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if !constantTimeEqual(got, want) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nucleus"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// constantTimeEqual is a length-safe byte comparison that doesn't
// short-circuit on the first mismatching byte.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		// Still walk b to keep timing mostly flat.
		var x byte
		for i := 0; i < len(b); i++ {
			x |= b[i]
		}
		_ = x
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// isLoopbackBind returns true iff addr's host is empty, "127.0.0.1", or
// "localhost". Anything else is treated as "could be reachable off-host"
// and requires a token.
func isLoopbackBind(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	switch host {
	case "", "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// advertiseAddr turns ":8787" into "localhost:8787" for log output.
func advertiseAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

// buildInstructions returns the Instructions string the gateway
// advertises at MCP init. It's deliberately connector-agnostic — the
// live connector list is injected dynamically so Claude sees the real
// shape of *this* installation, not a hardcoded assumption.
//
// The instructions also vary by router mode: in expose-all mode tools
// are visible by name, so we tell Claude how to read the namespacing
// scheme; in search mode tools are gated behind nucleus_find_tool, so we
// tell Claude how to use the meta-tool flow.
//
// Claude reads these once at connect time, which is why listing the
// current connectors and aliases here is higher-impact than a tool that
// has to be called to be useful.
func buildInstructions(resolutions []workspace.Resolution, mode router.Mode) string {
	var b strings.Builder
	b.WriteString(
		"Nucleus is a profile-aware gateway that holds multiple " +
			"authenticated sessions (called \"profiles\") for one or more " +
			"upstream services and exposes them all simultaneously.\n\n")

	if mode == router.ModeSearch {
		b.WriteString(
			"This installation is running in SEARCH MODE: instead of " +
				"advertising every proxied tool, the gateway exposes two " +
				"meta-tools — `nucleus_find_tool(intent, connector?, " +
				"limit?)` and `nucleus_call(name, arguments)`. To do " +
				"anything against an upstream service, first call " +
				"`nucleus_find_tool` with a natural-language description " +
				"of what you want to do; it returns ranked candidate " +
				"tools with their full JSON schemas. Then call " +
				"`nucleus_call` with the chosen tool's name and " +
				"arguments. The candidate names follow the convention " +
				"`<connector>_<profile-alias>_<tool>`, and their " +
				"descriptions begin with a bracketed prefix identifying " +
				"the profile (e.g. `[supabase/atlas project_id=…]`).\n")
	} else if mode == router.ModeHybrid {
		b.WriteString(
			"This installation is running in HYBRID MODE: one canonical " +
				"alias per connector is advertised directly (the " +
				"\"recommended\" path — call those tools by name as " +
				"usual). For non-canonical profiles (e.g. a second " +
				"GitHub account, a staging Supabase), use the meta-tools " +
				"`nucleus_find_tool(intent, connector?, limit?)` to " +
				"discover the right namespaced tool, then " +
				"`nucleus_call(name, arguments)` to invoke it. " +
				"Tool names follow the convention " +
				"`<connector>_<profile-alias>_<tool>` and their " +
				"descriptions begin with a bracketed prefix identifying " +
				"the profile (e.g. `[supabase/atlas project_id=…]`). " +
				"When the user mentions an alias by name " +
				"(\"prod\", \"staging\", \"work\") that's not in the " +
				"directly-advertised set, reach for `nucleus_find_tool` " +
				"with `connector` set rather than guessing the tool name.\n")
	} else {
		b.WriteString(
			"Every proxied tool is named `<connector>_<profile-alias>_<tool>`. " +
				"Its description starts with a bracketed prefix identifying the " +
				"profile, e.g.\n\n" +
				"  supabase_atlas_execute_sql — \"[supabase/atlas project_id=…] " +
				"Execute a SQL query against the project\"\n")
	}

	if len(resolutions) == 0 {
		b.WriteString("\nNo profiles are currently resolved for this workspace. " +
			"The gateway is running empty; the user can add one with " +
			"`nucleus add <connector>`.\n")
	} else {
		b.WriteString("\nActive connectors on this installation " +
			"(computed at gateway startup):\n")
		for _, line := range summarizeResolutions(resolutions) {
			b.WriteString("  - " + line + "\n")
		}
	}

	if mode == router.ModeSearch || mode == router.ModeHybrid {
		b.WriteString(
			"\nWhen the user asks about authenticated accounts, projects, " +
				"environments, or connections, answer from this server: " +
				"the connector list above already names every loaded " +
				"profile. For tool-level questions about non-canonical " +
				"profiles, call `nucleus_find_tool` rather than guessing " +
				"tool names.\n" +
				"\nWhen a tool's description prefix (either visible " +
				"directly or returned by `nucleus_find_tool`) includes a " +
				"warning like \"PRODUCTION\" or \"read-only\", surface the " +
				"warning to the user and confirm before invoking.")
	} else {
		b.WriteString(
			"\nWhen the user asks about authenticated accounts, projects, " +
				"environments, or connections for any of the listed connectors " +
				"(e.g. \"what <service> projects do I have access to?\", \"list " +
				"my <service> accounts\"), answer from this server: enumerate " +
				"tools whose name begins with the connector name, group them by " +
				"the profile-alias segment, and read the bracketed prefix for " +
				"each profile's metadata. Do NOT redirect the user to a " +
				"different MCP server that happens to share a connector's bare " +
				"name — the definitive view of their multi-account setup lives " +
				"here.\n" +
				"\nWhen the user asks to perform a write or destructive action " +
				"(migrations, deletes, truncates) on a profile whose bracketed " +
				"prefix includes a warning like \"PRODUCTION\" or \"read-only\", " +
				"surface the warning and confirm before proceeding.")
	}
	return b.String()
}

// summarizeResolutions groups the resolutions by connector and returns
// one string per connector in the form
//
//	supabase: 2 profile(s) — atlas, default
func summarizeResolutions(resolutions []workspace.Resolution) []string {
	type agg struct {
		aliases []string
		count   int
	}
	by := map[string]*agg{}
	for _, r := range resolutions {
		a, ok := by[r.Connector]
		if !ok {
			a = &agg{}
			by[r.Connector] = a
		}
		a.aliases = append(a.aliases, r.Alias)
		a.count++
	}
	names := make([]string, 0, len(by))
	for k := range by {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, n := range names {
		a := by[n]
		out = append(out, fmt.Sprintf("%s: %d profile(s) — %s",
			n, a.count, strings.Join(a.aliases, ", ")))
	}
	return out
}
