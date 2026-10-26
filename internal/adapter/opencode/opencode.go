// Package opencode renders the Forge registry into OpenCode configuration:
// skills (<scope>/.opencode/skills/<name>/) and MCP servers (the top-level "mcp"
// object of the project's opencode.json). It only builds adapter.Plan values; all
// writing, backup, journal and rollback is done by the adapter engine, so every
// safety rule of the Claude Code adapter (docs/sync.md) holds here too.
//
// What is deliberately NOT done (docs/clients/opencode.md, docs/sync.md):
//   - The OpenCode binary is never executed: formats come from the documented
//     [doc]/[bin]/[help]/[local] findings only; nothing [미확인] is guessed.
//   - Hooks/plugins are out of scope (--no-hooks / --remove-hooks are errors).
//   - The user-level OpenCode config (~/.config/opencode/opencode.json[c],
//     config.json), service.json, mcp-auth.json, opencode.db and credential files
//     are never read or written (adapter.GuardPath); user-scope MCP servers are
//     printed as `opencode mcp add --global` commands only.
//   - opencode.jsonc (comments) is not supported: comments cannot be preserved.
//
// Verified format versions: see SupportedVersions and testdata/.
package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/adapter/claudecode"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// Name is the --target value (and the Forge client id used for MCP exposure).
const Name = "opencode"

// SupportedVersions lists the OpenCode versions the formats were documented
// against (docs/clients/opencode.md). Fixtures live in testdata/<version>/.
// Sync never runs the opencode binary to check the installed version.
var SupportedVersions = []string{"2.0.24"}

// GatewayServerName is the opencode.json "mcp" entry that points at `forge mcp serve`.
const GatewayServerName = "forge"

// ConfigFile is the only config file this adapter writes (project scope).
const ConfigFile = "opencode.json"

// JSONCFile is detected but never written or read (comments cannot be preserved).
const JSONCFile = "opencode.jsonc"

// Adapter implements adapter.Adapter for OpenCode.
type Adapter struct {
	// ForgeHome holds the user-scope state file (<ForgeHome>/sync-state/).
	ForgeHome string
}

// New returns the adapter for the given Forge home directory.
func New(forgeHome string) *Adapter { return &Adapter{ForgeHome: forgeHome} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return Name }

type paths struct {
	adapter.Paths
	configJSON string // project scope only
	configJC   string // project scope only; looked at (Lstat), never read or written
	// skillsNote tells where the user-scope skills go when that is not the default
	// (XDG_CONFIG_HOME) or why an XDG value was ignored (plan note).
	skillsNote string
}

// paths names the files of a scope. State and journal use their own file names
// (...opencode...) so that a project synced for both targets keeps two independent
// sidecars; the Claude Code names are unchanged.
func (a *Adapter) paths(scope adapter.Scope, root string) paths {
	p := paths{Paths: adapter.Paths{Root: root}}
	if scope == adapter.ScopeProject {
		p.SkillsDir = filepath.Join(root, ".opencode", "skills")
		p.configJSON = filepath.Join(root, ConfigFile)
		p.configJC = filepath.Join(root, JSONCFile)
		p.State = filepath.Join(root, ".forge", "sync-state.opencode.json")
		p.Journal = filepath.Join(root, ".forge", "sync-journal.opencode.json")
		p.StateBase = root
	} else {
		p.SkillsDir, p.skillsNote = userSkillsDir(root)
		p.State = filepath.Join(a.ForgeHome, "sync-state", "opencode-user.json")
		p.Journal = filepath.Join(a.ForgeHome, "sync-state", "opencode-user.journal.json")
		p.StateBase = a.ForgeHome
	}
	return p
}

