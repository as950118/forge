package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/as950118/forge/internal/export"
	"github.com/as950118/forge/internal/history"
	"github.com/as950118/forge/internal/store"
	"github.com/as950118/forge/internal/textsafe"
)

func exportCmd(version string) *cobra.Command {
	var format, output string
	var toPG, yes, force bool
	cmd := &cobra.Command{
		Use:   "export (--format json | --to-postgres [--yes])",
		Short: "Export an aggregate-only snapshot (JSON or Postgres)",
		Long: `로컬 원장에서 "공개해도 되는 집계"만 뽑아 내보냅니다. forge.db 는 읽기 전용으로만 열며 수정하지 않습니다.

  forge export --format json [--output 파일 [--force]]   스냅샷을 JSON 으로 출력
  forge export --to-postgres                              dry-run: 대상(비밀번호 마스킹)과 행 수만 표시, 연결하지 않음
  forge export --to-postgres --yes                        Postgres 스키마 forge 의 내용을 한 트랜잭션으로 전체 교체

내용: 스킬·도구·MCP 서버/도구 이름, 종류, 호출 횟수, 성공/실패 수, 성공률, 최근 호출 시각, 일별(UTC) 집계, 생성 시각, Forge 버전.
포함하지 않는 것: 명령줄, 경로, 인자, 환경변수, 헤더, 세션, 프로젝트 경로, 클라이언트, 입출력 본문, 오류 메시지.
이름의 제어·비가시 문자는 \uXXXX 로 바뀝니다. 이름 자체는 공개되므로 비밀이 들어간 이름은 쓰지 마세요.

Postgres: 환경변수 FORGE_EXPORT_URL(postgresql://사용자:비밀번호@호스트:5432/postgres)에 Supabase 의 Session pooler 연결
문자열을 넣습니다. 포트 6543(transaction pooler)은 거부합니다. 선택 FORGE_PG_CA 에 서버 CA PEM 을 넣으면 인증서를 검증합니다.
URL 의 비밀번호는 출력과 오류에서 가려집니다.`,
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if toPG == (format != "") {
				return fmt.Errorf("--format json 과 --to-postgres 중 정확히 하나를 고르세요 (forge export --help)")
			}
			if format != "" && format != "json" {
				return fmt.Errorf("--format 값 %q 은(는) 지원하지 않습니다 (json)", shownArg(format))
			}
			if yes && !toPG {
				return fmt.Errorf("--yes 는 --to-postgres 에서만 쓸 수 있습니다")
			}
			if (output != "" || force) && toPG {
				return fmt.Errorf("--output/--force 는 --format json 에서만 쓸 수 있습니다")
			}
			var tgt *export.Target
			if toPG { // validate before touching the database
				t, err := export.ParseTarget(os.Getenv(export.EnvURL), os.Getenv(export.EnvCA))
				if err != nil {
					return err
				}
				tgt = t
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
			snap, err := export.Build(cmd.Context(), db, time.Now(), version)
			if err != nil {
				return err
			}
			if !toPG {
				data, err := json.MarshalIndent(snap, "", "  ")
				if err != nil {
					return err
				}
				data = append(data, '\n')
				if output == "" || output == "-" {
					_, err = cmd.OutOrStdout().Write(data)
					return err
				}
				dbp := store.DBPath(home)
				if err := history.WriteFile(output, data, force, dbp, dbp+"-wal", dbp+"-shm"); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "저장했습니다: %s (json, 항목 %d, 일별 %d행)\n", textsafe.Display(output, 300), len(snap.Items), len(snap.Daily))
				return nil
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "대상: %s (스키마 %s)\n내용: 항목 %d, 일별 %d행 (일 단위는 UTC)\n", tgt.Masked, export.SchemaName, len(snap.Items), len(snap.Daily))
			if !yes {
				fmt.Fprintln(out, "dry-run: 연결하지 않았고 아무것도 기록하지 않았습니다. 기록하려면 --yes 를 주세요 (스키마 forge 의 기존 내용은 전체 교체됩니다).")
				return nil
			}
			if err := export.Write(cmd.Context(), tgt, snap); err != nil {
				return err
			}
			fmt.Fprintln(out, "기록했습니다: 스키마 forge 의 meta·items·daily 를 한 트랜잭션으로 교체했습니다.")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&format, "format", "", "json")
	f.BoolVar(&toPG, "to-postgres", false, "Postgres(FORGE_EXPORT_URL)로 내보내기 (기본 dry-run)")
	f.BoolVar(&yes, "yes", false, "--to-postgres 에서 실제로 기록")
	f.StringVar(&output, "output", "", "JSON 을 파일로 저장 (새 파일만, 권한 0600). - 는 표준 출력")
	f.BoolVar(&force, "force", false, "--output 대상이 일반 파일이면 덮어쓰기 (심볼릭 링크는 항상 거부)")
	return cmd
}
