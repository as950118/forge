package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SkillNameRe is the directory-name rule of a skill (and of every path AllowedPath
// accepts one level below a skills directory).
var SkillNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Paths names the files of one scope of a target. Only the target knows the
// layout; the security logic in this file works on whatever it is given.
type Paths struct {
	Root      string
	SkillsDir string
	State     string
	StateBase string // symlink-check base of the state file
	Journal   string // copy of the apply journal, next to the state file
}

// Planner is the part of a Plan call that does not depend on the client: sidecar
// state, interrupted-sync journals, "is this item Forge-managed" judgements and
// the final state write. A target embeds it and adds only format knowledge
// (schemas, paths, substitution syntax); every security decision lives here so a
// fix lands once for all targets.
type Planner struct {
	A     Adapter
	In    Inputs
	Req   Request
	Scope Scope
	P     Paths
	Now   string
	State *State
	Plan  *Plan

	stateOrig []byte
	stateSnap FileState
	// journals are the believable pending journals of interrupted syncs and
	// jitems their items (newest journal wins): see Adopt.
	jitems map[string]StateItem
}

// NewPlanner starts a Plan for target a. req.Scope must already be set and root
// resolved (ResolveRoot).
func NewPlanner(a Adapter, in Inputs, req Request, root string, p Paths, notes []string) *Planner {
	nowFn := req.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	pl := &Planner{A: a, In: in, Req: req, Scope: req.Scope, P: p, Now: nowFn().UTC().Format(time.RFC3339)}
	pl.Plan = &Plan{Target: a.Name(), Scope: req.Scope, Root: root, LockKey: LockKey(a.Name(), req.Scope, root),
		Items: []Item{}, Notes: notes, SkipConflicts: false}
	pl.Plan.JournalPath, pl.Plan.JournalBase = p.Journal, p.StateBase
	pl.Plan.StatePath = p.State
	return pl
}

// Item appends a plan table row.
func (pl *Planner) Item(it Item) { pl.Plan.Items = append(pl.Plan.Items, it) }

// Note appends a plan note.
func (pl *Planner) Note(s string) { pl.Plan.Notes = append(pl.Plan.Notes, s) }

// Bin is the forge command written into client configs.
func (pl *Planner) Bin() string {
	if pl.Req.ForgeBin == "" {
		return "forge"
	}
	return pl.Req.ForgeBin
}

// ValidateBin checks --bin before it is written into a client config. extra is
// the target's own check (substitution syntax of its config format); nil for none.
func ValidateBin(bin string, extra func(string) error) error {
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
	if extra != nil {
		return extra(bin)
	}
	return nil
}

// Preflight looks (read-only) for what a killed sync left behind: leftover
// temporary files and a pending journal. Both are reported in the plan; Apply
// cleans up and finishes the interrupted work. dirs are the directories next to
// which an apply creates temporary files; those below a symlink are dropped.
func (pl *Planner) Preflight(dirs []string) {
	var out []string
	for _, d := range dirs {
		base := pl.P.Root
		if !strings.HasPrefix(d, pl.P.Root+string(filepath.Separator)) && d != pl.P.Root {
			base = pl.P.StateBase
		}
		if CheckNoSymlinks(base, d) == nil {
			out = append(out, d)
		}
	}
	pl.Plan.TempDirs = out
	if tmp := FindStaleTemps(pl.Plan.TempDirs); len(tmp) > 0 {
		pl.Plan.StaleTemps = tmp
	}
	if pl.Req.BackupDir == "" {
		return
	}
	if pl.Req.SyncInProgress {
		pl.Note("다른 forge sync 가 진행 중이라 중단된 sync 여부는 확인하지 않았습니다")
		return
	}
	det := DetectInterrupted(pl.A, pl.Req.BackupDir, pl.Scope, pl.Plan.Root)
	pl.Plan.InvalidJournals = det.Invalid
	pl.Plan.Interrupted = det.Infos
	pl.jitems = map[string]StateItem{}
	for _, j := range det.Journals { // oldest first: the newest journal wins
		pl.Plan.RecoverIDs = append(pl.Plan.RecoverIDs, j.ID)
		for _, it := range j.Items {
			pl.jitems[it.Kind+"\x00"+it.Name] = it
		}
	}
	for _, id := range det.Invalid {
		pl.Note("백업 " + id + " 의 저널이 올바르지 않아(손상 또는 변조) 무시했습니다. 필요하면 forge sync --restore " + id + " 로 되돌리거나 --prune-backups 로 정리하세요")
	}
}

// Adopt applies the journal of an interrupted sync to one item. When the state
// file does not (yet) record the item the way the interrupted apply intended but
// the journal does, and the content on disk hashes to exactly the journaled value,
// the item is Forge-managed: the state file simply never got its final write.
// Anything else (no journal entry, different content) is left to the normal rules.
func (pl *Planner) Adopt(kind, name, cur string) *StateItem {
	st := pl.State.Find(kind, name)
	if ji, ok := pl.jitems[kind+"\x00"+name]; ok && cur != "" && ji.Hash == cur && (st == nil || st.Hash != cur) {
		if st == nil {
			pl.State.Items = append(pl.State.Items, ji)
		} else {
			st.Hash, st.AppliedAt = ji.Hash, ji.AppliedAt
		}
		return pl.State.Find(kind, name)
	}
	return st
}

// JournalListed reports whether an interrupted sync's journal lists the item.
func (pl *Planner) JournalListed(kind, name string) bool {
	_, ok := pl.jitems[kind+"\x00"+name]
	return ok
}

// ManagedModified is the hash-conflict judgement for an item the state file
// records: it was modified by hand when its current hash differs from the
// recorded one (a hash the journal of a killed sync vouches for counts as
// recorded) or from any extra evidence hash (a skill's marker hash).
func (pl *Planner) ManagedModified(kind string, si StateItem, cur string, evidence ...string) bool {
	if ji, ok := pl.jitems[kind+"\x00"+si.Name]; ok && ji.Hash == cur {
		si.Hash = cur // written by a killed sync (see Adopt)
	}
	if cur != si.Hash {
		return true
	}
	for _, e := range evidence {
		if cur != e {
			return true
		}
	}
	return false
}

// JournalOrphans returns journal items that an interrupted apply wrote but the
// state file does not list, that are not wanted any more, and whose content on disk
// still matches the journal: they are removed like any managed item. Only skills
// use this (the management marker is extra evidence); an MCP server or hook that is
// no longer wanted stays in the file as an ordinary entry.
func (pl *Planner) JournalOrphans(kind string, wanted func(name string) bool, curHash func(name string) (string, bool)) []StateItem {
	var out []StateItem
	for _, it := range pl.jitems {
		if it.Kind != kind || pl.State.Find(kind, it.Name) != nil || wanted(it.Name) {
			continue
		}
		if h, ok := curHash(it.Name); ok && h == it.Hash {
			out = append(out, it)
		}
	}
	return out
}

// LoadState reads the sidecar state file (never through a link, never a guarded file).
func (pl *Planner) LoadState() error {
	pl.State = NewState(pl.A.Name(), pl.Scope)
	if err := GuardPath(pl.P.State); err != nil {
		return err
	}
	if err := CheckNoSymlinks(pl.P.StateBase, pl.P.State); err != nil {
		return fmt.Errorf("상태 파일 경로가 안전하지 않습니다: %w", err)
	}
	b, _, ok, err := ReadFileSafe(pl.P.State)
	if err != nil {
		return fmt.Errorf("상태 파일을 읽을 수 없습니다: %w", err)
	}
	snap, err := Snapshot(pl.P.State)
	if err != nil {
		return err
	}
	pl.stateSnap = snap
	if !ok {
		return nil
	}
	pl.stateOrig = b
	st, err := ParseState(b, pl.A.Name(), pl.Scope)
	if err != nil {
		return err
	}
	pl.State = st
	return nil
}

// FinishState turns the state mutations into one write operation (only when the
// serialized content really changed, so a second apply writes nothing). It must
// be the last operation of the plan.
func (pl *Planner) FinishState() {
	pl.Plan.NewState = pl.State
	if len(pl.Plan.Ops) == 0 && pl.stateOrig == nil && len(pl.State.Items) == 0 {
		return
	}
	out := pl.State.Marshal()
	if pl.stateOrig != nil && string(out) == string(pl.stateOrig) {
		return
	}
	if pl.stateOrig == nil && len(pl.State.Items) == 0 {
		return
	}
	if reason := WriteBlocker(pl.P.State); reason != "" && pl.stateSnap.Exists {
		pl.Note("상태 파일: " + reason)
		return
	}
	mode := os.FileMode(0o644)
	if pl.Scope == ScopeUser {
		mode = 0o600
	}
	if pl.stateSnap.Exists {
		mode = os.FileMode(pl.stateSnap.Mode)
	}
	pl.Plan.Ops = append(pl.Plan.Ops, Op{Kind: OpWriteFile, Path: pl.P.State, Data: out, Mode: mode,
		Before: pl.stateSnap, BeforeData: pl.stateOrig})
}

// AllowedPath is the common body of Adapter.AllowedPath: a guarded path is never
// writable; otherwise only the state file, the files the target lists (path ->
// symlink-check base) and skill directories one level below the skills directory.
func AllowedPath(target string, scope Scope, root, path string, p Paths, files map[string]string) (string, error) {
	if err := GuardPath(path); err != nil {
		return "", err
	}
	if scope != ScopeProject && scope != ScopeUser {
		return "", fmt.Errorf("알 수 없는 scope: %q", string(scope))
	}
	if path == p.State {
		return p.StateBase, nil
	}
	if base, ok := files[path]; ok && path != "" {
		return base, nil
	}
	if filepath.Dir(path) == p.SkillsDir && SkillNameRe.MatchString(filepath.Base(path)) {
		return root, nil
	}
	return "", fmt.Errorf("%s 어댑터가 쓸 수 없는 경로입니다: %s", target, MaskLine(path))
}

// ForbiddenDir is a directory a project root may not be, with the reason.
type ForbiddenDir struct{ Path, Why string }

// ResolveRoot finds the sync root: the home directory (user scope) or the
// repository root (a parent holding .git; else the given directory). The home
// directory, the Forge home and every directory extra(home) lists are refused:
// they hold user-level state, not a project. The decision is about the directory
// itself (os.SameFile after EvalSymlinks), not about spelling: on a
// case-insensitive filesystem /Users/ME and /users/me are the same directory.
func ResolveRoot(req Request, forgeHome string, extra func(home string) []ForbiddenDir) (root string, notes []string, err error) {
	home := ""
	if req.Home != "" {
		if h, e := filepath.EvalSymlinks(req.Home); e == nil {
			home = h
		}
	}
	if req.Scope == ScopeUser {
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
	forbidden := []ForbiddenDir{
		{home, "프로젝트 루트가 홈 디렉터리입니다. 홈의 설정은 --scope user 로 동기화하세요"},
		{forgeHome, "프로젝트 루트가 Forge 홈 디렉터리입니다. 프로젝트 디렉터리를 지정하세요"},
	}
	if extra != nil {
		forbidden = append(forbidden, extra(home)...)
	}
	if why := forbiddenRoot(root, forbidden); why != "" {
		return "", nil, errors.New(why)
	}
	return root, notes, nil
}

func forbiddenRoot(root string, dirs []ForbiddenDir) string {
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
	for _, d := range dirs {
		if same(d.Path) {
			return d.Why
		}
	}
	return ""
}

// ReadForeignState reads, read-only, the sidecar state file of ANOTHER target
// (path below base) to learn what it manages. It returns warn instead of an error
// so the caller can skip its check with a note: the file is refused when it is a
// guarded file or reached through a symlink, unreadable or not a valid state of
// that target and scope. A missing file is (nil, "").
func ReadForeignState(path, base, target string, scope Scope) (st *State, warn string) {
	if GuardPath(path) != nil || CheckNoSymlinks(base, path) != nil {
		return nil, "경로가 안전하지 않음"
	}
	b, _, ok, err := ReadFileSafe(path)
	if err != nil {
		return nil, "읽을 수 없음"
	}
	if !ok {
		return nil, ""
	}
	st, err = ParseState(b, target, scope)
	if err != nil {
		return nil, "해석할 수 없음"
	}
	return st, ""
}
