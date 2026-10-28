package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RestoreOptions configures Restore.
type RestoreOptions struct {
	ApplyOptions
	// ID is a resolved backup id (see ResolveBackup).
	ID     string
	DryRun bool
	// Force restores although files were changed after the backed-up apply.
	Force bool
}

// RestoreEntry is one path of a restore plan.
type RestoreEntry struct {
	Path   string `json:"path"`
	Action string `json:"action"` // restore | delete | unchanged
	// Drift is set when the current content matches neither the state before the
	// apply nor the state right after it: someone edited the file since.
	Drift bool `json:"drift,omitempty"`
}

// RestoreResult reports a restore (or its dry run).
type RestoreResult struct {
	ID          string         `json:"id"`
	Target      string         `json:"target"`
	Scope       Scope          `json:"scope"`
	Root        string         `json:"root"`
	DryRun      bool           `json:"dry_run"`
	Entries     []RestoreEntry `json:"entries"`
	Drift       int            `json:"drift"`
	NewBackupID string         `json:"new_backup_id,omitempty"`
}

// ErrDrift is returned when files changed after the backed-up apply and --force is missing.
var ErrDrift = errors.New("백업 이후 사용자가 수정한 파일이 있습니다. 복원하면 그 수정이 사라집니다 (현재 상태는 새 백업으로 보존됩니다). 계속하려면 --force")

// ValidateManifest checks that every path in a manifest is something the adapter
// may write below the manifest root, so a tampered manifest cannot redirect a
// restore elsewhere (path escape, symlinks, the Claude Code user-level state file).
func ValidateManifest(a Adapter, m *Manifest) error {
	if m.Target != a.Name() {
		return fmt.Errorf("백업 대상(%q)과 어댑터(%q)가 다릅니다", cutStr(m.Target, 40), a.Name())
	}
	if m.Scope != ScopeProject && m.Scope != ScopeUser {
		return errors.New("백업 manifest 의 scope 가 올바르지 않습니다")
	}
	if !filepath.IsAbs(m.Root) || filepath.Clean(m.Root) != m.Root {
		return errors.New("백업 manifest 의 root 가 올바르지 않습니다")
	}
	for _, e := range m.Entries {
		if !filepath.IsAbs(e.Path) || filepath.Clean(e.Path) != e.Path {
			return fmt.Errorf("manifest 경로가 올바르지 않습니다: %q", cutStr(e.Path, 80))
		}
		if err := GuardPath(e.Path); err != nil {
			return err
		}
		base, err := a.AllowedPath(m.Scope, m.Root, e.Path)
		if err != nil {
			return err
		}
		if err := CheckNoSymlinks(base, e.Path); err != nil {
			return err
		}
	}
	for _, d := range m.CreatedDirs {
		if !filepath.IsAbs(d) || filepath.Clean(d) != d || !isBelowAny(d, m.Entries) {
			return fmt.Errorf("manifest 의 생성 디렉터리가 올바르지 않습니다: %q", cutStr(d, 80))
		}
		if err := GuardPath(d); err != nil {
			return err
		}
	}
	return nil
}

