package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/as950118/forge/internal/ledger"
)

// ingestHook is replaced by tests to inject a panic and prove the recover path.
var ingestHook = ledger.IngestHook

// hookCmd: `forge hook ingest` is invoked by client hooks and must never disturb
// the client: every outcome (bad JSON, locked DB, missing FORGE_HOME, panic,
// bad flags) ends with exit code 0 and nothing on stdout. A one-line diagnostic
// goes to stderr only when FORGE_HOOK_DEBUG=1.
func hookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Client hook helpers (ingest|snippet)",
	}
	cmd.AddCommand(hookIngestCmd(), hookSnippetCmd())
	return cmd
}

func hookIngestCmd() *cobra.Command {
	var client, event string
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Record one hook event read from stdin (always exits 0, silent)",
		// Unknown flags / args must not make the hook fail.
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		SilenceUsage:       true,
		SilenceErrors:      true,
		Run: func(cmd *cobra.Command, _ []string) {
			defer func() {
				if r := recover(); r != nil {
					hookDebug(cmd, fmt.Errorf("panic: %v", r))
				}
			}()
			opts := ledger.HookOptions{Client: client, Event: event}
			if d, ok := hookTimeoutFromEnv(os.Getenv("FORGE_HOOK_TIMEOUT_MS")); ok {
				opts.Timeout = d
			}
			if err := ingestHook(cmd.InOrStdin(), opts); err != nil {
				hookDebug(cmd, err)
			}
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "client id (claude-code)")
	cmd.Flags().StringVar(&event, "event", "", "pre|post (fallback when the payload has no hook_event_name)")
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		hookDebug(c, err)
		return nil
	})
	return cmd
}

// maxHookTimeoutMS is the upper bound of FORGE_HOOK_TIMEOUT_MS.
const maxHookTimeoutMS = 10000

// hookTimeoutFromEnv parses FORGE_HOOK_TIMEOUT_MS. Non-numeric (including
// trailing garbage), zero and negative values are ignored (default timeout);
// values above 10000 are clamped to 10000.
func hookTimeoutFromEnv(v string) (time.Duration, bool) {
	ms, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange || strings.HasPrefix(strings.TrimSpace(v), "-") {
			return 0, false
		}
		ms = maxHookTimeoutMS // positive overflow
	}
	if ms <= 0 {
		return 0, false
	}
	if ms > maxHookTimeoutMS {
		ms = maxHookTimeoutMS
	}
	return time.Duration(ms) * time.Millisecond, true
}

func hookDebug(cmd *cobra.Command, err error) {
	if os.Getenv("FORGE_HOOK_DEBUG") == "1" {
		fmt.Fprintf(cmd.ErrOrStderr(), "forge hook: %v\n", err)
	}
}

func hookSnippetCmd() *cobra.Command {
	var client, bin string
	cmd := &cobra.Command{
		Use:   "snippet",
		Short: "Print the settings.json hooks fragment (does not modify any file)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if client != ledger.ClientClaudeCode {
				return fmt.Errorf("--client 는 claude-code 만 지원합니다 (OpenCode 는 플러그인 payload 미검증)")
			}
			b, err := ledger.ClaudeCodeSnippet(bin)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "client id (claude-code)")
	cmd.Flags().StringVar(&bin, "bin", "", "forge 바이너리 절대 경로 (기본: PATH 의 forge)")
	_ = cmd.MarkFlagRequired("client")
	return cmd
}
