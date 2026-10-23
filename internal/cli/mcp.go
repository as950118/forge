package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/heonjinjeong/forge/internal/gateway"
	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/profile"
	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/tool"
)

func openMCP(cmd *cobra.Command) (*mcp.Registry, func(), error) {
	_, conn, err := openRegistry(cmd.Context())
	if err != nil {
		return nil, nil, err
	}
	return &mcp.Registry{DB: conn}, func() { _ = conn.Close() }, nil
}

func mcpCmd() *cobra.Command {
	var project bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage MCP servers (add|list|show|remove|refresh|enable|disable|expose|serve)",
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if project {
				// TODO(project-scope): see mcp.ScopeGlobal.
				return fmt.Errorf("--project 는 아직 지원하지 않습니다 (TODO: 프로젝트 범위 MCP). 현재는 전역(global) 범위만 가능합니다")
			}
			return nil
		},
	}
	cmd.PersistentFlags().BoolVar(&project, "project", false, "프로젝트 범위 (미지원)")
	cmd.AddCommand(
		mcpAddCmd(), mcpListCmd(), mcpShowCmd(), mcpRemoveCmd(), mcpRefreshCmd(),
		mcpToggleCmd("enable", true), mcpToggleCmd("disable", false), mcpExposeCmd(),
		mcpServeCmd(),
	)
	return cmd
}

