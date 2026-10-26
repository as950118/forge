package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/adapter/jsonedit"
	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

const keySep = " :: "

// hookDef is one handler Forge installs into settings.json.
type hookDef struct {
	event, matcher, command string
	handler                 jsonedit.Obj
	hash                    string
}

func (h hookDef) key() string { return h.event + keySep + h.matcher + keySep + h.command }

func parseHookKey(k string) (event, matcher, command string, ok bool) {
	parts := strings.SplitN(k, keySep, 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// desiredHooks derives the handlers from ledger.ClaudeCodeSnippet so that the
// hook text exists in exactly one place.
func desiredHooks(bin string) ([]hookDef, error) {
	snip, err := ledger.ClaudeCodeSnippet(bin)
	if err != nil {
		return nil, err
	}
	root, err := jsonedit.Parse(snip)
	if err != nil {
		return nil, err
	}
	hooks, _, _ := root.Get("hooks")
	var out []hookDef
	for _, ev := range hooks.Members {
		for _, g := range ev.Val.Elems {
			mn, _, _ := g.Get("matcher")
			matcher, _ := mn.StringValue(snip)
			hn, _, _ := g.Get("hooks")
			for _, h := range hn.Elems {
				raw := h.Raw(snip)
				var parsed struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				}
				if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Type != "command" || parsed.Command == "" {
					return nil, fmt.Errorf("hook 스니펫 형식이 예상과 다릅니다")
				}
				hash, err := adapter.HashJSON(raw)
				if err != nil {
					return nil, err
				}
				out = append(out, hookDef{event: ev.Key, matcher: matcher, command: parsed.Command, handler: handlerObj(raw), hash: hash})
			}
		}
	}
	return out, nil
}

// handlerLoc locates a handler by exact identity (event, group matcher, type command, command string).
type handlerLoc struct {
	found      bool
	group, idx int
	raw        []byte
}

func findHandler(src []byte, event, matcher, command string) (handlerLoc, error) {
	root, err := jsonedit.Parse(src)
	if err != nil {
		return handlerLoc{}, err
	}
	hooks, ok, err := root.Get("hooks")
	if err != nil || !ok {
		return handlerLoc{}, err
	}
	if hooks.Kind != jsonedit.Object {
		return handlerLoc{}, fmt.Errorf("settings.json 의 hooks 가 객체가 아닙니다")
	}
	ev, ok, err := hooks.Get(event)
	if err != nil || !ok {
		return handlerLoc{}, err
	}
	if ev.Kind != jsonedit.Array {
		return handlerLoc{}, fmt.Errorf("settings.json 의 hooks.%s 가 배열이 아닙니다", textsafe.Display(event, 60))
	}
	for gi, g := range ev.Elems {
		if g.Kind != jsonedit.Object {
			continue
		}
		mn, ok, _ := g.Get("matcher")
		if !ok {
			continue
		}
		if m, ok := mn.StringValue(src); !ok || m != matcher {
			continue
		}
		hn, ok, _ := g.Get("hooks")
		if !ok || hn.Kind != jsonedit.Array {
			continue
		}
		for hi, h := range hn.Elems {
			if h.Kind != jsonedit.Object {
				continue
			}
			var p struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			}
			if json.Unmarshal(h.Raw(src), &p) != nil {
				continue
			}
			if p.Type == "command" && p.Command == command {
				return handlerLoc{found: true, group: gi, idx: hi, raw: append([]byte(nil), h.Raw(src)...)}, nil
			}
		}
	}
	return handlerLoc{}, nil
}

