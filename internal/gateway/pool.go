package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/as950118/forge/internal/mcp"
)

// pool holds one lazily created connection per proxied server (created on the
// first call, reused afterwards, re-created when it died).
type pool struct {
	g      *Gateway
	mu     sync.Mutex
	items  map[string]*poolItem
	closed bool
}

type poolItem struct {
	mu   sync.Mutex // serializes dialing and replacing conn
	conn *mcp.Conn
}

func newPool(g *Gateway) *pool { return &pool{g: g, items: map[string]*poolItem{}} }

func (p *pool) item(name string) (*poolItem, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("게이트웨이가 종료 중입니다")
	}
	it := p.items[name]
	if it == nil {
		it = &poolItem{}
		p.items[name] = it
	}
	return it, nil
}

// nonRetryable failures will not heal by trying again right away.
var nonRetryable = map[string]bool{
	mcp.ClassCommandNotFound: true, mcp.ClassExecFailed: true, mcp.ClassEnvMissing: true, mcp.ClassAuthRequired: true,
}

// get returns a live connection, dialing (up to DialRetries attempts with a short
// backoff) when there is none. The definition is re-read on every dial so a
// changed/disabled registration takes effect without restarting the gateway.
func (p *pool) get(ctx context.Context, name string) (*mcp.Conn, *mcp.DialError) {
	it, err := p.item(name)
	if err != nil {
		return nil, &mcp.DialError{Class: "connection", Message: err.Error()}
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.conn != nil {
		if it.conn.Alive() {
			return it.conn, nil
		}
		p.g.log.Debug("upstream connection lost; reconnecting", "server", name)
		it.conn.Close()
		it.conn = nil
	}
	info, gerr := p.g.o.Servers.Get(ctx, name)
	if gerr != nil {
		return nil, &mcp.DialError{Class: "not_found", Message: gerr.Error()}
	}
	if !info.Enabled {
		return nil, &mcp.DialError{Class: "disabled", Message: "서버가 비활성화되었습니다"}
	}
	var derr *mcp.DialError
	for attempt := 1; attempt <= p.g.o.DialRetries; attempt++ {
		start := time.Now()
		var c *mcp.Conn
		c, derr = mcp.Dial(ctx, info, mcp.DialOptions{Env: p.g.o.ProxyEnv, ClientName: "forge-gateway", ClientVersion: p.g.o.Version})
		if derr == nil {
			p.g.log.Debug("upstream connected", "server", name, "ms", time.Since(start).Milliseconds())
			it.conn = c
			return c, nil
		}
		p.g.log.Debug("upstream dial failed", "server", name, "attempt", attempt, "class", derr.Class)
		if nonRetryable[derr.Class] || attempt == p.g.o.DialRetries {
			break
		}
		select {
		case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
		case <-ctx.Done():
			return nil, &mcp.DialError{Class: mcp.ClassTimeout, Message: "연결 대기 중 취소되었습니다"}
		}
	}
	return nil, derr
}

// drop closes conn if it is still the pooled connection of name.
func (p *pool) drop(name string, conn *mcp.Conn) {
	p.mu.Lock()
	it := p.items[name]
	p.mu.Unlock()
	if it == nil {
		conn.Close()
		return
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.conn == conn {
		it.conn = nil
	}
	conn.Close()
}

// closeAll stops every connection (children get stdin EOF, SIGTERM, SIGKILL on
// their process groups) and refuses new ones.
func (p *pool) closeAll() {
	p.mu.Lock()
	p.closed = true
	items := make([]*poolItem, 0, len(p.items))
	for _, it := range p.items {
		items = append(items, it)
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		go func(it *poolItem) {
			defer wg.Done()
			it.mu.Lock()
			defer it.mu.Unlock()
			if it.conn != nil {
				it.conn.Close()
				it.conn = nil
			}
		}(it)
	}
	wg.Wait()
}
