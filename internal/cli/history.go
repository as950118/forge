package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/heonjinjeong/forge/internal/history"
	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// historyOpts are the flags shared by `forge history` and its subcommands.
type historyOpts struct {
	since, until, kind, client, project, session, name, nameContains, source, server string
	ok, failed                                                                       bool
	limit                                                                            int
	format, output                                                                   string
	asJSON, force, raw                                                               bool
	groupBy                                                                          string
}

// Flag groups (which subcommand accepts which flags).
var (
	outFlags    = []string{"format", "json", "output", "force"}
	filterFlags = []string{"since", "until", "kind", "client", "project", "session", "name", "name-contains", "server", "source", "ok", "failed", "limit"}
)

func historyCmd() *cobra.Command {
	o := &historyOpts{}
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Query usage history (list|summary|top|unused|misses)",
		Long: `호출 이력(원장)을 조회·집계합니다. 읽기 전용이며 forge.db 를 수정하지 않습니다.

  forge history [필터]            최근 호출 목록 (같은 tool_use_id 를 여러 source 가 기록해도 1건으로 병합, --raw 로 원본 행)
  forge history summary           --group-by kind|name|client|project|day|source 로 집계 (기본 kind)
  forge history top               상위 사용량 (이름별)
  forge history unused            등록돼 있지만 기간 내 호출이 없는 스킬/도구/MCP 서버
  forge history misses            미등록 호출 이름 (found=0), MCP 는 서버별

기간: --since/--until 에 7d 24h 90m 30s 2w (지금부터 그만큼 전), 2026-10-01 (로컬 날짜 00:00),
RFC3339(2026-10-01T09:00:00+09:00) 를 줄 수 있습니다. --since 는 포함, --until 은 제외입니다.
--name 은 정확히 일치, --name-contains 는 부분 일치(ASCII 대소문자 무시)입니다.
--format table|json|csv (--json = --format json), --output 파일 은 새 파일만(덮어쓰기는 --force), 권한 0600.
day 집계는 UTC 날짜 기준입니다. 표의 시각은 로컬 시간대, JSON 은 UTC(RFC3339)와 epoch ms 입니다.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.groupBy != "" { // forge history --group-by X  ==  forge history summary --group-by X
				return runSummary(cmd, o, o.groupBy)
			}
			if err := o.allow(cmd, append(append([]string{}, filterFlags...), append(outFlags, "raw")...)...); err != nil {
				return err
			}
			return o.run(cmd, func(ctx context.Context, db *sql.DB, f history.Filter, limit int, loc *time.Location) (history.Report, error) {
				f.Raw = o.raw
				calls, err := history.List(ctx, db, f, limit)
				if err != nil {
					return history.Report{}, err
				}
				return history.ListReport(calls, limit, o.raw, loc), nil
			})
		},
	}
	pf := cmd.PersistentFlags()
	pf.StringVar(&o.since, "since", "", "이 시각 이후(포함): 7d|24h|90m|2026-10-01|RFC3339")
	pf.StringVar(&o.until, "until", "", "이 시각 이전(제외)")
	pf.StringVar(&o.kind, "kind", "", "skill|tool|mcp")
	pf.StringVar(&o.client, "client", "", "클라이언트 id (예: claude-code)")
	pf.StringVar(&o.project, "project", "", "프로젝트 경로 (정확히 일치)")
	pf.StringVar(&o.session, "session", "", "세션 ID")
	pf.StringVar(&o.name, "name", "", "이름 정확히 일치")
	pf.StringVar(&o.nameContains, "name-contains", "", "이름 부분 일치 (ASCII 대소문자 무시)")
	pf.StringVar(&o.server, "server", "", "MCP 서버 이름 ('-' 와 '_' 는 같은 이름)")
	pf.StringVar(&o.source, "source", "", "hook|gateway|transcript (병합된 호출이 해당 source 를 포함하는 것)")
	pf.BoolVar(&o.ok, "ok", false, "성공한 호출만")
	pf.BoolVar(&o.failed, "failed", false, "실패한 호출만")
	pf.IntVar(&o.limit, "limit", history.DefaultLimit, fmt.Sprintf("최대 행 수 (1~%d)", history.MaxLimit))
	pf.StringVar(&o.format, "format", "", "table|json|csv (기본 table)")
	pf.BoolVar(&o.asJSON, "json", false, "JSON 출력 (--format json 과 같음)")
	pf.StringVar(&o.output, "output", "", "결과를 파일로 저장 (새 파일만, 권한 0600)")
	pf.BoolVar(&o.force, "force", false, "--output 대상이 일반 파일이면 덮어쓰기 (심볼릭 링크는 항상 거부)")
	cmd.Flags().BoolVar(&o.raw, "raw", false, "병합 전 원본 원장 행 보기")
	cmd.Flags().StringVar(&o.groupBy, "group-by", "", "집계 기준 kind|name|client|project|day|source (summary 와 같음)")

	cmd.AddCommand(historySummaryCmd(o), historyTopCmd(o), historyUnusedCmd(o), historyMissesCmd(o))
	return cmd
}

func historySummaryCmd(o *historyOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Aggregate calls: count, ok/failed, avg/max latency, last call",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			g := o.groupBy
			if g == "" {
				g = "kind"
			}
			return runSummary(cmd, o, g)
		},
	}
	cmd.Flags().StringVar(&o.groupBy, "group-by", "", "kind|name|client|project|day|source (기본 kind)")
	return cmd
}

func runSummary(cmd *cobra.Command, o *historyOpts, groupBy string) error {
	ok := false
	for _, g := range history.GroupBys {
		ok = ok || g == groupBy
	}
	if !ok {
		return fmt.Errorf("--group-by 값 %q 은(는) 지원하지 않습니다 (kind|name|client|project|day|source)", cleanArg(groupBy))
	}
	if err := o.allow(cmd, append(append([]string{"group-by"}, filterFlags...), outFlags...)...); err != nil {
		return err
	}
	return o.run(cmd, func(ctx context.Context, db *sql.DB, f history.Filter, limit int, loc *time.Location) (history.Report, error) {
		gs, err := history.Summary(ctx, db, f, groupBy, limit)
		if err != nil {
			return history.Report{}, err
		}
		return history.SummaryReport(gs, groupBy, loc), nil
	})
}

func historyTopCmd(o *historyOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "top",
		Short: "Most used skills/tools/MCP tools (top N by calls, --limit N)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.allow(cmd, append(append([]string{}, filterFlags...), outFlags...)...); err != nil {
				return err
			}
			return o.run(cmd, func(ctx context.Context, db *sql.DB, f history.Filter, limit int, loc *time.Location) (history.Report, error) {
				gs, err := history.Summary(ctx, db, f, "name", limit)
				if err != nil {
					return history.Report{}, err
				}
				return history.SummaryReport(gs, "name", loc), nil
			})
		},
	}
}

func historyUnusedCmd(o *historyOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "unused",
		Short: "Registered skills/tools/MCP servers with no call in the period (--kind, --since)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.allow(cmd, append([]string{"kind", "since", "until"}, outFlags...)...); err != nil {
				return err
			}
			return o.run(cmd, func(ctx context.Context, db *sql.DB, f history.Filter, _ int, loc *time.Location) (history.Report, error) {
				items, err := history.UnusedItems(ctx, db, f.Kind, f.Since, f.Until)
				if err != nil {
					return history.Report{}, err
				}
				return history.UnusedReport(items, loc), nil
			})
		},
	}
}

func historyMissesCmd(o *historyOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "misses",
		Short: "Unregistered call names (found=0) of skills, tools and MCP tools (per MCP server)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.allow(cmd, append([]string{"kind", "client", "since", "until", "limit"}, outFlags...)...); err != nil {
				return err
			}
			return o.run(cmd, func(ctx context.Context, db *sql.DB, f history.Filter, limit int, loc *time.Location) (history.Report, error) {
				ms, err := history.Misses(ctx, db, f, limit)
				if err != nil {
					return history.Report{}, err
				}
				return history.MissesReport(ms, loc), nil
			})
		},
	}
}

// skillMissesCmd is `forge skill misses`: skills that were called but are not in
// the registry (found=0). Calls with unknown found (NULL: "plugin:skill" names,
// unidentified) and non-skill kinds never appear.
func skillMissesCmd() *cobra.Command {
	o := &historyOpts{}
	cmd := &cobra.Command{
		Use:   "misses",
		Short: "Show skills that were called but are not registered (found=0)",
		Long: `Forge 레지스트리에 없는 이름으로 호출된 스킬(found=0)을 이름별로 집계합니다
(횟수, 첫/마지막 호출, 클라이언트별 분포). 읽기 전용입니다.
plugin:skill 형태(클라이언트 플러그인 소유)와 결과를 알 수 없는 호출은 포함하지 않습니다.
도구/MCP 의 미등록 호출은 forge history misses 를 쓰세요.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.allow(cmd, append([]string{"since", "until", "client", "limit"}, outFlags...)...); err != nil {
				return err
			}
			o.kind = "skill"
			return o.run(cmd, func(ctx context.Context, db *sql.DB, f history.Filter, limit int, loc *time.Location) (history.Report, error) {
				ms, err := history.Misses(ctx, db, f, limit)
				if err != nil {
					return history.Report{}, err
				}
				return history.MissesReport(ms, loc), nil
			})
		},
	}
	pf := cmd.Flags()
	pf.StringVar(&o.since, "since", "", "이 시각 이후(포함): 7d|24h|90m|2026-10-01|RFC3339")
	pf.StringVar(&o.until, "until", "", "이 시각 이전(제외)")
	pf.StringVar(&o.client, "client", "", "클라이언트 id")
	pf.IntVar(&o.limit, "limit", history.DefaultLimit, fmt.Sprintf("최대 행 수 (1~%d)", history.MaxLimit))
	pf.StringVar(&o.format, "format", "", "table|json|csv (기본 table)")
	pf.BoolVar(&o.asJSON, "json", false, "JSON 출력")
	pf.StringVar(&o.output, "output", "", "결과를 파일로 저장 (새 파일만, 권한 0600)")
	pf.BoolVar(&o.force, "force", false, "--output 대상이 일반 파일이면 덮어쓰기")
	return cmd
}

