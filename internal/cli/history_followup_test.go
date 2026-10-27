package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/history"
	"github.com/heonjinjeong/forge/internal/store"
)

// TASK-0021 AC-6: cobra's English flag errors are Korean, --since garbage has a clear
// message, --output - is standard output.
func TestFlagErrorsAreKorean(t *testing.T) {
	initHome(t)
	cases := map[string][]string{
		"invalid int": {"history", "--limit", "abc"},
		"unknown":     {"history", "--nope"},
		"shorthand":   {"history", "-z"},
		"missing arg": {"history", "--since"},
		"top":         {"history", "top", "--limit", "x1"},
	}
	for name, args := range cases {
		_, err := runErr(t, "", args...)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		msg := err.Error()
		for _, en := range []string{"invalid argument", "unknown flag", "unknown shorthand", "flag needs an argument", "strconv"} {
			if strings.Contains(msg, en) {
				t.Errorf("%s: English text %q left in %q", name, en, msg)
			}
		}
		if !strings.Contains(msg, "--help") {
			t.Errorf("%s: no pointer to --help: %q", name, msg)
		}
	}
	_, err := runErr(t, "", "history", "--limit", "abc")
	if err == nil || !strings.Contains(err.Error(), `--limit 값 "abc"`) {
		t.Errorf("--limit abc: %v", err)
	}
	// echoed values never carry terminal escapes
	_, err = runErr(t, "", "history", "--limit", "\x1b[31mred")
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("escape echoed: %q", err)
	}
}

func TestSinceGarbageMessage(t *testing.T) {
	initHome(t)
	_, err := runErr(t, "", "history", "--since", "foo")
	if err == nil || !strings.Contains(err.Error(), "--since") || strings.Contains(err.Error(), "단위") {
		t.Errorf("--since foo: %v", err)
	}
}

func TestOutputDashIsStdout(t *testing.T) {
	initHome(t)
	dir := t.TempDir()
	t.Chdir(dir)
	out := mustRun(t, "history", "summary", "--format", "csv", "--output", "-")
	if !strings.HasPrefix(out, "kind,calls,") || strings.Contains(out, "저장했습니다") {
		t.Errorf("stdout output expected, got:\n%s", out)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("--output - created a file: %v", ents)
	}
	// a real file name still works and "./-" names a file called "-"
	mustRun(t, "history", "summary", "--format", "csv", "--output", "./-")
	if _, err := os.Stat(filepath.Join(dir, "-")); err != nil {
		t.Errorf("./- not written: %v", err)
	}
}

// AC-7: a large ledger without --since gets a hint on stderr (same buffer here), a
// small one does not.
func TestLargeLedgerHint(t *testing.T) {
	home := initHome(t)
	out := mustRun(t, "history", "summary")
	if strings.Contains(out, "--since 7d") {
		t.Errorf("small ledger must not get the hint:\n%s", out)
	}
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO ledger(id,ts,kind,name,source) VALUES (?,1,'skill','far','hook')`, history.LargeLedgerRows+1); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	for _, args := range [][]string{{"history", "summary"}, {"history", "top"}} {
		if out := mustRun(t, args...); !strings.Contains(out, "--since 7d") {
			t.Errorf("%v: no hint:\n%s", args, out)
		}
	}
	if out := mustRun(t, "history", "summary", "--since", "7d"); strings.Contains(out, "알림: 기록이 많아") {
		t.Errorf("--since given, no hint expected:\n%s", out)
	}
	if out := mustRun(t, "history"); strings.Contains(out, "알림: 기록이 많아") {
		t.Errorf("list does not scan everything, no hint expected:\n%s", out)
	}
}

// AC-8 end to end: the list explains hidden_by_profile.
func TestHistoryShowsHiddenByProfile(t *testing.T) {
	home := initHome(t)
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO ledger(ts,kind,name,client,source,ok,found,error_class) VALUES (strftime('%s','now')*1000,'tool','greet','claude-code','gateway',0,1,'hidden_by_profile')`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if out := mustRun(t, "history", "--name", "greet"); !strings.Contains(out, "hidden_by_profile") || !strings.Contains(out, "미등록이 아님") {
		t.Errorf("list:\n%s", out)
	}
	if out := mustRun(t, "history", "misses"); strings.Contains(out, "greet") {
		t.Errorf("a hidden tool is not a miss:\n%s", out)
	}
}
