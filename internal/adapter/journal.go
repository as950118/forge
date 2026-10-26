package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The apply journal makes a killed sync recoverable.
//
// Before an apply changes anything it writes <backup>/journal.json (status
// "pending") and, next to the sidecar state file, a copy of it. The journal lists
// every path the apply will write (with the content hash before and after), the
// sidecar state the apply will end with (items + hash of its serialized content)
// and the backup id. The state file is the LAST operation of an apply. When
// everything is written the journal is marked "complete" and the copy removed.
//
// The next sync that gets the lock finds a pending journal, says so in its plan
// and, with --apply, finishes the job: it re-plans from what is on disk (so edits
// made since are respected) and treats an item as Forge-managed although the
// state file does not list it yet when, and only when, the journal lists it and
// the content on disk hashes to exactly the journaled value. Nothing is ever
// deleted because the journal says so: journal contents are compared against, never
// used as a path to write or remove, and paths in a journal are re-validated
// against the adapter's allowed paths before the journal is believed at all.
const JournalVersion = 1

// Journal statuses.
const (
	JournalPending    = "pending"     // the apply started and has not been marked finished
	JournalComplete   = "complete"    // finished normally
	JournalRolledBack = "rolled_back" // failed and was rolled back automatically
	JournalRecovered  = "recovered"   // a later sync finished (or confirmed) the work
)

// JournalOp is one planned file operation.
type JournalOp struct {
	Path         string `json:"path"`
	Op           OpKind `json:"op"`
	BeforeExists bool   `json:"before_exists"`
	BeforeHash   string `json:"before_hash,omitempty"`
	AfterExists  bool   `json:"after_exists"`
	AfterHash    string `json:"after_hash,omitempty"`
}

// Journal is <backup>/journal.json.
type Journal struct {
	Version   int         `json:"version"`
	ID        string      `json:"id"`
	CreatedAt string      `json:"created_at"`
	Kind      string      `json:"kind"` // "sync" | "restore"
	Target    string      `json:"target"`
	Scope     Scope       `json:"scope"`
	Root      string      `json:"root"`
	Status    string      `json:"status"`
	Ops       []JournalOp `json:"ops"`
	// StatePath/StateHash: the sidecar state file the apply ends with (sync only).
	StatePath string `json:"state_path,omitempty"`
	StateHash string `json:"state_hash,omitempty"`
	// Items is the content of that state file.
	Items       []StateItem `json:"items,omitempty"`
	RecoveredBy string      `json:"recovered_by,omitempty"`
	// RestoreOf: for a restore, the backup it was putting back (what to run again).
	RestoreOf string `json:"restore_of,omitempty"`
}

var hex64Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

func journalFile(bdir string) string { return filepath.Join(bdir, "journal.json") }

func (j *Journal) marshal() []byte {
	b, _ := json.MarshalIndent(j, "", "  ")
	return append(b, '\n')
}

