package clientver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/heonjinjeong/forge/internal/mcp"
)

// Limits of one --version run. Timeout is a variable only so that tests can
// shorten it; nothing else changes it.
var Timeout = 5 * time.Second

// MaxOutput is the number of bytes kept per stream; more output stops the process.
const MaxOutput = 4 << 10

// VersionArg is the one and only argument ever passed to a client binary.
const VersionArg = "--version"

// Exec is the production Runner. Safety design (docs/doctor.md):
//   - argv is exactly [path, "--version"]; nothing else is ever run.
//   - stdin is /dev/null, the working directory is an empty temporary directory.
//   - the environment is the allow-listed base environment (mcp.MinimalEnv:
//     no tokens or credentials) WITHOUT HOME/XDG_*/TMPDIR; instead HOME, XDG_*
//     and TMPDIR point into the same temporary directory. The client can neither
//     read the user's configuration or credentials nor write into the user's
//     home (first-run migrations, update checks, caches); the directory is
//     removed afterwards. The cost: a shim that needs the real HOME to find its
//     runtime (nvm/asdf style wrappers) fails, which is reported as "run-failed"
//     (warn), not as a verified version.
//   - the directory is named forge-ver-<pid>-<random>; a later run removes the ones
//     of dead processes under strict conditions (stale.go).
//   - 5 second timeout, 4 KiB cap per stream (the process is stopped when it
//     talks more), the whole process group is killed afterwards.
type Exec struct{}

// RunVersion implements Runner.
func (Exec) RunVersion(ctx context.Context, path string) (RunResult, error) {
	if !filepath.IsAbs(path) {
		return RunResult{}, errors.New("실행 경로가 절대 경로가 아닙니다")
	}
	sweepStaleOnce() // directories of killed runs (stale.go)
	tmp, err := os.MkdirTemp("", tempPattern())
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	cmd := exec.Command(path, VersionArg)
	cmd.Dir = tmp
	cmd.Env = isolatedEnv(tmp)
	cmd.Stdin = nil // /dev/null
	setProcGroup(cmd)
	cmd.WaitDelay = time.Second // a descendant that keeps the pipes open cannot block us
	var res RunResult
	stop := make(chan struct{})
	var once sync.Once
	over := func() { once.Do(func() { close(stop) }) }
	so, se := &capWriter{max: MaxOutput, onOver: over}, &capWriter{max: MaxOutput, onOver: over}
	cmd.Stdout, cmd.Stderr = so, se
	if err := cmd.Start(); err != nil {
		return RunResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-done:
	case <-ctx.Done():
		killGroup(cmd)
		werr = <-done
		if ctx.Err() == context.DeadlineExceeded {
			res.TimedOut = true
		} else {
			werr = ctx.Err()
		}
	case <-stop:
		killGroup(cmd)
		werr = <-done
	}
	killGroup(cmd) // leftovers of the group (wrappers that spawned children)
	res.Stdout, res.Stderr = so.bytes(), se.bytes()
	if so.over() || se.over() {
		res.Truncated = true
	}
	var ee *exec.ExitError
	switch {
	case werr == nil:
	case errors.As(werr, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode == 0 || res.ExitCode < 0 {
			res.ExitCode = -1 // killed by a signal
		}
	case res.TimedOut || res.Truncated:
	default:
		return RunResult{}, werr
	}
	return res, nil
}

// isolatedEnv builds the child environment (see Exec).
func isolatedEnv(tmp string) []string {
	var env []string
	for _, kv := range mcp.MinimalEnv() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "HOME" || k == "TMPDIR" || k == "USERPROFILE" || k == "APPDATA" || k == "LOCALAPPDATA" || strings.HasPrefix(k, "XDG_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+tmp, "TMPDIR="+tmp,
		"XDG_CONFIG_HOME="+filepath.Join(tmp, "config"),
		"XDG_DATA_HOME="+filepath.Join(tmp, "data"),
		"XDG_CACHE_HOME="+filepath.Join(tmp, "cache"),
		"XDG_STATE_HOME="+filepath.Join(tmp, "state"),
		"NO_COLOR=1", "CI=1",
	)
}

// capWriter keeps at most max bytes and reports the first byte over the cap.
type capWriter struct {
	mu     sync.Mutex
	buf    []byte
	max    int
	isOver bool
	onOver func()
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	room := w.max - len(w.buf)
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		w.buf = append(w.buf, p[:room]...)
	}
	trig := room < len(p) && !w.isOver
	if room < len(p) {
		w.isOver = true
	}
	w.mu.Unlock()
	if trig && w.onOver != nil {
		w.onOver()
	}
	return len(p), nil
}

func (w *capWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf...)
}

func (w *capWriter) over() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.isOver
}
