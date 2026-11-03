package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/as950118/forge/internal/history"
	"github.com/as950118/forge/internal/ledger"
	"github.com/as950118/forge/internal/store"
	"github.com/as950118/forge/internal/textsafe"
)

// historyOpts are the flags shared by `forge history` and its subcommands.
type historyOpts struct {
	since, until, kind, client, project, session, name, nameContains, source, server string
	ok, failed                                                                       bool
	limit                                                                            int
	format, output                                                                   string
	asJSON, force, raw                                                               bool
	groupBy                                                                          string
	scanAll                                                                          bool // aggregate over the whole ledger unless --since is given
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
		Long: `호출 이력(원장)을 조회·집계합니다. prune 을 제외한 모든 명령은 읽기 전용이며 forge.db 를 수정하지 않습니다.

  forge history [필터]            최근 호출 목록 (같은 tool_use_id 를 여러 source 가 기록해도 1건으로 병합, --raw 로 원본 행)
  forge history summary           --group-by kind|name|client|project|day|source 로 집계 (기본 kind)
  forge history top               상위 사용량 (이름별)
  forge history unused            등록돼 있지만 기간 내 호출이 없는 스킬/도구/MCP 서버
  forge history misses            미등록 호출 이름 (found=0), MCP 는 서버별
  forge history prune --before X  오래된 기록 삭제 (기본 dry-run, 실제 삭제는 --yes; 아래 prune 설명)

기간: --since/--until 에 7d 24h 90m 30s 2w (지금부터 그만큼 전), 2026-10-01 (로컬 날짜 00:00),
RFC3339(2026-10-01T09:00:00+09:00) 를 줄 수 있습니다. --since 는 포함, --until 은 제외입니다.
--name 은 정확히 일치, --name-contains 는 부분 일치(ASCII 대소문자 무시)입니다.
--format table|json|csv (--json = --format json), --output 파일 은 새 파일만(덮어쓰기는 --force), 권한 0600.
--output - 는 파일이 아니라 표준 출력입니다(파일 이름이 - 라면 ./- 로 쓰세요). --output 의 부모 디렉터리가 심볼릭 링크여도
따라가서 씁니다(사용자가 고른 경로로 보고 허용). 마지막 구성요소가 심볼릭 링크이거나 Forge 저장소 파일이면 거부합니다.
기간(--since)을 주지 않은 summary/top 은 원장 전체를 집계하므로 기록이 아주 많으면(100만 건 이상) 수 초 이상 걸립니다.
이때는 --since 7d 처럼 기간을 주세요.
day 집계는 UTC 날짜 기준입니다. 표의 시각은 로컬 시간대, JSON 은 UTC(RFC3339)와 epoch ms 입니다.`,
		Args:          noArgs,
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
	pf.StringVar(&o.output, "output", "", "결과를 파일로 저장 (새 파일만, 권한 0600). - 는 표준 출력")
	pf.BoolVar(&o.force, "force", false, "--output 대상이 일반 파일이면 덮어쓰기 (심볼릭 링크는 항상 거부)")
	cmd.Flags().BoolVar(&o.raw, "raw", false, "병합 전 원본 원장 행 보기")
	cmd.Flags().StringVar(&o.groupBy, "group-by", "", "집계 기준 kind|name|client|project|day|source (summary 와 같음)")

	cmd.AddCommand(historySummaryCmd(o), historyTopCmd(o), historyUnusedCmd(o), historyMissesCmd(o), historyPruneCmd(o))
	return cmd
}

