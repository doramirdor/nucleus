package main

import (
	"errors"
	"fmt"

	"github.com/doramirdor/nucleusmcp/internal/connectors"
	"github.com/doramirdor/nucleusmcp/internal/registry"
	"github.com/doramirdor/nucleusmcp/pkg/manifest"
	"github.com/spf13/cobra"
)

func newRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "remove <profile-id>",
		Aliases: []string{"rm", "delete"},
		Short:   "Remove a profile and its credentials",
		Long: `Remove a profile. The profile ID is "<connector>:<name>" — see the ID
column in 'nucleus list'.

For PAT connectors the personal access token is deleted from the OS
keychain. For OAuth connectors the per-profile OAuth directory under
~/.nucleusmcp/oauth/ is removed so future re-adds start with a fresh
authorization flow.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()

			p, err := reg.Get(id)
			if err != nil {
				if errors.Is(err, registry.ErrNotFound) {
					return fmt.Errorf("no profile with id %q", id)
				}
				return err
			}

			if !force {
				stderrf(
					"This will remove profile %s and delete its credentials.",
					p.ID)
				stderrf("Re-run with --force to confirm.")
				return nil
			}

			v := newVault()

			// Clean up auth state appropriate to the connector's auth mode.
			// If the connector manifest was removed between add and remove
			// we skip cleanup rather than fail — the profile row is still
			// removable.
			if m, ok := connectors.Get(p.Connector); ok {
				switch m.Auth {
				case manifest.AuthPAT:
					credKeys := uniqueCredKeys(m.Spawn.EnvFromCreds)
					if err := v.DeleteProfile(p.ID, credKeys); err != nil {
						stderrf("warning: failed to delete some keychain entries: %v", err)
					}
				case manifest.AuthOAuth:
					if err := v.DeleteAuthDir(p.ID); err != nil {
						stderrf("warning: failed to delete auth dir: %v", err)
					}
				}
			}

			if err := reg.Delete(p.ID); err != nil {
				return fmt.Errorf("delete profile: %w", err)
			}

			// If this was the last profile of a custom (non-builtin)
			// connector, remove the manifest file too. Built-in manifests
			// are never deleted.
			if connectors.IsCustom(p.Connector) {
				remaining, err := reg.ListByConnector(p.Connector)
				if err == nil && len(remaining) == 0 {
					if err := connectors.DeleteCustom(p.Connector); err != nil {
						stderrf("warning: failed to remove custom manifest: %v", err)
					} else {
						stderrf("  (custom connector %q had no more profiles — manifest removed)", p.Connector)
					}
				}
			}

			stderrf("✓ Removed %s", p.ID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "confirm deletion")
	return cmd
}

func uniqueCredKeys(envFromCreds map[string]string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(envFromCreds))
	for _, ck := range envFromCreds {
		if _, ok := seen[ck]; ok {
			continue
		}
		seen[ck] = struct{}{}
		out = append(out, ck)
	}
	return out
}
