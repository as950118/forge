// Package cli wires the cobra command tree for forge.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
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
	root.SetFlagErrorFunc(localizeFlagError)
	root.SetVersionTemplate(fmt.Sprintf("forge version %s (commit %s, built %s)\n", info.Version, info.Commit, info.Date))

	root.AddCommand(
		initCmd(),
		skillCmd(),
		mcpCmd(),
		toolCmd(),
		hookCmd(),
		profileCmd(),
		syncCmd(),
		historyCmd(),
		doctorCmd(),
	)
	return root
}
