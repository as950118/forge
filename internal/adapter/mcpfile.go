package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/heonjinjeong/forge/internal/adapter/jsonedit"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

var (
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	serverRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// MCPEntry is one MCP server to render.
type MCPEntry struct {
	Name string
	Val  jsonedit.Obj // the server value in the client's own schema
	Hash string       // canonical content hash of Val
	Cmd  []string     // the client's "add" command words (user scope)
}

// HashJSON is the canonical content hash of a JSON value.
func HashJSON(raw []byte) (string, error) {
	c, err := jsonedit.Canonical(raw)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(c)
	return hex.EncodeToString(h[:]), nil
}

// NewMCPEntry hashes val and returns the entry (the gateway entry uses it directly).
func NewMCPEntry(name string, val jsonedit.Obj, cmd []string) MCPEntry {
	e := MCPEntry{Name: name, Val: val, Cmd: cmd}
	e.Hash, _ = HashJSON(jsonedit.Compact(val))
	return e
}

// MCPFormat is what differs between clients when a registry server is rendered:
// the schema (Build), the reference syntax and the text the client would
// substitute. The re-validation and refusal rules around them are common.
type MCPFormat struct {
	// CwdReason is the skip reason for a server with a working directory (the
	// client's format has no cwd, or it is unconfirmed).
	CwdReason string
	// Ref renders an environment-variable reference ("${VAR}", "{env:VAR}").
	Ref func(name string) string
	// Subst matches text the client substitutes inside its config; a literal value
	// containing it is refused.
	Subst *regexp.Regexp
	// SubstInCommand also refuses it in command, args and URL (clients that expand
	// substitutions there too).
	SubstInCommand bool
	// Build lays out the server value and the user-scope command in the client's
	// schema. kvs lists the rendered values of one slot (env or header).
	Build func(s *mcp.Info, kvs func(slot string) jsonedit.Obj) (val jsonedit.Obj, cmd []string)
}

// RenderServer re-validates a registry server right before rendering (the
// registry already refuses plaintext secrets; this is the second line) and builds
// the entry. Secrets appear only as environment references. bad is a refusal
// reason; skip tells that the server is merely not representable (cwd, keychain)
// rather than refused.
func RenderServer(s *mcp.Info, f MCPFormat) (e MCPEntry, bad string, skip bool) {
	if !serverRe.MatchString(s.Name) {
		return MCPEntry{}, "서버 이름이 올바르지 않습니다", false
	}
	if s.Cwd != "" {
		return MCPEntry{}, f.CwdReason, true
	}
	spec := mcp.Spec{Name: s.Name, Transport: s.Transport, Command: s.Command, Args: s.Args, URL: s.URL, TimeoutMS: s.TimeoutMS, Values: s.Values}
	if err := spec.Validate(); err != nil {
		return MCPEntry{}, "렌더 거부(재검증 실패): " + textsafe.Display(err.Error(), 200), false
	}
	if f.SubstInCommand {
		for _, t := range append([]string{s.Command, s.URL}, s.Args...) {
			if f.Subst.MatchString(t) {
				return MCPEntry{}, "렌더 거부: 명령/인자/URL 에 {env:}/{file:}/${} 치환 문법이 있어 클라이언트가 값을 바꿔 읽을 수 있습니다", false
			}
		}
	}
	vals := map[string][2]string{} // slot/key -> [key, rendered]
	var order []string
	for _, v := range s.Values {
		var out string
		if err := CheckValueKey(v.Slot, v.Key); err != nil {
			return MCPEntry{}, "렌더 거부: 환경변수/헤더 이름이 올바르지 않거나 치환 문법이 있습니다 (" + textsafe.Display(v.Key, 60) + ")", false
		}
		switch v.RefType {
		case mcp.RefEnv:
			if !envNameRe.MatchString(v.Ref) {
				return MCPEntry{}, "렌더 거부: 환경변수 참조 이름이 올바르지 않습니다", false
			}
			out = f.Ref(v.Ref)
		case mcp.RefLiteral:
			if v.IsSecret || mcp.LooksSecret(v.Key) || mcp.LooksSecretKV(v.Key, v.Literal) || f.Subst.MatchString(v.Literal) || f.Subst.MatchString(v.Key) {
				return MCPEntry{}, "렌더 거부: 비밀로 보이거나 치환 문법이 있는 literal 값 (" + textsafe.Display(v.Key, 60) + ")", false
			}
			out = v.Literal
		default:
			return MCPEntry{}, "keychain 참조는 직접 노출할 수 없습니다 (exposure=gateway 필요)", true
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
	val, cmd := f.Build(s, kvs)
	for _, c := range cmd {
		for _, r := range c {
			if textsafe.NeedsEscape(r) {
				return MCPEntry{}, "렌더 거부: 값에 제어/비가시 문자가 있습니다", false
			}
		}
	}
	e = NewMCPEntry(s.Name, val, cmd)
	if e.Hash == "" {
		return MCPEntry{}, "렌더 실패", false
	}
	return e, "", false
}

// MCPTarget is what a target hands to the common MCP planning.
type MCPTarget struct {
	Gateway     MCPEntry // the `forge mcp serve` entry (always rendered)
	GatewayName string   // its name: a registry server may not use it
	// Render is the target's RenderServer call.
	Render func(*mcp.Info) (MCPEntry, string, bool)
	// Rendered (optional) runs once the desired entries are known, e.g. to add
	// target-specific notes about what was rendered.
	Rendered func()
}

// DesiredMCP returns the entries to render for the client (the gateway entry
// first) plus items for servers that are deliberately not rendered. Every refusal
// becomes a conflict (a skip when the server is merely not representable).
func (pl *Planner) DesiredMCP(t MCPTarget) (entries []MCPEntry, items []Item) {
	gatewayName, render := t.GatewayName, t.Render
	entries = append(entries, t.Gateway)
	srv := append([]*mcp.Info{}, pl.In.Servers...)
	sort.Slice(srv, func(i, j int) bool { return srv[i].Name < srv[j].Name })
	for _, s := range srv {
		it := Item{Kind: KindMCP, Name: s.Name}
		if s.Name == gatewayName {
			it.Action, it.Reason = ActionConflict, "레지스트리 서버 이름 'forge' 가 게이트웨이 항목 이름과 충돌합니다 (다른 이름으로 다시 등록하세요)"
			items = append(items, it)
			continue
		}
		mode := s.Exposure[pl.In.Client]
		if mode == "" {
			mode = mcp.ExposureDirect
		}
		switch mode {
		case mcp.ExposureGateway:
			it.Action, it.Reason = ActionSkip, "exposure=gateway: 게이트웨이(forge)가 프록시하므로 직접 쓰지 않음"
			items = append(items, it)
			continue
		case mcp.ExposureHidden:
			it.Action, it.Reason = ActionSkip, "exposure=hidden: 노출하지 않음"
			items = append(items, it)
			continue
		}
		e, bad, skip := render(s)
		if bad != "" {
			it.Action, it.Reason = ActionConflict, bad
			if skip {
				it.Action = ActionSkip
			}
			items = append(items, it)
			continue
		}
		entries = append(entries, e)
	}
	if t.Rendered != nil {
		t.Rendered()
	}
	return entries, items
}

// MCPFile locates the servers object inside the client's project config file.
type MCPFile struct {
	Path  string // the config file (below the project root)
	Key   string // top-level object holding the servers ("mcpServers", "mcp")
	Label string // file name used in reasons (".mcp.json")
}

// PlanMCPFile plans the servers of a project-scope config file: only the member
// Key is edited, every other byte of the file is kept. A server is Forge-managed
// only when the state file (or the journal of a killed sync) records it and its
// canonical hash still matches; a same-named entry the user wrote is a conflict.
// It reports whether the file will be written.
func (pl *Planner) PlanMCPFile(f MCPFile, t MCPTarget) (changed bool, err error) {
	doc, err := LoadDoc(pl.P.Root, f.Path, 0o644)
	if err != nil {
		return false, err
	}
	// serversNode re-parses the working copy: offsets go stale after each edit.
	serversNode := func() (*jsonedit.Node, error) {
		root, err := jsonedit.Parse(doc.Cur)
		if err != nil {
			return nil, err
		}
		n, ok, err := root.Get(f.Key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", textsafe.Display(f.Path, 200), err)
		}
		if !ok {
			return nil, nil
		}
		if n.Kind != jsonedit.Object {
			return nil, fmt.Errorf("%s 의 %s 가 객체가 아니라 아무것도 쓰지 않고 중단합니다", textsafe.Display(f.Path, 200), f.Key)
		}
		return n, nil
	}
	if _, err := serversNode(); err != nil { // validate the structure up front
		return false, err
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
		return append([]byte(nil), n.Raw(doc.Cur)...), true, nil
	}

	entries, items := pl.DesiredMCP(t)
	desired := map[string]MCPEntry{}
	for _, e := range entries {
		desired[e.Name] = e
	}
	var stale []StateItem
	for _, si := range pl.State.Items {
		if si.Kind != KindMCP {
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
		if it.Action == ActionSkip && staleNames[it.Name] {
			continue
		}
		it.Path = f.Path
		pl.Item(it)
	}

	edit := func(fn func() ([]byte, error)) bool {
		b, err := fn()
		if err != nil {
			pl.Note("편집 실패: " + textsafe.Display(err.Error(), 200))
			return false
		}
		doc.Cur = b
		return true
	}
	blocked := func(it Item) bool {
		if doc.Blocked != "" {
			it.Action, it.Reason, it.Overridable = ActionConflict, doc.Blocked, false
			pl.Item(it)
			return true
		}
		return false
	}
	// removals
	for _, si := range stale {
		it := Item{Kind: KindMCP, Name: si.Name, Path: f.Path}
		if !serverRe.MatchString(si.Name) {
			pl.State.Remove(KindMCP, si.Name)
			continue
		}
		raw, ok, err := member(si.Name)
		if err != nil {
			return false, err
		}
		if !ok {
			it.Action, it.Reason = ActionSkip, "이미 없음: 상태 항목만 정리"
			pl.Item(it)
			pl.State.Remove(KindMCP, si.Name)
			continue
		}
		cur, _ := HashJSON(raw)
		if pl.ManagedModified(KindMCP, si, cur) && !pl.Req.ForceManaged {
			it.Action, it.Reason, it.Overridable = ActionConflict, "제거 대상 관리 서버가 수정되었습니다 (해시 불일치). 지우려면 --force-managed (백업됨)", true
			pl.Item(it)
			continue
		}
		if blocked(it) {
			continue
		}
		if edit(func() ([]byte, error) {
			b, _, err := jsonedit.Delete(doc.Cur, jsonedit.Path{f.Key, si.Name})
			return b, err
		}) {
			it.Action = ActionRemove
			it.Reason = "레지스트리에서 비활성화/삭제되었거나 gateway·hidden 으로 바뀜"
			pl.Item(it)
			pl.State.Remove(KindMCP, si.Name)
		}
	}
	// adds / updates
	for _, e := range entries {
		it := Item{Kind: KindMCP, Name: e.Name, Path: f.Path}
		raw, exists, err := member(e.Name)
		if err != nil {
			return false, err
		}
		st := pl.State.Find(KindMCP, e.Name)
		if exists {
			if h, herr := HashJSON(raw); herr == nil {
				st = pl.Adopt(KindMCP, e.Name, h)
			}
		}
		reasonNew := "직접 노출 서버"
		if e.Name == t.GatewayName {
			reasonNew = "게이트웨이 서버 (forge mcp serve)"
		}
		set := func(a Action, reason string) {
			if blocked(it) {
				return
			}
			if edit(func() ([]byte, error) { return jsonedit.Set(doc.Cur, jsonedit.Path{f.Key, e.Name}, e.Val) }) {
				it.Action, it.Reason = a, reason
				pl.Item(it)
				pl.State.Upsert(KindMCP, e.Name, e.Hash, pl.Now)
			}
		}
		switch {
		case !exists && st == nil:
			set(ActionAdd, reasonNew)
		case !exists:
			set(ActionAdd, "관리 항목이 없어 다시 추가")
		case st == nil:
			cur, _ := HashJSON(raw)
			why := "내용은 다릅니다"
			if cur == e.Hash {
				why = "내용은 동일합니다"
			}
			reason := "사용자가 " + f.Label + " 에 직접 정의한 같은 이름의 서버가 있어 건드리지 않습니다 (" + why + "; Forge 관리 대상 아님)"
			if pl.JournalListed(KindMCP, e.Name) {
				reason = "중단된 이전 sync 가 쓴 항목이지만 그 뒤 내용이 수정되어 관리 항목으로 인정하지 않습니다 (" + why + ")"
			}
			it.Action, it.Reason = ActionConflict, reason
			pl.Item(it)
		default:
			cur, _ := HashJSON(raw)
			modified := pl.ManagedModified(KindMCP, *st, cur)
			switch {
			case modified && !pl.Req.ForceManaged:
				it.Action, it.Reason, it.Overridable = ActionConflict, "관리 서버가 손으로 수정되었습니다 (해시 불일치). 덮어쓰려면 --force-managed (백업됨)", true
				pl.Item(it)
			case cur == e.Hash && cur == st.Hash:
				it.Action, it.Reason = ActionUnchanged, "최신"
				pl.Item(it)
			case cur == e.Hash:
				// Edited by hand to exactly the desired content: just re-record it.
				it.Action, it.Reason = ActionUnchanged, "최신"
				pl.Item(it)
				pl.State.Upsert(KindMCP, e.Name, e.Hash, pl.Now)
			default:
				reason := "레지스트리 변경 반영"
				if modified {
					reason = "수정된 관리 서버를 --force-managed 로 덮어씀"
				}
				set(ActionUpdate, reason)
			}
		}
	}
	if op, ok := doc.Op(); ok {
		pl.Plan.Ops = append(pl.Plan.Ops, op)
		changed = true
	}
	return changed, nil
}

// PlanMCPCommands prints the client's "add" commands for the user scope. Nothing
// is written: the user-level MCP config of the client is neither read nor
// modified. addReason is the row text; notes are appended to the plan.
func (pl *Planner) PlanMCPCommands(t MCPTarget, addReason string, notes ...string) {
	entries, items := pl.DesiredMCP(t)
	for _, it := range items {
		pl.Item(it)
	}
	for _, e := range entries {
		parts := make([]string, len(e.Cmd))
		for i, c := range e.Cmd {
			parts[i] = ShellQuote(c)
		}
		pl.Plan.Commands = append(pl.Plan.Commands, strings.Join(parts, " "))
		pl.Item(Item{Kind: KindCommand, Name: e.Name, Action: ActionInfo, Reason: addReason})
	}
	pl.Plan.Notes = append(pl.Plan.Notes, notes...)
}
