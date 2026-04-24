package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/doramirdor/nucleusmcp/internal/connectors"
	"github.com/doramirdor/nucleusmcp/internal/registry"
	"github.com/doramirdor/nucleusmcp/internal/supervisor"
	"github.com/doramirdor/nucleusmcp/pkg/manifest"
	"github.com/spf13/cobra"
)

func newInfoCmd() *cobra.Command {
	var (
		noProbe bool
		all     bool
	)
	cmd := &cobra.Command{
		Use:     "info [profile-id]",
		Aliases: []string{"inspect", "status"},
		Short:   "Show profile config + a live health check of the upstream MCP",
		Long: `Print profile configuration and (by default) probe the upstream MCP
with this profile's credentials to confirm auth is healthy and report
the resources visible to it.

  nucleus info                  # one section per profile
  nucleus info supabase:prod    # just that one
  nucleus info --no-probe       # static config only, no upstream calls

The probe spawns the upstream child briefly and tears it down; npm-based
children may be slow on first run while npx fetches the package.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()

			var targets []registry.Profile
			if len(args) == 1 {
				p, err := reg.Get(args[0])
				if err != nil {
					if errors.Is(err, registry.ErrNotFound) {
						return fmt.Errorf("no profile with id %q", args[0])
					}
					return err
				}
				targets = []registry.Profile{p}
			} else {
				targets, err = reg.List()
				if err != nil {
					return err
				}
				if len(targets) == 0 {
					stderrf("No profiles configured. Add one with `nucleus add <connector>`.")
					return nil
				}
			}

			_ = all // reserved for future flags

			for i, p := range targets {
				if i > 0 {
					fmt.Fprintln(os.Stdout)
				}
				printProfileSection(p, noProbe)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noProbe, "no-probe", false,
		"skip the live upstream connection; only print stored config")
	return cmd
}

// printProfileSection renders one profile's static config and (unless
// noProbe) a live probe summary.
func printProfileSection(p registry.Profile, noProbe bool) {
	m, ok := connectors.Get(p.Connector)
	bullet := "═══"
	fmt.Fprintf(os.Stdout, "%s %s\n", bullet, p.ID)

	if !ok {
		fmt.Fprintf(os.Stdout, "  connector  : %s (UNKNOWN — manifest missing)\n", p.Connector)
		return
	}

	transport := string(m.Transport)
	if transport == "" {
		transport = "stdio"
	}
	kind := "builtin"
	if connectors.IsCustom(m.Name) {
		kind = "custom"
	}

	fmt.Fprintf(os.Stdout, "  connector  : %s (%s)\n", m.Name, kind)
	fmt.Fprintf(os.Stdout, "  description: %s\n", m.Description)
	fmt.Fprintf(os.Stdout, "  transport  : %s\n", transport)
	if m.URL != "" {
		fmt.Fprintf(os.Stdout, "  url        : %s\n", m.URL)
	}
	fmt.Fprintf(os.Stdout, "  auth       : %s\n", m.Auth)
	fmt.Fprintf(os.Stdout, "  created    : %s ago\n", humanAge(time.Since(p.CreatedAt)))
	if p.IsDefault {
		fmt.Fprintln(os.Stdout, "  default    : ✓ (used when no other rule matches)")
	}
	if len(p.Metadata) > 0 {
		keys := make([]string, 0, len(p.Metadata))
		for k := range p.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(os.Stdout, "  metadata   :")
		for _, k := range keys {
			fmt.Fprintf(os.Stdout, "    %s = %s\n", k, p.Metadata[k])
		}
	}

	// Show where credentials live (without printing them).
	switch m.Auth {
	case manifest.AuthOAuth:
		dir, _ := newVault().AuthDir(p.ID)
		fmt.Fprintf(os.Stdout, "  auth dir   : %s\n", dir)
	case manifest.AuthPAT:
		fmt.Fprintf(os.Stdout, "  auth dir   : OS keychain (service=nucleusmcp)\n")
	}

	if noProbe {
		return
	}

	probe, err := probeProfile(p, m)
	if err != nil {
		fmt.Fprintf(os.Stdout, "  probe      : ✗ %v\n", err)
		return
	}
	fmt.Fprintf(os.Stdout, "  probe      : ✓ %d tools advertised\n", probe.toolCount)
	if len(probe.resources) > 0 {
		fmt.Fprintf(os.Stdout, "  resources  : %d visible\n", len(probe.resources))
		for i, line := range probe.resources {
			fmt.Fprintf(os.Stdout, "    %d. %s\n", i+1, line)
		}
	} else if probe.discovererRan {
		fmt.Fprintln(os.Stdout, "  resources  : (none returned)")
	}
}

// probeResult is what the live upstream check found.
type probeResult struct {
	toolCount     int
	discovererRan bool
	resources     []string // pre-formatted "label   (summary)" lines
}

// probeTimeout bounds the live upstream connection. npm-pull on first
// spawn of an HTTP connector can be slow; 60s is generous.
const probeTimeout = 60 * time.Second

// probeProfile spawns the upstream briefly, lists tools, optionally runs
// the connector's discoverer, then tears down.
func probeProfile(p registry.Profile, m manifest.Manifest) (probeResult, error) {
	sup := supervisor.New("nucleus-info", version)
	defer sup.Shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	child, err := sup.SpawnProfile(ctx, m, p, newVault())
	if err != nil {
		return probeResult{}, err
	}

	res := probeResult{toolCount: len(child.Tools)}

	if d, ok := connectors.Discoverer(p.Connector); ok {
		res.discovererRan = true
		opts, err := d(ctx, child.Client)
		if err != nil {
			return res, fmt.Errorf("discovery: %w", err)
		}
		for _, o := range opts {
			line := o.Label
			if o.Summary != "" {
				line += "   (" + o.Summary + ")"
			}
			res.resources = append(res.resources, line)
		}
	}
	return res, nil
}

