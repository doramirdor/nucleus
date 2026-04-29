// Command nucleus runs the Nucleus gateway and its CLI.
//
// The gateway is a profile-aware MCP server that Claude/Cursor/etc. connect
// to. It spawns upstream MCP servers (Supabase, Gmail, ...) on demand,
// injects per-profile credentials pulled from the OS keychain, and proxies
// tool calls back to the client.
package main

import (
	"fmt"
	"os"

	"github.com/doramirdor/nucleusmcp/internal/connectors"
	"github.com/spf13/cobra"
)

// version is overridden at build time via -ldflags "-X main.version=..."
var version = "dev"

// configPath is an optional --config override. Empty means "use default".
var configPath string

func main() {
	root := &cobra.Command{
		Use:     "nucleus",
		Short:   "Profile-aware MCP gateway — one connector, many accounts",
		Version: version,
		// We manage our own error printing; silence cobra's auto-usage
		// when a RunE returns an error.
		SilenceUsage: true,
		// Every subcommand needs custom connectors loaded before it
		// queries the connector registry. Runs once per process.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := connectors.LoadCustom(); err != nil {
				return fmt.Errorf("load custom connectors: %w", err)
			}
			return nil
		},
	}
	root.PersistentFlags().StringVar(&configPath, "config", "",
		"path to config.toml (default: ~/.nucleusmcp/config.toml)")

	root.AddCommand(newServeCmd())
	root.AddCommand(newAddCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newInfoCmd())
	root.AddCommand(newRemoveCmd())
	root.AddCommand(newUseCmd())
	root.AddCommand(newConnectorsCmd())
	root.AddCommand(newInstallCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newDoctorCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
