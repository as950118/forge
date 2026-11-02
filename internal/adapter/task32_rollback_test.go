package adapter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/adapter"
)

// TASK-0032 AC-11. A failure after an operation that created directories and
// completed: the rollback must undo that operation AND remove the directories the
// apply created (skills directory, the .forge state directory that also holds the
// journal copy), without any file-size limit or permission trick (so it also runs
// as root and where RLIMIT_FSIZE is unavailable). The failure is injected before
// each operation k >= 1 in turn.
//
// Mutations this catches: rollback never called (operation 0's skill directory
// stays), restoreEntry skipped for completed operations, and removal of the created
// directories at BOTH places that do it (rollback() and the explicit call after the
// journal copy is dropped). Removing only the explicit call after dropJournalCopy
// is an equivalent mutant: rollback() already removed the same directories
// (m.CreatedDirs == created), and the journal copy's own directory is removed by
// dropJournalCopy; docs/adapter.md records this.
func TestSharedRollbackAfterCompletedOperationRemovesCreatedDirs(t *testing.T) {
	for _, tg := range conformTargets {
		probe := newConformProject(t, tg)
		pp, err := probe.plan(t, []adapter.SkillInput{conformSkill(t, probe.forge, "a-one"), conformSkill(t, probe.forge, "b-two")})
		if err != nil {
			t.Fatal(err)
		}
		n := len(pp.Ops)
		if n < 3 {
			t.Fatalf("%s: want at least 3 operations, got %d", tg.name, n)
		}
		for k := 1; k < n; k++ {
			k := k
			t.Run(tg.name+"/fail-before-op-"+string(rune('0'+k)), func(t *testing.T) {
				c := newConformProject(t, tg)
				skills := []adapter.SkillInput{conformSkill(t, c.forge, "a-one"), conformSkill(t, c.forge, "b-two")}
				p, err := c.plan(t, skills)
				if err != nil {
					t.Fatal(err)
				}
				roots := []string{c.proj, filepath.Join(c.forge, "sync-state")}
				before := treeSig(t, roots...)
				boom := errors.New("injected failure")
				_, aerr := adapter.Apply(context.Background(), c.ad, p, adapter.ApplyOptions{
					BackupDir: filepath.Join(c.forge, "backups"),
					Inject: func(step int, op adapter.Op) error {
						if step == k {
							return boom
						}
						return nil
					}})
				if aerr == nil || !strings.Contains(aerr.Error(), "롤백했습니다") {
					t.Fatalf("want an automatic rollback, got: %v", aerr)
				}
				after := treeSig(t, roots...)
				for path, v := range before {
					if after[path] != v {
						t.Errorf("not restored: %s: %q -> %q", path, v, after[path])
					}
				}
				for path := range after {
					if _, ok := before[path]; !ok {
						t.Errorf("left behind (created directory or file not removed): %s", path)
					}
				}
				if _, err := os.Stat(filepath.Join(c.proj, ".forge", "sync-journal.json")); err == nil {
					t.Error("journal copy left behind")
				}
				p2, err := c.plan(t, skills)
				if err != nil {
					t.Fatal(err)
				}
				if len(p2.Interrupted) != 0 || len(p2.StaleTemps) != 0 {
					t.Fatalf("a rolled back apply left a trace: %+v %v", p2.Interrupted, p2.StaleTemps)
				}
			})
		}
	}
}
