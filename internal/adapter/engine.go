package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ApplyOptions configures Apply.
type ApplyOptions struct {
	// BackupDir is <forge home>/backups.
	BackupDir string
	Now       func() time.Time
	// Inject, when set, is called before each operation; a non-nil error makes
	// that step fail (tests use it to prove the rollback).
	Inject func(step int, op Op) error
	// Crash simulates a SIGKILL for tests: it is asked at every step boundary and
	// inside the file primitives; returning true makes the engine panic with
	// SimulatedCrash at exactly that point, without any cleanup or rollback.
	Crash func(point string) bool
}

// SimulatedCrash is the panic value raised when ApplyOptions.Crash asks for a crash.
type SimulatedCrash struct{ Point string }

func (o ApplyOptions) crashFn() crashFn {
	if o.Crash == nil {
		return nil
	}
	return func(point string) {
		if o.Crash(point) {
			panic(SimulatedCrash{Point: point})
		}
	}
}

func (o ApplyOptions) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// ApplyResult reports what Apply did.
type ApplyResult struct {
	BackupID   string       `json:"backup_id,omitempty"`
	BackupPath string       `json:"backup_path,omitempty"`
	Changed    []FileChange `json:"changed"`
	// CleanedTemps are leftover temporary files of killed syncs that were removed.
	CleanedTemps []string `json:"cleaned_temp_files,omitempty"`
	// Recovered are the interrupted syncs (backup ids) this apply finished.
	Recovered []string `json:"recovered,omitempty"`
}

// ErrStale is returned when a file changed between planning and applying.
var ErrStale = errors.New("계획을 세운 뒤 파일이 변경되었습니다. 아무것도 쓰지 않았습니다 (다시 계획하세요)")

// ErrConflicts is returned by Apply when the plan has conflicts and skipping was not requested.
var ErrConflicts = errors.New("충돌이 있어 아무것도 쓰지 않았습니다")

// Apply executes the plan's operations: verify -> backup -> journal -> write ->
// (rollback on failure). The caller holds the sync lock. Leftover temporary files
// of killed syncs are removed first, and interrupted syncs the plan recognized
// are marked recovered once this apply has converged the files.
func Apply(ctx context.Context, a Adapter, p *Plan, opts ApplyOptions) (*ApplyResult, error) {
	if c := p.Conflicts(); c > 0 && !p.SkipConflicts {
		return nil, fmt.Errorf("%w (충돌 %d건; --skip-conflicts 로 충돌 항목을 제외하고 적용할 수 있습니다)", ErrConflicts, c)
	}
	cleaned := CleanStaleTemps(p.TempDirs)
	if len(p.Ops) == 0 {
		res := &ApplyResult{Changed: []FileChange{}, CleanedTemps: cleaned}
		if len(p.RecoverIDs) > 0 {
			removeJournalCopy(p.JournalPath, p.RecoverIDs)
			if err := recoverJournals(opts.BackupDir, p.RecoverIDs, ""); err != nil {
				return res, fmt.Errorf("파일은 이미 일치하지만 중단된 sync 의 저널을 완료 표시하지 못했습니다: %w", err)
			}
			res.Recovered = p.RecoverIDs
		}
		return res, nil
	}
	meta := manifestMeta{Kind: "sync", Target: p.Target, Scope: p.Scope, Root: p.Root, State: p.NewState, StatePath: p.StatePath,
		JournalPath: p.JournalPath, JournalBase: p.JournalBase, Supersedes: p.RecoverIDs}
	res, err := applyOps(ctx, a, meta, p.Ops, opts)
	if err != nil {
		return res, err
	}
	res.CleanedTemps = cleaned
	if len(p.RecoverIDs) > 0 {
		if err := recoverJournals(opts.BackupDir, p.RecoverIDs, res.BackupID); err != nil {
			return res, fmt.Errorf("적용은 끝났지만 중단된 sync 의 저널을 완료 표시하지 못했습니다: %w", err)
		}
		res.Recovered = p.RecoverIDs
	}
	return res, nil
}

type manifestMeta struct {
	Kind, Target, Note string
	Scope              Scope
	Root               string
	// State/StatePath: the sidecar state the apply ends with (journal content).
	State     *State
	StatePath string
	// JournalPath is the journal copy next to the state file ("" for restores).
	JournalPath, JournalBase string
	// Supersedes: interrupted syncs this apply finishes (recorded in the manifest).
	Supersedes []string
}