func historySummaryCmd(o *historyOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Aggregate calls: count, ok/failed, avg/max latency, last call",
		Args:  noArgs,
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
	o.scanAll = true
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
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.allow(cmd, append(append([]string{}, filterFlags...), outFlags...)...); err != nil {
				return err
			}
			o.scanAll = true
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
		Args:  noArgs,
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
		Args:  noArgs,
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
		Args: noArgs,
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
	pf.StringVar(&o.output, "output", "", "결과를 파일로 저장 (새 파일만, 권한 0600). - 는 표준 출력")
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
	db, note, err := history.OpenNote(cmd.Context(), home)
	if err != nil {
		return err
	}
	defer db.Close()
	if note != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), note)
	}
	if o.scanAll && f.Since == nil {
		if n := history.LedgerSizeHint(cmd.Context(), db); n >= history.LargeLedgerRows {
			fmt.Fprintf(cmd.ErrOrStderr(), "알림: 기록이 많아(약 %d건) 기간 없이 집계하면 시간이 걸립니다. --since 7d 처럼 기간을 주면 빨라집니다.\n", n)
		}
	}
	rep, err := build(cmd.Context(), db, f, limit, loc)
	if err != nil {
		return err
	}
	if o.output == "" || o.output == "-" { // "-" = standard output, like most CLIs
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

func historyPruneCmd(o *historyOpts) *cobra.Command {
	var before string
	var yes, noBackup, asJSON bool
	var batch int
	cmd := &cobra.Command{
		Use:   "prune --before <기간|날짜> [--yes] [--no-backup] [--json]",
		Short: "Delete ledger rows older than a period (dry-run unless --yes)",
		Long: `원장(호출 이력)에서 --before 이전의 기록을 지웁니다. 되돌릴 수 없으므로 기본은 지울 행 수만 보여 주는 dry-run 이고,
--yes 를 줄 때만 지웁니다.

  forge history prune --before 180d            # dry-run: 지울 행 수·기간만 표시
  forge history prune --before 2026-01-01 --yes

--before: 180d 24h 2w (지금부터 그만큼 전), 2026-01-01 (로컬 날짜 00:00), RFC3339. 그 시각 이전(제외)의 기록이 대상입니다.
보호: 가장 최근 1일의 기록은 지울 수 없습니다(--before 가 1일보다 최근이면 거부).
병합 규칙(ADR-F0002/F0003): hook 과 gateway 가 같은 호출을 기록한 쌍은 history 에서 1건으로 보이므로 항상 같이 지웁니다.
한쪽이 기준 시각 이후라면 쌍 전체를 남기므로 기준 시각 이후의 행은 지워지지 않고 호출의 한쪽만 남는 일도 없습니다.
함께 지워지는 것: 그 행의 payload(ledger_payloads). 지우지 않는 것: sessions, llm_usage, 레지스트리.
백업: --yes 는 먼저 DB 전체 사본을 <FORGE_HOME>/backups/forge-pre-prune-<시각>.db 에 만듭니다(복원: forge 를 쓰는 프로세스가 없을 때
그 파일을 forge.db 로 복사). 디스크가 부족하거나 직접 내보냈다면 --no-backup. 먼저 내보내기: forge history --format csv --output ledger.csv --until <같은 값>.
큰 원장은 5000행씩 나눠 지우며(중단되면 같은 명령을 다시 실행), 지운 뒤에도 파일 크기는 줄지 않습니다.
공간을 돌려받으려면 forge 를 쓰는 프로세스가 없을 때 sqlite3 <FORGE_HOME>/forge.db 'VACUUM' 을 직접 실행하세요(임시로 DB 크기만큼 디스크가 더 필요).`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.allow(cmd, "before", "yes", "no-backup", "json", "batch"); err != nil {
				return err
			}
			if before == "" {
				return errors.New("--before 가 필요합니다 (예: --before 180d, --before 2026-01-01)")
			}
			if noBackup && !yes {
				return errors.New("--no-backup 은 --yes 와 함께만 쓸 수 있습니다")
			}
			if batch < 0 || batch > 1_000_000 {
				return errors.New("--batch 는 1~1000000 입니다")
			}
			cutoff, err := history.ParseTime("before", before, time.Local)
			if err != nil {
				return err
			}
			if err := history.CheckPruneCutoff(cutoff); err != nil {
				return err
			}
			home, err := store.Home()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var plan history.PrunePlan
			var backup string
			if !yes {
				db, note, err := history.OpenNote(cmd.Context(), home)
				if err != nil {
					return err
				}
				defer db.Close()
				if note != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), note)
				}
				if plan, err = history.PrunePreview(cmd.Context(), db, cutoff); err != nil {
					return err
				}
			} else {
				if _, err := os.Stat(store.DBPath(home)); err != nil {
					return fmt.Errorf("%w (%s)", history.ErrNotInitialized, store.DBPath(home))
				}
				_, db, err := openRegistry(cmd.Context())
				if err != nil {
					return err
				}
				defer db.Close()
				if plan, err = history.PrunePreview(cmd.Context(), db, cutoff); err != nil {
					return err
				}
				if plan.Rows > 0 && !noBackup {
					if backup, err = history.BackupBeforePrune(cmd.Context(), db, home); err != nil {
						return fmt.Errorf("%w (아무것도 지우지 않았습니다. 백업 없이 지우려면 --no-backup)", err)
					}
				}
				deleted, perr := history.Prune(cmd.Context(), db, cutoff, batch)
				plan.Rows, plan.LinkedRows = deleted, deleted-plan.Calls
				if perr != nil {
					return perr
				}
				if plan.Rows > 0 {
					_, _ = db.ExecContext(cmd.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
				}
			}
			if asJSON {
				return writeJSON(out, map[string]any{"schema": 1, "dry_run": !yes, "before": time.UnixMilli(cutoff).UTC().Format(time.RFC3339), "backup": backup, "plan": plan})
			}
			when := time.UnixMilli(cutoff).Local().Format("2006-01-02 15:04")
			if plan.Rows == 0 {
				fmt.Fprintf(out, "%s 이전에 지울 기록이 없습니다.", when)
				if plan.Kept > 0 {
					fmt.Fprintf(out, " (호출 쌍이 기준 시각에 걸려 남기는 행 %d)", plan.Kept)
				}
				fmt.Fprintln(out)
				return nil
			}
			verb := "지울 대상 (dry-run: 아무것도 지우지 않았습니다)"
			if yes {
				verb = "지웠습니다"
			}
			fmt.Fprintf(out, "%s 이전의 원장 기록 %s:\n", when, verb)
			fmt.Fprintf(out, "  행 %d개 (병합된 호출 %d건; hook+gateway 쌍으로 함께 지우는 행 %d)\n", plan.Rows, plan.Calls, plan.LinkedRows)
			fmt.Fprintf(out, "  기간: %s ~ %s\n", time.UnixMilli(plan.OldestMS).Local().Format("2006-01-02 15:04"), time.UnixMilli(plan.NewestMS).Local().Format("2006-01-02 15:04"))
			fmt.Fprintf(out, "  함께 지우는 payload 행: %d\n", plan.Payloads)
			if plan.Kept > 0 {
				fmt.Fprintf(out, "  기준 시각에 걸친 호출 쌍의 행 %d개는 쌍을 유지하려고 남깁니다\n", plan.Kept)
			}
			if yes {
				if backup != "" {
					fmt.Fprintf(out, "  백업: %s (복원: forge 프로세스를 멈춘 뒤 이 파일을 forge.db 로 복사)\n", textsafe.Display(backup, 300))
				}
				fmt.Fprintf(out, "  파일 크기는 줄지 않습니다. 공간을 돌려받으려면 forge 를 쓰는 프로세스가 없을 때: sqlite3 %s 'VACUUM'\n", textsafe.Display(store.DBPath(home), 300))
			} else {
				fmt.Fprintf(out, "실제로 지우려면: forge history prune --before %s --yes\n", shellQuote(before))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&before, "before", "", "이 시각 이전의 기록을 지움: 180d|24h|2w|2026-01-01|RFC3339 (최근 1일은 보호)")
	f.BoolVar(&yes, "yes", false, "실제로 지움 (기본은 dry-run)")
	f.BoolVar(&noBackup, "no-backup", false, "삭제 전 DB 백업을 만들지 않음 (--yes 와 함께)")
	f.BoolVar(&asJSON, "json", false, "JSON 출력")
	f.IntVar(&batch, "batch", 0, "한 트랜잭션에서 지울 행 수 (기본 5000)")
	_ = cmd.Flags().MarkHidden("batch")
	return cmd
}