// userSkillsDir returns the user-scope skills directory below the home directory
// root: $XDG_CONFIG_HOME/opencode/skills when XDG_CONFIG_HOME is set and usable,
// else ~/.config/opencode/skills. OpenCode is documented to use ~/.config/opencode;
// that it follows XDG_CONFIG_HOME is not confirmed by docs/clients/opencode.md
// ([미확인]), so the XDG location is used conservatively when set, and an unusable
// value (relative, with control characters, outside the home directory, or a
// symbolic link) is ignored with a warning: the default is used then.
func userSkillsDir(root string) (dir, note string) {
	def := filepath.Join(root, ".config", "opencode", "skills")
	x := os.Getenv("XDG_CONFIG_HOME")
	if x == "" {
		return def, ""
	}
	ignore := func(why string) (string, string) {
		return def, "경고: XDG_CONFIG_HOME=" + textsafe.Display(x, 80) + " 은(는) " + why + " 무시하고 기본 경로(~/.config/opencode/skills)를 사용합니다. OpenCode 가 실제로 읽는 위치와 다를 수 있습니다"
	}
	for _, r := range x {
		if textsafe.NeedsEscape(r) {
			return ignore("제어/비가시 문자가 있어")
		}
	}
	if !filepath.IsAbs(x) {
		return ignore("절대 경로가 아니어서")
	}
	x = filepath.Clean(x)
	if fi, err := os.Lstat(x); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return ignore("심볼릭 링크라서")
	}
	// Resolve the nearest existing ancestor (macOS /var -> /private/var) so the
	// comparison with the resolved home directory is exact.
	rest, cur := "", x
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			cur = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ignore("해석할 수 없어")
		}
		rest, cur = filepath.Join(filepath.Base(cur), rest), parent
	}
	if rel, err := filepath.Rel(root, cur); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ignore("홈 디렉터리 밖이어서")
	}
	dir = filepath.Join(cur, "opencode", "skills")
	return dir, "user 스킬 대상: " + textsafe.Display(dir, 200) + " (XDG_CONFIG_HOME 사용; OpenCode 가 XDG_CONFIG_HOME 을 따르는지는 [미확인]이라 보수적으로 처리)"
}

// AllowedPath implements adapter.Adapter: only the project opencode.json, the
// state file and the skill directories (one level below the skills directory)
// are writable; the OpenCode secret/user-level files are refused by GuardPath.
func (a *Adapter) AllowedPath(scope adapter.Scope, root, path string) (string, error) {
	p := a.paths(scope, root)
	return adapter.AllowedPath(Name, scope, root, path, p.Paths, map[string]string{p.configJSON: root})
}

// ResolveRoot resolves the sync root (the CLI needs it to name the sync lock
// before planning: plan and apply run under one lock). The home directory, the
// Forge home and the OpenCode/Claude user directories hold user-level state (and
// a project opencode.json there would BE the global config): never a project root.
func (a *Adapter) ResolveRoot(req adapter.Request) (string, []string, error) {
	if req.Scope == "" {
		req.Scope = adapter.ScopeProject
	}
	return adapter.ResolveRoot(req, a.ForgeHome, func(home string) []adapter.ForbiddenDir {
		var out []adapter.ForbiddenDir
		config, data := adapter.OpenCodeDirs()
		for _, d := range append(config, data...) {
			out = append(out, adapter.ForbiddenDir{Path: d, Why: "프로젝트 루트가 OpenCode 사용자 디렉터리(" + textsafe.Display(d, 120) + ")입니다. 사용자 설정은 --scope user 로 동기화하세요"})
		}
		if home != "" {
			for _, rel := range [][]string{{".config", "opencode"}, {".local", "share", "opencode"}, {".local", "state", "opencode"}, {".cache", "opencode"}, {".claude"}} {
				out = append(out, adapter.ForbiddenDir{Path: filepath.Join(append([]string{home}, rel...)...),
					Why: "프로젝트 루트가 OpenCode/Claude 사용자 디렉터리(~/" + filepath.ToSlash(filepath.Join(rel...)) + ")입니다. 사용자 설정은 --scope user 로 동기화하세요"})
			}
		}
		return out
	})
}

// planner adds the opencode paths and the claimed skills to the common planner.
type planner struct {
	*adapter.Planner
	a *Adapter
	p paths
	// claimed are skills the claude-code target manages in the same scope
	// (.claude/skills): OpenCode reads that directory too, so they are not
	// rendered a second time. Read from the claude-code sidecar, never written.
	claimed map[string]bool
}

