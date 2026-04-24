package main

import (
	"errors"
	"fmt"

	"github.com/doramirdor/nucleusmcp/internal/registry"
	"github.com/spf13/cobra"
)

func newUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <profile-id>",
		Short: "Set a profile as the default for its connector",
		Long: `Mark a profile as the default for its connector. When no
.mcp-profiles.toml binding and no manifest autodetect rule matches, the
gateway will fall back to the default.

Profile IDs are of the form "<connector>:<name>" — see 'nucleus list'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()

			if _, err := reg.Get(id); err != nil {
				if errors.Is(err, registry.ErrNotFound) {
					return fmt.Errorf("no profile with id %q", id)
				}
				return err
			}

			if err := reg.SetDefault(id); err != nil {
				return fmt.Errorf("set default: %w", err)
			}
			stderrf("✓ %s is now the default for its connector", id)
			return nil
		},
	}
}
