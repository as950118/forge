package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/heonjinjeong/forge/internal/adapter"
)

// marker is the content of <skill dir>/.forge-managed. Together with the sidecar
// state it proves that a directory was written by Forge. It lives outside
// SKILL.md so no unknown frontmatter field is introduced.
type marker struct {
	ManagedBy string `json:"managed_by"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	TreeHash  string `json:"tree_hash"`
}

func (m marker) bytes() []byte {
	b, _ := json.MarshalIndent(m, "", "  ")
	return append(b, '\n')
}

func readMarker(dir string) (marker, []byte, bool) {
	b, _, ok, err := adapter.ReadFileSafe(filepath.Join(dir, adapter.MarkerName))
	if err != nil || !ok {
		return marker{}, nil, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil || m.ManagedBy != "forge" {
		return marker{}, nil, false
	}
	return m, b, true
}

func (pl *planner) planSkills(ctx context.Context) error {
	desired := map[string]adapter.SkillInput{}
	for _, s := range pl.in.Skills {
		if !skillNameRe.MatchString(s.Name) {
			pl.item(adapter.Item{Kind: adapter.KindSkill, Name: s.Name, Action: adapter.ActionSkip, Reason: "스킬 이름이 디렉터리 이름 규칙(소문자·숫자·하이픈)에 맞지 않아 건너뜀"})
			continue
		}
		desired[s.Name] = s
	}
	names := make([]string, 0, len(desired))
	for n := range desired {
		names = append(names, n)
	}
	sort.Strings(names)

	// Stale managed skills first (removals), then adds/updates.
	var stale []adapter.StateItem
	for _, it := range pl.state.Items {
		if it.Kind == adapter.KindSkill {
			if _, ok := desired[it.Name]; !ok {
				stale = append(stale, it)
			}
		}
	}
	stale = append(stale, pl.journalOrphans(adapter.KindSkill,
		func(name string) bool { _, ok := desired[name]; return ok },
		func(name string) (string, bool) {
			if !skillNameRe.MatchString(name) {
				return "", false
			}
			target := filepath.Join(pl.p.skillsDir, name)
			if pl.skillBlocked(target) != "" {
				return "", false
			}
			if mk, _, ok := readMarker(target); !ok || mk.Name != name {
				return "", false
			}
			h, _, err := adapter.TreeHash(target, true)
			return h, err == nil
		})...)
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	for _, it := range stale {
		if err := ctx.Err(); err != nil {
			return err
		}
		pl.planStaleSkill(it)
	}
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		pl.planSkill(desired[n])
	}
	return nil
}

// skillBlocked returns a reason when the target must not be written (symlink, read-only).
func (pl *planner) skillBlocked(target string) string {
	if err := adapter.CheckNoSymlinks(pl.p.root, target); err != nil {
		return "심볼릭 링크 경로라 따라가지 않고 거부합니다 (.claude 또는 skills 가 링크인지 확인)"
	}
	return ""
}

func (pl *planner) planSkill(s adapter.SkillInput) {
	target := filepath.Join(pl.p.skillsDir, s.Name)
	it := adapter.Item{Kind: adapter.KindSkill, Name: s.Name, Path: target}
	conflict := func(reason string, overridable bool) {
		it.Action, it.Reason, it.Overridable = adapter.ActionConflict, reason, overridable
		pl.item(it)
	}
	if r := pl.skillBlocked(target); r != "" {
		conflict(r, false)
		return
	}
	if gerr := adapter.GuardPath(target); gerr != nil {
		conflict("보안 정책: 대상 경로가 비밀 파일과 같은 파일이라 쓰지 않습니다 ("+adapter.MaskLine(gerr.Error())+")", false)
		return
	}
	if gerr := adapter.GuardPath(s.StoreDir); gerr != nil {
		conflict("보안 정책으로 레지스트리 스냅샷 경로를 읽을 수 없습니다: "+adapter.MaskLine(gerr.Error()), false)
		return
	}
	wantHash, _, err := adapter.TreeHash(s.StoreDir, false)
	if err != nil || (s.Hash != "" && wantHash != s.Hash) {
		conflict("레지스트리 스냅샷을 검증할 수 없습니다 (손상 또는 변조: 내용 해시가 등록 값과 다름)", false)
		return
	}
	mk := marker{ManagedBy: "forge", Name: s.Name, Version: s.Version, TreeHash: wantHash}
	snap, err := adapter.Snapshot(target)
	if err != nil {
		conflict("대상 디렉터리를 안전하게 읽을 수 없습니다 (링크/특수 파일 포함)", false)
		return
	}
	st := pl.state.Find(adapter.KindSkill, s.Name)
	op := adapter.Op{Kind: adapter.OpReplaceTree, Path: target, Before: snap,
		Tree: &adapter.TreeSpec{SrcDir: s.StoreDir, Extra: map[string][]byte{adapter.MarkerName: mk.bytes()}}}
	commit := func(a adapter.Action, reason string) {
		if r := adapter.WriteBlocker(target); r != "" {
			conflict(r, false)
			return
		}
		it.Action, it.Reason = a, reason
		pl.item(it)
		pl.plan.Ops = append(pl.plan.Ops, op)
		pl.state.Upsert(adapter.KindSkill, s.Name, wantHash, pl.now)
	}
	if !snap.Exists {
		reason := "레지스트리 " + s.Version
		if st != nil {
			reason = "관리 디렉터리가 없어 다시 생성 (" + s.Version + ")"
		}
		commit(adapter.ActionAdd, reason)
		return
	}
	if snap.Kind != "dir" {
		conflict("같은 이름의 파일이 있습니다 (스킬은 디렉터리여야 함)", false)
		return
	}
	cur, curBytes, hasMarker := readMarker(target)
	if !hasMarker || cur.Name != s.Name {
		conflict("관리 마커(.forge-managed)가 없는 같은 이름의 디렉터리 — 사용자 스킬로 보고 덮어쓰지 않습니다", false)
		return
	}
	// A directory written by a killed sync whose state file never got its final
	// write is adopted when the journal lists it and the content matches exactly.
	if h, _, err := adapter.TreeHash(target, true); err == nil {
		st = pl.adopt(adapter.KindSkill, s.Name, h)
	}
	if st == nil {
		if pl.journalListed(adapter.KindSkill, s.Name) {
			conflict("중단된 이전 sync 가 만든 디렉터리이지만 그 뒤 내용이 수정되어 관리 항목으로 인정하지 않습니다 (되돌리거나 지운 뒤 sync)", false)
			return
		}
		conflict("관리 마커는 있으나 상태 파일에 기록이 없습니다 (다른 곳에서 복사된 디렉터리일 수 있음)", false)
		return
	}
	curHash, _, err := adapter.TreeHash(target, true)
	if err != nil {
		conflict("관리 디렉터리를 검사할 수 없습니다: 링크/특수 파일이 있음", true)
		return
	}
	modified := curHash != st.Hash || curHash != cur.TreeHash
	if modified && !pl.req.ForceManaged {
		conflict("관리 디렉터리가 수정되었습니다 (해시 불일치). 덮어쓰려면 --force-managed (백업됨)", true)
		return
	}
	if !modified && curHash == wantHash && string(curBytes) == string(mk.bytes()) {
		it.Action, it.Reason = adapter.ActionUnchanged, "최신 ("+s.Version+")"
		pl.item(it)
		return
	}
	reason := fmt.Sprintf("%s -> %s", cur.Version, s.Version)
	if modified {
		reason = "수정된 관리 디렉터리를 --force-managed 로 덮어씀"
	} else if cur.Version == s.Version {
		reason = "내용/마커 갱신 (" + s.Version + ")"
	}
	commit(adapter.ActionUpdate, reason)
}

func (pl *planner) planStaleSkill(si adapter.StateItem) {
	if !skillNameRe.MatchString(si.Name) {
		pl.state.Remove(adapter.KindSkill, si.Name)
		pl.plan.Notes = append(pl.plan.Notes, "상태 파일의 올바르지 않은 스킬 항목을 무시하고 정리합니다")
		return
	}
	target := filepath.Join(pl.p.skillsDir, si.Name)
	it := adapter.Item{Kind: adapter.KindSkill, Name: si.Name, Path: target}
	conflict := func(reason string, overridable bool) {
		it.Action, it.Reason, it.Overridable = adapter.ActionConflict, reason, overridable
		pl.item(it)
	}
	if r := pl.skillBlocked(target); r != "" {
		conflict(r, false)
		return
	}
	snap, err := adapter.Snapshot(target)
	if err != nil {
		conflict("대상 디렉터리를 안전하게 읽을 수 없습니다", false)
		return
	}
	if !snap.Exists {
		it.Action, it.Reason = adapter.ActionSkip, "이미 없음: 상태 항목만 정리"
		pl.item(it)
		pl.state.Remove(adapter.KindSkill, si.Name)
		return
	}
	mk, _, hasMarker := readMarker(target)
	if snap.Kind != "dir" || !hasMarker || mk.Name != si.Name {
		it.Action, it.Reason = adapter.ActionSkip, "관리 마커가 없어 사용자 항목으로 대체된 것으로 보고 건드리지 않음 (상태 항목만 정리)"
		pl.item(it)
		pl.state.Remove(adapter.KindSkill, si.Name)
		return
	}
	curHash, _, err := adapter.TreeHash(target, true)
	if err == nil && pl.jitems[adapter.KindSkill+"\x00"+si.Name].Hash == curHash {
		si.Hash = curHash // written by a killed sync (see adopt)
	}
	modified := err != nil || curHash != si.Hash || curHash != mk.TreeHash
	if modified && !pl.req.ForceManaged {
		conflict("제거 대상 관리 디렉터리가 수정되었습니다 (해시 불일치). 지우려면 --force-managed (백업됨)", true)
		return
	}
	if r := adapter.WriteBlocker(target); r != "" {
		conflict(r, false)
		return
	}
	it.Action, it.Reason = adapter.ActionRemove, "레지스트리에서 비활성화되었거나 삭제됨 (백업 후 제거)"
	pl.item(it)
	pl.plan.Ops = append(pl.plan.Ops, adapter.Op{Kind: adapter.OpDeleteTree, Path: target, Before: snap})
	pl.state.Remove(adapter.KindSkill, si.Name)
}
