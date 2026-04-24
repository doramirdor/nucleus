package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/doramirdor/nucleusmcp/internal/connectors"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := openRegistry()
			if err != nil {
				return fmt.Errorf("open registry: %w", err)
			}
			defer reg.Close()

			profiles, err := reg.List()
			if err != nil {
				return err
			}

			if len(profiles) == 0 {
				stderrf("No profiles configured.")
				stderrf("")
				stderrf("Add one with:")
				stderrf("  nucleus add <connector> <name>")
				stderrf("")
				stderrf("Available connectors:")
				for _, m := range connectors.All() {
					stderrf("  %-12s %s", m.Name, m.Description)
				}
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tDEFAULT\tAGE\tMETADATA")
			for _, p := range profiles {
				def := ""
				if p.IsDefault {
					def = "✓"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
					p.ID, def,
					humanAge(time.Since(p.CreatedAt)),
					renderMetadata(p.Metadata))
			}
			return tw.Flush()
		},
	}
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func renderMetadata(m map[string]string) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}
