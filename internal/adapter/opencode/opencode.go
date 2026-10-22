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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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

var skillNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

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
	root       string
	skillsDir  string
	configJSON string // project scope only
	configJC   string // project scope only; looked at (Lstat), never read or written
	state      string
	stateBase  string // symlink-check base of the state file
	journal    string // copy of the apply journal, next to the state file
	// skillsNote tells where the user-scope skills go when that is not the default
	// (XDG_CONFIG_HOME) or why an XDG value was ignored (plan note).
	skillsNote string
}

// paths names the files of a scope. State and journal use their own file names
// (…opencode…) so that a project synced for both targets keeps two independent
// sidecars; the Claude Code names are unchanged.
func (a *Adapter) paths(scope adapter.Scope, root string) paths {
	p := paths{root: root}
	if scope == adapter.ScopeProject {
		p.skillsDir = filepath.Join(root, ".opencode", "skills")
		p.configJSON = filepath.Join(root, ConfigFile)
		p.configJC = filepath.Join(root, JSONCFile)
		p.state = filepath.Join(root, ".forge", "sync-state.opencode.json")
		p.journal = filepath.Join(root, ".forge", "sync-journal.opencode.json")
		p.stateBase = root
	} else {
		p.skillsDir, p.skillsNote = userSkillsDir(root)
		p.state = filepath.Join(a.ForgeHome, "sync-state", "opencode-user.json")
		p.journal = filepath.Join(a.ForgeHome, "sync-state", "opencode-user.journal.json")
		p.stateBase = a.ForgeHome
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
	if err := adapter.GuardPath(path); err != nil {
		return "", err
	}
	if scope != adapter.ScopeProject && scope != adapter.ScopeUser {
		return "", fmt.Errorf("알 수 없는 scope: %q", string(scope))
	}
	p := a.paths(scope, root)
	if path == p.state {
		return p.stateBase, nil
	}
	if scope == adapter.ScopeProject && path == p.configJSON {
		return root, nil
	}
	if filepath.Dir(path) == p.skillsDir && skillNameRe.MatchString(filepath.Base(path)) {
		return root, nil
	}
	return "", fmt.Errorf("opencode 어댑터가 쓸 수 없는 경로입니다: %s", adapter.MaskLine(path))
}

// forbiddenRoot reports why root may not be used as a project root: the home
// directory, the Forge home and the OpenCode/Claude user directories hold
// user-level state (and a project opencode.json there would BE the global config).
// The decision is about the directory itself (os.SameFile after EvalSymlinks).
func (a *Adapter) forbiddenRoot(root, home string) string {
	fi, err := os.Stat(root)
	if err != nil {
		return ""
	}
	same := func(other string) bool {
		if other == "" {
			return false
		}
		if r, e := filepath.EvalSymlinks(other); e == nil {
			other = r
		}
		if other == root {
			return true
		}
		ofi, err := os.Stat(other)
		return err == nil && os.SameFile(fi, ofi)
	}
	if same(home) {
		return "프로젝트 루트가 홈 디렉터리입니다. 홈의 설정은 --scope user 로 동기화하세요"
	}
	if same(a.ForgeHome) {
		return "프로젝트 루트가 Forge 홈 디렉터리입니다. 프로젝트 디렉터리를 지정하세요"
	}
	config, data := adapter.OpenCodeDirs()
	for _, d := range append(config, data...) {
		if same(d) {
			return "프로젝트 루트가 OpenCode 사용자 디렉터리(" + textsafe.Display(d, 120) + ")입니다. 사용자 설정은 --scope user 로 동기화하세요"
		}
	}
	if home != "" {
		for _, rel := range [][]string{{".config", "opencode"}, {".local", "share", "opencode"}, {".local", "state", "opencode"}, {".cache", "opencode"}, {".claude"}} {
			if same(filepath.Join(append([]string{home}, rel...)...)) {
				return "프로젝트 루트가 OpenCode/Claude 사용자 디렉터리(~/" + filepath.ToSlash(filepath.Join(rel...)) + ")입니다. 사용자 설정은 --scope user 로 동기화하세요"
			}
		}
	}
	return ""
}

func (a *Adapter) resolveRoot(req adapter.Request) (root string, notes []string, err error) {
	home := ""
	if req.Home != "" {
		if h, e := filepath.EvalSymlinks(req.Home); e == nil {
			home = h
		}
	}
	if req.Scope == adapter.ScopeUser {
		if home == "" {
			return "", nil, errors.New("홈 디렉터리를 알 수 없습니다 (HOME)")
		}
		return home, nil, nil
	}
	dir := req.ProjectDir
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, fmt.Errorf("프로젝트 디렉터리를 찾을 수 없습니다: %w", err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", nil, errors.New("프로젝트 디렉터리가 디렉터리가 아닙니다")
	}
	root = ""
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			root = d
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	if root == "" {
		root = abs
		notes = append(notes, "저장소 루트(.git)를 찾지 못해 지정한 디렉터리를 프로젝트 루트로 사용합니다")
	}
	if why := a.forbiddenRoot(root, home); why != "" {
		return "", nil, errors.New(why)
	}
	return root, notes, nil
}

// planner carries the state shared by the item planners of one Plan call.
type planner struct {
	a     *Adapter
	in    adapter.Inputs
	req   adapter.Request
	scope adapter.Scope
	p     paths
	now   string
	state *adapter.State
	plan  *adapter.Plan
	// stateOrig/stateSnap: the sidecar content the plan started from.
	stateOrig []byte
	stateSnap adapter.FileState
	// journals are the believable pending journals of interrupted syncs and
	// jitems their items (newest journal wins): see adopt.
	journals []*adapter.Journal
	jitems   map[string]adapter.StateItem
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
	if req.NoHooks || req.RemoveHooks {
		return nil, errors.New("opencode 타깃은 hook/플러그인을 지원하지 않습니다 (--no-hooks, --remove-hooks 를 쓸 수 없음)")
	}
	root, notes, err := a.resolveRoot(req)
	if err != nil {
		return nil, err
	}
	if err := validateBin(req.ForgeBin); err != nil {
		return nil, err
	}
	nowFn := req.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	pl := &planner{a: a, in: in, req: req, scope: req.Scope, p: a.paths(req.Scope, root), now: nowFn().UTC().Format(time.RFC3339)}
	pl.plan = &adapter.Plan{Target: Name, Scope: req.Scope, Root: root, LockKey: adapter.LockKey(Name, req.Scope, root),
		Items: []adapter.Item{}, Notes: notes, SkipConflicts: false}
	pl.plan.JournalPath, pl.plan.JournalBase = pl.p.journal, pl.p.stateBase
	pl.plan.StatePath = pl.p.state
	if pl.p.skillsNote != "" && req.Scope == adapter.ScopeUser {
		pl.plan.Notes = append(pl.plan.Notes, pl.p.skillsNote)
	}
	if req.Scope == adapter.ScopeProject {
		// Refuse before anything else: with only opencode.jsonc the gateway entry
		// cannot be placed and a half sync would mislead.
		if err := pl.checkConfigFiles(); err != nil {
			return nil, err
		}
	}
	pl.preflight()
	if err := pl.loadState(); err != nil {
		return nil, err
	}
	pl.loadClaims()
	if err := pl.planSkills(ctx); err != nil {
		return nil, err
	}
	if req.Scope == adapter.ScopeProject {
		if err := pl.planMCPFile(); err != nil {
			return nil, err
		}
	} else {
		pl.planMCPCommands()
	}
	pl.finishState()
	pl.plan.NewState = pl.state
	return pl.plan, nil
}

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
		pl.plan.Notes = append(pl.plan.Notes, "경고: opencode.jsonc 도 존재합니다. opencode.json 과 opencode.jsonc 중 어느 쪽이 우선하는지는 [미확인]이며, Forge 는 opencode.json 만 편집하고 opencode.jsonc 는 읽지 않습니다")
	}
	return nil
}

