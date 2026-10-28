package profile

import (
	"context"
	"os"
)

// maxDupScan bounds how many projects rows DuplicateProjects stats.
const maxDupScan = 5000

// DuplicateGroup is a set of projects rows whose paths are the same directory
// (same device and inode): typically "x/proj" and "x/PROJ" on a case-insensitive
// file system, from rows written before FindProject folded the spellings.
type DuplicateGroup struct {
	Paths []string `json:"paths"`
}

// DuplicateProjects lists groups of projects rows that name the same existing
// directory (read-only; stats at most maxDupScan rows). The rows are NOT merged:
// each has its own activations and project profiles, so merging would silently
// change which profile applies. A path that cannot be stat-ed (removed, no
// permission, stalled mount) is skipped: "same directory" is then unknown.
func DuplicateProjects(ctx context.Context, q Querier) ([]DuplicateGroup, error) {
	rows, err := q.QueryContext(ctx, `SELECT path FROM projects ORDER BY id LIMIT ?`, maxDupScan)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return nil, err
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	type seen struct {
		fi    os.FileInfo
		group int
	}
	var firsts []seen
	var groups []DuplicateGroup
	for _, p := range paths {
		if ctx.Err() != nil {
			return groups, ctx.Err()
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		matched := false
		for _, s := range firsts {
			if os.SameFile(fi, s.fi) {
				groups[s.group].Paths = append(groups[s.group].Paths, p)
				matched = true
				break
			}
		}
		if !matched {
			firsts = append(firsts, seen{fi, len(groups)})
			groups = append(groups, DuplicateGroup{Paths: []string{p}})
		}
	}
	var out []DuplicateGroup
	for _, g := range groups {
		if len(g.Paths) > 1 {
			out = append(out, g)
		}
	}
	return out, nil
}
