package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/doramirdor/nucleusmcp/internal/connectors"
	"github.com/spf13/cobra"
)

func newConnectorsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connectors",
		Short: "List connectors known to this gateway",
		Long: `List all connectors — both built-in (shipped with the binary) and
custom ones you've added via 'nucleus add <name> <URL>'. Custom
manifests live in ~/.nucleusmcp/connectors/.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tTRANSPORT\tAUTH\tDESCRIPTION")
			for _, m := range connectors.All() {
				kind := "builtin"
				if connectors.IsCustom(m.Name) {
					kind = "custom"
				}
				transport := string(m.Transport)
				if transport == "" {
					transport = "stdio"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					m.Name, kind, transport, m.Auth, m.Description)
			}
			return tw.Flush()
		},
	}
}
