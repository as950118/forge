package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/adapter/claudecode"
	"github.com/as950118/forge/internal/adapter/opencode"
	"github.com/as950118/forge/internal/clientver"
	"github.com/as950118/forge/internal/profile"
	"github.com/as950118/forge/internal/store"
	"github.com/as950118/forge/internal/textsafe"
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
const supportedTargets = "claude-code, opencode, all"

type syncFlags struct {
	target, scope, projectDir, bin   string
	dryRun, diff, apply, asJSON      bool
	noHooks, removeHooks             bool
	forceManaged, skipConfl          bool
	listBackups, pruneBackups, force bool
	checkVersion, allowUntested      bool
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
  forge sync --target all                               # claude-code + opencode 를 하나의 통합 계획으로
  forge sync --target all --apply                       # 순차 적용 (한쪽 실패 시 부분 성공을 표시)
  forge sync --target claude-code --check-version       # 계획과 함께 클라이언트 --version 확인(옵트인)
  forge sync --list-backups
  forge sync --restore latest [--dry-run] [--force]
  forge sync --prune-backups [--keep 20] [--dry-run]

Profiles: the active profile for (client, project) decides what is rendered (an allow
list); without one every enabled item is (docs/profile.md). Version gate: --apply runs
"<client> --version" (nothing else) and stops on an unverified client version unless
--allow-untested-version is given; a plan does that only with --check-version.

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
	fl.StringVar(&f.target, "target", "", "대상 클라이언트: claude-code | opencode | all (두 클라이언트를 하나의 통합 계획으로)")
	fl.BoolVar(&f.checkVersion, "check-version", false, "(계획용) 클라이언트 바이너리의 --version 을 실행해 검증된 버전인지 표시 (--apply 는 항상 확인)")
	fl.BoolVar(&f.allowUntested, "allow-untested-version", false, "--apply: 검증되지 않은 클라이언트 버전이어도 계속 진행")
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
		if f.apply || f.target != "" || f.diff || f.skipConfl || f.forceManaged || f.noHooks || f.removeHooks || f.checkVersion || f.allowUntested {
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

// syncTargetNames returns the adapters a --target value selects, in apply order.
// "all" is claude-code first: OpenCode also reads .claude/skills, so the skills
// claude-code manages are claimed before the opencode plan is made.
func syncTargetNames(target string) []string {
	if target == "all" {
		return []string{claudecode.Name, opencode.Name}
	}
	return []string{target}
}

// versionChecker builds the client version checker (tests replace it with a fake
// runner: they never execute a real claude/opencode).
var versionChecker = func(projectDirs []string) *clientver.Checker {
	return &clientver.Checker{OSPath: true, ProjectDirs: projectDirs}
}

// beforeApply is a test seam: it runs just before each target of `--target all`
// is applied (never set in production).
var beforeApply func(target string)

// syncRun is one target's share of a sync run.
type syncRun struct {
	a       adapter.Adapter
	req     adapter.Request
	root    string
	in      adapter.Inputs
	plan    *adapter.Plan
	release func()
	res     *adapter.ApplyResult
	status  string // "" | applied | failed | not-applied
	err     error
}

func runSyncPlan(cmd *cobra.Command, home string, f syncFlags) error {
	if f.target == "" {
		return fmt.Errorf("--target 이 필요합니다 (지원: %s)", supportedTargets)
	}
	adapters := syncAdapters(home)
	if f.target != "all" {
		if _, ok := adapters[f.target]; !ok {
			return fmt.Errorf("알 수 없는 --target: %q (지원: %s)", textsafe.Display(f.target, 60), supportedTargets)
		}
	}
	if (f.noHooks || f.removeHooks) && f.target != claudecode.Name && f.target != "all" {
		return fmt.Errorf("--no-hooks/--remove-hooks 는 --target %s 에서 지원하지 않습니다 (%s 어댑터는 hook 을 다루지 않습니다; claude-code 전용)", f.target, f.target)
	}
	if f.allowUntested && !f.apply {
		return errors.New("--allow-untested-version 은 --apply 와 함께만 쓸 수 있습니다")
	}
	if f.checkVersion && f.apply {
		return errors.New("--check-version 은 계획용 옵션입니다 (--apply 는 항상 버전을 확인합니다)")
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
	names := syncTargetNames(f.target)
	var runs []*syncRun
	for _, n := range names {
		r := &syncRun{a: adapters[n]}
		r.req = adapter.Request{Scope: scope, ProjectDir: f.projectDir, Home: userHome, ForgeBin: f.bin,
			ForceManaged: f.forceManaged, BackupDir: filepath.Join(home, "backups")}
		if n == claudecode.Name { // hooks exist only for claude-code
			r.req.NoHooks, r.req.RemoveHooks = f.noHooks, f.removeHooks
		}
		root, err := lockRoot(r.a, r.req)
		if err != nil {
			return err
		}
		r.root = root
		runs = append(runs, r)
	}

	// Version gate: --apply runs "<client> --version" (only that) before anything
	// is written and stops on an unverified version unless --allow-untested-version.
	// A plan only does it with --check-version.
	var vnotes = map[string][]string{}
	if f.apply || f.checkVersion {
		var dirs []string
		if scope == adapter.ScopeProject {
			for _, r := range runs {
				dirs = append(dirs, r.root)
			}
		}
		chk := versionChecker(dirs)
		var blocked, unsafe []string
		for _, r := range runs {
			spec, ok := clientver.SpecFor(r.a.Name())
			if !ok {
				continue
			}
			res := chk.Check(ctx, spec)
			vnotes[r.a.Name()] = append(vnotes[r.a.Name()], versionNote(res))
			if res.Status == clientver.StatusWarn {
				blocked = append(blocked, fmt.Sprintf("%s: %s", r.a.Name(), res.Message))
			}
			if res.Code == clientver.CodeUnsafeLoc {
				unsafe = append(unsafe, fmt.Sprintf("%s: %s", r.a.Name(), res.Message))
			}
		}
		// A client binary that other users can modify is never run and --apply is
		// never allowed to go on: --allow-untested-version accepts an unknown VERSION,
		// not a program another account could have planted.
		if f.apply && len(unsafe) > 0 {
			return fmt.Errorf("클라이언트 바이너리 위치가 안전하지 않아 아무것도 쓰지 않고 중단합니다 (--allow-untested-version 으로 우회할 수 없습니다):\n  %s\n점검: forge doctor",
				strings.Join(unsafe, "\n  "))
		}
		if f.apply && len(blocked) > 0 && !f.allowUntested {
			return fmt.Errorf("클라이언트 버전을 확인하지 못해 아무것도 쓰지 않고 중단합니다:\n  %s\n"+
				"그래도 적용하려면 --allow-untested-version 을 지정하세요 (생성한 설정이 올바르게 로드되지 않을 수 있습니다). 점검: forge doctor",
				strings.Join(blocked, "\n  "))
		}
	}

	if f.apply {
		// Plan and apply happen under the locks (always claude-code then opencode):
		// a concurrent sync waits and then sees the finished result instead of racing it.
		for _, r := range runs {
			release, err := adapter.AcquireLock(filepath.Join(home, "locks"), adapter.LockKey(r.a.Name(), scope, r.root), syncLockTimeout)
			if err != nil {
				return err
			}
			defer release()
		}
	} else {
		for _, r := range runs {
			// A plan does not take the lock (it writes nothing). If a sync is running
			// right now its journal is not an interruption.
			if adapter.LockHeld(filepath.Join(home, "locks"), adapter.LockKey(r.a.Name(), scope, r.root)) {
				r.req.SyncInProgress = true
			}
		}
	}
	claimed := map[string]bool{}
	for _, r := range runs {
		project := ""
		if scope == adapter.ScopeProject {
			project = r.root
		}
		in, err := adapter.LoadInputsFor(ctx, home, r.a.Name(), project)
		if err != nil {
			return err
		}
		r.in = in
		if r.a.Name() == opencode.Name && len(names) > 1 {
			r.req.ClaimedSkills = claimed
		}
		plan, err := r.a.Plan(ctx, in, r.req)
		if err != nil {
			if len(runs) > 1 {
				return fmt.Errorf("%s 계획 실패 (아무것도 쓰지 않았습니다): %w", r.a.Name(), err)
			}
			return err
		}
		plan.SkipConflicts = f.skipConfl
		plan.Notes = append([]string{in.Profile.Describe(r.a.Name()) + scopeNote(scope, in.Profile)}, plan.Notes...)
		plan.Notes = append(plan.Notes, vnotes[r.a.Name()]...)
		if f.diff {
			adapter.BuildDiffs(plan)
		}
		r.plan = plan
		if plan.NewState != nil && r.a.Name() == claudecode.Name {
			for _, it := range plan.NewState.Items {
				if it.Kind == adapter.KindSkill {
					claimed[it.Name] = true
				}
			}
		}
	}

	if len(runs) == 1 {
		return runSingle(cmd, home, f, runs[0])
	}
	return runAll(cmd, home, f, runs)
}

func scopeNote(scope adapter.Scope, r *profile.Resolved) string {
	if scope == adapter.ScopeUser {
		return " [--scope user: 모든 프로젝트에 적용되므로 전역 활성 프로필만 따릅니다]"
	}
	return ""
}

func versionNote(r clientver.Result) string {
	v := r.Version
	if v == "" {
		v = "-"
	}
	return fmt.Sprintf("클라이언트 버전 %s: %s [%s] %s", r.Client, v, r.Status, r.Message)
}

func runSingle(cmd *cobra.Command, home string, f syncFlags, r *syncRun) error {
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	a, plan := r.a, r.plan
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
				adapter.WriteTextAs(out, plan, nil, f.diff, adapter.ModeApplyNotApplied)
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

// allReport is the JSON document of `forge sync --target all --json`.
type allReport struct {
	Schema    int    `json:"schema"`
	Target    string `json:"target"`
	Mode      string `json:"mode"` // plan | apply
	Conflicts int    `json:"conflicts"`
	// Applied lists the targets that were written; Partial is true when some but
	// not all targets were (see Targets[].status and Restore).
	Applied []string    `json:"applied"`
	Partial bool        `json:"partial"`
	Targets []allTarget `json:"targets"`
	Restore []string    `json:"restore,omitempty"`
}

type allTarget struct {
	Target string          `json:"target"`
	Status string          `json:"status"` // planned | applied | failed | not-applied
	Error  string          `json:"error,omitempty"`
	Report *adapter.Report `json:"report"`
}

// runAll shows the two plans as one unified plan and, with --apply, applies them
// one after the other. Each adapter is atomic on its own (backup, journal,
// rollback); the pair is NOT one transaction: a failure of the second leaves the
// first applied and says so. Conflicts anywhere mean nothing is written (unless
// --skip-conflicts), decided after BOTH plans are computed.
func runAll(cmd *cobra.Command, home string, f syncFlags, runs []*syncRun) error {
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	total := 0
	for _, r := range runs {
		total += r.plan.Conflicts()
	}
	rep := &allReport{Schema: 1, Target: "all", Mode: "plan", Conflicts: total, Applied: []string{}}
	var applyErr error
	if f.apply {
		rep.Mode = "apply"
		switch {
		case total > 0 && !f.skipConfl:
			applyErr = fmt.Errorf("%w (두 클라이언트 합쳐 충돌 %d건; --skip-conflicts 로 충돌 항목을 제외하고 적용할 수 있습니다)", adapter.ErrConflicts, total)
			for _, r := range runs {
				r.status = "not-applied"
			}
		default:
			for i, r := range runs {
				if beforeApply != nil {
					beforeApply(r.a.Name())
				}
				res, err := adapter.Apply(ctx, r.a, r.plan, adapter.ApplyOptions{BackupDir: filepath.Join(home, "backups")})
				if err != nil {
					r.status, r.err = "failed", err
					for _, later := range runs[i+1:] {
						later.status = "not-applied"
					}
					applyErr = err
					break
				}
				r.res, r.status = res, "applied"
				rep.Applied = append(rep.Applied, r.a.Name())
			}
		}
		rep.Partial = len(rep.Applied) > 0 && len(rep.Applied) < len(runs)
		if rep.Partial {
			for _, r := range runs {
				if r.status == "applied" && r.res != nil && r.res.BackupID != "" {
					rep.Restore = append(rep.Restore, "forge sync --restore "+r.res.BackupID)
				}
			}
		}
	}
	for _, r := range runs {
		t := allTarget{Target: r.a.Name(), Status: "planned", Report: adapter.BuildReport(r.plan, r.res, f.diff)}
		if f.apply {
			t.Status = r.status
		}
		if r.err != nil {
			t.Error = textsafe.Display(r.err.Error(), 300)
		}
		rep.Targets = append(rep.Targets, t)
	}
	if f.asJSON {
		if err := writeJSON(out, rep); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "forge sync --target all: %s 의 통합 %s\n", runs[0].plan.Scope, map[bool]string{false: "계획", true: "적용"}[f.apply])
		for _, r := range runs {
			fmt.Fprintf(out, "\n=== %s ===\n", r.a.Name())
			mode := ""
			if f.apply && r.res == nil {
				mode = adapter.ModeApplyNotApplied
				if r.status == "failed" {
					mode = adapter.ModeApplyFailed
				}
			}
			adapter.WriteTextAs(out, r.plan, r.res, f.diff, mode)
		}
		fmt.Fprintf(out, "\n=== 통합 요약 ===\n충돌 %d건\n", total)
		if f.apply {
			for _, r := range runs {
				fmt.Fprintf(out, "  %s: %s\n", r.a.Name(), map[string]string{"applied": "적용됨", "failed": "실패", "not-applied": "적용하지 않음"}[r.status])
			}
			if rep.Partial {
				fmt.Fprintf(out, "부분 성공: %s 만 적용되었고 나머지는 적용되지 않았습니다. 각 클라이언트는 자체적으로 원자적이지만 둘을 하나로 묶지는 않습니다.\n", strings.Join(rep.Applied, ", "))
				fmt.Fprintln(out, "다시 시도: forge sync --target all --apply (이미 적용된 쪽은 변경 없음). 적용된 쪽을 되돌리려면:")
				for _, c := range rep.Restore {
					fmt.Fprintf(out, "  %s\n", c)
				}
			}
		} else if total > 0 {
			fmt.Fprintln(out, "--apply 는 두 클라이언트 중 어느 쪽이든 충돌이 있으면 아무것도 쓰지 않습니다.")
		}
	}
	if applyErr != nil {
		if rep.Partial {
			return fmt.Errorf("일부만 적용되었습니다: %w", applyErr)
		}
		return applyErr
	}
	if f.apply {
		if n := backupCount(home); n > adapter.DefaultKeepBackups && !f.asJSON {
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
