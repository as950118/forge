package profile

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Project identity (docs/profile.md "프로젝트 경로 동일성").
//
// The projects table stores one row per project path. On a case-insensitive file
// system (the macOS default) "x/proj" and "x/PROJ" are the same directory, and a
// shell keeps the spelling the user typed, so a plain string comparison would
// split one project into several rows with separate activations. Identity is
// therefore decided by the DIRECTORY: a path matches a stored row when the
// strings are equal or when both exist and os.SameFile says they are the same
// directory (same device and inode). Symlinks, "..", "." and trailing slashes are
// already folded by ProjectRoot before this point. On a case-sensitive file
// system "x/proj" and "x/PROJ" are different inodes, so they stay apart.

// FindProject returns the projects row for path: an exact string match first,
// else a row whose directory is the same directory as path (rows whose path no
// longer exists are skipped). found=false means no row refers to that directory.
func FindProject(ctx context.Context, q Querier, path string) (id int64, stored string, found bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT id, path FROM projects WHERE path = ?`, path).Scan(&id, &stored)
	if err == nil {
		return id, stored, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, err
	}
	fi, serr := os.Stat(path)
	if serr != nil {
		return 0, "", false, nil // not an existing directory: only the exact string can match
	}
	rows, err := q.QueryContext(ctx, `SELECT id, path FROM projects ORDER BY id`)
	if err != nil {
		return 0, "", false, err
	}
	type row struct {
		id   int64
		path string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.path); err != nil {
			_ = rows.Close()
			return 0, "", false, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, "", false, err
	}
	_ = rows.Close()
	for _, r := range all {
		if rfi, err := os.Stat(r.path); err == nil && os.SameFile(fi, rfi) {
			return r.id, r.path, true, nil
		}
	}
	return 0, "", false, nil
}

// LoosePath normalizes a project directory for LOOKUPS of rows that may point
// at a directory which no longer exists (profile unuse/current after the project
// was deleted or moved). An existing directory is canonicalized exactly like
// ProjectRoot. For a missing one only the string is normalized: absolute, cleaned,
// and the longest existing ancestor has its symlinks resolved (so /var/... and
// /private/var/... agree). It never touches the database and creates nothing.
func LoosePath(dir string) (string, error) {
	if root, err := ProjectRoot(dir); err == nil {
		return root, nil
	}
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rest := ""
	cur := abs
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// canonicalCase rewrites every existing component of an absolute path to the
// spelling stored on disk (only differs on case-insensitive file systems).
// Components it cannot verify keep their given spelling. Used only when a new
// projects row is written, so a later user typing another case still matches
// (FindProject) and the stored/printed path is the real one.
func canonicalCase(p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	vol := filepath.VolumeName(p)
	cur := vol + string(filepath.Separator)
	var out []string
	for _, comp := range strings.Split(strings.TrimPrefix(p, cur), string(filepath.Separator)) {
		if comp == "" {
			continue
		}
		out = append(out, realName(cur, comp))
		cur = filepath.Join(cur, out[len(out)-1])
	}
	return filepath.Join(cur[:len(vol)+1], filepath.Join(out...))
}

// realName returns the on-disk spelling of dir/name.
func realName(dir, name string) string {
	alt := swapCase(name)
	if alt == name {
		return name // no letters with case
	}
	a, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		return name
	}
	b, err := os.Lstat(filepath.Join(dir, alt))
	if err != nil || !os.SameFile(a, b) {
		return name // the other spelling is a different file or absent: case-sensitive, name is exact
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return name
	}
	for _, e := range ents {
		if e.Name() == name {
			return name
		}
	}
	for _, e := range ents {
		if strings.EqualFold(e.Name(), name) {
			if c, err := os.Lstat(filepath.Join(dir, e.Name())); err == nil && os.SameFile(a, c) {
				return e.Name()
			}
		}
	}
	return name
}

func swapCase(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsUpper(r):
			return unicode.ToLower(r)
		case unicode.IsLower(r):
			return unicode.ToUpper(r)
		}
		return r
	}, s)
}
