// Command forge is the Forge CLI entrypoint.
package main

import (
	"fmt"
	"os"

	"github.com/heonjinjeong/forge/internal/cli"
)

// Injected at build time via -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = "dev"
	date    = "dev"
)

func main() {
	root := cli.NewRootCmd(cli.BuildInfo{Version: version, Commit: commit, Date: date})
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
