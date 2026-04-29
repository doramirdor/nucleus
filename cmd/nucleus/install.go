package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newInstallCmd() *cobra.Command {
	var (
		printOnly bool
		scope     string
	)
	cmd := &cobra.Command{
		Use:   "install [client]",
		Short: "Register the gateway with an MCP client (default: claude)",
		Long: `Register the nucleus gateway with an MCP client so it starts automatically.

Supported clients:
  claude   — Claude Code / Claude Desktop

If the 'claude' CLI is on your PATH, runs 'claude mcp add nucleus ...'
for you. Otherwise prints a JSON snippet you can paste into Claude's
config file manually.

Examples:
  nucleus install                    # registers with Claude
  nucleus install claude --scope user
  nucleus install --print            # print config, don't modify anything`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := "claude"
			if len(args) >= 1 {
				client = args[0]
			}
			switch client {
			case "claude", "claude-code", "claude-desktop":
				return installClaude(printOnly, scope)
			default:
				return fmt.Errorf("unknown client %q (supported: claude)", client)
			}
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false,
		"print the config snippet instead of running the client's CLI")
	cmd.Flags().StringVar(&scope, "scope", "",
		"scope to pass to `claude mcp add` (user/project/local)")
	return cmd
}

// mcpEntryName is the name we use when registering with an MCP client.
// Matches the CLI binary so `claude mcp list` shows a predictable label.
const mcpEntryName = "nucleus"

// installClaude wires nucleus into Claude Code. Preferred path is the
// `claude` CLI — it knows where its config lives and handles merging.
// Fallback: print a JSON snippet the user can drop into the right file.
func installClaude(printOnly bool, scope string) error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	absBin, err := filepath.Abs(selfPath)
	if err != nil {
		return err
	}
	// If invoked via a symlink, resolve it. Claude's config should point
	// at a stable path, not at, say, /var/folders/... tempdir.
	if resolved, err := filepath.EvalSymlinks(absBin); err == nil {
		absBin = resolved
	}

	if printOnly {
		return printClaudeConfig(absBin)
	}

	claudeCli, err := exec.LookPath("claude")
	if err != nil {
		stderrf("note: 'claude' CLI not found on PATH.")
		stderrf("      Falling back to printing a config snippet for manual install.")
		stderrf("")
		return printClaudeConfig(absBin)
	}

	// claude mcp add [--scope X] <name> <command> [args...]
	args := []string{"mcp", "add"}
	if scope != "" {
		args = append(args, "--scope", scope)
	}
	args = append(args, mcpEntryName, absBin, "serve")

	runCmd := exec.Command(claudeCli, args...)
	runCmd.Stdout = os.Stdout
	runCmd.Stderr = os.Stderr
	if err := runCmd.Run(); err != nil {
		return fmt.Errorf("claude mcp add failed: %w", err)
	}

	stderrf("")
	stderrf("Registered %s with Claude.", mcpEntryName)
	stderrf("Restart your Claude Code session to pick up the new tools.")
	stderrf("")
	stderrf("Next steps:")
	stderrf("  nucleus add <connector>      # register your first profile (try 'nucleus connectors')")
	stderrf("  nucleus doctor               # confirm everything is wired up")
	stderrf("  nucleus logs                 # tail the per-call audit trail (after first use)")
	return nil
}

// printClaudeConfig emits the MCP-server stanza for Claude's config file.
// Written to stdout (human-readable); helper messages go to stderr.
func printClaudeConfig(bin string) error {
	entry := map[string]any{
		"mcpServers": map[string]any{
			mcpEntryName: map[string]any{
				"command": bin,
				"args":    []string{"serve"},
			},
		},
	}
	out, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	stderrf("Add this to Claude's config (e.g. ~/.claude.json, or your project's .mcp.json):")
	stderrf("")
	fmt.Fprintln(os.Stdout, string(out))
	stderrf("")
	stderrf("Then restart Claude Code.")
	return nil
}
