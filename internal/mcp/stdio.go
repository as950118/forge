package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// lineLimit counts bytes since the last newline on a stdio server's stdout.
type lineLimit struct {
	max  int
	mu   sync.Mutex
	run  int
	over bool
	kill func()
}

func (l *lineLimit) exceeded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.over
}

// add feeds a chunk and reports whether the cap was exceeded.
func (l *lineLimit) add(p []byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i := bytes.LastIndexByte(p, '\n'); i >= 0 {
		l.run = 0
		p = p[i+1:]
	}
	l.run += len(p)
	if l.run > l.max {
		l.over = true
	}
	return l.over
}

var errLineTooLong = errors.New("stdout line exceeds limit")

type limitedReader struct {
	r   io.Reader
	lim *lineLimit
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	n, err := lr.r.Read(p)
	if n > 0 && lr.lim.add(p[:n]) {
		if lr.lim.kill != nil {
			lr.lim.kill()
		}
		return 0, errLineTooLong
	}
	return n, err
}

// limitedCommandTransport is sdk.CommandTransport with a cap on one stdout
// line, so that a server printing endless output without a newline cannot
// grow Forge's memory. On overflow the process group is killed.
type limitedCommandTransport struct {
	cmd *exec.Cmd
	lim *lineLimit
}

func (t *limitedCommandTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := t.cmd.Start(); err != nil {
		return nil, err
	}
	t.lim.kill = func() { killGroup(t.cmd) }
	return (&sdk.IOTransport{
		Reader: io.NopCloser(&limitedReader{r: stdout, lim: t.lim}),
		Writer: &cmdStdin{cmd: t.cmd, w: stdin},
	}).Connect(ctx)
}

// cmdStdin closes the child's stdin (the MCP stdio shutdown signal) and waits
// briefly for exit, then kills the process group.
type cmdStdin struct {
	cmd *exec.Cmd
	w   io.WriteCloser
}

func (c *cmdStdin) Write(p []byte) (int, error) { return c.w.Write(p) }

func (c *cmdStdin) Close() error {
	err := c.w.Close()
	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		// stdin EOF did not end it: SIGTERM the whole group, then SIGKILL.
		termGroup(c.cmd)
		select {
		case <-done:
		case <-time.After(time.Second):
			killGroup(c.cmd)
			<-done
		}
	}
	return err
}