// allow rejects flags that the subcommand does not use (instead of silently
// ignoring a filter the user believes is applied).
func (o *historyOpts) allow(cmd *cobra.Command, allowed ...string) error {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var bad string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if !ok[f.Name] && bad == "" {
			bad = f.Name
		}
	})
	if bad != "" {
		return fmt.Errorf("forge %s 는 --%s 를 지원하지 않습니다", strings.TrimPrefix(cmd.CommandPath(), "forge "), bad)
	}
	return nil
}

func cleanArg(s string) string {
	if len(s) > 40 {
		s = s[:40] + "..."
	}
	return textsafe.Display(s, 120)
}

// filter validates the flags and builds the query filter.
func (o *historyOpts) filter(loc *time.Location) (history.Filter, int, string, error) {
	f := history.Filter{Kind: o.kind, Client: o.client, Session: o.session, Server: o.server, Source: o.source}
	if o.kind != "" && !in(history.Kinds, o.kind) {
		return f, 0, "", fmt.Errorf("--kind 값 %q 은(는) 지원하지 않습니다 (skill|tool|mcp)", cleanArg(o.kind))
	}
	if o.source != "" && !in(history.Sources, o.source) {
		return f, 0, "", fmt.Errorf("--source 값 %q 은(는) 지원하지 않습니다 (hook|gateway|transcript)", cleanArg(o.source))
	}
	if o.ok && o.failed {
		return f, 0, "", errors.New("--ok 와 --failed 는 함께 쓸 수 없습니다")
	}
	if o.ok || o.failed {
		v := o.ok
		f.OK = &v
	}
	if o.name != "" && o.nameContains != "" {
		return f, 0, "", errors.New("--name(정확히 일치) 과 --name-contains(부분 일치) 는 함께 쓸 수 없습니다")
	}
	if o.name != "" {
		f.Name = o.name
	} else if o.nameContains != "" {
		f.Name, f.NameContains = o.nameContains, true
	}
	if o.project != "" {
		f.Project = ledger.NormalizeProject(o.project)
	}
	if o.since != "" {
		v, err := history.ParseTime("since", o.since, loc)
		if err != nil {
			return f, 0, "", err
		}
		f.Since = &v
	}
	if o.until != "" {
		v, err := history.ParseTime("until", o.until, loc)
		if err != nil {
			return f, 0, "", err
		}
		f.Until = &v
	}
	if f.Since != nil && f.Until != nil && *f.Since >= *f.Until {
		return f, 0, "", errors.New("--since 가 --until 보다 같거나 늦습니다")
	}
	if o.limit < 1 || o.limit > history.MaxLimit {
		return f, 0, "", fmt.Errorf("--limit 은 1 이상 %d 이하여야 합니다", history.MaxLimit)
	}
	format := o.format
	if o.asJSON {
		if format != "" && format != history.FormatJSON {
			return f, 0, "", errors.New("--json 은 --format json 의 별칭이라 다른 --format 과 함께 쓸 수 없습니다")
		}
		format = history.FormatJSON
	}
	switch format {
	case "":
		format = history.FormatTable
	case history.FormatTable, history.FormatJSON, history.FormatCSV:
	default:
		return f, 0, "", fmt.Errorf("--format 값 %q 은(는) 지원하지 않습니다 (table|json|csv)", cleanArg(format))
	}
	return f, o.limit, format, nil
}

