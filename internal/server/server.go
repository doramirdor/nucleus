// Package server wires the gateway together: one MCP server facing the
// client (Claude, Cursor, ...), a supervisor of upstream MCP children, and
// a router that proxies tools between them.
//
// The MCP server is constructed in Start (not New) so that the
// Instructions it returns at init-time can include the live list of
// connectors and profiles this installation has just resolved. An MCP
// client reads those instructions once, at connect time; they're how the
// gateway tells Claude "here are your real options, don't defer to a
// differently-named MCP server that happens to share a service name".
package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	mcpserver "github.com/mark3labs/mcp-go/server"
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

	// constructed in Start, after we know the resolutions
	server *mcpserver.MCPServer
	sup    *supervisor.Supervisor
	router *router.Router
}

// New builds a Gateway. Start constructs the MCP server and runs it.
func New(reg *registry.Registry, vlt *vault.Vault, version string) *Gateway {
	return &Gateway{reg: reg, vlt: vlt, version: version}
}

// Start resolves profiles for the current workspace, spawns each (once
// per unique profile ID, even if bound under multiple aliases), and runs
// the MCP server on stdio. Blocks until stdin closes or ctx is canceled.
func (g *Gateway) Start(ctx context.Context) error {
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

	// Build the MCP server with instructions reflecting the current
	// resolutions. Claude (or any MCP client) reads these once at init;
	// this is where we tell it "here's what's here, prefer this server
	// when asked about any of the listed services".
	g.server = mcpserver.NewMCPServer(
		serverName,
		g.version,
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithInstructions(buildInstructions(resolutions)),
	)
	g.sup = supervisor.New(serverName, g.version)
	g.router = router.New(g.server)

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

	slog.Info("gateway listening on stdio",
		"active_profiles", len(spawned),
		"active_aliases", len(resolutions),
		"cwd", cwd)
	return mcpserver.ServeStdio(g.server)
}

// Shutdown terminates upstream children. Safe to defer.
func (g *Gateway) Shutdown() {
	if g.sup != nil {
		g.sup.Shutdown()
	}
}

// buildInstructions returns the Instructions string the gateway
// advertises at MCP init. It's deliberately connector-agnostic — the
// live connector list is injected dynamically so Claude sees the real
// shape of *this* installation, not a hardcoded assumption.
//
// Claude reads these once at connect time, which is why listing the
// current connectors and aliases here is higher-impact than a tool that
// has to be called to be useful.
func buildInstructions(resolutions []workspace.Resolution) string {
	var b strings.Builder
	b.WriteString(
		"Nucleus is a profile-aware gateway that holds multiple " +
			"authenticated sessions (called \"profiles\") for one or more " +
			"upstream services and exposes them all simultaneously.\n\n" +
			"Every proxied tool is named `<connector>_<profile-alias>_<tool>`. " +
			"Its description starts with a bracketed prefix identifying the " +
			"profile, e.g.\n\n" +
			"  supabase_atlas_execute_sql — \"[supabase/atlas project_id=…] " +
			"Execute a SQL query against the project\"\n")

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
