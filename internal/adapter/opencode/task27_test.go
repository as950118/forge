package opencode

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/adapter"
)

// TASK-0027 AC-12: a backup made while XDG_CONFIG_HOME pointed somewhere else (or
// was unset) cannot be restored with another value. It fails safely (nothing is
// written) and the message says what to do.
func TestRestoreAfterXDGConfigHomeChangedSaysHowToRecover(t *testing.T) {
	for name, after := range map[string]func(e *env, t *testing.T){
		"unset":     func(e *env, t *testing.T) { t.Setenv("XDG_CONFIG_HOME", "") },
		"other dir": func(e *env, t *testing.T) { t.Setenv("XDG_CONFIG_HOME", filepath.Join(e.home, "xdg-other")) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			standardRegistry(t, e)
			xdgC, _ := e.setXDG(t)
			_, res := e.mustSync(adapter.ScopeUser)
			skillDir := filepath.Join(xdgC, "opencode", "skills", "demo-skill")
			if !e.existsAbs(filepath.Join(skillDir, "SKILL.md")) {
				t.Fatal("the sync did not write below XDG_CONFIG_HOME")
			}
			after(e, t)
			_, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{
				ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID, Force: true})
			if err == nil {
				t.Fatal("a restore with a different XDG_CONFIG_HOME must fail")
			}
			for _, want := range []string{"쓸 수 없는 경로", "XDG_CONFIG_HOME", "되돌리고 다시 실행", "홈 디렉터리 밖"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error lacks %q: %v", want, err)
				}
			}
			if !e.existsAbs(filepath.Join(skillDir, "SKILL.md")) {
				t.Error("the failed restore changed the files")
			}
			// Putting the value back makes the same restore work.
			t.Setenv("XDG_CONFIG_HOME", xdgC)
			if _, err := adapter.Restore(context.Background(), e.ad, adapter.RestoreOptions{
				ApplyOptions: adapter.ApplyOptions{BackupDir: e.backupDir()}, ID: res.BackupID, Force: true}); err != nil {
				t.Fatalf("restore with the original value: %v", err)
			}
			if e.existsAbs(filepath.Join(skillDir, "SKILL.md")) {
				t.Error("the restore did not remove the skill that the backed-up sync had added")
			}
		})
	}
}

// Other disallowed paths do not get the XDG advice.
func TestAllowedPathHintOnlyForUserSkillShapes(t *testing.T) {
	e := newEnv(t)
	root := e.home
	_, err := e.ad.AllowedPath(adapter.ScopeUser, root, filepath.Join(root, "somewhere", "else.txt"))
	if err == nil || strings.Contains(err.Error(), "XDG_CONFIG_HOME") {
		t.Fatalf("unexpected hint or acceptance: %v", err)
	}
	_, err = e.ad.AllowedPath(adapter.ScopeProject, e.proj, filepath.Join(e.home, "x", "opencode", "skills", "demo-skill"))
	if err == nil || strings.Contains(err.Error(), "XDG_CONFIG_HOME") {
		t.Fatalf("project scope must not get the XDG hint: %v", err)
	}
}