// Restore puts the files of a backup back. Before it changes anything it
// preserves the current state as a new backup (so a restore can be undone with
// `--restore latest`), and it refuses when files were edited after the backed-up
// apply unless Force is set.
func Restore(ctx context.Context, a Adapter, opts RestoreOptions) (*RestoreResult, error) {
	m, bdir, err := LoadManifest(opts.BackupDir, opts.ID)
	if err != nil {
		return nil, err
	}
	if m.Status == StatusRolledBack {
		return nil, errors.New("이 백업은 적용 실패 후 자동 롤백된 것이라 복원할 변경이 없습니다")
	}
	if err := ValidateManifest(a, m); err != nil {
		return nil, err
	}
	res := &RestoreResult{ID: m.ID, Target: m.Target, Scope: m.Scope, Root: m.Root, DryRun: opts.DryRun, Entries: []RestoreEntry{}}
	entries, createdDirs, err := effectiveEntries(a, opts.BackupDir, m, bdir)
	if err != nil {
		return nil, err
	}
	var ops []Op
	for _, se := range entries {
		e := se.Entry
		cur, err := Snapshot(e.Path)
		if err != nil {
			return nil, err
		}
		re := RestoreEntry{Path: e.Path}
		if cur.Equal(e.Before) {
			re.Action = "unchanged"
			res.Entries = append(res.Entries, re)
			continue
		}
		// After is "known" when its hash was recorded (or the path was deleted);
		// a directory of a crashed (pending) apply has no recorded hash.
		matchesAfter := (e.After.Hash != "" || !e.After.Exists) && cur.Equal(e.After)
		if !matchesAfter {
			re.Drift = true
			res.Drift++
		}
		op := Op{Path: e.Path, Before: cur}
		switch {
		case !e.Before.Exists:
			re.Action = "delete"
			op.Kind = OpDeleteFile
			if cur.Kind == "dir" {
				op.Kind = OpDeleteTree
			}
			if !cur.Exists {
				re.Action = "unchanged"
				res.Entries = append(res.Entries, re)
				continue
			}
		case e.Before.Kind == "file":
			re.Action = "restore"
			src := backupFilePath(se.Dir, e.Path)
			b, _, ok, err := ReadFileSafe(src)
			if err != nil || !ok || SHA256Hex(b) != e.Before.Hash {
				return nil, fmt.Errorf("백업 파일이 없거나 손상되었습니다: %s", e.Path)
			}
			op.Kind, op.Data, op.Mode = OpWriteFile, b, os.FileMode(e.Before.Mode)
		case e.Before.Kind == "dir":
			re.Action = "restore"
			src := backupFilePath(se.Dir, e.Path)
			h, _, err := TreeHash(src, false)
			if err != nil || h != e.Before.Hash {
				return nil, fmt.Errorf("백업 디렉터리가 없거나 손상되었습니다: %s", e.Path)
			}
			modes := map[string]uint32{}
			for _, f := range e.Before.Files {
				modes[f.Rel] = f.Mode
			}
			op.Kind, op.Tree = OpReplaceTree, &TreeSpec{SrcDir: src, Modes: modes}
		default:
			return nil, fmt.Errorf("알 수 없는 백업 항목: %s", e.Path)
		}
		res.Entries = append(res.Entries, re)
		ops = append(ops, op)
	}
	if opts.DryRun {
		return res, nil
	}
	if res.Drift > 0 && !opts.Force {
		return res, ErrDrift
	}
	if len(ops) > 0 {
		meta := manifestMeta{Kind: "restore", Target: m.Target, Scope: m.Scope, Root: m.Root, Note: "복원 직전 상태 (복원 대상 백업: " + m.ID + ")", RestoreOf: m.ID}
		ar, err := applyOps(ctx, a, meta, ops, opts.ApplyOptions)
		if err != nil {
			return res, err
		}
		res.NewBackupID = ar.BackupID
	}
	removeCreatedDirs(createdDirs)
	// This restore finishes an earlier, interrupted restore of the same backup:
	// the safety backup that one left is no longer a pending interruption.
	resumeInterruptedRestores(opts.BackupDir, m, res.NewBackupID)
	// Restoring an interrupted (still pending) backup is the user's choice to roll
	// it back: it is no longer an interruption the next sync should finish.
	if j, err := loadJournal(opts.BackupDir, m.ID); err == nil && j.Status == JournalPending {
		_ = markJournal(opts.BackupDir, m.ID, JournalRolledBack, "")
	}
	return res, nil
}