// loadClaims reads the claude-code sidecar of the same scope (read-only) and
// records the skills that target manages under .claude/skills.
func (pl *planner) loadClaims() {
	pl.claimed = map[string]bool{}
	cc := claudecode.New(pl.a.ForgeHome)
	path := cc.StatePath(pl.scope, pl.p.root)
	warn := func(why string) {
		pl.plan.Notes = append(pl.plan.Notes, "claude-code 상태 파일을 확인하지 못해 .claude/skills 중복 노출 검사를 건너뜁니다 ("+why+")")
	}
	if adapter.GuardPath(path) != nil || adapter.CheckNoSymlinks(pl.p.stateBase, path) != nil {
		warn("경로가 안전하지 않음")
		return
	}
	b, _, ok, err := adapter.ReadFileSafe(path)
	if err != nil {
		warn("읽을 수 없음")
		return
	}
	if !ok {
		return
	}
	st, err := adapter.ParseState(b, claudecode.Name, pl.scope)
	if err != nil {
		warn("해석할 수 없음")
		return
	}
	for _, it := range st.Items {
		if it.Kind == adapter.KindSkill {
			pl.claimed[it.Name] = true
		}
	}
}

// tempDirs are the directories next to which an apply creates temporary files.
func (pl *planner) tempDirs() []string {
	var dirs []string
	if pl.scope == adapter.ScopeProject {
		dirs = []string{pl.p.root, pl.p.skillsDir, filepath.Dir(pl.p.state)}
	} else {
		dirs = []string{pl.p.skillsDir, filepath.Dir(pl.p.state)}
	}
	var out []string
	for _, d := range dirs {
		base := pl.p.root
		if !strings.HasPrefix(d, pl.p.root+string(filepath.Separator)) && d != pl.p.root {
			base = pl.p.stateBase
		}
		if adapter.CheckNoSymlinks(base, d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// preflight looks (read-only) for what a killed sync left behind: leftover
// temporary files and a pending journal. Both are reported in the plan; Apply
// cleans up and finishes the interrupted work.
func (pl *planner) preflight() {
	pl.plan.TempDirs = pl.tempDirs()
	if tmp := adapter.FindStaleTemps(pl.plan.TempDirs); len(tmp) > 0 {
		pl.plan.StaleTemps = tmp
	}
	if pl.req.BackupDir == "" {
		return
	}
	if pl.req.SyncInProgress {
		pl.plan.Notes = append(pl.plan.Notes, "다른 forge sync 가 진행 중이라 중단된 sync 여부는 확인하지 않았습니다")
		return
	}
	det := adapter.DetectInterrupted(pl.a, pl.req.BackupDir, pl.scope, pl.plan.Root)
	pl.plan.InvalidJournals = det.Invalid
	pl.journals = det.Journals
	pl.plan.Interrupted = det.Infos
	pl.jitems = map[string]adapter.StateItem{}
	for _, j := range det.Journals { // oldest first: the newest journal wins
		pl.plan.RecoverIDs = append(pl.plan.RecoverIDs, j.ID)
		for _, it := range j.Items {
			pl.jitems[it.Kind+"\x00"+it.Name] = it
		}
	}
	for _, id := range det.Invalid {
		pl.plan.Notes = append(pl.plan.Notes, "백업 "+id+" 의 저널이 올바르지 않아(손상 또는 변조) 무시했습니다. 필요하면 forge sync --restore "+id+" 로 되돌리거나 --prune-backups 로 정리하세요")
	}
}

// adopt applies the journal of an interrupted sync to one item. When the state
// file does not (yet) record the item the way the interrupted apply intended but
// the journal does, and the content on disk hashes to exactly the journaled value,
// the item is Forge-managed: the state file simply never got its final write.
// Anything else (no journal entry, different content) is left to the normal rules.
func (pl *planner) adopt(kind, name, cur string) *adapter.StateItem {
	st := pl.state.Find(kind, name)
	if ji, ok := pl.jitems[kind+"\x00"+name]; ok && cur != "" && ji.Hash == cur && (st == nil || st.Hash != cur) {
		if st == nil {
			pl.state.Items = append(pl.state.Items, ji)
		} else {
			st.Hash, st.AppliedAt = ji.Hash, ji.AppliedAt
		}
		return pl.state.Find(kind, name)
	}
	return st
}

// journalListed reports whether an interrupted sync's journal lists the item.
func (pl *planner) journalListed(kind, name string) bool {
	_, ok := pl.jitems[kind+"\x00"+name]
	return ok
}

// journalOrphans returns journal items that an interrupted apply wrote but the
// state file does not list, that are not wanted any more, and whose content on disk
// still matches the journal: they are removed like any managed item. Only skills
// use this (the management marker is extra evidence); an MCP server that is no
// longer wanted stays in the file as an ordinary entry.
func (pl *planner) journalOrphans(kind string, wanted func(name string) bool, curHash func(name string) (string, bool)) []adapter.StateItem {
	var out []adapter.StateItem
	for _, it := range pl.jitems {
		if it.Kind != kind || pl.state.Find(kind, it.Name) != nil || wanted(it.Name) {
			continue
		}
		if h, ok := curHash(it.Name); ok && h == it.Hash {
			out = append(out, it)
		}
	}
	return out
}

func validateBin(bin string) error {
	if bin == "" {
		return nil
	}
	for _, r := range bin {
		if r < 0x20 || r == 0x7f {
			return errors.New("--bin 에 제어 문자를 넣을 수 없습니다")
		}
	}
	if strings.ContainsRune(bin, '/') && !filepath.IsAbs(bin) {
		return errors.New("--bin 은 PATH 의 명령 이름(forge) 이거나 절대 경로여야 합니다 (설정 파일에 상대 경로를 쓰지 않습니다)")
	}
	if unsafeSubst(bin) {
		return errors.New("--bin 에 {env:...}/{file:...}/${...} 치환 문법을 넣을 수 없습니다")
	}
	return nil
}

func (pl *planner) bin() string {
	if pl.req.ForgeBin == "" {
		return "forge"
	}
	return pl.req.ForgeBin
}

func (pl *planner) item(it adapter.Item) {
	pl.plan.Items = append(pl.plan.Items, it)
}

func (pl *planner) loadState() error {
	pl.state = adapter.NewState(Name, pl.scope)
	if err := adapter.GuardPath(pl.p.state); err != nil {
		return err
	}
	if err := adapter.CheckNoSymlinks(pl.p.stateBase, pl.p.state); err != nil {
		return fmt.Errorf("상태 파일 경로가 안전하지 않습니다: %w", err)
	}
	b, _, ok, err := adapter.ReadFileSafe(pl.p.state)
	if err != nil {
		return fmt.Errorf("상태 파일을 읽을 수 없습니다: %w", err)
	}
	snap, err := adapter.Snapshot(pl.p.state)
	if err != nil {
		return err
	}
	pl.stateSnap = snap
	if !ok {
		return nil
	}
	pl.stateOrig = b
	st, err := adapter.ParseState(b, Name, pl.scope)
	if err != nil {
		return err
	}
	pl.state = st
	return nil
}

// finishState turns the state mutations into one write operation (only when the
// serialized content really changed, so a second apply writes nothing).
func (pl *planner) finishState() {
	if len(pl.plan.Ops) == 0 && pl.stateOrig == nil && len(pl.state.Items) == 0 {
		return
	}
	out := pl.state.Marshal()
	if pl.stateOrig != nil && string(out) == string(pl.stateOrig) {
		return
	}
	if pl.stateOrig == nil && len(pl.state.Items) == 0 {
		return
	}
	if reason := adapter.WriteBlocker(pl.p.state); reason != "" && pl.stateSnap.Exists {
		pl.plan.Notes = append(pl.plan.Notes, "상태 파일: "+reason)
		return
	}
	mode := os.FileMode(0o644)
	if pl.scope == adapter.ScopeUser {
		mode = 0o600
	}
	if pl.stateSnap.Exists {
		mode = os.FileMode(pl.stateSnap.Mode)
	}
	pl.plan.Ops = append(pl.plan.Ops, adapter.Op{Kind: adapter.OpWriteFile, Path: pl.p.state, Data: out, Mode: mode,
		Before: pl.stateSnap, BeforeData: pl.stateOrig})
}

// ResolveRoot exposes the root resolution so that the CLI can name the sync lock
// before planning (plan and apply run under one lock).
func (a *Adapter) ResolveRoot(req adapter.Request) (string, []string, error) {
	if req.Scope == "" {
		req.Scope = adapter.ScopeProject
	}
	return a.resolveRoot(req)
}
