package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Backup status values.
const (
	StatusPending    = "pending"     // changes may be partially applied (crash)
	StatusApplied    = "applied"     // fully applied
	StatusRolledBack = "rolled_back" // apply failed and was rolled back automatically
	StatusRecovered  = "recovered"   // a crashed (pending) apply that a later sync finished
)

// DefaultKeepBackups is the retention limit reported by `forge sync` (never enforced
// automatically: only --prune-backups deletes backups).
const DefaultKeepBackups = 20

var backupIDRe = regexp.MustCompile(`^\d{8}T\d{6}\.\d{3}Z-[0-9a-f]{6}$`)

// ValidBackupID reports whether id has the shape of a backup directory name.
func ValidBackupID(id string) bool { return backupIDRe.MatchString(id) }

// Entry records one path touched by an apply: how it looked before (and where
// the copy is), and how it looked right after.
type Entry struct {
	Path   string    `json:"path"`
	Before FileState `json:"before"`
	After  FileState `json:"after"`
}

// Manifest is <backup>/manifest.json.
type Manifest struct {
	Version     int      `json:"version"`
	ID          string   `json:"id"`
	CreatedAt   string   `json:"created_at"`
	Kind        string   `json:"kind"` // "sync" | "restore"
	Target      string   `json:"target"`
	Scope       Scope    `json:"scope"`
	Root        string   `json:"root"`
	Status      string   `json:"status"`
	Entries     []Entry  `json:"entries"`
	CreatedDirs []string `json:"created_dirs,omitempty"`
	Note        string   `json:"note,omitempty"`
	// Supersedes lists interrupted syncs (backup ids) that this apply finished.
	// Restoring this backup also undoes their changes (see Restore).
	Supersedes []string `json:"supersedes,omitempty"`
}

// BackupInfo is one row of --list-backups.
type BackupInfo struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	CreatedAt string `json:"created_at,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Target    string `json:"target,omitempty"`
	Scope     Scope  `json:"scope,omitempty"`
	Root      string `json:"root,omitempty"`
	Status    string `json:"status,omitempty"`
	Files     int    `json:"files"`
	Error     string `json:"error,omitempty"`
}

// backupFilePath is where the copy of an original path lives inside a backup.
// It is derived from the path, never taken from the manifest.
func backupFilePath(backupDir, orig string) string {
	rel := strings.TrimPrefix(filepath.ToSlash(orig), "/")
	rel = strings.ReplaceAll(rel, ":", "_")
	return filepath.Join(backupDir, "files", filepath.FromSlash(rel))
}

func writeManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o600)
}

// LoadManifest reads <backupDir>/<id>/manifest.json. id must be a backup id.
func LoadManifest(backupDir, id string) (*Manifest, string, error) {
	if !backupIDRe.MatchString(id) {
		return nil, "", fmt.Errorf("백업 id 형식이 올바르지 않습니다: %q", id)
	}
	dir := filepath.Join(backupDir, id)
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, "", fmt.Errorf("백업을 찾을 수 없습니다: %s", id)
	}
	if !fi.IsDir() {
		return nil, "", fmt.Errorf("백업이 디렉터리가 아닙니다: %s", id)
	}
	b, _, ok, err := ReadFileSafe(filepath.Join(dir, "manifest.json"))
	if err != nil || !ok {
		return nil, "", fmt.Errorf("백업 manifest 를 읽을 수 없습니다: %s", id)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, "", fmt.Errorf("백업 manifest 가 손상되었습니다: %s", id)
	}
	if m.ID != id {
		return nil, "", fmt.Errorf("백업 manifest 의 id 가 디렉터리와 다릅니다: %s", id)
	}
	return &m, dir, nil
}

// ListBackups returns all backups, oldest first.
func ListBackups(backupDir string) ([]BackupInfo, error) {
	ents, err := os.ReadDir(backupDir)
	if errors.Is(err, fs.ErrNotExist) {
		return []BackupInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []BackupInfo{}
	for _, e := range ents {
		if !backupIDRe.MatchString(e.Name()) || !e.IsDir() {
			continue
		}
		bi := BackupInfo{ID: e.Name(), Path: filepath.Join(backupDir, e.Name())}
		m, _, err := LoadManifest(backupDir, e.Name())
		if err != nil {
			bi.Error = err.Error()
		} else {
			bi.CreatedAt, bi.Kind, bi.Target, bi.Scope, bi.Root, bi.Status, bi.Files =
				m.CreatedAt, m.Kind, m.Target, m.Scope, m.Root, m.Status, len(m.Entries)
		}
		out = append(out, bi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ResolveBackup maps "latest", an exact id or a unique id prefix to a backup id.
// "latest" is the newest backup that was not rolled back.
func ResolveBackup(backupDir, ref string) (string, error) {
	all, err := ListBackups(backupDir)
	if err != nil {
		return "", err
	}
	if ref == "latest" {
		for i := len(all) - 1; i >= 0; i-- {
			if all[i].Error == "" && all[i].Status != StatusRolledBack {
				return all[i].ID, nil
			}
		}
		return "", errors.New("복원할 백업이 없습니다")
	}
	var match []string
	for _, b := range all {
		if b.ID == ref {
			return b.ID, nil
		}
		if ref != "" && strings.HasPrefix(b.ID, ref) {
			match = append(match, b.ID)
		}
	}
	switch len(match) {
	case 0:
		return "", fmt.Errorf("백업을 찾을 수 없습니다: %q (forge sync --list-backups)", cutStr(ref, 40))
	case 1:
		return match[0], nil
	}
	return "", fmt.Errorf("백업 id %q 가 여러 개와 일치합니다. 더 길게 지정하세요", cutStr(ref, 40))
}

func cutStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// PruneBackups deletes all but the newest keep backups. It only ever removes
// directories that look like Forge backups (exact id pattern), never follows links.
func PruneBackups(backupDir string, keep int, dryRun bool) ([]string, error) {
	if keep < 1 {
		return nil, errors.New("--keep 은 1 이상이어야 합니다")
	}
	all, err := ListBackups(backupDir)
	if err != nil {
		return nil, err
	}
	if len(all) <= keep {
		return nil, nil
	}
	var removed []string
	for _, b := range all[:len(all)-keep] {
		removed = append(removed, b.ID)
		if dryRun {
			continue
		}
		p := filepath.Join(backupDir, b.ID)
		fi, err := os.Lstat(p)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := removeTree(p); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func newBackupID(now time.Time) string {
	return now.UTC().Format("20060102T150405.000Z") + "-" + randHex(3)
}
