// Package doctor implements `forge doctor`: a read-only health check of the
// Forge home, the client installations, sync drift, broken paths, history and
// profiles. It never changes anything: every finding that has a remedy carries
// the exact command for the user to run.
//
// Safety design (docs/doctor.md):
//   - Read-only. The database is opened read-only and, when no WAL content
//     exists, "immutable" so that not even the -wal/-shm helper files are
//     created. Sync drift is computed with the adapters' Plan (which writes
//     nothing). Nothing under the Forge home or the client directories is
//     created, changed or removed.
//   - The only program that is ever executed is "<client> --version" (package
//     clientver); a command found in a client config is only looked up in PATH.
//   - ~/.claude.json and the OpenCode secret/config files are never opened; the
//     path guard of the sync code (adapter.GuardPath) stays in front of every
//     file read, and no other user-level file is touched.
//   - Every check is independent: a check that cannot run reports a warn
//     ("점검 불가") and the others continue. The whole run has a time budget and
//     every list in the output is capped.
package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/clientver"
	"github.com/heonjinjeong/forge/internal/profile"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// Severity of a check.
type Severity string

// Severities (ascending).
const (
	OK   Severity = "ok"
	Info Severity = "info"
	Warn Severity = "warn"
	Fail Severity = "fail"
)

// Check is one finding. Fix holds exact commands for the user to run.
type Check struct {
	ID      string   `json:"id"`
	Group   string   `json:"group"`
	Title   string   `json:"title"`
	Status  Severity `json:"status"`
	Message string   `json:"message"`
	Cause   string   `json:"cause,omitempty"`
	Fix     []string `json:"fix,omitempty"`
}

// Report is the stable JSON document of `forge doctor --json` (schema 1).
type Report struct {
	Schema     int            `json:"schema"`
	Home       string         `json:"home"`
	Project    string         `json:"project,omitempty"`
	Scope      string         `json:"scope"`
	Strict     bool           `json:"strict"`
	ExitCode   int            `json:"exit_code"`
	Summary    map[string]int `json:"summary"`
	Checks     []Check        `json:"checks"`
	DurationMS int64          `json:"duration_ms"`
	Truncated  bool           `json:"truncated,omitempty"`
}

// Options configures a run.
type Options struct {
	Home       string // Forge home ($FORGE_HOME or ~/.forge)
	UserHome   string // the user's HOME (user scope root)
	ProjectDir string // project scope starts here (default ".")
	Scope      string // project | user | all
	Strict     bool
	// Exe is the running forge binary (for the gateway path check); "" = os.Executable.
	Exe     string
	Checker *clientver.Checker
	// HookDays is the window of the hook-activity check (default 7).
	HookDays int
	// Timeout is the budget of the whole run (default 30s).
	Timeout time.Duration
	Now     func() time.Time
}

// Limits.
const (
	maxListed       = 8 // names listed in one message
	maxFixes        = 6 // commands per check
	defaultTimeout  = 30 * time.Second
	quickCheckLimit = 8 * time.Second
)

type env struct {
	o     Options
	ctx   context.Context
	now   time.Time
	exe   string
	scope []adapter.Scope
	// project is the canonical project root ("" when it cannot be resolved).
	project    string
	projectErr error
	db         *sql.DB
	dbErr      error
	dbState    string // "" | "missing" | "unreadable"
	plans      map[string]*planned
	// inexact holds the quoted paths that q() could not reproduce exactly (control
	// or invisible characters shown as escapes, or a cut path): a command that
	// contains one must not be pasted blindly (see noteInexact).
	inexact map[string]bool
}

func (e *env) q(path string) string {
	for _, r := range path {
		if textsafe.NeedsEscape(r) {
			out := "'" + strings.ReplaceAll(textsafe.Display(path, 300), "'", `'\''`) + "'"
			if e.inexact == nil {
				e.inexact = map[string]bool{}
			}
			e.inexact[out] = true
			return out
		}
	}
	return adapter.ShellQuote(path)
}

// inexactNote is appended to a check whose fix commands name a path that is shown
// with visible escapes (AC-3 of TASK-0024). The escapes are safe to print but are
// literal text in a shell, so the command would act on a different path.
const inexactNote = "# 주의: 위 명령의 경로에는 제어·비가시 문자가 있어 \\x1b, \\u202e 같은 보이는 이스케이프로 표시했습니다. 그대로 붙여 넣으면 실제 경로와 다른 경로를 가리킵니다. ls -b 등으로 실제 이름을 확인한 뒤 직접 입력하세요"

// noteInexact adds inexactNote to every check with such a command (before the
// fix list is capped, so the note is never cut off by the cap's "생략" line).
func (e *env) noteInexact(c *Check) {
	if len(e.inexact) == 0 {
		return
	}
	for _, f := range c.Fix {
		if f == inexactNote {
			return // already added by the check itself (checks_clients.go)
		}
	}
	for _, f := range c.Fix {
		for q := range e.inexact {
			if strings.Contains(f, q) {
				if len(c.Fix) >= maxFixes {
					c.Fix = c.Fix[:maxFixes-1]
				}
				c.Fix = append(c.Fix, inexactNote)
				return
			}
		}
	}
}

