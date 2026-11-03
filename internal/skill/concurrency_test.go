package skill

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/as950118/forge/internal/store"
)

const concN = 8

// Parallel adds of different skills, each goroutine with its own connection
// (like separate processes), starting from an empty home so concurrent
// migration is exercised too (ADR-F0001 D1: parallel ingest must be safe).
func TestConcurrentAddDifferentSkills(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(func() { makeWritable(home) })
	if err := store.EnsureLayout(home); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	srcs := make([]string, concN)
	for i := range srcs {
		name := fmt.Sprintf("par-skill-%d", i)
		srcs[i] = dirSkill(t, name, fmt.Sprintf("---\nname: %s\ndescription: d%d\n---\nbody\n", name, i), nil)
	}
	start := make(chan struct{})
	errs := make([]error, concN)
	var wg sync.WaitGroup
	for i := 0; i < concN; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			conn, err := store.Open(ctx, store.DBPath(home))
			if err != nil {
				errs[i] = err
				return
			}
			defer conn.Close()
			p, err := Prepare(ctx, srcs[i], PrepareOptions{TmpDir: filepath.Join(home, "tmp")})
			if err != nil {
				errs[i] = err
				return
			}
			defer p.Cleanup()
			_, errs[i] = (&Registry{DB: conn, Home: home}).Add(ctx, p)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
	conn, err := store.Open(ctx, store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	list, err := (&Registry{DB: conn, Home: home}).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != concN {
		t.Errorf("registered %d skills, want %d", len(list), concN)
	}
}

// Same with real forge processes (new FORGE_HOME, concN concurrent `skill add`).
func TestConcurrentAddProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the forge binary")
	}
	bin := filepath.Join(t.TempDir(), "forge")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/as950118/forge/cmd/forge").CombinedOutput(); err != nil {
		t.Skipf("cannot build forge binary: %v\n%s", err, out)
	}
	home := filepath.Join(t.TempDir(), "home")
	t.Cleanup(func() { makeWritable(home) })
	cmds := make([]*exec.Cmd, concN)
	outs := make([][]byte, concN)
	errs := make([]error, concN)
	for i := range cmds {
		name := fmt.Sprintf("proc-skill-%d", i)
		src := dirSkill(t, name, fmt.Sprintf("---\nname: %s\ndescription: d\n---\nbody\n", name), nil)
		cmds[i] = exec.Command(bin, "skill", "add", src)
		cmds[i].Env = append(os.Environ(), "FORGE_HOME="+home)
	}
	var wg sync.WaitGroup
	for i := range cmds {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = cmds[i].CombinedOutput()
		}(i)
	}
	wg.Wait()
	for i := range cmds {
		if errs[i] != nil {
			t.Errorf("proc %d: %v\n%s", i, errs[i], outs[i])
		}
	}
	ctx := context.Background()
	conn, err := store.Open(ctx, store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	list, err := (&Registry{DB: conn, Home: home}).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != concN {
		t.Errorf("registered %d skills, want %d", len(list), concN)
	}
}