// isBelowAny reports whether dir is an ancestor of one of the entry paths
// (created directories only ever exist to hold an entry).
func isBelowAny(dir string, es []Entry) bool {
	for _, e := range es {
		if strings.HasPrefix(e.Path, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// srcEntry is a manifest entry together with the backup directory that holds its copy.
type srcEntry struct {
	Entry Entry
	Dir   string
}

// effectiveEntries returns what restoring m has to put back. Normally that is m's
// own entries. When m finished interrupted syncs (m.Supersedes) their changes are
// part of what the user wants undone: for a path that an interrupted sync touched,
// the state before the OLDEST interrupted sync is restored (from that backup),
// and paths only the interrupted sync touched are restored too. Superseded
// backups are validated like any manifest and must belong to the same target,
// scope and root.
func effectiveEntries(a Adapter, backupDir string, m *Manifest, bdir string) ([]srcEntry, []string, error) {
	if len(m.Supersedes) == 0 {
		own := make([]srcEntry, len(m.Entries))
		for i, e := range m.Entries {
			own[i] = srcEntry{Entry: e, Dir: bdir}
		}
		return own, m.CreatedDirs, nil
	}
	type loaded struct {
		m   *Manifest
		dir string
		j   *Journal
	}
	var chain []loaded // oldest first
	seen := map[string]bool{m.ID: true}
	var walk func(cur *Manifest, depth int) error
	walk = func(cur *Manifest, depth int) error {
		if depth > 8 {
			return errors.New("백업 체인이 너무 깁니다")
		}
		for _, id := range cur.Supersedes {
			if seen[id] {
				continue
			}
			seen[id] = true
			sm, sdir, err := LoadManifest(backupDir, id)
			if err != nil {
				return fmt.Errorf("복구된 이전 백업을 읽을 수 없습니다: %w", err)
			}
			if sm.Target != m.Target || sm.Scope != m.Scope || sm.Root != m.Root {
				return fmt.Errorf("복구된 이전 백업(%s)의 대상이 다릅니다", id)
			}
			if err := ValidateManifest(a, sm); err != nil {
				return err
			}
			if err := walk(sm, depth+1); err != nil {
				return err
			}
			j, _ := loadJournal(backupDir, id)
			chain = append(chain, loaded{sm, sdir, j})
		}
		return nil
	}
	if err := walk(m, 0); err != nil {
		return nil, nil, err
	}
	orig := map[string]srcEntry{}
	var order []string
	for _, l := range chain {
		for _, e := range l.m.Entries {
			if _, ok := orig[e.Path]; ok {
				continue // the oldest backup holds the original state
			}
			if l.j != nil { // the state the interrupted sync intended: what drift is measured against
				for _, jo := range l.j.Ops {
					if jo.Path == e.Path {
						e.After = FileState{Exists: jo.AfterExists, Hash: jo.AfterHash}
						if jo.AfterExists {
							if jo.Op == OpWriteFile {
								e.After.Kind = "file"
							} else {
								e.After.Kind = "dir"
							}
						}
					}
				}
			}
			orig[e.Path] = srcEntry{Entry: e, Dir: l.dir}
			order = append(order, e.Path)
		}
	}
	var out []srcEntry
	for _, e := range m.Entries {
		if o, ok := orig[e.Path]; ok {
			out = append(out, srcEntry{Entry: Entry{Path: e.Path, Before: o.Entry.Before, After: e.After}, Dir: o.Dir})
			delete(orig, e.Path)
			continue
		}
		out = append(out, srcEntry{Entry: e, Dir: bdir})
	}
	for _, p := range order {
		if o, ok := orig[p]; ok {
			out = append(out, o)
		}
	}
	created := append([]string{}, m.CreatedDirs...)
	for _, l := range chain {
		created = append(created, l.m.CreatedDirs...)
	}
	sort.SliceStable(created, func(i, j int) bool { return len(created[i]) < len(created[j]) }) // outermost first
	return out, created, nil
}

// legacyRestoreOf reports whether a restore journal without restore_of was
// putting back backup m: it has operations, and each one targets a path of m and
// ends in the state m recorded as "before" for it. Only a status flag depends on
// this; nothing is written or deleted because of it.
func legacyRestoreOf(j *Journal, m *Manifest) bool {
	if len(j.Ops) == 0 {
		return false
	}
	want := map[string]FileState{}
	for _, e := range m.Entries {
		want[e.Path] = e.Before
	}
	for _, o := range j.Ops {
		b, ok := want[o.Path]
		if !ok || o.AfterExists != b.Exists || (b.Exists && o.AfterHash != b.Hash) {
			return false
		}
	}
	return true
}

// resumeInterruptedRestores marks the pending journals of earlier restores of the
// same backup (same target, scope and root) as recovered by the restore that just
// completed, so a finished resume does not keep showing up as an interruption.
func resumeInterruptedRestores(backupDir string, m *Manifest, by string) {
	ents, err := os.ReadDir(backupDir)
	if err != nil {
		return
	}
	var ids []string
	for _, e := range ents {
		if !backupIDRe.MatchString(e.Name()) || !e.IsDir() || e.Name() == m.ID || e.Name() == by {
			continue
		}
		j, err := loadJournal(backupDir, e.Name())
		if err != nil || j.Status != JournalPending || j.Kind != "restore" || j.ID != e.Name() ||
			j.Target != m.Target || j.Scope != m.Scope || j.Root != m.Root {
			continue
		}
		// A journal written before restore_of existed names no backup: it is this
		// restore's only when every one of its operations puts back exactly what m
		// recorded for that path (TASK-0027 AC-17).
		if j.RestoreOf != m.ID && !(j.RestoreOf == "" && legacyRestoreOf(j, m)) {
			continue
		}
		ids = append(ids, e.Name())
	}
	_ = recoverJournals(backupDir, ids, by)
}