func mcpAddCmd() *cobra.Command {
	var (
		transport, url, cwd string
		envs, headers       []string
		timeoutMS           int
		asJSON              bool
	)
	cmd := &cobra.Command{
		Use:   "add <name> [flags] [-- <command> [args...]]",
		Short: "Register an MCP server (stdio: command after --, http/sse: --url)",
		Long: `MCP 서버를 등록합니다. 비밀 값은 평문으로 저장하지 않습니다.

  stdio: forge mcp add github --env GITHUB_TOKEN='${GITHUB_TOKEN}' -- npx -y @modelcontextprotocol/server-github
  http : forge mcp add docs --transport http --url https://example.com/mcp --header 'Authorization=${DOCS_TOKEN}'
  sse  : forge mcp add old --transport sse  --url https://example.com/sse

값 규칙: KEY=${ENV_NAME} 은 환경변수 참조로 저장, KEY=literal 은 TOKEN/KEY/SECRET/PASSWORD/AUTH 등
비밀로 보이는 이름이 아니고 값도 토큰/키 형태(sk-, ghp_, eyJ..., Bearer ..., 긴 무작위 문자열)가 아닐 때만 허용합니다.
URL 경로/쿼리와 -- 뒤 인자에도 같은 검사를 적용하며, 비밀이 감지되면 저장을 거부합니다(우회 옵션 없음). keychain 참조는 아직 지원하지 않습니다.
셸이 값을 확장하지 않도록 '${ENV_NAME}' 처럼 작은따옴표로 감싸세요.

환경: stdio 서버는 refresh 와 게이트웨이 모두 최소 환경(PATH/HOME/LANG/프록시 등)과 --env 로 선언한 변수만 받습니다.
부모 셸의 다른 환경변수(API 키 등)는 전달되지 않으므로 서버가 필요로 하는 변수는 --env KEY='${KEY}' 로 선언하세요.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			rest := args[1:]
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				if d != 1 {
					return errors.New("이름은 하나만 지정하세요: forge mcp add <name> [flags] -- <command> [args...]")
				}
			}
			spec := mcp.Spec{Name: name, URL: url, Cwd: cwd, TimeoutMS: timeoutMS, Transport: transport}
			if len(rest) > 0 {
				spec.Command, spec.Args = rest[0], rest[1:]
			}
			if spec.Transport == "" {
				switch {
				case spec.URL != "" && spec.Command == "":
					return errors.New("--url 만으로는 전송 방식을 알 수 없습니다: --transport http 또는 sse 를 지정하세요")
				case spec.Command != "":
					spec.Transport = mcp.TransportStdio
				default:
					return errors.New("stdio 는 `-- <command>` 를, http/sse 는 --transport 와 --url 을 지정하세요")
				}
			}
			for _, e := range envs {
				v, err := mcp.ParseValue(mcp.SlotEnv, e)
				if err != nil {
					return err
				}
				spec.Values = append(spec.Values, v)
			}
			for _, h := range headers {
				v, err := mcp.ParseValue(mcp.SlotHeader, h)
				if err != nil {
					return err
				}
				spec.Values = append(spec.Values, v)
			}
			if err := spec.Validate(); err != nil {
				return err
			}
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.Add(cmd.Context(), spec); err != nil {
				return err
			}
			if asJSON {
				info, err := reg.Get(cmd.Context(), name)
				if err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), map[string]any{"server": info})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "추가됨: %s (%s). 도구 목록을 가져오려면: forge mcp refresh %s\n", name, spec.Transport, name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&transport, "transport", "", "전송 방식: stdio|http|sse (명령이 있으면 기본 stdio)")
	f.StringVar(&url, "url", "", "http/sse 서버 URL (http/https)")
	f.StringArrayVar(&envs, "env", nil, "stdio 환경변수 KEY=${ENV_NAME} 또는 KEY=literal (반복 가능)")
	f.StringArrayVar(&headers, "header", nil, "http/sse 헤더 Name=${ENV_NAME} 또는 Name=literal (반복 가능)")
	f.StringVar(&cwd, "cwd", "", "stdio 작업 디렉터리")
	f.IntVar(&timeoutMS, "timeout-ms", 0, "발견 타임아웃(ms). 기본 30000")
	f.BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func discoveryLabel(i *mcp.Info) string {
	switch {
	case i.Discovery == nil:
		return "미발견"
	case i.Discovery.OK:
		return "ok"
	default:
		if c := mcp.ErrorClassOf(i.Discovery.Error); c != "" {
			return "실패(" + c + ")"
		}
		return "실패"
	}
}

func target(i mcp.Info) string {
	if i.Transport == mcp.TransportStdio {
		return strings.TrimSpace(i.Command + " " + strings.Join(i.Args, " "))
	}
	return i.URL
}

func mcpListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered MCP servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			items, err := reg.List(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, items)
			}
			if len(items) == 0 {
				fmt.Fprintln(out, "등록된 MCP 서버가 없습니다.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTRANSPORT\tENABLED\tTOOLS\tDISCOVERY\tTARGET")
			for i := range items {
				it := &items[i]
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", it.Name, it.Transport, yesNo(it.Enabled), it.ToolCount, discoveryLabel(it), truncate(target(*it), 60))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func mcpShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show an MCP server, its secret references and cached tools",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			i, err := reg.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, i)
			}
			fmt.Fprintf(out, "이름:       %s\n범위:       %s\n전송:       %s\n활성:       %s\n", i.Name, i.Scope, i.Transport, yesNo(i.Enabled))
			if i.Transport == mcp.TransportStdio {
				fmt.Fprintf(out, "명령:       %s\n", target(*i))
				if i.Cwd != "" {
					fmt.Fprintf(out, "cwd:        %s\n", i.Cwd)
				}
			} else {
				fmt.Fprintf(out, "URL:        %s\n", i.URL)
			}
			if i.TimeoutMS > 0 {
				fmt.Fprintf(out, "타임아웃:   %dms\n", i.TimeoutMS)
			}
			clients := make([]string, 0, len(i.Exposure))
			for c := range i.Exposure {
				clients = append(clients, c)
			}
			sort.Strings(clients)
			var ex []string
			for _, c := range clients {
				ex = append(ex, c+"="+i.Exposure[c])
			}
			fmt.Fprintf(out, "노출:       %s\n", strings.Join(ex, ", "))
			if len(i.Values) > 0 {
				fmt.Fprintln(out, "\n값 (참조만 표시, 비밀 값은 저장되지 않음):")
				for _, v := range i.Values {
					fmt.Fprintf(out, "  %-6s %s = %s  [%s]\n", v.Slot, v.Key, v.Display, v.RefType)
				}
			}
			fmt.Fprintln(out)
			if d := i.Discovery; d == nil {
				fmt.Fprintf(out, "발견:       아직 없음 (forge mcp refresh %s)\n", i.Name)
			} else {
				fmt.Fprintf(out, "발견:       %s (%s)\n", discoveryLabel(i), time.UnixMilli(d.LastAttemptAt).Local().Format("2006-01-02 15:04:05"))
				if d.ServerName != "" {
					fmt.Fprintf(out, "서버 정보:  %s %s (protocol %s)\n", d.ServerName, d.ServerVersion, d.ProtocolVersion)
				}
				if !d.OK {
					fmt.Fprintf(out, "마지막 오류: %s\n", d.Error)
					if h := mcp.Hint(mcp.ErrorClassOf(d.Error), d.Error); h != "" {
						fmt.Fprintf(out, "해결 안내:  %s\n", h)
					}
					fmt.Fprintln(out, "(아래 도구 목록은 이전 성공 시점의 캐시입니다)")
				}
			}
			if len(i.Tools) > 0 {
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "\nTOOL\tSTALE\tDESCRIPTION")
				for _, t := range i.Tools {
					st := ""
					if t.Stale {
						st = "stale"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Name, st, truncate(t.Description, 70))
				}
				return tw.Flush()
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func mcpRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an MCP server and its cached discovery (requires --yes or confirmation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if _, err := reg.Get(cmd.Context(), args[0]); err != nil {
				return err
			}
			warnProfileMembers(cmd, reg.DB, profile.KindMCP, args[0])
			if !yes {
				fmt.Fprintf(cmd.ErrOrStderr(), "MCP 서버 %q 와 도구 캐시를 삭제합니다. 계속할까요? [y/N] ", args[0])
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return errors.New("취소되었습니다 (삭제하려면 --yes 를 지정하세요)")
				}
			}
			if err := reg.Remove(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "삭제됨: %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "확인 없이 삭제")
	return cmd
}

func mcpToggleCmd(use string, enable bool) *cobra.Command {
	var client string
	word := map[bool]string{true: "활성화", false: "비활성화"}[enable]
	cmd := &cobra.Command{
		Use:   use + " <name> [--client claude-code|opencode]",
		Short: use + " an MCP server (--client: only for that client; enable=direct, disable=hidden)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if client != "" {
				mode := mcp.ExposureHidden
				if enable {
					mode = mcp.ExposureDirect
				}
				if err := refuseWhileProfileActive(cmd, reg, client, args[0], mode); err != nil {
					return err
				}
				if err := reg.SetExposure(cmd.Context(), args[0], client, mode); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (클라이언트 %s, 노출=%s)\n", word, args[0], client, mode)
				return nil
			}
			if err := reg.SetEnabled(cmd.Context(), args[0], enable); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", word, args[0])
			noteProfilesActive(cmd, reg)
			return nil
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "대상 클라이언트 (claude-code|opencode). 생략하면 전체")
	return cmd
}

func mcpExposeCmd() *cobra.Command {
	var client, mode string
	cmd := &cobra.Command{
		Use:   "expose <name> --client <client> --mode direct|gateway|hidden",
		Short: "Set how an MCP server is exposed to a client (applied by sync later)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if client == "" || mode == "" {
				return errors.New("--client 와 --mode 가 필요합니다")
			}
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := refuseWhileProfileActive(cmd, reg, client, args[0], mode); err != nil {
				return err
			}
			if err := reg.SetExposure(cmd.Context(), args[0], client, mode); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "노출 설정: %s -> %s = %s\n", args[0], client, mode)
			return nil
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "클라이언트 ID")
	cmd.Flags().StringVar(&mode, "mode", "", "direct|gateway|hidden")
	return cmd
}

func mcpRefreshCmd() *cobra.Command {
	var (
		asJSON  bool
		verbose bool
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "refresh [name]",
		Short: "Connect to MCP server(s), run initialize + tools/list and cache the result",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			var names []string
			if len(args) == 1 {
				names = args
			} else {
				items, err := reg.List(cmd.Context())
				if err != nil {
					return err
				}
				for _, i := range items {
					names = append(names, i.Name)
				}
				if len(names) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "등록된 MCP 서버가 없습니다.")
					return nil
				}
			}
			out := cmd.OutOrStdout()
			var results []*mcp.RefreshResult
			failed := 0
			for _, n := range names {
				res, err := reg.Refresh(cmd.Context(), n, mcp.RefreshOptions{Timeout: timeout})
				if res != nil {
					results = append(results, res)
					if !res.OK {
						failed++
					}
				}
				if err != nil {
					return err
				}
				if asJSON {
					continue
				}
				if res.OK {
					fmt.Fprintf(out, "OK   %s: 도구 %d개 (신규 %d, 변경 %d, 사라짐 %d) · %s %s · %dms\n",
						res.Name, res.ToolCount, res.Added, res.Changed, res.Removed, res.ServerName, res.ServerVersion, res.DurationMS)
				} else {
					fmt.Fprintf(out, "FAIL %s: [%s] %s\n     해결: %s\n     (이전 캐시는 유지되었습니다)\n", res.Name, res.ErrorClass, res.Error, res.Hint)
					if verbose && res.Detail != "" {
						fmt.Fprintf(out, "     상세(원문): %s\n", res.Detail)
					}
				}
			}
			if asJSON {
				if err := writeJSON(out, results); err != nil {
					return err
				}
			}
			if failed > 0 {
				return fmt.Errorf("MCP 발견 실패: %d개 서버", failed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "실패 시 SDK/OS 원문 진단(detail)도 표시")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "서버당 타임아웃 (예: 10s). 기본은 서버 설정 또는 30s")
	return cmd
}

func mcpServeCmd() *cobra.Command {
	var client, name string
	cmd := &cobra.Command{
		Use:   "serve [--client claude-code|opencode|auto] [--name forge]",
		Short: "Run the Forge MCP gateway on stdio (custom tools, MCP proxy, forge_skill)",
		Long: `Forge 를 stdio MCP 서버(게이트웨이)로 실행합니다. 클라이언트에는 이 서버 하나만 등록하면 됩니다.

  claude mcp add --scope user forge -- forge mcp serve
  opencode.json:  "mcp": {"forge": {"type": "local", "command": ["forge", "mcp", "serve"]}}

노출 대상: 활성 커스텀 도구(forge_<이름>), exposure=gateway 인 MCP 서버의 캐시된 도구(<서버>__<도구>),
스킬 조회 forge_skill, 목록 forge_list. 이름 규칙과 보안 정책은 docs/gateway.md 를 보세요.
표준 출력은 프로토콜 전용이며 로그는 FORGE_GATEWAY_DEBUG=1 일 때만 표준 오류로 나옵니다.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Getenv(gateway.EnvActive) == "1" {
				return errors.New("게이트웨이 안에서 다시 forge mcp serve 를 실행할 수 없습니다 (재귀 방지). 등록된 MCP 서버 중 forge mcp serve 가 있는지 확인하세요")
			}
			// The gateway is started by a client from a configured setup, never
			// the place to bootstrap state: do not create or migrate the home.
			home, err := store.Home()
			if err != nil {
				return err
			}
			if st, err := os.Stat(store.DBPath(home)); err != nil || st.IsDir() {
				return errors.New("Forge 저장소가 초기화되지 않았습니다 (" + store.DBPath(home) + "). forge init 을 먼저 실행하세요 (FORGE_HOME 을 쓰는 경우 클라이언트 설정의 env 에 같은 값을 지정했는지 확인)")
			}
			// Validate the database before anything is created: a corrupt forge.db
			// must not leave skills/ or tmp/ behind (openRegistry creates them).
			if err := store.CheckReadable(cmd.Context(), store.DBPath(home)); err != nil {
				return fmt.Errorf("forge.db 를 열 수 없습니다(손상?): %s. 백업이 있으면 복원하거나 forge doctor 를 확인하세요", err)
			}
			skills, conn, err := openRegistry(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			cwd, _ := os.Getwd()
			// The active profile for (client, this project) decides what is exposed
			// (docs/profile.md); the project is the repository root of the cwd.
			root, perr := profile.ProjectRoot(cwd)
			if perr != nil {
				root = ""
			}
			gw, err := gateway.New(gateway.Options{
				Name: name, Version: cmd.Root().Version, Client: client, Project: cwd,
				Servers:  &mcp.Registry{DB: conn},
				Tools:    &tool.Registry{DB: conn, Home: skills.Home},
				Skills:   skills,
				Recorder: &ledger.SQLite{DB: conn},
				Exposure: gateway.ProfileExposure{DB: conn, Project: root},
			})
			if err != nil {
				return err
			}
			return gw.ServeStdio(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&client, "client", "auto", "클라이언트: claude-code|opencode|auto (auto = initialize 의 clientInfo 로 식별, 모르면 gateway 노출만)")
	cmd.Flags().StringVar(&name, "name", gateway.DefaultName, "클라이언트 설정에 등록한 게이트웨이 서버 이름 (도구 이름 길이 계산과 자기 자신 프록시 방지에 사용)")
	return cmd
}