// projFlag is " --project-dir <root>" for fix commands of project-scoped
// operations (sync), so they act on the project doctor looked at, not the cwd.
func (e *env) projFlag() string {
	if e.project == "" {
		return ""
	}
	return " --project-dir " + e.q(e.project)
}

func safe(s string, n int) string { return textsafe.Display(s, n) }

// Run executes every check and returns the report. It never returns an error:
// a check that cannot run is itself a finding.
func Run(ctx context.Context, o Options) *Report {
	start := time.Now()
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.HookDays <= 0 {
		o.HookDays = 7
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Scope == "" {
		o.Scope = "project"
	}
	if o.Checker == nil {
		o.Checker = &clientver.Checker{OSPath: true}
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	e := &env{o: o, ctx: ctx, now: o.Now(), plans: map[string]*planned{}}
	e.exe = o.Exe
	if e.exe == "" {
		e.exe, _ = os.Executable()
	}
	if r, err := filepath.EvalSymlinks(e.exe); err == nil && e.exe != "" {
		e.exe = r
	}
	switch o.Scope {
	case "project":
		e.scope = []adapter.Scope{adapter.ScopeProject}
	case "user":
		e.scope = []adapter.Scope{adapter.ScopeUser}
	default:
		e.scope = []adapter.Scope{adapter.ScopeProject, adapter.ScopeUser}
	}
	e.project, e.projectErr = profile.ProjectRoot(o.ProjectDir)
	if e.projectErr != nil {
		e.project = ""
	}
	rep := &Report{Schema: 1, Home: safe(o.Home, 300), Project: safe(e.project, 300), Scope: o.Scope, Strict: o.Strict,
		Summary: map[string]int{"ok": 0, "info": 0, "warn": 0, "fail": 0}, Checks: []Check{}}

	steps := []struct {
		name string
		fn   func(*env) []Check
	}{
		{"home", checkHome},
		{"database", checkDatabase},
		{"clients", checkClients},
		{"drift", checkDrift},
		{"paths", checkPaths},
		{"history", checkHistory},
		{"hygiene", checkHygiene},
		{"profile", checkProfile},
		{"security", checkSecurity},
	}
	for _, s := range steps {
		if ctx.Err() != nil {
			rep.Truncated = true
			rep.Checks = append(rep.Checks, Check{ID: s.name + ".skipped", Group: s.name, Title: s.name, Status: Warn,
				Message: "점검 불가: 전체 시간 제한(" + o.Timeout.String() + ")을 넘겨 건너뜀",
				Cause:   "큰 DB 나 설정 파일, 느린 파일 시스템", Fix: []string{"forge doctor --scope project   # 범위를 좁혀 다시 실행"}})
			continue
		}
		rep.Checks = append(rep.Checks, e.guarded(s.name, s.fn)...)
	}
	if e.db != nil {
		_ = e.db.Close()
	}
	for i := range rep.Checks {
		c := &rep.Checks[i]
		c.Message, c.Cause = clean(c.Message), clean(c.Cause)
		e.noteInexact(c)
		if len(c.Fix) > maxFixes {
			c.Fix = append(c.Fix[:maxFixes], "# … 외 "+fmt.Sprint(len(c.Fix)-maxFixes)+"개 생략")
		}
		rep.Summary[string(c.Status)]++
	}
	sort.SliceStable(rep.Checks, func(i, j int) bool { return groupRank(rep.Checks[i].Group) < groupRank(rep.Checks[j].Group) })
	switch {
	case rep.Summary["fail"] > 0:
		rep.ExitCode = 1
	case o.Strict && rep.Summary["warn"] > 0:
		rep.ExitCode = 1
	}
	rep.DurationMS = time.Since(start).Milliseconds()
	return rep
}

var groupOrder = []string{"home", "client", "drift", "paths", "history", "hygiene", "profile", "security"}

func groupRank(g string) int {
	for i, n := range groupOrder {
		if n == g {
			return i
		}
	}
	return len(groupOrder)
}

// clean makes a message safe for a terminal and bounded (checks build messages
// from file contents, names and paths that may be hostile).
func clean(s string) string {
	out, _ := textsafe.EscapeRaw(s, 900, false)
	return out
}

// guarded runs one group of checks; a panic or error inside is a "점검 불가" finding.
func (e *env) guarded(name string, fn func(*env) []Check) (out []Check) {
	defer func() {
		if r := recover(); r != nil {
			out = append(out, Check{ID: name + ".error", Group: name, Title: name, Status: Warn,
				Message: "점검 불가: 점검 중 예기치 않은 오류", Cause: safe(fmt.Sprint(r), 200)})
		}
	}()
	return fn(e)
}

func mk(id, group, title string, st Severity, msg, cause string, fix ...string) Check {
	return Check{ID: id, Group: group, Title: title, Status: st, Message: msg, Cause: cause, Fix: fix}
}

// unavailable is the finding of a check that could not run.
func unavailable(id, group, title string, err error) Check {
	return mk(id, group, title, Warn, "점검 불가: "+safe(err.Error(), 250), "점검 자체가 실패했습니다 (다른 점검은 계속했습니다)")
}

func listNames(names []string) string {
	sort.Strings(names)
	if len(names) <= maxListed {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:maxListed], ", ") + fmt.Sprintf(" 외 %d건", len(names)-maxListed)
}

func contextWithTimeout(e *env, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.ctx, d)
}
