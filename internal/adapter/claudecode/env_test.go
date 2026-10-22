package claudecode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/skill"
	"github.com/heonjinjeong/forge/internal/store"
)

// Supported Claude Code versions and the status of each format item used here.
//
//	2.1.305  (docs/clients/claude-code.md, checked 2026-10-21)
//	  [doc]     skills: <repo>/.claude/skills/<name>/SKILL.md, ~/.claude/skills/<name>/
//	  [doc]     .mcp.json: top-level "mcpServers"; stdio {type,command,args,env},
//	            http/sse {type,url,headers}; "type" must always be explicit;
//	            ${VAR} expansion in command/args/env/url/headers
//	  [doc]     settings.json "hooks": event -> [ {matcher, hooks:[{type:"command",command,timeout}]} ]
//	  [local]   a real user settings.json contains a Notification hook of this shape
//	  [미확인]  the key layout of the Claude Code user-level state file: never read or
//	            written; user-scope MCP servers are printed as `claude mcp add` commands
//	  [미확인]  `claude mcp add` option order/variadic handling and ${VAR} handling for
//	            user-scope servers (the commands are only printed, never executed)
//	  [미확인]  whether unknown fields in .mcp.json/settings.json are rejected: the adapter
//	            never adds any
//	  [미확인]  behaviour of Skill/UserPromptExpansion hook payloads (ledger concern)
const fixtureVersion = "2.1.305"

var fixedNow = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)

type env struct {
	t         *testing.T
	home      string // HOME
	forgeHome string
	proj      string
	conn      *sql.DB
	sreg      *skill.Registry
	mreg      *mcp.Registry
	ad        *Adapter
	decoy     string
}