// expectedTreeHash is the tree hash (TreeHash with skipMarker=false) the target
// directory will have once spec has been materialized.
func expectedTreeHash(spec *TreeSpec) (string, error) {
	type ent struct {
		exec bool
		size int64
		path string // source file, or "" for an extra
		data []byte
	}
	files := map[string]ent{}
	err := filepath.WalkDir(spec.SrcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("일반 파일이 아닌 항목: %s", p)
		}
		rel, _ := filepath.Rel(spec.SrcDir, p)
		rel = filepath.ToSlash(rel)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode := SnapshotMode(rel, fi.Mode())
		if m, ok := spec.Modes[rel]; ok {
			mode = fs.FileMode(m)
		}
		files[rel] = ent{exec: mode&0o111 != 0, size: fi.Size(), path: p}
		return nil
	})
	if err != nil {
		return "", err
	}
	for n, b := range spec.Extra {
		files[n] = ent{size: int64(len(b)), data: b}
	}
	rels := make([]string, 0, len(files))
	for r := range files {
		rels = append(rels, r)
	}
	sort.Strings(rels)
	h := sha256.New()
	for _, rel := range rels {
		e := files[rel]
		x := "-"
		if e.exec {
			x = "x"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", rel, x, e.size)
		if e.path != "" {
			b, err := os.ReadFile(e.path)
			if err != nil {
				return "", err
			}
			h.Write(b)
		} else {
			h.Write(e.data)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func journalOpFor(op Op) (JournalOp, error) {
	jo := JournalOp{Path: op.Path, Op: op.Kind, BeforeExists: op.Before.Exists, BeforeHash: op.Before.Hash}
	switch op.Kind {
	case OpWriteFile:
		jo.AfterExists, jo.AfterHash = true, SHA256Hex(op.Data)
	case OpReplaceTree:
		h, err := expectedTreeHash(op.Tree)
		if err != nil {
			return jo, err
		}
		jo.AfterExists, jo.AfterHash = true, h
	}
	return jo, nil
}

func buildJournal(m *Manifest, ops []Op, meta manifestMeta) (*Journal, error) {
	j := &Journal{Version: JournalVersion, ID: m.ID, CreatedAt: m.CreatedAt, Kind: m.Kind, Target: m.Target,
		Scope: m.Scope, Root: m.Root, Status: JournalPending, RestoreOf: m.RestoreOf}
	for _, op := range ops {
		jo, err := journalOpFor(op)
		if err != nil {
			return nil, err
		}
		j.Ops = append(j.Ops, jo)
	}
	if meta.State != nil && meta.StatePath != "" {
		j.StatePath = meta.StatePath
		j.Items = append([]StateItem{}, meta.State.Items...)
		for _, jo := range j.Ops {
			if jo.Path == meta.StatePath {
				j.StateHash = jo.AfterHash
			}
		}
	}
	return j, nil
}

func writeJournal(bdir string, j *Journal) error {
	return WriteFileAtomic(journalFile(bdir), j.marshal(), 0o600)
}

func loadJournal(backupDir, id string) (*Journal, error) {
	b, _, ok, err := ReadFileSafe(filepath.Join(backupDir, id, "journal.json"))
	if err != nil || !ok {
		return nil, errors.New("저널을 읽을 수 없습니다")
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, errors.New("저널이 손상되었습니다")
	}
	return &j, nil
}

// validateJournal decides whether a journal may be believed. Every path must be
// one the adapter may write below the journal root, so a tampered journal cannot
// name anything else (and nothing in it is ever deleted or written anyway).
func validateJournal(a Adapter, dirID string, j *Journal) error {
	if j.Version != JournalVersion || j.ID != dirID || !backupIDRe.MatchString(j.ID) {
		return errors.New("저널의 버전/id 가 올바르지 않습니다")
	}
	if j.Target != a.Name() {
		return errors.New("저널의 대상이 어댑터와 다릅니다")
	}
	if j.Scope != ScopeProject && j.Scope != ScopeUser {
		return errors.New("저널의 scope 가 올바르지 않습니다")
	}
	if !filepath.IsAbs(j.Root) || filepath.Clean(j.Root) != j.Root {
		return errors.New("저널의 root 가 올바르지 않습니다")
	}
	if j.Kind != "sync" && j.Kind != "restore" {
		return errors.New("저널의 종류가 올바르지 않습니다")
	}
	if j.RestoreOf != "" && (j.Kind != "restore" || !backupIDRe.MatchString(j.RestoreOf)) {
		return errors.New("저널의 복원 대상 id 가 올바르지 않습니다")
	}
	if len(j.Ops) > 100000 || len(j.Items) > 100000 {
		return errors.New("저널이 너무 큽니다")
	}
	check := func(p string) error {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("저널 경로가 올바르지 않습니다: %q", cutStr(p, 80))
		}
		if err := GuardPath(p); err != nil {
			return err
		}
		base, err := a.AllowedPath(j.Scope, j.Root, p)
		if err != nil {
			return err
		}
		return CheckNoSymlinks(base, p)
	}
	stateOpHash := ""
	for _, o := range j.Ops {
		if err := check(o.Path); err != nil {
			return err
		}
		switch o.Op {
		case OpWriteFile, OpDeleteFile, OpReplaceTree, OpDeleteTree:
		default:
			return errors.New("저널의 작업 종류가 올바르지 않습니다")
		}
		if (o.BeforeHash != "" && !hex64Re.MatchString(o.BeforeHash)) || (o.AfterHash != "" && !hex64Re.MatchString(o.AfterHash)) {
			return errors.New("저널의 해시가 올바르지 않습니다")
		}
		if j.StatePath != "" && o.Path == j.StatePath {
			stateOpHash = o.AfterHash
		}
	}
	if j.StatePath != "" {
		if err := check(j.StatePath); err != nil {
			return err
		}
		for _, it := range j.Items {
			switch it.Kind {
			case KindSkill, KindMCP, KindHook:
			default:
				return errors.New("저널 항목의 종류가 올바르지 않습니다")
			}
			if it.Name == "" || len(it.Name) > 1024 || !hex64Re.MatchString(it.Hash) {
				return errors.New("저널 항목이 올바르지 않습니다")
			}
		}
		st := &State{Version: StateVersion, Target: j.Target, Scope: j.Scope, Items: j.Items}
		if st.Items == nil {
			st.Items = []StateItem{}
		}
		if j.StateHash != "" && SHA256Hex(st.Marshal()) != j.StateHash {
			return errors.New("저널의 상태 해시가 항목 목록과 일치하지 않습니다")
		}
		if stateOpHash != "" && j.StateHash != stateOpHash {
			return errors.New("저널의 상태 해시가 작업 목록과 일치하지 않습니다")
		}
	} else if len(j.Items) > 0 {
		return errors.New("저널에 상태 경로 없이 항목이 있습니다")
	}
	return nil
}

// InterruptedInfo describes a sync that was killed before it finished.
type InterruptedInfo struct {
	ID        string `json:"backup_id"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
	Ops       int    `json:"ops"`
	// RestoreOf: for an interrupted restore, the backup it was restoring. THAT id
	// is what to run again; ID is only the safety backup of the state before it.
	RestoreOf string `json:"restore_of,omitempty"`
	// Done are paths already in their final state, Todo still in their original
	// state, Changed neither (edited since, or in an intermediate state).
	Done    int `json:"done"`
	Todo    int `json:"todo"`
	Changed int `json:"changed"`
}

// Detection is the outcome of looking for interrupted syncs.
type Detection struct {
	// Journals are the believable pending journals of this target+root, oldest first.
	Journals []*Journal
	Infos    []InterruptedInfo
	// Invalid lists backup ids whose pending journal could not be believed.
	Invalid []string
}

// DetectInterrupted finds pending journals for this target/scope/root. It only
// reads. Journals of other roots are ignored; unreadable or forged ones are
// reported in Invalid and never used.
func DetectInterrupted(a Adapter, backupDir string, scope Scope, root string) *Detection {
	d := &Detection{}
	ents, err := os.ReadDir(backupDir)
	if err != nil {
		return d
	}
	var ids []string
	for _, e := range ents {
		if backupIDRe.MatchString(e.Name()) && e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		j, err := loadJournal(backupDir, id)
		if err != nil {
			continue // no journal (old backup, or killed before it was written: nothing was changed)
		}
		if j.Status != JournalPending {
			continue
		}
		if !validateJournalIdentity(j) {
			d.Invalid = append(d.Invalid, id)
			continue
		}
		if j.Root != root || j.Scope != scope || j.Target != a.Name() {
			continue // someone else's journal
		}
		if err := validateJournal(a, id, j); err != nil {
			d.Invalid = append(d.Invalid, id)
			continue
		}
		d.Journals = append(d.Journals, j)
		d.Infos = append(d.Infos, progressOf(j))
	}
	return d
}

// validateJournalIdentity is true when the journal at least names a plausible
// other root (so it is silently ignored instead of reported as forged).
func validateJournalIdentity(j *Journal) bool {
	return j.Version == JournalVersion && filepath.IsAbs(j.Root) && filepath.Clean(j.Root) == j.Root &&
		(j.Scope == ScopeProject || j.Scope == ScopeUser)
}

func progressOf(j *Journal) InterruptedInfo {
	info := InterruptedInfo{ID: j.ID, Kind: j.Kind, CreatedAt: j.CreatedAt, Ops: len(j.Ops), RestoreOf: j.RestoreOf}
	for _, o := range j.Ops {
		cur, err := Snapshot(o.Path)
		switch {
		case err != nil:
			info.Changed++
		case cur.Exists == o.BeforeExists && cur.Hash == o.BeforeHash:
			info.Todo++
		case cur.Exists == o.AfterExists && cur.Hash == o.AfterHash:
			info.Done++
		default:
			info.Changed++
		}
	}
	return info
}

// markJournal sets the status of a journal (atomically). Used when an apply
// finishes, rolls back, or a later sync recovers it.
func markJournal(backupDir, id, status, by string) error {
	j, err := loadJournal(backupDir, id)
	if err != nil {
		return err
	}
	j.Status, j.RecoveredBy = status, by
	return WriteFileAtomic(filepath.Join(backupDir, id, "journal.json"), j.marshal(), 0o600)
}

// recoverJournals marks the interrupted syncs as recovered (their manifests too,
// so --list-backups shows it). Failures are returned but never fatal: a journal
// that stays pending only makes the next sync re-check an already converged state.
func recoverJournals(backupDir string, ids []string, by string) error {
	var errs []string
	for _, id := range ids {
		if err := markJournal(backupDir, id, JournalRecovered, by); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if m, _, err := LoadManifest(backupDir, id); err == nil && m.Status == StatusPending {
			m.Status = StatusRecovered
			_ = writeManifest(filepath.Join(backupDir, id), m)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