// installHandler adds h to the matcher group (creating hooks/event/group as
// needed). It never replaces a group: only this handler is appended.
func installHandler(src []byte, event, matcher string, h jsonedit.Obj) ([]byte, error) {
	group := jsonedit.Obj{{K: "matcher", V: matcher}, {K: "hooks", V: []any{h}}}
	root, err := jsonedit.Parse(src)
	if err != nil {
		return nil, err
	}
	hooks, ok, err := root.Get("hooks")
	if err != nil {
		return nil, err
	}
	if !ok {
		return jsonedit.Set(src, jsonedit.Path{"hooks"}, jsonedit.Obj{{K: event, V: []any{group}}})
	}
	if hooks.Kind != jsonedit.Object {
		return nil, fmt.Errorf("settings.json 의 hooks 가 객체가 아닙니다")
	}
	ev, ok, err := hooks.Get(event)
	if err != nil {
		return nil, err
	}
	if !ok {
		return jsonedit.Set(src, jsonedit.Path{"hooks", event}, []any{group})
	}
	if ev.Kind != jsonedit.Array {
		return nil, fmt.Errorf("settings.json 의 hooks.%s 가 배열이 아닙니다", textsafe.Display(event, 60))
	}
	for gi, g := range ev.Elems {
		if g.Kind != jsonedit.Object {
			continue
		}
		mn, ok, _ := g.Get("matcher")
		if !ok {
			continue
		}
		if m, ok := mn.StringValue(src); !ok || m != matcher {
			continue
		}
		hn, ok, err := g.Get("hooks")
		if err != nil {
			return nil, err
		}
		if !ok {
			return jsonedit.Set(src, jsonedit.Path{"hooks", event, gi, "hooks"}, []any{h})
		}
		if hn.Kind != jsonedit.Array {
			return nil, fmt.Errorf("settings.json 의 hooks.%s 그룹의 hooks 가 배열이 아닙니다", textsafe.Display(event, 60))
		}
		return jsonedit.Append(src, jsonedit.Path{"hooks", event, gi, "hooks"}, h)
	}
	return jsonedit.Append(src, jsonedit.Path{"hooks", event}, group)
}

// removeHandler deletes one handler and then the containers that became empty
// because of it (group created by us, event array, hooks object).
func removeHandler(src []byte, event, matcher, command string) ([]byte, error) {
	loc, err := findHandler(src, event, matcher, command)
	if err != nil || !loc.found {
		return src, err
	}
	out, _, err := jsonedit.Delete(src, jsonedit.Path{"hooks", event, loc.group, "hooks", loc.idx})
	if err != nil {
		return nil, err
	}
	root, err := jsonedit.Parse(out)
	if err != nil {
		return nil, err
	}
	hn, _, _ := jsonedit.Lookup(out, root, jsonedit.Path{"hooks", event, loc.group, "hooks"})
	g, _, _ := jsonedit.Lookup(out, root, jsonedit.Path{"hooks", event, loc.group})
	if hn != nil && len(hn.Elems) == 0 && g != nil && len(g.Members) == 2 {
		if out, _, err = jsonedit.Delete(out, jsonedit.Path{"hooks", event, loc.group}); err != nil {
			return nil, err
		}
		root, _ = jsonedit.Parse(out)
		if evn, _, _ := jsonedit.Lookup(out, root, jsonedit.Path{"hooks", event}); evn != nil && len(evn.Elems) == 0 {
			if out, _, err = jsonedit.Delete(out, jsonedit.Path{"hooks", event}); err != nil {
				return nil, err
			}
			root, _ = jsonedit.Parse(out)
			if hk, _, _ := jsonedit.Lookup(out, root, jsonedit.Path{"hooks"}); hk != nil && len(hk.Members) == 0 {
				out, _, err = jsonedit.Delete(out, jsonedit.Path{"hooks"})
			}
		}
	}
	return out, err
}

func updateHandler(src []byte, event, matcher, command string, h jsonedit.Obj) ([]byte, error) {
	loc, err := findHandler(src, event, matcher, command)
	if err != nil || !loc.found {
		return nil, fmt.Errorf("갱신할 hook 을 찾을 수 없습니다")
	}
	return jsonedit.Set(src, jsonedit.Path{"hooks", event, loc.group, "hooks", loc.idx}, h)
}

