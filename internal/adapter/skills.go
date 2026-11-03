package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/as950118/forge/internal/textsafe"
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

// IsManagedSkillDir reports whether dir is a real directory (not a link, not below
// one) carrying a Forge management marker for name.
func IsManagedSkillDir(dir, name string) bool {
	if !SkillNameRe.MatchString(name) || GuardPath(dir) != nil {
		return false
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	// Lstat follows links in the parent components, so "below a link" needs its own
	// check: the resolved path must be the path itself (callers pass a resolved
	// root; TASK-0027 AC-16).
	if r, err := filepath.EvalSymlinks(dir); err != nil || r != filepath.Clean(dir) {
		return false
	}
	mk, _, ok := readMarker(dir)
	return ok && mk.Name == name
}

func readMarker(dir string) (marker, []byte, bool) {
	b, _, ok, err := ReadFileSafe(filepath.Join(dir, MarkerName))
	if err != nil || !ok {
		return marker{}, nil, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil || m.ManagedBy != "forge" {
		return marker{}, nil, false
	}
	return m, b, true
}

// SkillPolicy is what differs between targets in skill planning: wording that
// names the target's directories, and the skills another target already exposes
// through a directory this client also reads (Claimed).
type SkillPolicy struct {
	// LinkHint is appended to the "symlink path" refusal (which directory to check).
	LinkHint string
	// Claimed are skills a different target manages in a directory this client
	// reads too; they are not rendered a second time (nil: no such directory).
	Claimed map[string]bool
	// ClaimedSkip / ClaimedRemove are the reasons shown for a claimed skill that
	// is skipped, or whose own copy is removed to end the duplicate.
	ClaimedSkip, ClaimedRemove string
}

// PlanSkills plans the skill directories under P.SkillsDir: stale managed skills
// first (removals), then adds/updates. A directory is Forge-managed only when its
// marker, the state file (or the journal of a killed sync) and the exact tree
// hash all agree; everything else is a user skill and never touched.
func (pl *Planner) PlanSkills(ctx context.Context, pol SkillPolicy) error {
	desired := map[string]SkillInput{}
	var claimedItems []Item
	for _, s := range pl.In.Skills {
		if !SkillNameRe.MatchString(s.Name) {
			pl.Item(Item{Kind: KindSkill, Name: s.Name, Action: ActionSkip, Reason: "스킬 이름이 디렉터리 이름 규칙(소문자·숫자·하이픈)에 맞지 않아 건너뜀"})
			continue
		}
		if pol.Claimed[s.Name] {
			claimedItems = append(claimedItems, Item{Kind: KindSkill, Name: s.Name, Action: ActionSkip, Reason: pol.ClaimedSkip})
			continue
		}
		desired[s.Name] = s
	}
	names := make([]string, 0, len(desired))
	for n := range desired {
		names = append(names, n)
	}
	sort.Strings(names)

	var stale []StateItem
	for _, it := range pl.State.Items {
		if it.Kind == KindSkill {
			if _, ok := desired[it.Name]; !ok {
				stale = append(stale, it)
			}
		}
	}
	stale = append(stale, pl.JournalOrphans(KindSkill,
		func(name string) bool { _, ok := desired[name]; return ok },
		func(name string) (string, bool) {
			if !SkillNameRe.MatchString(name) {
				return "", false
			}
			target := filepath.Join(pl.P.SkillsDir, name)
			if pl.skillBlocked(target, pol) != "" {
				return "", false
			}
			if mk, _, ok := readMarker(target); !ok || mk.Name != name {
				return "", false
			}
			h, _, err := TreeHash(target, true)
			return h, err == nil
		})...)
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	staleNames := map[string]bool{}
	for _, it := range stale {
		if err := ctx.Err(); err != nil {
			return err
		}
		staleNames[it.Name] = true
		pl.planStaleSkill(it, pol)
	}
	// A claimed skill that this target manages is removed above (stale); the others
	// only get the explanatory skip line.
	for _, it := range claimedItems {
		if !staleNames[it.Name] {
			pl.Item(it)
		}
	}
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		pl.planSkill(desired[n], pol)
	}
	return nil
}

// skillBlocked returns a reason when the target must not be written (symlink, read-only).
func (pl *Planner) skillBlocked(target string, pol SkillPolicy) string {
	if err := CheckNoSymlinks(pl.P.Root, target); err != nil {
		if !errors.Is(err, ErrSymlink) {
			// e.g. a skills directory without search permission (chmod 000): not a link.
			return "경로를 확인할 수 없어 거부합니다 (권한 또는 파일 시스템 오류: " + MaskLine(textsafe.Display(err.Error(), 160)) + ")"
		}
		return "심볼릭 링크 경로라 따라가지 않고 거부합니다" + pol.LinkHint
	}
	return ""
}

// TamperedSnapshotReason is the (short, table-friendly) conflict reason for a
// skill whose registry snapshot does not match its registered content hash. It
// names the real cause (not a hand edit of the client copy, not a name clash);
// the sync report adds the full way out below the table (TamperedSnapshotHelp).
func TamperedSnapshotReason(name string) string {
	return "레지스트리 스냅샷 변조·손상(해시 불일치) — forge skill check " + name
}

// HardLinkedSnapshotReason is the conflict reason for a registry snapshot that
// contains a hard-linked file (TASK-0027 AC-19): refused without reading it, which
// is not the same as a content hash mismatch.
func HardLinkedSnapshotReason(name string) string {
	return "레지스트리 스냅샷 안에 하드링크된 파일이 있어 읽지 않습니다(해시 불일치가 아님) — forge skill check " + name
}

// TamperedSnapshotHelp is the resolution text of a tampered registry snapshot.
const TamperedSnapshotHelp = "변조·손상되었거나 하드링크된 파일을 가진 레지스트리 스냅샷은 sync 되지 않습니다(--force-managed 는 효과 없음). " +
	"forge skill check <이름> 으로 확인하고, 변조가 맞으면 forge skill remove <이름> --yes 후 신뢰하는 소스에서 다시 추가하세요."

func (pl *Planner) planSkill(s SkillInput, pol SkillPolicy) {
	target := filepath.Join(pl.P.SkillsDir, s.Name)
	it := Item{Kind: KindSkill, Name: s.Name, Path: target}
	conflict := func(reason string, overridable bool) {
		it.Action, it.Reason, it.Overridable = ActionConflict, reason, overridable
		pl.Item(it)
	}
	if r := pl.skillBlocked(target, pol); r != "" {
		conflict(r, false)
		return
	}
	if gerr := GuardPath(target); gerr != nil {
		conflict("보안 정책: 대상 경로가 비밀/설정 파일 위치 또는 그 파일과 같은 파일이라 쓰지 않습니다 ("+MaskLine(gerr.Error())+")", false)
		return
	}
	if gerr := GuardPath(s.StoreDir); gerr != nil {
		conflict("보안 정책으로 레지스트리 스냅샷 경로를 읽을 수 없습니다: "+MaskLine(gerr.Error()), false)
		return
	}
	wantHash, _, err := TreeHash(s.StoreDir, false)
	if err != nil || (s.Hash != "" && wantHash != s.Hash) {
		it.Tampered = true
		if errors.Is(err, ErrHardLinked) {
			// Not a hash mismatch: a file of the registry snapshot has another name
			// (maybe ~/.ssh/...). It is never read; say so instead of "tampered".
			conflict(HardLinkedSnapshotReason(s.Name), false)
			return
		}
		conflict(TamperedSnapshotReason(s.Name), false)
		return
	}
	mk := marker{ManagedBy: "forge", Name: s.Name, Version: s.Version, TreeHash: wantHash}
	snap, err := Snapshot(target)
	if err != nil {
		conflict(unreadableTree(err), false)
		return
	}
	st := pl.State.Find(KindSkill, s.Name)
	op := Op{Kind: OpReplaceTree, Path: target, Before: snap,
		Tree: &TreeSpec{SrcDir: s.StoreDir, Extra: map[string][]byte{MarkerName: mk.bytes()}}}
	commit := func(a Action, reason string) {
		if r := WriteBlocker(target); r != "" {
			conflict(r, false)
			return
		}
		it.Action, it.Reason = a, reason
		pl.Item(it)
		pl.Plan.Ops = append(pl.Plan.Ops, op)
		pl.State.Upsert(KindSkill, s.Name, wantHash, pl.Now)
	}
	if !snap.Exists {
		reason := "레지스트리 " + s.Version
		if st != nil {
			reason = "관리 디렉터리가 없어 다시 생성 (" + s.Version + ")"
		}
		commit(ActionAdd, reason)
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
	if h, _, err := TreeHash(target, true); err == nil {
		st = pl.Adopt(KindSkill, s.Name, h)
	}
	if st == nil {
		if pl.JournalListed(KindSkill, s.Name) {
			conflict("중단된 이전 sync 가 만든 디렉터리이지만 그 뒤 내용이 수정되어 관리 항목으로 인정하지 않습니다 (되돌리거나 지운 뒤 sync)", false)
			return
		}
		if r := pl.LostBackupReason(KindSkill, s.Name); r != "" {
			conflict(r, false)
			return
		}
		conflict("관리 마커는 있으나 상태 파일에 기록이 없습니다 (다른 곳에서 복사된 디렉터리일 수 있음)", false)
		return
	}
	curHash, _, err := TreeHash(target, true)
	if err != nil {
		conflict("관리 디렉터리를 검사할 수 없습니다: 링크/특수 파일이 있음", true)
		return
	}
	modified := pl.ManagedModified(KindSkill, *st, curHash, cur.TreeHash)
	if modified && !pl.Req.ForceManaged {
		conflict("관리 디렉터리가 수정되었습니다 (해시 불일치). 덮어쓰려면 --force-managed (백업됨)", true)
		return
	}
	if !modified && curHash == wantHash && string(curBytes) == string(mk.bytes()) {
		it.Action, it.Reason = ActionUnchanged, "최신 ("+s.Version+")"
		pl.Item(it)
		return
	}
	reason := fmt.Sprintf("%s -> %s", cur.Version, s.Version)
	if modified {
		reason = "수정된 관리 디렉터리를 --force-managed 로 덮어씀"
	} else if cur.Version == s.Version {
		reason = "내용/마커 갱신 (" + s.Version + ")"
	}
	commit(ActionUpdate, reason)
}

func (pl *Planner) planStaleSkill(si StateItem, pol SkillPolicy) {
	if !SkillNameRe.MatchString(si.Name) {
		pl.State.Remove(KindSkill, si.Name)
		pl.Note("상태 파일의 올바르지 않은 스킬 항목을 무시하고 정리합니다")
		return
	}
	target := filepath.Join(pl.P.SkillsDir, si.Name)
	it := Item{Kind: KindSkill, Name: si.Name, Path: target}
	conflict := func(reason string, overridable bool) {
		it.Action, it.Reason, it.Overridable = ActionConflict, reason, overridable
		pl.Item(it)
	}
	if r := pl.skillBlocked(target, pol); r != "" {
		conflict(r, false)
		return
	}
	snap, err := Snapshot(target)
	if err != nil {
		conflict(unreadableTree(err), false)
		return
	}
	if !snap.Exists {
		it.Action, it.Reason = ActionSkip, "이미 없음: 상태 항목만 정리"
		pl.Item(it)
		pl.State.Remove(KindSkill, si.Name)
		return
	}
	mk, _, hasMarker := readMarker(target)
	if snap.Kind != "dir" || !hasMarker || mk.Name != si.Name {
		it.Action, it.Reason = ActionSkip, "관리 마커가 없어 사용자 항목으로 대체된 것으로 보고 건드리지 않음 (상태 항목만 정리)"
		pl.Item(it)
		pl.State.Remove(KindSkill, si.Name)
		return
	}
	curHash, _, err := TreeHash(target, true)
	modified := err != nil || pl.ManagedModified(KindSkill, si, curHash, mk.TreeHash)
	if modified && !pl.Req.ForceManaged {
		conflict("제거 대상 관리 디렉터리가 수정되었습니다 (해시 불일치). 지우려면 --force-managed (백업됨)", true)
		return
	}
	if r := WriteBlocker(target); r != "" {
		conflict(r, false)
		return
	}
	it.Action, it.Reason = ActionRemove, "레지스트리에서 비활성화되었거나 삭제됨 (백업 후 제거)"
	if pol.Claimed[si.Name] {
		it.Reason = pol.ClaimedRemove
	}
	pl.Item(it)
	pl.Plan.Ops = append(pl.Plan.Ops, Op{Kind: OpDeleteTree, Path: target, Before: snap})
	pl.State.Remove(KindSkill, si.Name)
}

// unreadableTree is the conflict reason for a skill directory that cannot be
// snapshotted. A hard-linked file inside it gets its own wording (with the remedy
// first), because the generic text would call it a link or special file.
func unreadableTree(err error) string {
	if errors.Is(err, ErrHardLinked) {
		return "스킬 디렉터리 안에 하드링크된 파일이 있어 읽지 않습니다. " + MaskLine(textsafe.Display(err.Error(), 200))
	}
	return "대상 디렉터리를 안전하게 읽을 수 없습니다 (링크/특수 파일 포함)"
}
