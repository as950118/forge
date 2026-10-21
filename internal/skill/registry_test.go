package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func context_() context.Context { return context.Background() }

func addDir(t *testing.T, r *Registry, dir string) (*AddResult, error) {
	t.Helper()
	p, err := Prepare(context_(), dir, PrepareOptions{TmpDir: filepath.Join(r.Home, "tmp")})
	if err != nil {
		return nil, err
	}
	defer p.Cleanup()
	return r.Add(context_(), p)
}

func TestAddIdempotentAndVersions(t *testing.T) {
	r := newRegistry(t)
	d := dirSkill(t, "demo-skill", goodSkill, map[string]string{"ref/a.md": "a"})
	res, err := addDir(t, r, d)
	if err != nil || res.Status != "added" || !strings.HasPrefix(res.Version, "h-") {
		t.Fatalf("%v %+v", err, res)
	}
	final := filepath.Join(r.Home, "skills", "demo-skill@"+res.Version, "SKILL.md")
	st, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o222 != 0 {
		t.Errorf("snapshot should be read-only: %v", st.Mode())
	}
	// same content again: unchanged, no new rows
	res2, err := addDir(t, r, d)
	if err != nil || res2.Status != "unchanged" || res2.Hash != res.Hash {
		t.Fatalf("%v %+v", err, res2)
	}
	info, _ := r.Get(context_(), "demo-skill")
	if info.VersionCount != 1 {
		t.Errorf("versions=%d", info.VersionCount)
	}
	// changed content: new version becomes current, old snapshot kept
	writeFile(t, filepath.Join(d, "ref", "a.md"), "changed")
	res3, err := addDir(t, r, d)
	if err != nil || res3.Status != "new_version" || res3.Version == res.Version {
		t.Fatalf("%v %+v", err, res3)
	}
	if res3.PreviousVersion != res.Version {
		t.Errorf("PreviousVersion=%q want %q", res3.PreviousVersion, res.Version)
	}
	info, _ = r.Get(context_(), "demo-skill")
	if info.VersionCount != 2 || info.CurrentVersion != res3.Version {
		t.Errorf("%+v", info)
	}
	if _, err := os.Stat(filepath.Join(r.Home, "skills", "demo-skill@"+res.Version, "SKILL.md")); err != nil {
		t.Errorf("old version must remain: %v", err)
	}
}

func TestAddSameVersionDifferentContentRejected(t *testing.T) {
	r := newRegistry(t)
	md := "---\nname: v-skill\ndescription: d\nversion: \"1.0.0\"\n---\nbody1\n"
	d := dirSkill(t, "v-skill", md, nil)
	if _, err := addDir(t, r, d); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(d, "SKILL.md"), strings.Replace(md, "body1", "body2", 1))
	_, err := addDir(t, r, d)
	if err == nil || !strings.Contains(err.Error(), "불변") {
		t.Errorf("err=%v", err)
	}
	if _, e := os.Stat(filepath.Join(r.Home, "skills", "v-skill@1.0.0", "SKILL.md")); e != nil {
		t.Error("original snapshot lost")
	}
}

func TestEnableDisableRemove(t *testing.T) {
	r := newRegistry(t)
	d := dirSkill(t, "demo-skill", goodSkill, nil)
	res, err := addDir(t, r, d)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context_()
	if err := r.SetEnabled(ctx, "demo-skill", false); err != nil {
		t.Fatal(err)
	}
	if i, _ := r.Get(ctx, "demo-skill"); i.Enabled {
		t.Error("should be disabled")
	}
	if err := r.SetEnabled(ctx, "demo-skill", false); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := r.SetEnabled(ctx, "demo-skill", true); err != nil {
		t.Fatal(err)
	}
	if i, _ := r.Get(ctx, "demo-skill"); !i.Enabled {
		t.Error("should be enabled")
	}
	if err := r.SetEnabled(ctx, "nope", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v", err)
	}
	if err := r.Remove(ctx, "demo-skill"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "demo-skill"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Home, "skills", "demo-skill@"+res.Version)); !os.IsNotExist(err) {
		t.Errorf("snapshot not removed: %v", err)
	}
}

func TestPrepareRejectsSymlinkAndBadName(t *testing.T) {
	d := dirSkill(t, "demo-skill", goodSkill, nil)
	if err := os.Symlink("/etc/passwd", filepath.Join(d, "link")); err != nil {
		t.Skip("symlink unsupported")
	}
	if _, err := Prepare(context_(), d, PrepareOptions{TmpDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "심볼릭") {
		t.Errorf("err=%v", err)
	}
	bad := dirSkill(t, "x", "---\nname: Bad_Name\ndescription: d\n---\n", nil)
	if _, err := Prepare(context_(), bad, PrepareOptions{TmpDir: t.TempDir()}); err == nil {
		t.Error("expected name error")
	}
}

func TestHashTreeDeterministic(t *testing.T) {
	a := dirSkill(t, "a", goodSkill, map[string]string{"x/y.txt": "1", "z.txt": "2"})
	b := dirSkill(t, "b", goodSkill, map[string]string{"z.txt": "2", "x/y.txt": "1"})
	ha, _ := HashTree(a)
	hb, _ := HashTree(b)
	if ha != hb {
		t.Error("hash should not depend on creation order or dir name")
	}
	writeFile(t, filepath.Join(b, "z.txt"), "3")
	if hb2, _ := HashTree(b); hb2 == ha {
		t.Error("hash must change with content")
	}
}
