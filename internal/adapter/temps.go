package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// Temporary files and directories that an apply creates next to its targets
// (temp file + rename, tree replacement) are named
//
//	.forge-tmp-<pid>-<8 hex>-<target name>     new content being prepared
//	.forge-old-<pid>-<8 hex>-<target name>     previous tree moved aside
//
// A sync killed in the middle leaves them behind. The sync that holds the lock
// removes the ones left by dead processes (CleanStaleTemps); a plan only reports
// them (FindStaleTemps). Only names with exactly this shape are ever touched.
const (
	tmpPrefix = ".forge-tmp-"
	oldPrefix = ".forge-old-"
)

var tempRe = regexp.MustCompile(`^\.forge-(?:tmp|old)-(\d{1,7})-[0-9a-f]{8}-.+$`)

func tempName(prefix, base string) string {
	return fmt.Sprintf("%s%d-%s-%s", prefix, os.Getpid(), randHex(4), base)
}

// A backup directory holds two files written with the same temp-file + rename
// primitive: manifest.json and journal.json. A sync killed while writing one of
// them leaves .forge-tmp-<pid>-<8 hex>-manifest.json (or -journal.json) there; no
// later apply touches that directory again, so they are looked for explicitly.
// Only those two names are accepted.
var backupTempRe = regexp.MustCompile(`^\.forge-tmp-(\d{1,7})-[0-9a-f]{8}-(?:manifest|journal)\.json$`)

// staleTempCandidates lists entries of dir that look like our leftovers, without
// following symbolic links and without touching anything.
func staleTempCandidates(dir string) []string { return staleCandidates(dir, tempRe, true) }

// staleBackupTempCandidates lists leftover manifest/journal temp files (regular
// files only) at the top of every backup directory below backupDir. Links are never
// followed: neither backupDir's entries nor the temp files themselves.
func staleBackupTempCandidates(backupDir string) []string {
	if backupDir == "" {
		return nil
	}
	fi, err := os.Lstat(backupDir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	ents, err := os.ReadDir(backupDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !backupIDRe.MatchString(e.Name()) || e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		out = append(out, staleCandidates(filepath.Join(backupDir, e.Name()), backupTempRe, false)...)
	}
	return out
}

// FindStaleBackupTemps lists (sorted) the leftover temp files of killed syncs in
// the backup directories.
func FindStaleBackupTemps(backupDir string) []string {
	out := staleBackupTempCandidates(backupDir)
	sort.Strings(out)
	return out
}

// CleanStaleBackupTemps removes what FindStaleBackupTemps lists (the caller holds
// the sync lock).
func CleanStaleBackupTemps(backupDir string) []string {
	var removed []string
	for _, p := range FindStaleBackupTemps(backupDir) {
		if li, err := os.Lstat(p); err != nil || !li.Mode().IsRegular() {
			continue
		}
		if os.Remove(p) == nil {
			removed = append(removed, p)
		}
	}
	return removed
}

func staleCandidates(dir string, re *regexp.Regexp, allowDirs bool) []string {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		m := re.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || (pid != os.Getpid() && processAlive(pid)) {
			continue // possibly still in use by a running process
		}
		p := filepath.Join(dir, e.Name())
		li, err := os.Lstat(p)
		if err != nil || li.Mode()&os.ModeSymlink != 0 {
			continue // never follow (or delete through) a link
		}
		if !li.IsDir() && !li.Mode().IsRegular() {
			continue
		}
		if li.IsDir() && !allowDirs {
			continue
		}
		out = append(out, p)
	}
	return out
}

// FindStaleTemps lists leftover temporary files in the given directories (sorted).
func FindStaleTemps(dirs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		for _, p := range staleTempCandidates(d) {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// CleanStaleTemps removes the leftovers FindStaleTemps would list. The caller
// holds the sync lock, so no live Forge process is writing next to them.
func CleanStaleTemps(dirs []string) []string {
	var removed []string
	for _, p := range FindStaleTemps(dirs) {
		li, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if li.IsDir() {
			err = removeTree(p)
		} else {
			err = os.Remove(p)
		}
		if err == nil {
			removed = append(removed, p)
		}
	}
	return removed
}