// removeJournalCopy deletes the journal copy at path when it really is a journal
// of one of ids (it never deletes some other file that happens to be there).
func removeJournalCopy(path string, ids []string) {
	if path == "" {
		return
	}
	b, _, ok, err := ReadFileSafe(path)
	if err != nil || !ok {
		return
	}
	var j Journal
	if json.Unmarshal(b, &j) != nil {
		return
	}
	for _, id := range ids {
		if j.ID == id {
			_ = os.Remove(path)
			return
		}
	}
}

func applyOps(ctx context.Context, a Adapter, meta manifestMeta, ops []Op, opts ApplyOptions) (*ApplyResult, error) {
	// 1. Verify. Nothing has been touched yet.
	bases := make([]string, len(ops))
	for i, op := range ops {
		base, err := a.AllowedPath(meta.Scope, meta.Root, op.Path)
		if err != nil {
			return nil, err
		}
		bases[i] = base
		if err := CheckNoSymlinks(base, op.Path); err != nil {
			return nil, err
		}
		cur, err := Snapshot(op.Path)
		if err != nil {
			return nil, err
		}
		if !cur.Equal(op.Before) {
			return nil, fmt.Errorf("%w: %s", ErrStale, op.Path)
		}
	}
	created := missingDirs(bases, ops)
	crash := opts.crashFn()

	// 2. Backup everything that exists, plus the manifest. Failure here leaves
	// the user's files untouched.
	now := opts.now()
	if err := os.MkdirAll(opts.BackupDir, 0o700); err != nil {
		return nil, fmt.Errorf("백업 디렉터리 생성 실패: %w", err)
	}
	var id, bdir string
	for i := 0; ; i++ {
		id = newBackupID(now)
		bdir = filepath.Join(opts.BackupDir, id)
		err := os.Mkdir(bdir, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || i > 5 {
			return nil, fmt.Errorf("백업 디렉터리 생성 실패: %w", err)
		}
	}
	m := &Manifest{Version: 1, ID: id, CreatedAt: now.Format(time.RFC3339), Kind: meta.Kind, Target: meta.Target,
		Scope: meta.Scope, Root: meta.Root, Status: StatusPending, CreatedDirs: created, Note: meta.Note, Supersedes: meta.Supersedes}
	abortBackup := func(err error) (*ApplyResult, error) {
		_ = removeTree(bdir)
		return nil, fmt.Errorf("백업 실패, 아무것도 변경하지 않았습니다: %w", err)
	}
	for _, op := range ops {
		e := Entry{Path: op.Path, Before: op.Before, After: expectedAfter(op)}
		if op.Before.Exists {
			if err := copyToBackup(bdir, op); err != nil {
				return abortBackup(err)
			}
		}
		m.Entries = append(m.Entries, e)
	}
	if err := writeManifest(bdir, m); err != nil {
		return abortBackup(err)
	}
	crash.at("backup-done")

	// 2b. Journal: written (backup dir first, then the copy next to the state file)
	// before the first change, so that a kill from here on is recognizable.
	j, err := buildJournal(m, ops, meta)
	if err != nil {
		return abortBackup(err)
	}
	if err := writeJournal(bdir, j); err != nil {
		return abortBackup(err)
	}
	crash.at("journal-written")
	jdirs, err := writeJournalCopy(meta, j)
	if err != nil {
		return abortBackup(err)
	}
	crash.at("journal-copy-written")
	dropJournalCopy := func() {
		if meta.JournalPath != "" {
			_ = os.Remove(meta.JournalPath)
			for i := len(jdirs) - 1; i >= 0; i-- {
				_ = os.Remove(jdirs[i]) // only empty directories this apply created
			}
		}
	}

	// 3. Execute.
	done := 0
	var execErr error
	for i, op := range ops {
		if err := ctx.Err(); err != nil {
			execErr = err
			break
		}
		if opts.Inject != nil {
			if err := opts.Inject(i, op); err != nil {
				execErr = err
				break
			}
		}
		done = i + 1
		var opCrash crashFn
		if crash != nil {
			idx := i
			opCrash = func(point string) { crash(fmt.Sprintf("op-%d:%s", idx, point)) }
		}
		if err := executeOp(op, opCrash); err != nil {
			execErr = fmt.Errorf("%s: %w", op.Path, err)
			break
		}
		crash.at(fmt.Sprintf("op-%d:done", i))
	}
	if execErr != nil {
		rbErr := rollback(bdir, m, done)
		m.Status = StatusRolledBack
		_ = writeManifest(bdir, m)
		if rbErr != nil {
			// The journal stays pending: the next sync will see an interrupted state.
			return nil, fmt.Errorf("적용 실패(%v); 자동 롤백도 실패: %v. 백업 %s 에서 forge sync --restore %s --force 로 복원하세요", execErr, rbErr, id, id)
		}
		dropJournalCopy()
		removeCreatedDirs(created)
		_ = markJournal(opts.BackupDir, id, JournalRolledBack, "")
		return nil, fmt.Errorf("적용 실패, 변경을 롤백했습니다 (백업 %s): %w", id, execErr)
	}
	crash.at("ops-done")

	// 4. Record the real final state and mark the backup applied.
	res := &ApplyResult{BackupID: id, BackupPath: bdir, Changed: []FileChange{}}
	for i, op := range ops {
		if st, err := Snapshot(op.Path); err == nil {
			m.Entries[i].After = st
		}
		res.Changed = append(res.Changed, FileChange{Path: op.Path, Change: changeKind(op), BeforeHash: op.Before.Hash, AfterHash: m.Entries[i].After.Hash})
	}
	m.Status = StatusApplied
	if err := writeManifest(bdir, m); err != nil {
		return res, fmt.Errorf("적용은 끝났지만 백업 manifest 갱신에 실패했습니다: %w", err)
	}
	crash.at("manifest-applied")
	dropJournalCopy()
	crash.at("journal-copy-removed")
	if err := markJournal(opts.BackupDir, id, JournalComplete, ""); err != nil {
		return res, fmt.Errorf("적용은 끝났지만 저널을 완료 표시하지 못했습니다: %w", err)
	}
	crash.at("journal-complete")
	return res, nil
}

// writeJournalCopy writes the journal next to the sidecar state file and returns
// the directories it had to create (removed again when they end up empty).
func writeJournalCopy(meta manifestMeta, j *Journal) ([]string, error) {
	if meta.JournalPath == "" {
		return nil, nil
	}
	if err := GuardPath(meta.JournalPath); err != nil {
		return nil, err
	}
	if err := CheckNoSymlinks(meta.JournalBase, meta.JournalPath); err != nil {
		return nil, err
	}
	missing := missingDirs([]string{meta.JournalBase}, []Op{{Path: meta.JournalPath}})
	if err := ensureParent(meta.JournalPath); err != nil {
		return nil, err
	}
	mode := os.FileMode(0o644)
	if meta.Scope == ScopeUser {
		mode = 0o600
	}
	if err := WriteFileAtomic(meta.JournalPath, j.marshal(), mode); err != nil {
		return nil, err
	}
	return missing, nil
}

func changeKind(op Op) string {
	switch {
	case op.Kind == OpDeleteFile || op.Kind == OpDeleteTree:
		return "delete"
	case !op.Before.Exists:
		return "create"
	case op.Kind == OpReplaceTree:
		return "replace"
	}
	return "modify"
}

func expectedAfter(op Op) FileState {
	switch op.Kind {
	case OpWriteFile:
		return FileState{Exists: true, Kind: "file", Hash: SHA256Hex(op.Data), Mode: uint32(op.Mode)}
	case OpReplaceTree:
		return FileState{Exists: true, Kind: "dir"} // hash known only after the copy
	}
	return FileState{}
}

// missingDirs lists the ancestors (below root) of every op path that do not exist yet,
// outermost first.
func missingDirs(bases []string, ops []Op) []string {
	seen := map[string]bool{}
	var out []string
	for i, op := range ops {
		root := bases[i]
		var chain []string
		for d := filepath.Dir(op.Path); d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
			if _, err := os.Lstat(d); err == nil {
				break
			}
			chain = append(chain, d)
		}
		for i := len(chain) - 1; i >= 0; i-- {
			if !seen[chain[i]] {
				seen[chain[i]] = true
				out = append(out, chain[i])
			}
		}
	}
	return out
}

