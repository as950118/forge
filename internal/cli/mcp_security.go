package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

func approvalLabel(a string) string {
	switch a {
	case mcp.ApprovalApproved:
		return "승인됨"
	case mcp.ApprovalChanged:
		return "변경됨"
	default:
		return "미승인"
	}
}

func renderMCPPreview(w io.Writer, pv *mcp.Preview, title string) {
	p := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }
	p("== %s ==\n", title)
	p("이름:    %s\n전송:    %s\n", pv.Name, pv.Transport)
	if pv.Transport == mcp.TransportStdio {
		p("실행될 명령줄:  %s\n", pv.CommandLine)
		if pv.Cwd != "" {
			p("작업 디렉터리:  %s\n", pv.Cwd)
		}
		if pv.Runner != "" {
			p("패키지 러너:    %s", pv.Runner)
			if pv.Package != "" {
				p("  패키지/이미지: %s  (버전 고정: %s)", pv.Package, pinLabel(pv.Pin))
			}
			p("\n")
		}
		if pv.ChildEnv != "" {
			p("자식 환경:      %s\n", pv.ChildEnv)
		}
	} else {
		p("URL:     %s\n스킴/호스트: %s / %s\n", pv.URL, pv.Scheme, pv.Host)
	}
	if len(pv.Values) > 0 {
		p("환경변수·헤더(참조 이름만, 값은 표시하지 않음):\n")
		for _, v := range pv.Values {
			p("  %-6s %s = %s\n", v.Slot, v.Key, v.Ref)
		}
	}
	if len(pv.Findings) > 0 {
		p("점검 (휴리스틱 — 서버 동작에 대한 판정이 아님):\n")
		for _, f := range pv.Findings {
			mark := "  - "
			if f.Level == "warn" {
				mark = "  ! "
			}
			p("%s%s\n", mark, f.Text)
		}
	}
	p("\n")
}

func pinLabel(pin string) string {
	switch pin {
	case "pinned":
		return "고정됨"
	case "range":
		return "범위(고정 아님)"
	case "tag":
		return "이동 가능한 태그(고정 아님)"
	case "local":
		return "로컬 경로"
	default:
		return "고정되지 않음"
	}
}

// renderRefreshChanges prints the approval-relative findings of a refresh.
func renderRefreshChanges(out io.Writer, res *mcp.RefreshResult) {
	if len(res.Changes) == 0 && res.Unapproved == 0 {
		return
	}
	srv := shq(res.Name)
	var newNames []string
	for _, c := range res.Changes {
		switch c.Kind {
		case "new":
			newNames = append(newNames, c.Name)
		case "removed":
			extra := ""
			if c.Approved {
				extra = " (승인했던 도구)"
			}
			fmt.Fprintf(out, "  ! 사라짐: %s%s\n", c.Name, extra)
		case "changed":
			label := "변경됨(승인된 정의와 다름)"
			if c.Baseline == "previous" {
				label = "변경됨(이전 캐시와 다름, 미승인)"
			}
			hid := ""
			if c.Hidden {
				hid = " → enforce: 게이트웨이에서 숨김"
			}
			fmt.Fprintf(out, "  ! %s: %s%s\n", label, c.Name, hid)
			if c.DescriptionChanged {
				fmt.Fprintf(out, "      설명 변경 (프롬프트 인젝션 위험: 바뀐 부분을 읽어 보세요)\n        이전: %s\n        이후: %s\n", c.OldSpan, c.NewSpan)
			}
			for _, d := range c.SchemaDiff {
				fmt.Fprintf(out, "      입력 스키마: %s\n", d)
			}
		}
	}
	if len(newNames) > 0 {
		show := newNames
		more := ""
		if len(show) > 10 {
			show, more = show[:10], fmt.Sprintf(" 외 %d개", len(newNames)-10)
		}
		fmt.Fprintf(out, "  ! 미승인(신규) 도구 %d개: %s%s\n", len(newNames), strings.Join(show, ", "), more)
	}
	if res.ChangedFromApproved > 0 || res.Unapproved > 0 {
		fmt.Fprintf(out, "    확인: forge mcp show %s   승인: forge mcp approve %s   (승인된 정의만 노출: forge mcp pin %s --enforce)\n", srv, srv, srv)
	}
}

