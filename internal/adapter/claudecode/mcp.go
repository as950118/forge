package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/adapter/jsonedit"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

var (
	envNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	serverName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// entry is one MCP server to render.
type entry struct {
	name string
	val  jsonedit.Obj // .mcp.json value (type always explicit)
	hash string       // canonical content hash of val
	// For user-scope commands.
	cmd []string
}

func hashJSON(raw []byte) (string, error) {
	c, err := jsonedit.Canonical(raw)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(c)
	return hex.EncodeToString(h[:]), nil
}

// gatewayEntry is the `forge mcp serve` server (PLAN §3.1).
func (pl *planner) gatewayEntry() entry {
	args := []string{"mcp", "serve", "--client", "claude-code"}
	anyArgs := make([]any, len(args))
	for i, a := range args {
		anyArgs[i] = a
	}
	val := jsonedit.Obj{{K: "type", V: "stdio"}, {K: "command", V: pl.bin()}, {K: "args", V: anyArgs}}
	e := entry{name: GatewayServerName, val: val}
	e.hash, _ = hashJSON(jsonedit.Compact(val))
	e.cmd = append([]string{"claude", "mcp", "add", "--scope", "user", "--transport", "stdio", GatewayServerName, "--", pl.bin()}, args...)
	return e
}

// renderServer re-validates a registry server right before rendering (the
// registry already refuses plaintext secrets; this is the second line) and builds
// the entry. Secrets appear only as ${VAR} references.
func renderServer(s *mcp.Info) (entry, string) {
	if !serverName.MatchString(s.Name) {
		return entry{}, "서버 이름이 올바르지 않습니다"
	}
	if s.Cwd != "" {
		return entry{}, "cwd 지정 서버는 Claude Code 형식(.mcp.json)에 cwd 가 없어 직접 노출할 수 없습니다 (exposure=gateway 사용)"
	}
	spec := mcp.Spec{Name: s.Name, Transport: s.Transport, Command: s.Command, Args: s.Args, URL: s.URL, TimeoutMS: s.TimeoutMS, Values: s.Values}
	if err := spec.Validate(); err != nil {
		return entry{}, "렌더 거부(재검증 실패): " + textsafe.Display(err.Error(), 200)
	}
	vals := map[string][2]string{} // slot/key -> [key, rendered]
	var order []string
	for _, v := range s.Values {
		var out string
		if err := adapter.CheckValueKey(v.Slot, v.Key); err != nil {
			return entry{}, "렌더 거부: 환경변수/헤더 이름이 올바르지 않거나 치환 문법이 있습니다 (" + textsafe.Display(v.Key, 60) + ")"
		}
		switch v.RefType {
		case mcp.RefEnv:
			if !envNameRe.MatchString(v.Ref) {
				return entry{}, "렌더 거부: 환경변수 참조 이름이 올바르지 않습니다"
			}
			out = "${" + v.Ref + "}"
		case mcp.RefLiteral:
			if v.IsSecret || mcp.LooksSecret(v.Key) || mcp.LooksSecretKV(v.Key, v.Literal) || strings.Contains(v.Literal, "${") {
				return entry{}, "렌더 거부: 비밀로 보이는 literal 값 (" + textsafe.Display(v.Key, 60) + ")"
			}
			out = v.Literal
		default:
			return entry{}, "keychain 참조는 직접 노출할 수 없습니다 (exposure=gateway 필요)"
		}
		k := v.Slot + "\x00" + v.Key
		vals[k] = [2]string{v.Key, out}
		order = append(order, k)
	}
	kvs := func(slot string) jsonedit.Obj {
		var o jsonedit.Obj
		for _, k := range order {
			if strings.HasPrefix(k, slot+"\x00") {
				o = append(o, jsonedit.KV{K: vals[k][0], V: vals[k][1]})
			}
		}
		return o
	}
	var val jsonedit.Obj
	e := entry{name: s.Name}
	switch s.Transport {
	case mcp.TransportStdio:
		val = jsonedit.Obj{{K: "type", V: "stdio"}, {K: "command", V: s.Command}}
		if len(s.Args) > 0 {
			a := make([]any, len(s.Args))
			for i, x := range s.Args {
				a[i] = x
			}
			val = append(val, jsonedit.KV{K: "args", V: a})
		}
		if env := kvs(mcp.SlotEnv); len(env) > 0 {
			val = append(val, jsonedit.KV{K: "env", V: env})
		}
		e.cmd = []string{"claude", "mcp", "add", "--scope", "user", "--transport", "stdio", s.Name}
		for _, kv := range kvs(mcp.SlotEnv) {
			e.cmd = append(e.cmd, "--env", kv.K+"="+kv.V.(string))
		}
		e.cmd = append(e.cmd, "--", s.Command)
		e.cmd = append(e.cmd, s.Args...)
	case mcp.TransportHTTP, mcp.TransportSSE:
		val = jsonedit.Obj{{K: "type", V: s.Transport}, {K: "url", V: s.URL}}
		if h := kvs(mcp.SlotHeader); len(h) > 0 {
			val = append(val, jsonedit.KV{K: "headers", V: h})
		}
		e.cmd = []string{"claude", "mcp", "add", "--scope", "user", "--transport", s.Transport, s.Name, s.URL}
		for _, kv := range kvs(mcp.SlotHeader) {
			e.cmd = append(e.cmd, "--header", kv.K+": "+kv.V.(string))
		}
	}
	for _, c := range e.cmd {
		for _, r := range c {
			if textsafe.NeedsEscape(r) {
				return entry{}, "렌더 거부: 값에 제어/비가시 문자가 있습니다"
			}
		}
	}
	e.val = val
	var err error
	if e.hash, err = hashJSON(jsonedit.Compact(val)); err != nil {
		return entry{}, "렌더 실패"
	}
	return e, ""
}

// desiredMCP returns the entries to render for the client plus items for servers
// that are deliberately not rendered.
func (pl *planner) desiredMCP() (entries []entry, items []adapter.Item) {
	entries = append(entries, pl.gatewayEntry())
	srv := append([]*mcp.Info{}, pl.in.Servers...)
	sort.Slice(srv, func(i, j int) bool { return srv[i].Name < srv[j].Name })
	for _, s := range srv {
		it := adapter.Item{Kind: adapter.KindMCP, Name: s.Name}
		if s.Name == GatewayServerName {
			it.Action, it.Reason = adapter.ActionConflict, "레지스트리 서버 이름 'forge' 가 게이트웨이 항목 이름과 충돌합니다 (다른 이름으로 다시 등록하세요)"
			items = append(items, it)
			continue
		}
		mode := s.Exposure[pl.in.Client]
		if mode == "" {
			mode = mcp.ExposureDirect
		}
		switch mode {
		case mcp.ExposureGateway:
			it.Action, it.Reason = adapter.ActionSkip, "exposure=gateway: 게이트웨이(forge)가 프록시하므로 직접 쓰지 않음"
			items = append(items, it)
			continue
		case mcp.ExposureHidden:
			it.Action, it.Reason = adapter.ActionSkip, "exposure=hidden: 노출하지 않음"
			items = append(items, it)
			continue
		}
		e, bad := renderServer(s)
		if bad != "" {
			it.Action, it.Reason = adapter.ActionConflict, bad
			if strings.HasPrefix(bad, "cwd ") || strings.HasPrefix(bad, "keychain ") {
				it.Action = adapter.ActionSkip
			}
			items = append(items, it)
			continue
		}
		entries = append(entries, e)
	}
	return entries, items
}

// planMCPFile plans <repo>/.mcp.json (project scope).
func (pl *planner) planMCPFile() error {
	doc, err := loadDoc(pl.p.root, pl.p.mcpJSON, 0o644)
	if err != nil {
		return err
	}
	// serversNode re-parses the working copy: offsets go stale after each edit.
	serversNode := func() (*jsonedit.Node, error) {
		root, err := jsonedit.Parse(doc.cur)
		if err != nil {
			return nil, err
		}
		n, ok, err := root.Get("mcpServers")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", textsafe.Display(pl.p.mcpJSON, 200), err)
		}
		if !ok {
			return nil, nil
		}
		if n.Kind != jsonedit.Object {
			return nil, fmt.Errorf("%s 의 mcpServers 가 객체가 아니라 아무것도 쓰지 않고 중단합니다", textsafe.Display(pl.p.mcpJSON, 200))
		}
		return n, nil
	}
	if _, err := serversNode(); err != nil { // validate the structure up front
		return err
	}
	member := func(name string) (raw []byte, ok bool, err error) {
		sn, err := serversNode()
		if err != nil || sn == nil {
			return nil, false, err
		}
		n, ok, err := sn.Get(name)
		if err != nil || !ok {
			return nil, false, err
		}
		return append([]byte(nil), n.Raw(doc.cur)...), true, nil
	}

	entries, items := pl.desiredMCP()
	desired := map[string]entry{}
	for _, e := range entries {
		desired[e.name] = e
	}
	skippedNames := map[string]bool{}
	for _, it := range items {
		skippedNames[it.Name] = true
	}
	var stale []adapter.StateItem
	for _, si := range pl.state.Items {
		if si.Kind != adapter.KindMCP {
			continue
		}
		if _, ok := desired[si.Name]; !ok {
			stale = append(stale, si)
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	staleNames := map[string]bool{}
	for _, s := range stale {
		staleNames[s.Name] = true
	}
	// Items about servers that are not rendered; a server that is stale gets its removal item instead.
	for _, it := range items {
		if it.Action == adapter.ActionSkip && staleNames[it.Name] {
			continue
		}
		it.Path = pl.p.mcpJSON
		pl.item(it)
	}

	edit := func(f func() ([]byte, error)) bool {
		b, err := f()
		if err != nil {
			pl.plan.Notes = append(pl.plan.Notes, "편집 실패: "+textsafe.Display(err.Error(), 200))
			return false
		}
		doc.cur = b
		return true
	}
	blocked := func(it adapter.Item) bool {
		if doc.blocked != "" {
			it.Action, it.Reason, it.Overridable = adapter.ActionConflict, doc.blocked, false
			pl.item(it)
			return true
		}
		return false
	}
	// removals
	for _, si := range stale {
		it := adapter.Item{Kind: adapter.KindMCP, Name: si.Name, Path: pl.p.mcpJSON}
		if !serverName.MatchString(si.Name) {
			pl.state.Remove(adapter.KindMCP, si.Name)
			continue
		}
		raw, ok, err := member(si.Name)
		if err != nil {
			return err
		}
		if !ok {
			it.Action, it.Reason = adapter.ActionSkip, "이미 없음: 상태 항목만 정리"
			pl.item(it)
			pl.state.Remove(adapter.KindMCP, si.Name)
			continue
		}
		cur, _ := hashJSON(raw)
		if pl.jitems[adapter.KindMCP+"\x00"+si.Name].Hash == cur {
			si.Hash = cur // written by a killed sync (see adopt)
		}
		if cur != si.Hash && !pl.req.ForceManaged {
			it.Action, it.Reason, it.Overridable = adapter.ActionConflict, "제거 대상 관리 서버가 수정되었습니다 (해시 불일치). 지우려면 --force-managed (백업됨)", true
			pl.item(it)
			continue
		}
		if blocked(it) {
			continue
		}
		if edit(func() ([]byte, error) {
			b, _, err := jsonedit.Delete(doc.cur, jsonedit.Path{"mcpServers", si.Name})
			return b, err
		}) {
			it.Action = adapter.ActionRemove
			it.Reason = "레지스트리에서 비활성화/삭제되었거나 gateway·hidden 으로 바뀜"
			pl.item(it)
			pl.state.Remove(adapter.KindMCP, si.Name)
		}
	}
	// adds / updates
	for _, e := range entries {
		it := adapter.Item{Kind: adapter.KindMCP, Name: e.name, Path: pl.p.mcpJSON}
		raw, exists, err := member(e.name)
		if err != nil {
			return err
		}
		st := pl.state.Find(adapter.KindMCP, e.name)
		if exists {
			if h, herr := hashJSON(raw); herr == nil {
				st = pl.adopt(adapter.KindMCP, e.name, h)
			}
		}
		reasonNew := "직접 노출 서버"
		if e.name == GatewayServerName {
			reasonNew = "게이트웨이 서버 (forge mcp serve)"
		}
		set := func(a adapter.Action, reason string) {
			if blocked(it) {
				return
			}
			if edit(func() ([]byte, error) { return jsonedit.Set(doc.cur, jsonedit.Path{"mcpServers", e.name}, e.val) }) {
				it.Action, it.Reason = a, reason
				pl.item(it)
				pl.state.Upsert(adapter.KindMCP, e.name, e.hash, pl.now)
			}
		}
		switch {
		case !exists && st == nil:
			set(adapter.ActionAdd, reasonNew)
		case !exists:
			set(adapter.ActionAdd, "관리 항목이 없어 다시 추가")
		case st == nil:
			cur, _ := hashJSON(raw)
			why := "내용은 다릅니다"
			if cur == e.hash {
				why = "내용은 동일합니다"
			}
			reason := "사용자가 .mcp.json 에 직접 정의한 같은 이름의 서버가 있어 건드리지 않습니다 (" + why + "; Forge 관리 대상 아님)"
			if pl.journalListed(adapter.KindMCP, e.name) {
				reason = "중단된 이전 sync 가 쓴 항목이지만 그 뒤 내용이 수정되어 관리 항목으로 인정하지 않습니다 (" + why + ")"
			}
			it.Action, it.Reason = adapter.ActionConflict, reason
			pl.item(it)
		default:
			cur, _ := hashJSON(raw)
			switch {
			case cur != st.Hash && !pl.req.ForceManaged:
				it.Action, it.Reason, it.Overridable = adapter.ActionConflict, "관리 서버가 손으로 수정되었습니다 (해시 불일치). 덮어쓰려면 --force-managed (백업됨)", true
				pl.item(it)
			case cur == e.hash && cur == st.Hash:
				it.Action, it.Reason = adapter.ActionUnchanged, "최신"
				pl.item(it)
			case cur == e.hash:
				// Edited by hand to exactly the desired content: just re-record it.
				it.Action, it.Reason = adapter.ActionUnchanged, "최신"
				pl.item(it)
				pl.state.Upsert(adapter.KindMCP, e.name, e.hash, pl.now)
			default:
				reason := "레지스트리 변경 반영"
				if cur != st.Hash {
					reason = "수정된 관리 서버를 --force-managed 로 덮어씀"
				}
				set(adapter.ActionUpdate, reason)
			}
		}
	}
	if op, ok := doc.op(); ok {
		pl.plan.Ops = append(pl.plan.Ops, op)
	}
	pl.plan.Notes = append(pl.plan.Notes, ".mcp.json 서버는 Claude Code 에서 최초 사용 시 사용자 승인이 필요합니다 (Pending approval).")
	return nil
}

// planMCPCommands prints `claude mcp add` commands for the user scope. Nothing is
// written: the user-level MCP file of Claude Code is neither read nor modified.
func (pl *planner) planMCPCommands() {
	entries, items := pl.desiredMCP()
	for _, it := range items {
		pl.item(it)
	}
	for _, e := range entries {
		parts := make([]string, len(e.cmd))
		for i, c := range e.cmd {
			parts[i] = adapter.ShellQuote(c)
		}
		pl.plan.Commands = append(pl.plan.Commands, strings.Join(parts, " "))
		pl.item(adapter.Item{Kind: adapter.KindCommand, Name: e.name, Action: adapter.ActionInfo, Reason: "claude mcp add 명령을 출력합니다 (파일을 쓰지 않음)"})
	}
	pl.plan.Notes = append(pl.plan.Notes,
		"user 스코프의 MCP 서버는 Claude Code 사용자 파일을 읽거나 쓰지 않고 위 명령만 출력합니다. 이미 등록된 이름은 add 가 실패하므로 갱신하려면 claude mcp remove --scope user <이름> 후 다시 add 하세요.")
}