func copyToBackup(bdir string, op Op) error {
	dst := backupFilePath(bdir, op.Path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	switch op.Before.Kind {
	case "file":
		b, _, ok, err := ReadFileSafe(op.Path)
		if err != nil || !ok {
			return fmt.Errorf("백업할 파일을 읽을 수 없습니다: %s", op.Path)
		}
		if SHA256Hex(b) != op.Before.Hash {
			return fmt.Errorf("%w: %s", ErrStale, op.Path)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	case "dir":
		if err := copyTree(op.Path, dst, 0o700, func(_ string, src fs.FileMode) fs.FileMode {
			if src&0o111 != 0 {
				return 0o700 // keep the exec bit: it is part of the tree hash
			}
			return 0o600
		}); err != nil {
			return err
		}
		h, _, err := TreeHash(dst, false)
		if err != nil {
			return err
		}
		if h != op.Before.Hash {
			return fmt.Errorf("%w: %s", ErrStale, op.Path)
		}
	}
	return nil
}

func ensureParent(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

func executeOp(op Op, crash crashFn) error {
	if err := GuardPath(op.Path); err != nil {
		return err
	}
	switch op.Kind {
	case OpWriteFile:
		if err := ensureParent(op.Path); err != nil {
			return err
		}
		return writeFileAtomic(op.Path, op.Data, op.Mode, crash)
	case OpDeleteFile:
		return os.Remove(op.Path)
	case OpDeleteTree:
		return removeTree(op.Path)
	case OpReplaceTree:
		if err := ensureParent(op.Path); err != nil {
			return err
		}
		return replaceTree(op, crash)
	}
	return fmt.Errorf("알 수 없는 작업: %s", op.Kind)
}

func replaceTree(op Op, crash crashFn) error {
	dir, base := filepath.Dir(op.Path), filepath.Base(op.Path)
	tmp := filepath.Join(dir, tempName(tmpPrefix, base))
	modeFn := SnapshotMode
	if op.Tree.Modes != nil {
		modes := op.Tree.Modes
		modeFn = func(rel string, src fs.FileMode) fs.FileMode {
			if m, ok := modes[rel]; ok {
				return fs.FileMode(m)
			}
			return SnapshotMode(rel, src)
		}
	}
	if err := copyTree(op.Tree.SrcDir, tmp, 0o755, modeFn); err != nil {
		_ = removeTree(tmp)
		return err
	}
	names := make([]string, 0, len(op.Tree.Extra))
	for n := range op.Tree.Extra {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(tmp, n), op.Tree.Extra[n], 0o644); err != nil {
			_ = removeTree(tmp)
			return err
		}
	}
	crash.at("tree-tmp-written")
	if op.Before.Exists {
		old := filepath.Join(dir, tempName(oldPrefix, base))
		if err := os.Rename(op.Path, old); err != nil {
			_ = removeTree(tmp)
			return err
		}
		crash.at("tree-old-renamed")
		if err := os.Rename(tmp, op.Path); err != nil {
			_ = os.Rename(old, op.Path)
			_ = removeTree(tmp)
			return err
		}
		crash.at("tree-new-renamed")
		return removeTree(old)
	}
	if err := os.Rename(tmp, op.Path); err != nil {
		_ = removeTree(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// rollback restores the first `upTo` entries (the last may be partially applied)
// from the backup, newest first, and removes directories the apply created.
func rollback(bdir string, m *Manifest, upTo int) error {
	var errs []string
	for i := upTo - 1; i >= 0; i-- {
		if err := restoreEntry(bdir, m.Entries[i]); err != nil {
			errs = append(errs, err.Error())
		}
	}
	removeCreatedDirs(m.CreatedDirs)
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func removeCreatedDirs(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // only succeeds when empty
	}
}

// restoreEntry puts one path back to its Before state using the backup copy.
func restoreEntry(bdir string, e Entry) error {
	if err := GuardPath(e.Path); err != nil {
		return err
	}
	if _, err := os.Lstat(e.Path); err == nil {
		fi, _ := os.Lstat(e.Path)
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlink, e.Path)
		}
	}
	if !e.Before.Exists {
		fi, err := os.Lstat(e.Path)
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			return removeTree(e.Path)
		}
		return os.Remove(e.Path)
	}
	src := backupFilePath(bdir, e.Path)
	switch e.Before.Kind {
	case "file":
		b, _, ok, err := ReadFileSafe(src)
		if err != nil || !ok {
			return fmt.Errorf("백업 파일을 읽을 수 없습니다: %s", e.Path)
		}
		if SHA256Hex(b) != e.Before.Hash {
			return fmt.Errorf("백업 파일이 손상되었습니다: %s", e.Path)
		}
		if err := ensureParent(e.Path); err != nil {
			return err
		}
		if fi, err := os.Lstat(e.Path); err == nil && fi.IsDir() {
			if err := removeTree(e.Path); err != nil {
				return err
			}
		}
		return WriteFileAtomic(e.Path, b, fs.FileMode(e.Before.Mode))
	case "dir":
		modes := map[string]uint32{}
		for _, f := range e.Before.Files {
			modes[f.Rel] = f.Mode
		}
		op := Op{Kind: OpReplaceTree, Path: e.Path, Tree: &TreeSpec{SrcDir: src, Modes: modes}}
		if fi, err := os.Lstat(e.Path); err == nil {
			op.Before = FileState{Exists: true}
			if !fi.IsDir() {
				if err := os.Remove(e.Path); err != nil {
					return err
				}
				op.Before = FileState{}
			}
		}
		if err := ensureParent(e.Path); err != nil {
			return err
		}
		return replaceTree(op, nil)
	}
	return fmt.Errorf("알 수 없는 백업 항목 종류: %s", e.Before.Kind)
}