func in(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

type buildFn func(ctx context.Context, db *sql.DB, f history.Filter, limit int, loc *time.Location) (history.Report, error)

// run validates the flags (before touching the database), opens the registry
// read-only, builds the report and writes it to stdout or --output.
func (o *historyOpts) run(cmd *cobra.Command, build buildFn) error {
	loc := time.Local
	f, limit, format, err := o.filter(loc)
	if err != nil {
		return err
	}
	home, err := store.Home()
	if err != nil {
		return err
	}
	db, err := history.Open(cmd.Context(), home)
	if err != nil {
		return err
	}
	defer db.Close()
	rep, err := build(cmd.Context(), db, f, limit, loc)
	if err != nil {
		return err
	}
	if o.output == "" {
		return rep.Write(cmd.OutOrStdout(), format)
	}
	var buf bytes.Buffer
	if err := rep.Write(&buf, format); err != nil {
		return err
	}
	dbp := store.DBPath(home)
	if err := history.WriteFile(o.output, buf.Bytes(), o.force, dbp, dbp+"-wal", dbp+"-shm"); err != nil {
		return err
	}
	rows := len(rep.Human)
	fmt.Fprintf(cmd.OutOrStdout(), "저장했습니다: %s (%s, %d행)\n", textsafe.Display(o.output, 300), format, rows)
	return nil
}