func (pl *planner) planHooks() error {
	if pl.Req.NoHooks {
		// --no-hooks: no install, update or removal; settings.json is not even opened.
		pl.Plan.Notes = append(pl.Plan.Notes, "--no-hooks: hook 설치/갱신을 건너뜁니다 (이미 설치된 관리 hook 은 그대로 둡니다; 제거하려면 --remove-hooks)")
		return nil
	}
	var want []hookDef
	if !pl.Req.RemoveHooks {
		var err error
		if want, err = desiredHooks(pl.Bin()); err != nil {
			return err
		}
	}
	desired := map[string]hookDef{}
	for _, h := range want {
		desired[h.key()] = h
	}
	var stale []adapter.StateItem
	for _, si := range pl.State.Items {
		if si.Kind != adapter.KindHook {
			continue
		}
		if _, ok := desired[si.Name]; !ok {
			stale = append(stale, si)
		}
	}
	if len(want) == 0 && len(stale) == 0 {
		return nil // nothing to do: settings.json is not even opened
	}
	doc, err := adapter.LoadDoc(pl.p.Root, pl.p.settings, pl.newSettingsMode())
	if err != nil {
		return err
	}
	if doc.Exists {
		root, _ := jsonedit.Parse(doc.Cur)
		if hk, ok, err := root.Get("hooks"); err != nil {
			return fmt.Errorf("%s: %w", textsafe.Display(pl.p.settings, 200), err)
		} else if ok && hk.Kind != jsonedit.Object {
			return fmt.Errorf("%s 의 hooks 가 객체가 아니라 아무것도 쓰지 않고 중단합니다", textsafe.Display(pl.p.settings, 200))
		}
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	label := func(h hookDef) string { return h.event }
	path := pl.p.settings
	blocked := func(it adapter.Item) bool {
		if doc.Blocked != "" {
			it.Action, it.Reason, it.Overridable = adapter.ActionConflict, doc.Blocked, false
			pl.Item(it)
			return true
		}
		return false
	}
	apply := func(it adapter.Item, f func() ([]byte, error), a adapter.Action, reason string) bool {
		if blocked(it) {
			return false
		}
		b, err := f()
		if err != nil {
			it.Action, it.Reason = adapter.ActionConflict, "settings.json 구조가 예상과 달라 편집할 수 없습니다: "+textsafe.Display(err.Error(), 160)
			pl.Item(it)
			return false
		}
		doc.Cur = b
		it.Action, it.Reason = a, reason
		pl.Item(it)
		return true
	}
	for _, si := range stale {
		ev, matcher, command, ok := parseHookKey(si.Name)
		it := adapter.Item{Kind: adapter.KindHook, Name: ev, Path: path}
		if !ok {
			pl.State.Remove(adapter.KindHook, si.Name)
			pl.Plan.Notes = append(pl.Plan.Notes, "상태 파일의 올바르지 않은 hook 항목을 무시하고 정리합니다")
			continue
		}
		loc, err := findHandler(doc.Cur, ev, matcher, command)
		if err != nil {
			return err
		}
		if !loc.found {
			it.Action, it.Reason = adapter.ActionSkip, "이미 없음: 상태 항목만 정리"
			pl.Item(it)
			pl.State.Remove(adapter.KindHook, si.Name)
			continue
		}
		cur, _ := adapter.HashJSON(loc.raw)
		if pl.ManagedModified(adapter.KindHook, si, cur) && !pl.Req.ForceManaged {
			it.Action, it.Reason, it.Overridable = adapter.ActionConflict, "제거 대상 관리 hook 이 수정되었습니다 (해시 불일치). 지우려면 --force-managed (백업됨)", true
			pl.Item(it)
			continue
		}
		reason := "더 이상 필요 없는 관리 hook 제거 (handler 단위)"
		if pl.Req.RemoveHooks {
			reason = "--remove-hooks: 관리 hook 제거 (handler 단위)"
		}
		if apply(it, func() ([]byte, error) { return removeHandler(doc.Cur, ev, matcher, command) }, adapter.ActionRemove, reason) {
			pl.State.Remove(adapter.KindHook, si.Name)
		}
	}
	for _, h := range want {
		it := adapter.Item{Kind: adapter.KindHook, Name: label(h), Path: path}
		loc, err := findHandler(doc.Cur, h.event, h.matcher, h.command)
		if err != nil {
			return err
		}
		st := pl.State.Find(adapter.KindHook, h.key())
		if loc.found {
			if hh, herr := adapter.HashJSON(loc.raw); herr == nil {
				st = pl.Adopt(adapter.KindHook, h.key(), hh)
			}
		}
		install := func(a adapter.Action, reason string) {
			if apply(it, func() ([]byte, error) { return installHandler(doc.Cur, h.event, h.matcher, h.handler) }, a, reason) {
				pl.State.Upsert(adapter.KindHook, h.key(), h.hash, pl.Now)
			}
		}
		switch {
		case !loc.found && st == nil:
			install(adapter.ActionAdd, "호출 원장 수집 hook (matcher "+h.matcher+")")
		case !loc.found:
			install(adapter.ActionAdd, "관리 hook 이 없어 다시 추가")
		case st == nil:
			cur, _ := adapter.HashJSON(loc.raw)
			why := "내용은 다릅니다"
			if cur == h.hash {
				why = "내용은 동일합니다"
			}
			reason := "사용자가 만든 같은 command 의 hook 이 있어 건드리지 않습니다 (" + why + "; Forge 관리 대상 아님)"
			if pl.JournalListed(adapter.KindHook, h.key()) {
				reason = "중단된 이전 sync 가 쓴 hook 이지만 그 뒤 내용이 수정되어 관리 항목으로 인정하지 않습니다 (" + why + ")"
			}
			it.Action, it.Reason = adapter.ActionConflict, reason
			pl.Item(it)
		default:
			cur, _ := adapter.HashJSON(loc.raw)
			modified := pl.ManagedModified(adapter.KindHook, *st, cur)
			switch {
			case modified && !pl.Req.ForceManaged:
				it.Action, it.Reason, it.Overridable = adapter.ActionConflict, "관리 hook 이 손으로 수정되었습니다 (해시 불일치). 덮어쓰려면 --force-managed (백업됨)", true
				pl.Item(it)
			case cur == h.hash:
				it.Action, it.Reason = adapter.ActionUnchanged, "최신"
				pl.Item(it)
				if st.Hash != h.hash {
					pl.State.Upsert(adapter.KindHook, h.key(), h.hash, pl.Now)
				}
			default:
				reason := "hook 정의 갱신"
				if modified {
					reason = "수정된 관리 hook 을 --force-managed 로 덮어씀"
				}
				if apply(it, func() ([]byte, error) { return updateHandler(doc.Cur, h.event, h.matcher, h.command, h.handler) }, adapter.ActionUpdate, reason) {
					pl.State.Upsert(adapter.KindHook, h.key(), h.hash, pl.Now)
				}
			}
		}
	}
	if op, ok := doc.Op(); ok {
		pl.Plan.Ops = append(pl.Plan.Ops, op)
	}
	return nil
}

func (pl *planner) newSettingsMode() os.FileMode {
	if pl.Scope == adapter.ScopeUser {
		return 0o600
	}
	return 0o644
}

// handlerObj converts a compact handler into an ordered value (key order of the snippet).
func handlerObj(raw []byte) jsonedit.Obj {
	n, err := jsonedit.Parse(raw)
	if err != nil {
		return nil
	}
	// type and command first (readable), the remaining members in snippet order.
	o := make(jsonedit.Obj, 0, len(n.Members))
	for _, first := range []string{"type", "command"} {
		for _, m := range n.Members {
			if m.Key == first {
				o = append(o, jsonedit.KV{K: m.Key, V: json.RawMessage(m.Val.Raw(raw))})
			}
		}
	}
	for _, m := range n.Members {
		if m.Key != "type" && m.Key != "command" {
			o = append(o, jsonedit.KV{K: m.Key, V: json.RawMessage(m.Val.Raw(raw))})
		}
	}
	return o
}
