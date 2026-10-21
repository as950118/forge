// Package cli wires the cobra command tree for forge.
package cli

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/heonjinjeong/forge/db"
)

// BuildInfo is injected from main (set by ldflags).
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// NewRootCmd builds the forge root command with all subcommands.
func NewRootCmd(info BuildInfo) *cobra.Command {
	root := &cobra.Command{
		Use:           "forge",
		Short:         "Forge: manage skills, MCP servers, tools and usage history for AI coding CLIs",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       info.Version,
	}
	root.SetVersionTemplate(fmt.Sprintf("forge version %s (commit %s, built %s)\n", info.Version, info.Commit, info.Date))

	root.AddCommand(
		initCmd(),
		skillCmd(),
		mcpCmd(),
		toolCmd(),
		hookCmd(),
		stub("profile", "Manage profiles (create|use|list)", "TASK-0012"),
		stub("sync", "Sync registry to client configs [--target] [--dry-run] [--diff]", "TASK-0010"),
		historyCmd(),
		doctorCmd(),
	)
	return root
}

func stub(use, short, task string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "forge %s: not implemented yet (%s)\n", use, task)
			return nil
		},
	}
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check client config and drift (stub: prints self-diagnostics)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			fmt.Fprintln(w, "forge doctor: not implemented yet (TASK-0012)")
			fmt.Fprintf(w, "go:     %s\n", runtime.Version())
			fmt.Fprintf(w, "os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
			fmt.Fprintf(w, "schema.sql embedded: %t (%d bytes)\n",
				strings.Contains(db.SchemaSQL, "schema_migrations"), len(db.SchemaSQL))
			return nil
		},
	}
}
