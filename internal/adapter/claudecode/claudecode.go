// Package claudecode renders the Forge registry into Claude Code configuration:
// skills (.claude/skills/<name>/), MCP servers (.mcp.json) and ledger hooks
// (settings.json). It only builds adapter.Plan values; all writing, backup and
// rollback is done by the adapter engine.
//
// Safety rules implemented here (docs/sync.md):
//   - Managed items are recognized only through the sidecar state file plus an
//     exact content-hash match; user-written items are never modified or deleted.
//   - Forge-specific fields are never added to .mcp.json or settings.json.
//   - The Claude Code user file in the home directory is never opened (see
//     adapter.GuardPath); user-scope MCP servers are printed as `claude mcp add`
//     commands only.
//
// Verified format versions: see SupportedVersions and testdata/.
package claudecode

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
)

// Name is the --target value.
const Name = "claude-code"

// SupportedVersions lists the Claude Code versions the formats were documented
// against (docs/clients/claude-code.md). Fixtures live in testdata/<version>/.
// Sync never runs the claude binary to check the installed version.
var SupportedVersions = []string{"2.1.305"}

// GatewayServerName is the .mcp.json entry that points at `forge mcp serve`.
const GatewayServerName = "forge"

var skillNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Adapter implements adapter.Adapter for Claude Code.
type Adapter struct {
	// ForgeHome holds the user-scope state file (<ForgeHome>/sync-state/).
	ForgeHome string
}

// New returns the adapter for the given Forge home directory.
func New(forgeHome string) *Adapter { return &Adapter{ForgeHome: forgeHome} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return Name }

type paths struct {
	root      string
	skillsDir string
	settings  string
	mcpJSON   string // project scope only
	state     string
	stateBase string // symlink-check base of the state file
	journal   string // copy of the apply journal, next to the state file
}

func (a *Adapter) paths(scope adapter.Scope, root string) paths {
	p := paths{root: root, skillsDir: filepath.Join(root, ".claude", "skills"), settings: filepath.Join(root, ".claude", "settings.json")}
	if scope == adapter.ScopeProject {
		p.mcpJSON = filepath.Join(root, ".mcp.json")
		p.state = filepath.Join(root, ".forge", "sync-state.json")
		p.journal = filepath.Join(root, ".forge", "sync-journal.json")
		p.stateBase = root
	} else {
		p.state = filepath.Join(a.ForgeHome, "sync-state", "claude-code-user.json")
		p.journal = filepath.Join(a.ForgeHome, "sync-state", "claude-code-user.journal.json")
		p.stateBase = a.ForgeHome
	}
	return p
}

// StatePath returns the sidecar state file of a scope (root is the project root
// or the home directory). Other adapters (OpenCode) read it, read-only, to see
// which skills this target manages; they never write it.
func (a *Adapter) StatePath(scope adapter.Scope, root string) string {
	return a.paths(scope, root).state
}

// AllowedPath implements adapter.Adapter: only the files and skill directories
// that sync manages are writable, and never the Claude Code user file.
func (a *Adapter) AllowedPath(scope adapter.Scope, root, path string) (string, error) {
	if err := adapter.GuardPath(path); err != nil {
		return "", err
	}
	if scope != adapter.ScopeProject && scope != adapter.ScopeUser {
		return "", fmt.Errorf("알 수 없는 scope: %q", string(scope))
	}
	p := a.paths(scope, root)
	switch path {
	case p.settings:
		return root, nil
	case p.state:
		return p.stateBase, nil
	}
	if scope == adapter.ScopeProject && path == p.mcpJSON {
		return root, nil
	}
	if filepath.Dir(path) == p.skillsDir && skillNameRe.MatchString(filepath.Base(path)) {
		return root, nil
	}
	return "", fmt.Errorf("claude-code 어댑터가 쓸 수 없는 경로입니다: %s", adapter.MaskLine(path))
}

// forbiddenRoot reports why root may not be used as a project root: the home
// directory, the Forge home and ~/.claude hold user-level state, not a project.
// The decision is about the directory itself (os.SameFile after EvalSymlinks),
// not about spelling: on a case-insensitive filesystem /Users/ME and /users/me
// are the same directory.
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
	switch {
	case same(home):
		return "프로젝트 루트가 홈 디렉터리입니다. 홈의 설정은 --scope user 로 동기화하세요"
	case same(a.ForgeHome):
		return "프로젝트 루트가 Forge 홈 디렉터리입니다. 프로젝트 디렉터리를 지정하세요"
	case home != "" && same(filepath.Join(home, ".claude")):
		return "프로젝트 루트가 ~/.claude 입니다. 사용자 설정은 --scope user 로 동기화하세요"
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
	// stateMut is true when the sidecar content must be rewritten.
	stateOrig []byte
	stateSnap adapter.FileState
	// journals are the believable pending journals of interrupted syncs and
	// jitems their items (newest journal wins): see adopt.
	journals []*adapter.Journal
	jitems   map[string]adapter.StateItem
}

// Plan implements adapter.Adapter. It reads files but writes nothing.
func (a *Adapter) Plan(ctx context.Context, in adapter.Inputs, req adapter.Request) (*adapter.Plan, error) {
	if req.Scope == "" {
		req.Scope = adapter.ScopeProject
	}
	root, notes, err := a.resolveRoot(req)
	if err != nil {
		return nil, err
	}
	if err := validateBin(req.ForgeBin); err != nil {
		return nil, err
	}
	if req.NoHooks && req.RemoveHooks {
		return nil, errors.New("--no-hooks 와 --remove-hooks 는 함께 쓸 수 없습니다")
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
	pl.preflight()
	if err := pl.loadState(); err != nil {
		return nil, err
	}
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
	if err := pl.planHooks(); err != nil {
		return nil, err
	}
	pl.finishState()
	pl.plan.NewState = pl.state
	return pl.plan, nil
}

// tempDirs are the directories next to which an apply creates temporary files.
func (pl *planner) tempDirs() []string {
	var dirs []string
	if pl.scope == adapter.ScopeProject {
		dirs = []string{pl.p.root, filepath.Dir(pl.p.settings), pl.p.skillsDir, filepath.Dir(pl.p.state)}
	} else {
		dirs = []string{filepath.Dir(pl.p.settings), pl.p.skillsDir, filepath.Dir(pl.p.state)}
	}
	out := dirs[:0]
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
// use this (the management marker is extra evidence); an MCP server or hook that is
// no longer wanted stays in the file as an ordinary entry.
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
