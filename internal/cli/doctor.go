package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/heonjinjeong/forge/internal/doctor"
	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

func doctorCmd() *cobra.Command {
	var (
		asJSON, strict bool
		scope, dir     string
		days           int
		timeout        time.Duration
	)
	cmd := &cobra.Command{
		Use:   "doctor [--scope project|user|all] [--project-dir <dir>] [--json] [--strict]",
		Short: "Check the Forge home, client installs, sync drift, broken paths, history and profiles (read-only except stale temp dirs)",
		Long: `Forge 의 상태를 점검합니다. Forge 홈·DB·클라이언트 설정은 읽기만 합니다: 아무것도 만들거나 고치거나 지우지 않고, 고칠 방법은 정확한 명령으로 안내만 합니다.
단 하나의 예외: 클라이언트 버전 확인(<client> --version)을 시작할 때 TMPDIR 의 죽은 forge 프로세스가 남긴 임시 디렉터리 forge-ver-<pid>-<난수> 를
(본인 소유, 1시간 넘게 오래됨, 그 pid 가 없을 때만, 정확한 경로) 지웁니다. 그 밖의 파일은 지우지 않습니다.

점검 항목: 홈·DB(권한, quick_check, 마이그레이션, WAL) / 클라이언트 탐지와 버전 게이팅(<client> --version 만 실행) /
sync 드리프트(미적용 변경, 충돌, 중단된 저널, 임시 파일, 백업 의존성, 링크 거부) / 깨진 경로·미연결(스킬 디렉터리,
MCP 명령 존재, 게이트웨이 경로, tools/list 캐시, 잔존 항목) / 이력 건강 / 위생(백업, 파일 권한) / 프로필.

심각도: ok, info(참고), warn(주의), fail(문제). 종료 코드: fail 이 있으면 1, 아니면 0 (--strict 는 warn 도 1).
~/.claude.json 과 OpenCode 의 비밀 파일은 열지 않습니다. 자세한 내용은 docs/doctor.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch scope {
			case "project", "user", "all":
			default:
				return fmt.Errorf("--scope 는 project|user|all 중 하나여야 합니다: %q", textsafe.Display(scope, 30))
			}
			home, err := store.Home()
			if err != nil {
				return err
			}
			userHome, _ := os.UserHomeDir()
			// versionChecker is the same constructor sync uses (tests replace it with a
			// fake runner: they never execute a real claude/opencode).
			o := doctor.Options{Home: home, UserHome: userHome, ProjectDir: dir, Scope: scope, Strict: strict,
				HookDays: days, Timeout: timeout, Checker: versionChecker(projectDirsFor(dir))}
			rep := doctor.Run(cmd.Context(), o)
			out := cmd.OutOrStdout()
			if asJSON {
				if err := writeJSON(out, rep); err != nil {
					return err
				}
			} else {
				writeDoctorText(out, rep)
			}
			if rep.ExitCode != 0 {
				if rep.Summary["fail"] > 0 {
					return fmt.Errorf("forge doctor: fail %d건", rep.Summary["fail"])
				}
				return fmt.Errorf("forge doctor --strict: warn %d건", rep.Summary["warn"])
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력 (schema 1)")
	cmd.Flags().BoolVar(&strict, "strict", false, "warn 도 종료 코드 1 로 처리")
	cmd.Flags().StringVar(&scope, "scope", "project", "project | user | all (sync 드리프트와 설정 점검 범위)")
	cmd.Flags().StringVar(&dir, "project-dir", "", "프로젝트 디렉터리 (기본: 현재 디렉터리; 저장소 루트를 찾아 사용)")
	cmd.Flags().IntVar(&days, "hook-days", 7, "hook 이벤트가 없으면 경고할 기간(일)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "전체 점검 시간 제한")
	return cmd
}

// projectDirsFor: directories whose binaries the version check refuses to run.
func projectDirsFor(dir string) []string {
	if dir == "" {
		dir = "."
	}
	return []string{dir}
}

var sevMark = map[doctor.Severity]string{doctor.OK: "[ ok ]", doctor.Info: "[info]", doctor.Warn: "[warn]", doctor.Fail: "[FAIL]"}

func writeDoctorText(w interface{ Write([]byte) (int, error) }, r *doctor.Report) {
	fmt.Fprintf(w, "forge doctor  home=%s  scope=%s", r.Home, r.Scope)
	if r.Project != "" {
		fmt.Fprintf(w, "  project=%s", r.Project)
	}
	fmt.Fprintln(w)
	group := ""
	for _, c := range r.Checks {
		if c.Group != group {
			group = c.Group
			label, ok := groupLabel[group]
			if !ok {
				label = group
			}
			fmt.Fprintf(w, "\n== %s ==\n", label)
		}
		fmt.Fprintf(w, "%s %s: %s\n", sevMark[c.Status], c.Title, c.Message)
		if c.Cause != "" && c.Status != doctor.OK {
			fmt.Fprintf(w, "       원인: %s\n", c.Cause)
		}
		for _, f := range c.Fix {
			fmt.Fprintf(w, "       해결: %s\n", f)
		}
	}
	fmt.Fprintf(w, "\n요약: ok %d · info %d · warn %d · fail %d  (%dms)\n", r.Summary["ok"], r.Summary["info"], r.Summary["warn"], r.Summary["fail"], r.DurationMS)
	switch {
	case r.Summary["fail"] > 0:
		fmt.Fprintln(w, "fail 항목이 있어 종료 코드 1 입니다.")
	case r.ExitCode != 0:
		fmt.Fprintln(w, "--strict: warn 항목이 있어 종료 코드 1 입니다.")
	}
}

var groupLabel = map[string]string{
	"home": "홈·DB", "client": "클라이언트", "drift": "sync 드리프트", "paths": "경로·연결",
	"history": "이력", "hygiene": "위생", "profile": "프로필", "security": "보안",
}
