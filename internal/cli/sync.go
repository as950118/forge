package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/adapter/claudecode"
	"github.com/heonjinjeong/forge/internal/adapter/opencode"
	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// syncLockTimeout is how long --apply / --restore wait for a concurrent sync.
var syncLockTimeout = 30 * time.Second

// syncAdapters returns the known adapters.
func syncAdapters(forgeHome string) map[string]adapter.Adapter {
	return map[string]adapter.Adapter{
		claudecode.Name: claudecode.New(forgeHome),
		opencode.Name:   opencode.New(forgeHome),
	}
}

// supportedTargets is the list shown in --target errors and help.
const supportedTargets = "claude-code, opencode"

type syncFlags struct {
	target, scope, projectDir, bin   string
	dryRun, diff, apply, asJSON      bool
	noHooks, removeHooks             bool
	forceManaged, skipConfl          bool
	listBackups, pruneBackups, force bool
	restore                          string
	keep                             int
}

func syncCmd() *cobra.Command {
	var f syncFlags
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Render the registry into client configs (plan by default; --apply writes)",
		Long: `Render the registry into a client's configuration.

By default sync only PRINTS a plan and writes nothing (same as --dry-run).
Files are written only with --apply, after everything that will change has been
backed up to <forge home>/backups. User-written items are never modified; an
item is Forge-managed only when it is recorded in the sidecar state file and its
content hash still matches.

  forge sync --target claude-code                       # plan
  forge sync --target claude-code --dry-run --diff      # plan with diffs
  forge sync --target claude-code --apply               # write
  forge sync --target claude-code --apply --no-hooks    # leave hooks alone (no install, update or removal)
  forge sync --target claude-code --apply --remove-hooks # remove the managed hooks (claude-code only)
  forge sync --target opencode --dry-run --diff         # OpenCode: skills + opencode.json "mcp" (no hooks)
  forge sync --target opencode --apply
  forge sync --list-backups
  forge sync --restore latest [--dry-run] [--force]
  forge sync --prune-backups [--keep 20] [--dry-run]

Targets: claude-code (skills, .mcp.json, hooks) and opencode (skills in
.opencode/skills, the "mcp" object of the project's opencode.json; no hooks).
Each target has its own sidecar state, so syncing both never touches the other's
items. Skills that claude-code already manages under .claude/skills are not
written again for opencode (OpenCode reads that directory too). Forge never runs
claude or opencode; after an opencode apply run "opencode reload" yourself.

If an earlier --apply was killed, the next plan says so and --apply finishes the
work (and removes leftover temporary files); files you edited meanwhile are kept
and reported as conflicts.

See docs/sync.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSync(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.target, "target", "", "대상 클라이언트: claude-code | opencode")
	fl.StringVar(&f.scope, "scope", "project", "project(기본: claude-code <repo>/.claude, .mcp.json | opencode <repo>/.opencode/skills, opencode.json) | user(claude-code ~/.claude/skills, settings.json | opencode ~/.config/opencode/skills; MCP 는 명령만 출력)")
	fl.StringVar(&f.projectDir, "project-dir", "", "프로젝트 디렉터리 (기본: 현재 디렉터리; 저장소 루트를 찾아 사용)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "계획만 출력하고 아무것도 쓰지 않음 (기본 동작과 동일)")
	fl.BoolVar(&f.diff, "diff", false, "파일별 통합 diff 표시 (비밀 값은 가림)")
	fl.BoolVar(&f.apply, "apply", false, "실제로 파일을 씀 (백업 후)")
	fl.BoolVar(&f.asJSON, "json", false, "JSON 출력")
	fl.StringVar(&f.bin, "bin", "forge", "설정 파일에 기록할 forge 명령 (이름 또는 절대 경로)")
	fl.BoolVar(&f.noHooks, "no-hooks", false, "hook 설치/갱신을 건너뜀 (이미 설치된 관리 hook 은 건드리지 않음; claude-code 전용)")
	fl.BoolVar(&f.removeHooks, "remove-hooks", false, "이미 설치된 관리 hook 을 제거 (--no-hooks 와 함께 쓸 수 없음; claude-code 전용)")
	fl.BoolVar(&f.forceManaged, "force-managed", false, "손으로 수정된 관리 항목을 덮어씀 (사용자 항목은 대상 아님, 백업됨)")
	fl.BoolVar(&f.skipConfl, "skip-conflicts", false, "충돌 항목만 제외하고 나머지를 적용 (기본: 충돌이 있으면 아무것도 쓰지 않음)")
	fl.BoolVar(&f.listBackups, "list-backups", false, "백업 목록")
	fl.StringVar(&f.restore, "restore", "", "백업 복원 (id 또는 latest)")
	fl.BoolVar(&f.force, "force", false, "--restore: 백업 이후 수정된 파일이 있어도 복원 (현재 상태는 새 백업으로 보존)")
	fl.BoolVar(&f.pruneBackups, "prune-backups", false, "오래된 백업 삭제 (이 명령으로만 삭제됨)")
	fl.IntVar(&f.keep, "keep", adapter.DefaultKeepBackups, "--prune-backups: 남길 백업 개수")
	return cmd
}

func runSync(cmd *cobra.Command, f syncFlags) error {
	modes := 0
	for _, b := range []bool{f.listBackups, f.restore != "", f.pruneBackups} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("--list-backups, --restore, --prune-backups 는 함께 쓸 수 없습니다")
	}
	if f.apply && f.dryRun {
		return errors.New("--apply 와 --dry-run 은 동시에 지정할 수 없습니다")
	}
	if f.noHooks && f.removeHooks {
		return errors.New("--no-hooks 와 --remove-hooks 는 동시에 지정할 수 없습니다")
	}
	home, err := store.Home()
	if err != nil {
		return err
	}
	if modes == 1 {
		if f.apply || f.target != "" || f.diff || f.skipConfl || f.forceManaged || f.noHooks || f.removeHooks {
			return errors.New("백업 명령(--list-backups/--restore/--prune-backups)에는 --apply, --target, --diff, --skip-conflicts, --force-managed, --no-hooks, --remove-hooks 를 쓸 수 없습니다")
		}
		switch {
		case f.listBackups:
			return runListBackups(cmd, home, f)
		case f.restore != "":
			return runRestore(cmd, home, f)
		default:
			return runPrune(cmd, home, f)
		}
	}
	if f.force || f.keep != adapter.DefaultKeepBackups {
		return errors.New("--force 는 --restore, --keep 은 --prune-backups 와 함께만 쓸 수 있습니다")
	}
	return runSyncPlan(cmd, home, f)
}

func runSyncPlan(cmd *cobra.Command, home string, f syncFlags) error {
	if f.target == "" {
		return fmt.Errorf("--target 이 필요합니다 (지원: %s)", supportedTargets)
	}
	a, ok := syncAdapters(home)[f.target]
	if !ok {
		return fmt.Errorf("알 수 없는 --target: %q (지원: %s)", textsafe.Display(f.target, 60), supportedTargets)
	}
	if (f.noHooks || f.removeHooks) && f.target != claudecode.Name {
		return fmt.Errorf("--no-hooks/--remove-hooks 는 --target %s 에서 지원하지 않습니다 (%s 어댑터는 hook 을 다루지 않습니다; claude-code 전용)", f.target, f.target)
	}
	scope := adapter.Scope(f.scope)
	if scope != adapter.ScopeProject && scope != adapter.ScopeUser {
		return fmt.Errorf("--scope 는 project|user 중 하나여야 합니다: %q", f.scope)
	}
	userHome, herr := os.UserHomeDir()
	if herr != nil && scope == adapter.ScopeUser {
		return fmt.Errorf("홈 디렉터리를 알 수 없습니다: %w", herr)
	}
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	req := adapter.Request{Scope: scope, ProjectDir: f.projectDir, Home: userHome, ForgeBin: f.bin,
		NoHooks: f.noHooks, RemoveHooks: f.removeHooks, ForceManaged: f.forceManaged, BackupDir: filepath.Join(home, "backups")}

	var release func()
	if f.apply {
		// Plan and apply happen under one lock: a concurrent sync waits and then
		// sees the finished result instead of racing it.
		root, err := lockRoot(a, req)
		if err != nil {
			return err
		}
		release, err = adapter.AcquireLock(filepath.Join(home, "locks"), adapter.LockKey(a.Name(), scope, root), syncLockTimeout)
		if err != nil {
			return err
		}
		defer release()
	} else if root, err := lockRoot(a, req); err == nil &&
		adapter.LockHeld(filepath.Join(home, "locks"), adapter.LockKey(a.Name(), scope, root)) {
		// A plan does not take the lock (it writes nothing). If a sync is running
		// right now its journal is not an interruption.
		req.SyncInProgress = true
	}
	in, err := adapter.LoadInputs(ctx, home, a.Name())
	if err != nil {
		return err
	}
	plan, err := a.Plan(ctx, in, req)
	if err != nil {
		return err
	}
	plan.SkipConflicts = f.skipConfl
	if f.diff {
		adapter.BuildDiffs(plan)
	}
	if !f.apply {
		if f.asJSON {
			return adapter.WriteJSON(out, adapter.BuildReport(plan, nil, f.diff))
		}
		adapter.WriteText(out, plan, nil, f.diff)
		return nil
	}
	res, err := adapter.Apply(ctx, a, plan, adapter.ApplyOptions{BackupDir: filepath.Join(home, "backups")})
	if err != nil {
		if errors.Is(err, adapter.ErrConflicts) {
			// Show the plan so the user can see the conflicts, then fail.
			if f.asJSON {
				_ = adapter.WriteJSON(out, adapter.BuildReport(plan, nil, f.diff))
			} else {
				adapter.WriteText(out, plan, nil, f.diff)
			}
		}
		return err
	}
	if f.asJSON {
		if err := adapter.WriteJSON(out, adapter.BuildReport(plan, res, f.diff)); err != nil {
			return err
		}
	} else {
		adapter.WriteText(out, plan, res, f.diff)
		if n := backupCount(home); n > adapter.DefaultKeepBackups {
			fmt.Fprintf(cmd.ErrOrStderr(), "참고: 백업이 %d개입니다 (권장 상한 %d). 자동으로 지우지 않습니다: forge sync --prune-backups\n", n, adapter.DefaultKeepBackups)
		}
	}
	return nil
}

// lockRoot resolves the root the plan will use, only to name the lock.
func lockRoot(a adapter.Adapter, req adapter.Request) (string, error) {
	type resolver interface {
		ResolveRoot(adapter.Request) (string, []string, error)
	}
	if r, ok := a.(resolver); ok {
		root, _, err := r.ResolveRoot(req)
		return root, err
	}
	return "", errors.New("어댑터가 루트를 해석하지 못했습니다")
}

func backupCount(home string) int {
	l, _ := adapter.ListBackups(filepath.Join(home, "backups"))
	return len(l)
}

func runListBackups(cmd *cobra.Command, home string, f syncFlags) error {
	list, err := adapter.ListBackups(filepath.Join(home, "backups"))
	if err != nil {
		return err
	}
	if f.asJSON {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"schema": 1, "keep_limit": adapter.DefaultKeepBackups, "backups": list})
	}
	adapter.WriteBackups(cmd.OutOrStdout(), list)
	return nil
}

func runRestore(cmd *cobra.Command, home string, f syncFlags) error {
	backupDir := filepath.Join(home, "backups")
	id, err := adapter.ResolveBackup(backupDir, f.restore)
	if err != nil {
		return err
	}
	m, _, err := adapter.LoadManifest(backupDir, id)
	if err != nil {
		return err
	}
	a, ok := syncAdapters(home)[m.Target]
	if !ok {
		return fmt.Errorf("이 백업의 대상(%q)을 처리할 어댑터가 없습니다", m.Target)
	}
	if !f.dryRun {
		release, err := adapter.AcquireLock(filepath.Join(home, "locks"), adapter.LockKey(m.Target, m.Scope, m.Root), syncLockTimeout)
		if err != nil {
			return err
		}
		defer release()
	}
	res, err := adapter.Restore(cmd.Context(), a, adapter.RestoreOptions{
		ApplyOptions: adapter.ApplyOptions{BackupDir: backupDir}, ID: id, DryRun: f.dryRun, Force: f.force})
	if res != nil {
		if f.asJSON {
			if jerr := writeJSON(cmd.OutOrStdout(), res); jerr != nil {
				return jerr
			}
		} else {
			adapter.WriteRestore(cmd.OutOrStdout(), res)
		}
	}
	return err
}

func runPrune(cmd *cobra.Command, home string, f syncFlags) error {
	removed, err := adapter.PruneBackups(filepath.Join(home, "backups"), f.keep, f.dryRun)
	if err != nil {
		return err
	}
	if f.asJSON {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"schema": 1, "dry_run": f.dryRun, "keep": f.keep, "removed": removed})
	}
	w := cmd.OutOrStdout()
	if len(removed) == 0 {
		fmt.Fprintf(w, "삭제할 백업이 없습니다 (보존 %d개 이하).\n", f.keep)
		return nil
	}
	verb := "삭제"
	if f.dryRun {
		verb = "삭제 예정 (--dry-run: 지우지 않음)"
	}
	fmt.Fprintf(w, "%s: %d개\n  %s\n", verb, len(removed), strings.Join(removed, "\n  "))
	return nil
}