// newEnv isolates HOME, FORGE_HOME and the project in temp directories and
// plants a decoy Claude Code user file that must never be touched.
func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	// Resolve symlinks (macOS /var -> /private/var) so test paths equal plan paths.
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, home: filepath.Join(base, "home"), forgeHome: filepath.Join(base, "forge"), proj: filepath.Join(base, "proj")}
	for _, d := range []string{e.home, e.proj, filepath.Join(e.proj, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", e.home)
	t.Setenv("FORGE_HOME", e.forgeHome)
	t.Cleanup(func() { // read-only snapshots / readonly test dirs
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	if err := store.EnsureLayout(e.forgeHome); err != nil {
		t.Fatal(err)
	}
	e.conn, err = store.Open(context.Background(), store.DBPath(e.forgeHome))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.conn.Close() })
	e.sreg = &skill.Registry{DB: e.conn, Home: e.forgeHome}
	e.mreg = &mcp.Registry{DB: e.conn}
	e.ad = New(e.forgeHome)
	e.decoy = filepath.Join(e.home, ".claude"+".json")
	if err := os.WriteFile(e.decoy, []byte(`{"decoy":"DECOY-NEVER-READ"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(e.decoy, old, old); err != nil {
		t.Fatal(err)
	}
	// Mode 0000: any attempt to open the decoy fails with EACCES, so a read would
	// surface as an error instead of going unnoticed (atime is only checked as a
	// bonus: relatime/APFS do not reliably update it).
	if err := os.Chmod(e.decoy, 0); err != nil {
		t.Fatal(err)
	}
	return e
}

type fileSig struct {
	hash         string
	mtime, atime time.Time
}

func (e *env) decoySig() fileSig {
	if err := os.Chmod(e.decoy, 0o600); err != nil {
		e.t.Fatal(err)
	}
	b, err := os.ReadFile(e.decoy)
	_ = os.Chmod(e.decoy, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	fi, _ := os.Stat(e.decoy) // after our own read, so that read is not counted against the code under test
	return fileSig{hash: adapter.SHA256Hex(b), mtime: fi.ModTime(), atime: atimeOf(fi)}
}

// checkDecoy asserts the decoy file is byte-identical with unchanged mtime and atime.
func (e *env) checkDecoy(before fileSig) {
	e.t.Helper()
	after := e.decoySig()
	if after.hash != before.hash || !after.mtime.Equal(before.mtime) {
		e.t.Fatal("decoy Claude Code user file was modified")
	}
	if !after.atime.Equal(before.atime) {
		e.t.Fatalf("decoy Claude Code user file was read (atime %v -> %v)", before.atime, after.atime)
	}
}

func (e *env) addSkill(name, version string, extra map[string]string) {
	e.t.Helper()
	d := filepath.Join(e.t.TempDir(), name)
	md := fmt.Sprintf("---\nname: %s\ndescription: %s skill.\nversion: %s\n---\n# %s %s\n", name, name, version, name, version)
	files := map[string]string{"SKILL.md": md}
	for k, v := range extra {
		files[k] = v
	}
	for rel, c := range files {
		p := filepath.Join(d, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(c), mode); err != nil {
			e.t.Fatal(err)
		}
	}
	p, err := skill.Prepare(context.Background(), d, skill.PrepareOptions{TmpDir: filepath.Join(e.forgeHome, "tmp")})
	if err != nil {
		e.t.Fatal(err)
	}
	defer p.Cleanup()
	if _, err := e.sreg.Add(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) addServer(s mcp.Spec) {
	e.t.Helper()
	if err := e.mreg.Add(context.Background(), s); err != nil {
		e.t.Fatal(err)
	}
}

func envRef(slot, key, name string) mcp.Value {
	v, err := mcp.ParseValue(slot, key+"=${"+name+"}")
	if err != nil {
		panic(err)
	}
	return v
}

func (e *env) req(scope adapter.Scope, mods ...func(*adapter.Request)) adapter.Request {
	r := adapter.Request{Scope: scope, ProjectDir: e.proj, Home: e.home, ForgeBin: "forge", BackupDir: e.backupDir(), Now: func() time.Time { return fixedNow }}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func (e *env) plan(scope adapter.Scope, mods ...func(*adapter.Request)) (*adapter.Plan, error) {
	in, err := adapter.LoadInputs(context.Background(), e.forgeHome, Name)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.ad.Plan(context.Background(), in, e.req(scope, mods...))
}

func (e *env) mustPlan(scope adapter.Scope, mods ...func(*adapter.Request)) *adapter.Plan {
	e.t.Helper()
	p, err := e.plan(scope, mods...)
	if err != nil {
		e.t.Fatalf("plan: %v", err)
	}
	return p
}

func (e *env) backupDir() string { return filepath.Join(e.forgeHome, "backups") }

func (e *env) apply(p *adapter.Plan) (*adapter.ApplyResult, error) {
	return adapter.Apply(context.Background(), e.ad, p, adapter.ApplyOptions{BackupDir: e.backupDir()})
}

func (e *env) mustSync(scope adapter.Scope, mods ...func(*adapter.Request)) (*adapter.Plan, *adapter.ApplyResult) {
	e.t.Helper()
	p := e.mustPlan(scope, mods...)
	r, err := e.apply(p)
	if err != nil {
		e.t.Fatalf("apply: %v", err)
	}
	return p, r
}

func (e *env) write(rel, content string, mode os.FileMode) string {
	e.t.Helper()
	p := filepath.Join(e.proj, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) read(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.proj, filepath.FromSlash(rel)))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(e.proj, filepath.FromSlash(rel)))
	return err == nil
}

// snapshot hashes every path under the roots: type, mode, content. The sqlite
// sidecar files of the registry are skipped (opening a database read-only may
// create -shm/-wal even though no data changes).
func snapshot(t *testing.T, roots ...string) string {
	t.Helper()
	h := sha256.New()
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			base := d.Name()
			if strings.HasPrefix(base, "forge.db-") {
				return nil
			}
			fi, _ := d.Info()
			fmt.Fprintf(h, "%s|%v|%o\n", p, d.Type(), fi.Mode().Perm())
			if d.Type().IsRegular() {
				if strings.HasSuffix(base, ".lock") {
					return nil
				}
				if base == "forge.db" {
					return nil // content is registry data, covered by mode+path
				}
				b, _ := os.ReadFile(p)
				h.Write(b)
			}
			return nil
		})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func itemFor(p *adapter.Plan, kind, name string) *adapter.Item {
	for i := range p.Items {
		if p.Items[i].Kind == kind && p.Items[i].Name == name {
			return &p.Items[i]
		}
	}
	return nil
}

func wantAction(t *testing.T, p *adapter.Plan, kind, name string, a adapter.Action) {
	t.Helper()
	it := itemFor(p, kind, name)
	if it == nil {
		t.Fatalf("no plan item %s/%s; items: %+v", kind, name, p.Items)
	}
	if it.Action != a {
		t.Fatalf("%s/%s: action %s (%s), want %s", kind, name, it.Action, it.Reason, a)
	}
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
