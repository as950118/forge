package skill

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func prepForRestage(t *testing.T) (*Prepared, string, string) {
	t.Helper()
	dir := dirSkill(t, "demo-skill", goodSkill, map[string]string{"a/b.txt": "x"})
	p, err := Prepare(context_(), dir, PrepareOptions{TmpDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Cleanup)
	h, err := HashTree(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	return p, h, t.TempDir()
}

func under(root, dir string) bool {
	rel, err := filepath.Rel(dir, root)
	return err == nil && !strings.HasPrefix(rel, "..")
}

// TASK-0025: Restage moves the tree under tmpDir with the approved hash.
func TestRestageMovesTreeUnderTmpWithApprovedHash(t *testing.T) {
	p, h, tmp := prepForRestage(t)
	old := p.Root
	if err := p.Restage(tmp, h); err != nil {
		t.Fatal(err)
	}
	if p.Root == old || !under(p.Root, tmp) {
		t.Fatalf("Root must move under tmpDir: %s", p.Root)
	}
	if got, err := HashTree(p.Root); err != nil || got != h {
		t.Fatalf("hash after move %s %v want %s", got, err, h)
	}
	p.Cleanup()
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("staging left behind: %v", ents)
	}
}

// The staging is replaced after the hash was approved: nothing is moved.
func TestRestageRefusesStagingChangedAfterApproval(t *testing.T) {
	p, h, tmp := prepForRestage(t)
	old := p.Root
	if err := os.WriteFile(filepath.Join(p.Root, "SKILL.md"), []byte(goodSkill+"\n# injected\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := p.Restage(tmp, h)
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("want ErrHashMismatch, got %v", err)
	}
	if p.Root != old {
		t.Error("Root must not change on failure")
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("new staging must be removed on failure: %v", ents)
	}
}

func TestRestageRefusesWrongOrMissingApprovedHash(t *testing.T) {
	p, _, tmp := prepForRestage(t)
	for _, h := range []string{"", strings.Repeat("0", 64)} {
		if err := p.Restage(tmp, h); err == nil {
			t.Errorf("hash %q must be refused", h)
		}
	}
	if ents, _ := os.ReadDir(tmp); len(ents) != 0 {
		t.Errorf("leftovers: %v", ents)
	}
}

// Registry.Add refuses a tree that is not the approved one (change after Restage).
func TestAddRefusesTreeChangedAfterRestage(t *testing.T) {
	r := newRegistry(t)
	p, h, _ := prepForRestage(t)
	if err := p.Restage(filepath.Join(r.Home, "tmp"), h); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Root, "SKILL.md"), []byte(goodSkill+"\n# late\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(context_(), p); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("Add must refuse: %v", err)
	}
}