// Plan implements adapter.Adapter. It reads files but writes nothing.
func (a *Adapter) Plan(ctx context.Context, in adapter.Inputs, req adapter.Request) (*adapter.Plan, error) {
	if req.Scope == "" {
		req.Scope = adapter.ScopeProject
	}
	root, notes, err := a.ResolveRoot(req)
	if err != nil {
		return nil, err
	}
	if err := adapter.ValidateBin(req.ForgeBin, func(bin string) error {
		if unsafeSubst(bin) {
			return errors.New("--bin 에 {env:...}/{file:...}/${...} 치환 문법을 넣을 수 없습니다")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if req.NoHooks || req.RemoveHooks {
		return nil, errors.New("opencode 타깃은 hook/플러그인을 지원하지 않습니다 (--no-hooks, --remove-hooks 를 쓸 수 없음)")
	}
	p := a.paths(req.Scope, root)
	pl := &planner{Planner: adapter.NewPlanner(a, in, req, root, p.Paths, notes), a: a, p: p}
	if p.skillsNote != "" && req.Scope == adapter.ScopeUser {
		pl.Note(p.skillsNote)
	}
	dirs := []string{p.SkillsDir, filepath.Dir(p.State)}
	if req.Scope == adapter.ScopeProject {
		// Refuse before anything else: with only opencode.jsonc the gateway entry
		// cannot be placed and a half sync would mislead.
		if err := pl.checkConfigFiles(); err != nil {
			return nil, err
		}
		dirs = append([]string{root}, dirs...)
	}
	pl.Preflight(dirs)
	if err := pl.LoadState(); err != nil {
		return nil, err
	}
	pl.loadClaims()
	if err := pl.PlanSkills(ctx, adapter.SkillPolicy{
		LinkHint:      " (.opencode 또는 skills 가 링크인지 확인)",
		Claimed:       pl.claimed,
		ClaimedSkip:   claimedReason,
		ClaimedRemove: ".claude/skills 로 이미 노출되어 중복을 없애기 위해 .opencode 사본을 제거 (백업 후)",
	}); err != nil {
		return nil, err
	}
	if req.Scope == adapter.ScopeProject {
		if err := pl.planMCPFile(); err != nil {
			return nil, err
		}
	} else {
		pl.planMCPCommands()
	}
	pl.FinishState()
	return pl.Plan, nil
}

// claimedReason explains why a skill is not written to the OpenCode skill directory.
const claimedReason = ".claude/skills 로 이미 노출됨 (claude-code 타깃이 관리) — 두 경로 동시 노출 시 동작 [미확인]"

// checkConfigFiles handles opencode.jsonc (Lstat only: the file is never opened).
func (pl *planner) checkConfigFiles() error {
	_, jerr := os.Lstat(pl.p.configJSON)
	_, cerr := os.Lstat(pl.p.configJC)
	hasJSON, hasJSONC := jerr == nil, cerr == nil
	switch {
	case !hasJSON && hasJSONC:
		return errors.New("프로젝트에 opencode.jsonc 만 있습니다: JSONC 는 아직 지원하지 않습니다(주석 보존 불가). 아무것도 쓰지 않고 중단합니다. " +
			"주석이 없다면 opencode.json 으로 옮긴 뒤 다시 실행하거나, mcp 항목을 직접 추가하세요 (docs/sync.md)")
	case hasJSON && hasJSONC:
		pl.Note("경고: opencode.jsonc 도 존재합니다. opencode.json 과 opencode.jsonc 중 어느 쪽이 우선하는지는 [미확인]이며, Forge 는 opencode.json 만 편집하고 opencode.jsonc 는 읽지 않습니다")
	}
	return nil
}

// loadClaims reads the claude-code sidecar of the same scope (read-only) and
// records the skills that target manages under .claude/skills.
func (pl *planner) loadClaims() {
	pl.claimed = map[string]bool{}
	for n, v := range pl.Req.ClaimedSkills {
		if v {
			pl.claimed[n] = true
		}
	}
	path := claudecode.New(pl.a.ForgeHome).StatePath(pl.Scope, pl.p.Root)
	st, warn := adapter.ReadForeignState(path, pl.p.StateBase, claudecode.Name, pl.Scope)
	if warn != "" {
		pl.Note("claude-code 상태 파일을 확인하지 못해 .claude/skills 중복 노출 검사를 건너뜁니다 (" + warn + ")")
		return
	}
	if st == nil {
		return
	}
	for _, it := range st.Items {
		if it.Kind == adapter.KindSkill {
			pl.claimed[it.Name] = true
		}
	}
}
