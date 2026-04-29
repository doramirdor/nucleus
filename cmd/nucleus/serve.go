package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/doramirdor/nucleusmcp/internal/config"
	"github.com/doramirdor/nucleusmcp/internal/router"
	"github.com/doramirdor/nucleusmcp/internal/server"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	var (
		httpAddr    string
		httpToken   string
		modeString  string
		alwaysOn    []string
		idleTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the gateway as an MCP server (stdio by default, --http for daemon mode)",
		Long: `Run the gateway.

Without flags, serves the MCP protocol over stdio — this is the mode
invoked by Claude Code / Cursor via their mcp server config (what
` + "`nucleus install`" + ` wires up).

With --http, the gateway runs as a long-lived HTTP server on a local
port. The endpoint is /mcp. Use this mode to register Nucleus in
Claude's UI "Add custom connector" dialog, which only accepts HTTP(S)
URLs.

Safety defaults for --http: loopback-only (127.0.0.1) unless you supply
--token, which activates bearer-token auth and allows non-loopback
binds.

Tool advertisement modes (--mode):
  expose-all (default): every proxied tool is advertised to the client.
                        Best for small numbers of profiles.
  hybrid:               one canonical alias per connector is advertised
                        directly (the "recommended" path); the rest of
                        the catalog is reachable via the
                        nucleus_find_tool / nucleus_call meta-tools.
                        Best when each service has a clear primary
                        account plus occasional secondary use.
                        Use --always-on to override which alias is
                        canonical per connector.
  search:               only nucleus_find_tool and nucleus_call are
                        advertised; the full catalog is searched
                        lexically on demand. Best when many
                        profiles/connectors are loaded — minimum
                        context cost, every tool needs a discovery hop.

Examples:
  nucleus serve                                                 # stdio, expose-all
  nucleus serve --mode hybrid                                   # recommend + search
  nucleus serve --mode hybrid --always-on supabase:atlas,github:work
  nucleus serve --mode search                                   # lazy tool discovery
  nucleus serve --http 127.0.0.1:8787                           # daemonize on loopback
  nucleus serve --http 0.0.0.0:9000 --token s3cret              # LAN-reachable + auth`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Logs to stderr — stdout is reserved for MCP protocol frames
			// in stdio mode.
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
				&slog.HandlerOptions{Level: slog.LevelInfo})))

			path, err := resolveConfigPath()
			if err != nil {
				return err
			}
			if _, err := config.Load(path); err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			reg, err := openRegistry()
			if err != nil {
				return fmt.Errorf("open registry: %w", err)
			}
			defer reg.Close()

			ctx, cancel := signal.NotifyContext(
				context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			mode, err := parseRouterMode(modeString)
			if err != nil {
				return err
			}

			if len(alwaysOn) > 0 && mode != router.ModeHybrid {
				slog.Warn("ignoring --always-on; only meaningful in hybrid mode",
					"mode", modeString)
			}

			gw := server.New(reg, newVault(), version).
				WithRouterMode(mode).
				WithAlwaysOn(alwaysOn).
				WithIdleTimeout(idleTimeout)
			defer gw.Shutdown()

			slog.Info("nucleus starting", "version", version, "config", path)

			// Fail fast on misconfigured HTTP flags *before* Prepare
			// spawns upstream children (which can take several seconds
			// on first run per connector). A non-empty --http value
			// means HTTP mode.
			httpMode := httpAddr != ""
			httpOpts := server.HTTPOptions{Addr: httpAddr, Token: httpToken}
			if httpMode {
				if err := httpOpts.Validate(); err != nil {
					return err
				}
			}

			if err := gw.Prepare(ctx); err != nil {
				return err
			}

			if httpMode {
				return gw.ServeHTTP(ctx, httpOpts)
			}
			return gw.ServeStdio()
		},
	}

	cmd.Flags().StringVar(&httpAddr, "http", "",
		"serve over streamable HTTP on this bind address, e.g. '127.0.0.1:8787' or ':9000'. "+
			"Non-loopback binds require --token.")
	cmd.Flags().StringVar(&httpToken, "token", "",
		"require this bearer token on every HTTP request (required for non-loopback binds)")
	cmd.Flags().StringVar(&modeString, "mode", "expose-all",
		"tool advertisement mode: 'expose-all' (default; every tool visible), "+
			"'hybrid' (canonical alias per connector + meta-tools for the rest), "+
			"or 'search' (only nucleus_find_tool + nucleus_call visible, full "+
			"catalog searched lazily)")
	cmd.Flags().StringSliceVar(&alwaysOn, "always-on", nil,
		"hybrid mode only: comma-separated '<connector>:<alias>' list to pin as "+
			"the canonical advertised tools. Empty falls back to first-alias-per-connector.")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 0,
		"reap upstream children after this much idle time (e.g. 15m, 1h). "+
			"Reaped children are transparently respawned on the next call. "+
			"Default 0 disables reaping — children live for the gateway lifetime.")
	return cmd
}

// parseRouterMode maps the --mode flag string to a router.Mode. Unknown
// values are an error rather than a silent fallback so a typo doesn't
// quietly leave the user in expose-all when they meant search.
func parseRouterMode(s string) (router.Mode, error) {
	switch s {
	case "", "expose-all", "expose_all", "all":
		return router.ModeExposeAll, nil
	case "search":
		return router.ModeSearch, nil
	case "hybrid", "recommend", "recommendation":
		return router.ModeHybrid, nil
	default:
		return 0, fmt.Errorf("unknown --mode %q (want 'expose-all', 'hybrid', or 'search')", s)
	}
}

func resolveConfigPath() (string, error) {
	if configPath != "" {
		return configPath, nil
	}
	return config.DefaultPath()
}
