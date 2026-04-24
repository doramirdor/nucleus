package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/doramirdor/nucleusmcp/internal/config"
	"github.com/doramirdor/nucleusmcp/internal/server"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the gateway as an MCP server over stdio",
		Long: `Run the gateway. This is the mode Claude (and other MCP clients)
invoke via their mcp server config. stdin/stdout carry the MCP JSON-RPC
stream; logs go to stderr.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Logs to stderr — stdout is reserved for MCP protocol frames.
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

			gw := server.New(reg, newVault(), version)
			defer gw.Shutdown()

			slog.Info("nucleus starting", "version", version, "config", path)
			return gw.Start(ctx)
		},
	}
}

func resolveConfigPath() (string, error) {
	if configPath != "" {
		return configPath, nil
	}
	return config.DefaultPath()
}
