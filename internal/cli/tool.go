package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/heonjinjeong/forge/internal/profile"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/tool"
)

func openTool(cmd *cobra.Command) (*tool.Registry, func(), error) {
	reg, conn, err := openRegistry(cmd.Context())
	if err != nil {
		return nil, nil, err
	}
	return &tool.Registry{DB: conn, Home: reg.Home}, func() { _ = conn.Close() }, nil
}

func toolCmd() *cobra.Command {
	var project bool
	cmd := &cobra.Command{
		Use:   "tool",
		Short: "Manage custom tools (add|list|show|remove|enable|disable|test)",
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if project {
				// TODO(project-scope): see tool.ScopeGlobal.
				return fmt.Errorf("--project 는 아직 지원하지 않습니다 (TODO: 프로젝트 범위 도구). 현재는 전역(global) 범위만 가능합니다")
			}
			return nil
		},
	}
	cmd.PersistentFlags().BoolVar(&project, "project", false, "프로젝트 범위 (미지원)")
	cmd.AddCommand(toolAddCmd(), toolListCmd(), toolShowCmd(), toolRemoveCmd(),
		toolToggleCmd("enable", true), toolToggleCmd("disable", false), toolTestCmd())
	return cmd
}

func toolAddCmd() *cobra.Command {
	var (
		kind, desc, schema, schemaFile, argvJSON, shell, interp, scriptFile, scriptBody string
		method, url, body, cwd, confirm                                                 string
		envs, headers, allow                                                            []string
		timeout                                                                         time.Duration
		maxOut, maxRedirects                                                            int
		allowDash, allowVarSub, allowPrivate, allowHTTP, replace, asJSON                bool
	)
	cmd := &cobra.Command{
		Use:   "add <name> [flags] [-- <command> [args...]]",
		Short: "Register a custom tool (command | script | http)",
		Long: `커스텀 도구를 등록합니다. 입력은 JSON Schema 로 검증되며(additionalProperties 기본 false),
템플릿 변수 {{name}} 는 스키마에 선언된 string/integer/number/boolean 속성만 쓸 수 있습니다.

  command: forge tool add git-log --description '최근 커밋' --schema '{"type":"object","properties":{"count":{"type":"integer","minimum":1}},"required":["count"]}' -- git log -n '{{count}}'
           (또는 --argv '["git","log","-n","{{count}}"]'). 셸 없이 argv 로 실행되며 치환은 한 argv 요소 안에서만 일어납니다.
           셸이 꼭 필요하면 --shell 'echo "$FORGE_ARG_msg"' (인자는 환경변수 FORGE_ARG_<이름> 으로만 전달)
  script : forge tool add calc --description '계산' --interpreter python3 --script-file ./calc.py   (인자 JSON 은 stdin, 메타는 FORGE_TOOL_*)
  http   : forge tool add ping --description 'API' --url 'https://api.example.com/items/{{id}}' --header 'Authorization=${API_TOKEN}'

비밀은 --env KEY='${ENV}' / --header Name='${ENV}' 참조로만 저장합니다 (평문 거부, 우회 옵션 없음).
기본 정책: 타임아웃 30s, 환경변수 최소화(PATH 등 + 명시한 ${ENV} 만), 출력 64KiB, https 만, 사설/루프백/메타데이터 IP 차단.
같은 이름을 다시 등록하려면 --replace (revision +1).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, rest := args[0], args[1:]
			if d := cmd.ArgsLenAtDash(); d >= 0 && d != 1 {
				return errors.New("이름은 하나만 지정하세요: forge tool add <name> [flags] -- <command> [args...]")
			}
			def := tool.Definition{Name: name, Description: desc, Kind: kind, Interpreter: interp, Method: method, URL: url, BodyTemplate: body}
			def.Policy = tool.Policy{TimeoutMS: int(timeout / time.Millisecond), Confirm: confirm, WorkingDir: cwd, Allow: allow,
				AllowLeadingDash: allowDash, AllowVarSubcommand: allowVarSub, AllowPrivate: allowPrivate, AllowHTTP: allowHTTP, MaxOutputBytes: maxOut, MaxRedirects: maxRedirects}
			if timeout < 0 || (timeout > 0 && timeout < time.Millisecond) {
				return errors.New("--timeout 은 1ms 이상이어야 합니다")
			}
			if cwd != "" {
				abs, err := filepath.Abs(cwd)
				if err != nil {
					return err
				}
				def.Policy.WorkingDir = abs
			}
			switch {
			case schema != "" && schemaFile != "":
				return errors.New("--schema 와 --schema-file 중 하나만 지정하세요")
			case schemaFile != "":
				b, err := os.ReadFile(schemaFile)
				if err != nil {
					return fmt.Errorf("스키마 파일을 읽을 수 없습니다: %w", err)
				}
				def.InputSchema = b
			case schema != "":
				def.InputSchema = json.RawMessage(schema)
			}
			// command forms
			switch {
			case argvJSON != "" && (len(rest) > 0 || shell != ""), shell != "" && len(rest) > 0:
				return errors.New("--argv, --shell, `-- <command>` 중 하나만 지정하세요")
			case argvJSON != "":
				var argv []string
				if err := json.Unmarshal([]byte(argvJSON), &argv); err != nil || len(argv) == 0 {
					return errors.New(`--argv 는 비어 있지 않은 JSON 문자열 배열이어야 합니다. 예: '["git","log","-n","{{count}}"]'`)
				}
				rest = argv
			case shell != "":
				def.Shell = true
				rest = []string{"sh", "-c", shell}
			}
			if len(rest) > 0 {
				def.Command, def.Args = rest[0], rest[1:]
			}
			if scriptFile != "" && scriptBody != "" {
				return errors.New("--script-file 과 --script-body 중 하나만 지정하세요")
			}
			if scriptFile != "" {
				fi, err := os.Stat(scriptFile)
				if err != nil {
					return fmt.Errorf("스크립트 파일을 읽을 수 없습니다: %w", err)
				}
				if fi.Size() > tool.MaxScriptBytes {
					return fmt.Errorf("스크립트가 너무 큽니다 (최대 %d바이트)", tool.MaxScriptBytes)
				}
				b, err := os.ReadFile(scriptFile)
				if err != nil {
					return fmt.Errorf("스크립트 파일을 읽을 수 없습니다: %w", err)
				}
				scriptBody = string(b)
			}
			def.ScriptBody = scriptBody
			if def.Kind == "" {
				switch {
				case def.Command != "":
					def.Kind = tool.KindCommand
				case def.Interpreter != "" || def.ScriptBody != "":
					def.Kind = tool.KindScript
				case def.URL != "":
					def.Kind = tool.KindHTTP
				default:
					return errors.New("--kind (command|script|http) 를 지정하세요")
				}
			}
			for _, e := range envs {
				v, err := mcp.ParseValue(mcp.SlotEnv, e)
				if err != nil {
					return err
				}
				def.Values = append(def.Values, v)
			}
			for _, h := range headers {
				v, err := mcp.ParseValue(mcp.SlotHeader, h)
				if err != nil {
					return err
				}
				def.Values = append(def.Values, v)
			}
			if err := def.ValidateNew(); err != nil {
				return err
			}
			reg, closeFn, err := openTool(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			saved, err := reg.Add(cmd.Context(), def, replace)
			if err != nil {
				return err
			}
			if asJSON {
				got, err := reg.Get(cmd.Context(), name)
				if err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), map[string]any{"tool": got})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "추가됨: %s (%s, revision %d). 실행해 보려면: forge tool test %s --args '{...}' [--dry-run]\n", saved.Name, saved.Kind, saved.Revision, saved.Name)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&kind, "kind", "", "command|script|http (생략하면 다른 플래그에서 추론)")
	f.StringVar(&desc, "description", "", "도구 설명 (필수)")
	f.StringVar(&schema, "schema", "", `입력 JSON Schema (기본 {"type":"object"} 인자 없음)`)
	f.StringVar(&schemaFile, "schema-file", "", "입력 JSON Schema 파일")
	f.StringVar(&argvJSON, "argv", "", `command: argv 템플릿 JSON 배열 '["git","log","-n","{{count}}"]'`)
	f.StringVar(&shell, "shell", "", "command: 셸 스크립트(sh -c). 인자는 $FORGE_ARG_<이름> 환경변수로만 전달")
	f.StringVar(&interp, "interpreter", "", "script: 인터프리터 (python3|python|node|bash|sh|ruby|perl)")
	f.StringVar(&scriptFile, "script-file", "", "script: 스크립트 파일 (저장소로 복사됨)")
	f.StringVar(&scriptBody, "script-body", "", "script: 스크립트 본문")
	f.StringVar(&method, "method", "", "http: GET|POST|PUT|PATCH|DELETE (기본 GET)")
	f.StringVar(&url, "url", "", "http: URL 템플릿 (경로/쿼리에만 {{변수}})")
	f.StringVar(&body, "body-template", "", `http: JSON 본문 템플릿 '{"q":"{{query}}"}'`)
	f.StringArrayVar(&envs, "env", nil, "command/script 환경변수 KEY=${ENV_NAME} 또는 KEY=literal (반복 가능)")
	f.StringArrayVar(&headers, "header", nil, "http 헤더 Name=${ENV_NAME} 또는 Name=literal (반복 가능)")
	f.DurationVar(&timeout, "timeout", 0, "실행 타임아웃 (기본 30s, 최대 10m)")
	f.StringVar(&confirm, "confirm", "", "none|ask|deny (기본 ask)")
	f.StringVar(&cwd, "cwd", "", "작업 디렉터리 루트 (기본: 실행 시 현재 디렉터리). 이 아래로만 실행 가능")
	f.StringArrayVar(&allow, "allow", nil, "allowlist: command=실행 파일 이름/절대경로, http=호스트(*.example.com), script=추가 인터프리터")
	f.BoolVar(&allowDash, "allow-dash", false, "치환된 값이 '-' 로 시작하는 것을 허용 (옵션 주입 위험)")
	f.BoolVar(&allowVarSub, "allow-var-subcommand", false, "command: argv[1](프로그램/서브커맨드 자리)에 {{변수}} 허용 (기본 거부; 데이터는 `cat -- {{file}}` 처럼 argv[2:] 에)")
	f.BoolVar(&allowPrivate, "allow-private", false, "http: 사설/루프백 주소 허용 (링크로컬·메타데이터는 항상 차단)")
	f.BoolVar(&allowHTTP, "allow-http", false, "http: 평문 http:// 허용")
	f.IntVar(&maxOut, "max-output", 0, "stdout/stderr(응답) 각각의 최대 바이트 (기본 65536, 최대 1048576)")
	f.IntVar(&maxRedirects, "max-redirects", 0, "http: 최대 리다이렉트 횟수 (기본 3, 최대 5)")
	f.BoolVar(&replace, "replace", false, "같은 이름이 있으면 정의를 교체 (revision +1)")
	f.BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func toolTarget(d tool.Definition) string {
	switch d.Kind {
	case tool.KindCommand:
		if d.Shell {
			return "sh -c " + truncate(d.Args[len(d.Args)-1], 50)
		}
		return strings.TrimSpace(d.Command + " " + strings.Join(d.Args, " "))
	case tool.KindScript:
		return d.Interpreter + " " + d.ScriptPath
	}
	return d.Method + " " + d.URL
}

func toolListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered custom tools",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, closeFn, err := openTool(cmd)
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
				fmt.Fprintln(out, "등록된 도구가 없습니다.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tENABLED\tREV\tCONFIRM\tTARGET")
			for _, it := range items {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", it.Name, it.Kind, yesNo(it.Enabled), it.Revision, it.Policy.Confirm, truncate(toolTarget(it), 60))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func toolShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a custom tool, its policy and secret references",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, closeFn, err := openTool(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			d, err := reg.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, d)
			}
			p := d.Policy
			fmt.Fprintf(out, "이름:       %s\n범위:       %s\n종류:       %s\n활성:       %s\nrevision:   %d\n설명:       %s\n", d.Name, d.Scope, d.Kind, yesNo(d.Enabled), d.Revision, d.Description)
			fmt.Fprintf(out, "대상:       %s\n", toolTarget(*d))
			if d.BodyTemplate != "" {
				fmt.Fprintf(out, "본문:       %s\n", d.BodyTemplate)
			}
			fmt.Fprintf(out, "\n정책:\n  timeout=%dms confirm=%s max_output=%dB", p.TimeoutMS, p.Confirm, p.MaxOutputBytes)
			if d.Kind == tool.KindHTTP {
				fmt.Fprintf(out, " max_redirects=%d allow_private=%s allow_http=%s", p.MaxRedirects, yesNo(p.AllowPrivate), yesNo(p.AllowHTTP))
			} else {
				fmt.Fprintf(out, " allow_leading_dash=%s allow_var_subcommand=%s", yesNo(p.AllowLeadingDash), yesNo(p.AllowVarSubcommand))
			}
			fmt.Fprintf(out, "\n  allow=%s\n  cwd 루트=%s\n", strings.Join(p.Allow, ","), orDefault(p.WorkingDir, "(실행 시 현재 디렉터리)"))
			fmt.Fprintf(out, "\n입력 스키마:\n  %s\n", d.InputSchema)
			if len(d.Values) > 0 {
				fmt.Fprintln(out, "\n값 (참조만 표시, 비밀 값은 저장되지 않음):")
				for _, v := range d.Values {
					fmt.Fprintf(out, "  %-6s %s = %s  [%s]\n", v.Slot, v.Key, v.Display, v.RefType)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func toolRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a custom tool (requires --yes or confirmation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, closeFn, err := openTool(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if _, err := reg.Get(cmd.Context(), args[0]); err != nil {
				return err
			}
			warnProfileMembers(cmd, reg.DB, profile.KindTool, args[0])
			if !yes {
				fmt.Fprintf(cmd.ErrOrStderr(), "도구 %q 를 삭제합니다. 계속할까요? [y/N] ", args[0])
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

func toolToggleCmd(use string, enable bool) *cobra.Command {
	word := map[bool]string{true: "활성화", false: "비활성화"}[enable]
	return &cobra.Command{
		Use:   use + " <name>",
		Short: use + " a custom tool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, closeFn, err := openTool(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.SetEnabled(cmd.Context(), args[0], enable); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", word, args[0])
			return nil
		},
	}
}

func toolTestCmd() *cobra.Command {
	var (
		argsJSON, argsFile, cwd string
		timeout                 time.Duration
		asJSON, dry, verbose    bool
	)
	cmd := &cobra.Command{
		Use:   "test <name> [--args '{...}'] [--timeout 10s] [--cwd DIR] [--dry-run] [--json]",
		Short: "Run a custom tool once, standalone (or --dry-run to see the masked command/request)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if argsJSON != "" && argsFile != "" {
				return errors.New("--args 와 --args-file 중 하나만 지정하세요")
			}
			raw := argsJSON
			if argsFile != "" {
				b, err := os.ReadFile(argsFile)
				if err != nil {
					return fmt.Errorf("인자 파일을 읽을 수 없습니다: %w", err)
				}
				raw = string(b)
			}
			in := map[string]any{}
			if strings.TrimSpace(raw) != "" {
				if err := json.Unmarshal([]byte(raw), &in); err != nil || in == nil {
					return errors.New(`--args 는 JSON 객체여야 합니다. 예: --args '{"count":3}'`)
				}
			}
			reg, closeFn, err := openTool(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			def, err := reg.Resolve(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !def.Enabled {
				fmt.Fprintf(cmd.ErrOrStderr(), "참고: 도구 %q 는 비활성 상태입니다 (test 는 그래도 실행합니다)\n", def.Name)
			}
			res, runErr := tool.Run(cmd.Context(), def, in, tool.RunOptions{Cwd: cwd, Timeout: timeout, DryRun: dry})
			out := cmd.OutOrStdout()
			if asJSON {
				if err := writeJSON(out, res); err != nil {
					return err
				}
			} else if runErr == nil {
				printResult(cmd, res)
			}
			if runErr != nil {
				if verbose && res.ErrorDetail != "" && !asJSON {
					fmt.Fprintf(cmd.ErrOrStderr(), "상세(검증기 원문): %s\n", res.ErrorDetail)
				}
				if asJSON {
					return fmt.Errorf("도구 실행이 거부되었습니다 [%s]", res.ErrorClass)
				}
				return runErr
			}
			if !res.OK {
				return fmt.Errorf("도구 실행 실패 [%s]: %s", res.ErrorClass, res.Error)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&argsJSON, "args", "", "호출 인자 JSON 객체")
	f.StringVar(&argsFile, "args-file", "", "호출 인자 JSON 파일")
	f.DurationVar(&timeout, "timeout", 0, "이번 실행의 타임아웃 (예: 10s; 최대 10m)")
	f.StringVar(&cwd, "cwd", "", "작업 디렉터리 (정책 루트 아래만 허용)")
	f.BoolVar(&verbose, "verbose", false, "스키마 위반 시 검증기 원문(영어)도 표시")
	f.BoolVar(&dry, "dry-run", false, "실행하지 않고 치환된 argv/요청을 비밀 마스킹해 표시")
	f.BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func printResult(cmd *cobra.Command, r *tool.Result) {
	out := cmd.OutOrStdout()
	if p := r.DryRun; p != nil {
		fmt.Fprintln(out, "[dry-run] 실행하지 않았습니다. 비밀 값은 마스킹됩니다.")
		if p.URL != "" {
			fmt.Fprintf(out, "%s %s\n", p.Method, p.URL)
			keys := make([]string, 0, len(p.Headers))
			for k := range p.Headers {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(out, "  %s: %s\n", k, p.Headers[k])
			}
			if p.Body != "" {
				fmt.Fprintf(out, "본문: %s\n", p.Body)
			}
			return
		}
		fmt.Fprintf(out, "argv: %q\ncwd:  %s\n", p.Argv, p.Cwd)
		if p.Body != "" {
			fmt.Fprintln(out, p.Body)
		}
		keys := make([]string, 0, len(p.Env))
		for k := range p.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(out, "env:")
		for _, k := range keys {
			fmt.Fprintf(out, "  %s=%s\n", k, p.Env[k])
		}
		return
	}
	if r.Stdout != "" {
		fmt.Fprint(out, r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			fmt.Fprintln(out)
		}
	}
	if r.Stderr != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s", r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			fmt.Fprintln(cmd.ErrOrStderr())
		}
	}
	status := "OK"
	if !r.OK {
		status = "FAIL"
	}
	extra := ""
	if r.HTTPStatus != 0 {
		extra = fmt.Sprintf(" http=%d", r.HTTPStatus)
	} else if r.ExitCode >= 0 {
		extra = fmt.Sprintf(" exit=%d", r.ExitCode)
	}
	if r.StdoutTruncated || r.StderrTruncated {
		extra += " (출력이 상한을 넘어 잘렸습니다)"
	}
	if r.TimedOut {
		extra += " (타임아웃)"
	}
	fmt.Fprintf(out, "[%s] %s%s %dms\n", status, r.Name, extra, r.DurationMS)
}