func mcpApproveCmd() *cobra.Command {
	var tools []string
	cmd := &cobra.Command{
		Use:   "approve <name> [--tool <도구>]...",
		Short: "Record the cached tool definitions of a server as approved (hash + definition)",
		Long: `현재 캐시된(forge mcp show 로 보이는) 도구 정의를 승인 상태로 기록합니다. 승인 해시·정의·시각이 저장되며,
이후 forge mcp refresh 에서 정의(이름·설명·입력 스키마)가 달라지면 "변경됨" 으로 경고하고, forge mcp pin --enforce 가 켜져 있으면
게이트웨이는 승인된 정의와 해시가 같은 도구만 노출합니다. --tool 로 일부 도구만 승인할 수 있습니다(생략하면 전체).
승인은 "지금 읽은 설명과 스키마를 신뢰한다" 는 뜻이므로 먼저 forge mcp show 로 읽어 보세요. 서버에 접속하지 않습니다.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			res, err := reg.Approve(cmd.Context(), args[0], tools)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "승인됨: %s — 도구 %d개\n", args[0], len(res.Approved))
			for _, a := range res.Approved {
				state := map[string]string{"none": "신규 승인", "same": "이미 승인됨(변경 없음)", "changed": "정의 변경을 승인(이전 승인 대체)"}[a.Previous]
				fmt.Fprintf(out, "  - %s: %s\n", textsafe.Display(a.Name, 80), state)
				if a.Change != nil {
					if a.Change.DescriptionChanged {
						fmt.Fprintf(out, "      설명 이전: %s\n      설명 이후: %s\n", a.Change.OldSpan, a.Change.NewSpan)
					}
					for _, d := range a.Change.SchemaDiff {
						fmt.Fprintf(out, "      입력 스키마: %s\n", d)
					}
				}
			}
			for _, n := range res.Pruned {
				fmt.Fprintf(out, "  - %s: 서버가 더 이상 제공하지 않아 승인 기록을 지웠습니다\n", n)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&tools, "tool", nil, "승인할 도구 이름 (반복 가능, 생략하면 전체)")
	return cmd
}

func mcpAllowPrivateCmd() *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:   "allow-private <name> [--revoke]",
		Short: "Allow a remote (http/sse) MCP server to connect to private-network addresses",
		Long: `원격(http/sse) MCP 서버 연결은 연결 시점에 주소를 검사합니다(DNS 해석 뒤, 리다이렉트 포함):
링크 로컬·클라우드 메타데이터 주소(169.254.x.x 등)는 항상 차단하고, 이름이 사설/루프백 주소(10.x, 192.168.x, 127.x ...)로
해석되는 연결도 기본으로 차단합니다. URL 의 호스트가 localhost 또는 사설/루프백 IP 리터럴이면(직접 쓴 로컬 개발 서버)
이 명령 없이 허용됩니다.

사내망 서버처럼 이름(예: mcp.corp.internal)이 사설 주소로 해석되는 서버를 쓰려면 신뢰하는 경우에만 이 명령으로 허용하세요.
허용해도 링크 로컬·메타데이터 주소는 계속 차단됩니다. --revoke 로 되돌립니다. stdio 서버에는 해당 없습니다.
환경변수 프록시(HTTPS_PROXY 등)를 쓰는 연결은 프록시가 대상에 접속하므로 Forge 가 대상 주소를 검사할 수 없습니다(docs/security.md).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			info, err := reg.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if info.Transport == mcp.TransportStdio {
				return fmt.Errorf("%s 는 stdio 서버입니다: allow-private 는 http/sse 서버에만 의미가 있습니다", textsafe.Display(args[0], 80))
			}
			if err := reg.SetAllowPrivate(cmd.Context(), args[0], !revoke); err != nil {
				return err
			}
			if revoke {
				fmt.Fprintf(cmd.OutOrStdout(), "사설 주소 허용 해제: %s\n", textsafe.Display(args[0], 80))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "사설 주소 허용: %s (루프백·사설·CGNAT 주소로 해석되는 연결을 허용합니다. 링크 로컬·메타데이터 주소는 계속 차단됩니다)\n", textsafe.Display(args[0], 80))
			return nil
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "허용을 해제")
	return cmd
}

func mcpPinCmd() *cobra.Command {
	var on, off bool
	cmd := &cobra.Command{
		Use:   "pin <name> --enforce|--no-enforce",
		Short: "Gateway exposes only approved, unchanged tool definitions of a server (default off)",
		Long: `--enforce: 게이트웨이(forge mcp serve)는 이 서버의 도구 중 승인된 정의와 해시가 같은 것만 노출하고,
변경됐거나 미승인인 도구는 숨깁니다(forge_list 에 숨긴 이유 표시). 기본은 꺼짐입니다. 켜면 안전하게 닫히는 방향이라
승인(forge mcp approve)이 없으면 도구가 모두 숨겨집니다.
제한: 게이트웨이를 거치는(exposure=gateway) 노출에만 적용됩니다. direct 로 클라이언트 설정에 직접 렌더링된 서버는
클라이언트가 서버와 직접 통신하므로 Forge 가 걸러낼 수 없습니다.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if on == off {
				return fmt.Errorf("--enforce 와 --no-enforce 중 하나를 지정하세요")
			}
			reg, closeFn, err := openMCP(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := reg.SetEnforce(cmd.Context(), args[0], on); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !on {
				fmt.Fprintf(out, "도구 고정 해제: %s (게이트웨이가 캐시된 모든 도구를 노출)\n", args[0])
				return nil
			}
			info, err := reg.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			ok, hidden := 0, 0
			for _, t := range info.Tools {
				if t.Stale {
					continue
				}
				if t.Approval == mcp.ApprovalApproved {
					ok++
				} else {
					hidden++
				}
			}
			fmt.Fprintf(out, "도구 고정 켜짐: %s — 노출 %d개, 숨김 %d개(변경됨/미승인)\n", args[0], ok, hidden)
			if hidden > 0 {
				fmt.Fprintf(out, "  숨겨진 도구를 쓰려면 설명을 읽고 승인: forge mcp show %s / forge mcp approve %s\n", shq(args[0]), shq(args[0]))
			}
			if info.Exposure != nil {
				direct := false
				for _, m := range info.Exposure {
					if m == mcp.ExposureDirect {
						direct = true
					}
				}
				if direct {
					fmt.Fprintf(out, "  주의: 이 서버는 일부 클라이언트에 direct 로 노출됩니다. enforce 는 게이트웨이 경유 노출에만 적용됩니다 (forge mcp expose %s --client <클라이언트> --mode gateway)\n", shq(args[0]))
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&on, "enforce", false, "승인된 정의만 노출")
	cmd.Flags().BoolVar(&off, "no-enforce", false, "고정 해제")
	return cmd
}
