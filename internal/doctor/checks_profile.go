package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/profile"
)

// checkProfile: which precedence rule applies per client (project, global or
// legacy) and the health of the active profiles.
func checkProfile(e *env) []Check {
	conn, err := e.database()
	if err != nil {
		return []Check{e.dbSkipped("profile.skipped", "profile", "프로필", "프로필")}
	}
	clients, err := profile.Clients(e.ctx, conn)
	if err != nil {
		return []Check{unavailable("profile.rule", "profile", "프로필", err)}
	}
	var out []Check
	seen := map[int64]bool{}
	var actives []*profile.Resolved
	byClient := map[string]*profile.Resolved{}
	for _, c := range clients {
		r, err := profile.Resolve(e.ctx, conn, c, e.project)
		if err != nil {
			out = append(out, unavailable("profile.rule."+c, "profile", "프로필 규칙 "+c, err))
			continue
		}
		if !r.Active() {
			fix := []string{"forge profile create <이름> --from-current   # 지금 노출 중인 것을 스냅샷", "forge profile use <이름> --client " + c + "   # 모든 프로젝트에 활성화"}
			if e.project != "" {
				fix = append(fix, "forge profile use <이름> --client "+c+" --project-dir "+e.q(e.project)+"   # 이 프로젝트에서만")
			}
			out = append(out, mk("profile.rule."+c, "profile", "프로필 규칙 "+c, Info, "프로필 없음: 레거시 동작(활성화된 항목 전체 + mcp.exposure.* 설정)이 적용됩니다",
				"이 클라이언트에는 활성 프로필이 없습니다 (프로필이 있으면 허용 목록으로 동작해 프로필에 없는 항목은 노출되지 않습니다)", fix...))
			continue
		}
		where := "이 프로젝트에 활성화"
		if r.Rule == profile.RuleGlobal {
			where = "전역 활성화"
		}
		out = append(out, mk("profile.rule."+c, "profile", "프로필 규칙 "+c, OK, fmt.Sprintf("프로필 %s 적용 (%s; 멤버 %d, 허용 목록)", safe(r.Profile.Name, 60), where, len(r.Skills)+len(r.Tools)+len(r.MCP)), ""))
		byClient[c] = r
		if !seen[r.Profile.ID] {
			seen[r.Profile.ID] = true
			actives = append(actives, r)
		}
		out = append(out, e.legacyIgnored(c)...)
	}
	for _, r := range actives {
		// The per-profile claude-code/opencode skill overlap only makes sense for a profile
		// that applies to BOTH clients; with different profiles per client the same
		// skills are reported once, by crossProfileOverlap (TASK-0032 AC-5).
		both := byClient["claude-code"] != nil && byClient["opencode"] != nil &&
			byClient["claude-code"].Profile.ID == r.Profile.ID && byClient["opencode"].Profile.ID == r.Profile.ID
		out = append(out, e.profileHealth(r, clients, both)...)
	}
	out = append(out, e.crossProfileOverlap(byClient)...)
	out = append(out, e.duplicateProjects()...)
	out = append(out, e.staleActivations()...)
	out = append(out, e.recentDeletions()...)
	return out
}

// deletionWindow is how far back recentDeletions looks.
const deletionWindow = 14 * 24 * time.Hour

// recentDeletions (TASK-0032 AC-1): a skill, tool or MCP server that was removed
// from the registry took its profile memberships with it (ON DELETE CASCADE). The
// migration-0005 trigger recorded what each deletion removed from which profile;
// this reports the recent ones. Warn when an affected profile still exists (it
// silently shrank), info when the profile is gone too. A database without the audit
// table (before 0005) has nothing to report.
func (e *env) recentDeletions() []Check {
	var has int
	if err := e.db.QueryRowContext(e.ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='registry_deletions'`).Scan(&has); err != nil || has == 0 {
		return nil
	}
	since := e.now.Add(-deletionWindow).UnixMilli()
	rows, err := e.db.QueryContext(e.ctx, `SELECT kind, name, members, deleted_at FROM registry_deletions
		WHERE deleted_at >= ? AND members <> '[]' ORDER BY deleted_at DESC LIMIT 200`, since)
	if err != nil {
		if isCtx(err) {
			return nil
		}
		return []Check{unavailable("profile.deleted", "profile", "삭제된 프로필 멤버", err)}
	}
	type delRow struct {
		kind, name, members string
		at                  int64
	}
	var dels []delRow
	for rows.Next() { // the handle has one connection: read everything before the next query
		var d delRow
		if rows.Scan(&d.kind, &d.name, &d.members, &d.at) == nil {
			dels = append(dels, d)
		}
	}
	_ = rows.Close()
	existing := map[string]bool{}
	if pr, err := e.db.QueryContext(e.ctx, `SELECT scope || ':' || name FROM profiles`); err == nil {
		for pr.Next() {
			var l string
			if pr.Scan(&l) == nil {
				existing[l] = true
			}
		}
		_ = pr.Close()
	}
	var items []string
	shrunk := false
	var firstProfile string
	for _, d := range dels {
		kind, name, at := d.kind, d.name, d.at
		var labels []string
		if json.Unmarshal([]byte(d.members), &labels) != nil || len(labels) == 0 {
			continue
		}
		var shown []string
		for _, l := range labels {
			shown = append(shown, safe(l, 70))
			if existing[l] {
				shrunk = true
				if firstProfile == "" {
					firstProfile = strings.TrimPrefix(strings.TrimPrefix(l, "global:"), "project:")
				}
			}
		}
		items = append(items, fmt.Sprintf("%s:%s (%s 삭제, 프로필 %s)", kind, safe(name, 50), time.UnixMilli(at).UTC().Format("2006-01-02"), strings.Join(shown, ", ")))
	}
	if len(items) == 0 {
		return nil
	}
	st, cause := Info, "해당 프로필은 이미 없어졌습니다 (참고용 기록)"
	if shrunk {
		st, cause = Warn, "프로필 멤버십은 레지스트리 항목과 함께 사라집니다(조용한 CASCADE). 같은 이름으로 다시 등록해도 멤버는 자동으로 돌아오지 않으므로 프로필에 다시 추가해야 합니다. 삭제한 적이 없다면 누가 지웠는지 확인하세요"
	}
	fix := []string{"forge profile list   # 프로필 목록과 멤버 수 확인"}
	if firstProfile != "" {
		fix = append(fix, "forge profile show "+adapter.ShellQuote(firstProfile)+"   # 지금 남은 멤버 확인",
			"forge profile add "+adapter.ShellQuote(firstProfile)+" --skill|--tool|--mcp <이름>   # 다시 등록한 항목을 프로필에 추가")
	}
	return []Check{mk("profile.deleted", "profile", "삭제로 사라진 프로필 멤버", st,
		fmt.Sprintf("최근 %d일 안에 레지스트리 삭제로 프로필 멤버가 사라졌습니다 (%d건): %s", int(deletionWindow.Hours()/24), len(items), listNames(items)), cause, fix...)}
}

// crossProfileOverlap (TASK-0024 AC-6): claude-code and opencode apply DIFFERENT
// profiles, and a skill that the claude-code profile renders as direct
// (.claude/skills) is not direct in the opencode profile (hidden, gateway or not a
// member). OpenCode reads .claude/skills too, so it still sees the skill. The
// same-profile case is claudeOpenCodeOverlap; this one needs both resolutions.
func (e *env) crossProfileOverlap(byClient map[string]*profile.Resolved) []Check {
	cc, oc := byClient["claude-code"], byClient["opencode"]
	if cc == nil || oc == nil || cc.Profile.ID == oc.Profile.ID {
		return nil
	}
	disabled := e.disabledSkills() // a disabled skill is not rendered, so OpenCode cannot see it
	var names []string
	for n, ex := range cc.Skills {
		if ex != profile.ExposureDirect || disabled[n] {
			continue
		}
		if oc.Skills[n] != profile.ExposureDirect {
			names = append(names, safe(n, 50))
		}
	}
	if len(names) == 0 {
		return nil
	}
	pflag := ""
	if cc.Profile.Scope == profile.ScopeProject {
		pflag = " --project-dir " + e.q(cc.Profile.Project)
	}
	return []Check{mk("profile.crossclient", "profile", "클라이언트별 프로필의 스킬 노출 불일치", Warn,
		fmt.Sprintf("claude-code 프로필 %s 에서는 direct 인 스킬 %d건이 opencode 프로필 %s 에서는 direct 가 아닙니다: %s", safe(cc.Profile.Name, 40), len(names), safe(oc.Profile.Name, 40), listNames(names)),
		"OpenCode 는 .claude/skills 도 읽으므로 claude-code 에 렌더링된 스킬은 opencode 프로필이 숨기거나 빼도 OpenCode 에 보입니다 (opencode 에서만 숨길 수 없음). 다른 프로필을 클라이언트별로 쓸 때의 한계입니다",
		"forge profile current --client all   # 클라이언트별 적용 프로필 확인",
		"forge profile add "+adapter.ShellQuote(cc.Profile.Name)+pflag+" --skill "+adapter.ShellQuote(sortedFirst(names))+" --exposure hidden --client claude-code   # claude-code 쪽에서도 숨김")}
}

// disabledSkills returns the names of registered skills whose enabled flag is off.
func (e *env) disabledSkills() map[string]bool {
	out := map[string]bool{}
	rows, err := e.db.QueryContext(e.ctx, `SELECT name FROM skills WHERE enabled = 0`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out[n] = true
		}
	}
	return out
}

func sortedFirst(names []string) string {
	s := append([]string(nil), names...)
	sort.Strings(s)
	return s[0]
}

// staleActivations: activations of projects whose directory no longer exists
// (deleted or moved; ADR-F0001 O-4). Read-only: it only stats the stored paths.
func (e *env) staleActivations() []Check {
	list, err := profile.Stale(e.ctx, e.db)
	if err != nil {
		return []Check{unavailable("profile.stale", "profile", "없어진 프로젝트의 활성화", err)}
	}
	var paths []string
	n := 0
	for _, p := range list {
		if len(p.Activations) == 0 {
			continue
		}
		n += len(p.Activations)
		paths = append(paths, safe(p.Path, 120)+" (마지막 활성화 "+time.UnixMilli(p.LastActivation).UTC().Format("2006-01-02")+")")
	}
	if n == 0 {
		return nil
	}
	return []Check{mk("profile.stale", "profile", "없어진 프로젝트의 활성화", Warn,
		fmt.Sprintf("경로가 더 이상 없는 프로젝트 %d개에 프로필 활성화 %d건이 남아 있습니다: %s", len(paths), n, listNames(paths)),
		"프로젝트 디렉터리가 삭제되었거나 이동되었습니다. 활성화는 아무 영향이 없지만 profile delete 를 막고 목록을 어지럽힙니다 (옮겼다면 새 위치에서 다시 활성화하세요)",
		"forge profile prune-stale --dry-run   # 지울 목록(경로·마지막 활성화) 확인", "forge profile prune-stale --yes   # 실제로 정리 (프로필은 지우지 않음)")}
}

// duplicateProjects (TASK-0024 AC-11): several projects rows that are the same
// directory (case variants written before FindProject folded them). Info only:
// the rows are never merged automatically because each has its own activations
// and project profiles.
func (e *env) duplicateProjects() []Check {
	groups, err := profile.DuplicateProjects(e.ctx, e.db)
	if err != nil {
		if isCtx(err) {
			return nil
		}
		return []Check{unavailable("profile.projects.duplicate", "profile", "중복 프로젝트 행", err)}
	}
	if len(groups) == 0 {
		return nil
	}
	var parts []string
	for _, g := range groups {
		var ps []string
		for _, p := range g.Paths {
			ps = append(ps, tailSafe(p, 100))
		}
		parts = append(parts, strings.Join(ps, " = "))
	}
	return []Check{mk("profile.projects.duplicate", "profile", "중복 프로젝트 행", Info,
		fmt.Sprintf("같은 디렉터리를 가리키는 프로젝트 행이 %d묶음 있습니다: %s", len(groups), listNames(parts)),
		"대소문자 변형 등으로 같은 디렉터리가 여러 철자로 기록되었습니다. 행마다 활성화와 프로젝트 프로필이 따로라 자동으로 합치지 않습니다(forge profile merge-projects 로 직접 합칠 수 있습니다). 조회는 입력한 철자와 정확히 같은 행을 먼저, 없으면 id 가 가장 작은 행을 씁니다 (FindProject)",
		"forge profile list   # 프로젝트 프로필이 어느 행에 속하는지 확인", "forge profile current --client all --project-dir <경로>   # 적용되는 프로필 확인",
		"forge profile merge-projects "+e.q(groups[0].Paths[1])+" "+e.q(groups[0].Paths[0])+"   # 합치기 미리보기(dry-run: 충돌이 있으면 중단). 실제 병합은 --yes")}
}

// tailSafe is safe() that keeps the END of a long path (where two spellings of
// one directory differ) and cuts the beginning.
func tailSafe(p string, n int) string {
	d := []rune(safe(p, 4000))
	if len(d) <= n {
		return string(d)
	}
	return "…" + string(d[len(d)-n:])
}

// idName makes a profile name safe as a check id segment: "." and "%" are
// escaped so a name like "x.disabled" cannot collide with another check's suffix.
func idName(name string) string {
	return strings.NewReplacer("%", "%25", ".", "%2E").Replace(safe(name, 60))
}

// legacyIgnored: mcp.exposure.* settings are not used while a profile applies.
func (e *env) legacyIgnored(client string) []Check {
	var n int
	err := e.db.QueryRowContext(e.ctx, `SELECT count(*) FROM settings WHERE key LIKE 'mcp.exposure.%.'||? AND value IN ('gateway','hidden')`, client).Scan(&n)
	if err != nil || n == 0 {
		return nil
	}
	return []Check{mk("profile.legacy."+client, "profile", "레거시 노출 설정 "+client, Info, fmt.Sprintf("mcp.exposure.* 설정 %d건이 있지만 프로필이 적용 중이라 무시됩니다", n),
		"프로필이 노출의 정식 저장소입니다 (--from-current 로 만들었다면 이미 복사되어 있습니다)", "forge profile show <이름>   # 프로필의 노출 확인")}
}

func (e *env) profileHealth(r *profile.Resolved, clients []string, appliesToBoth bool) []Check {
	d, err := profile.GetDetail(e.ctx, e.db, r.Profile.ID)
	if err != nil {
		return []Check{unavailable("profile.health", "profile", "프로필 "+safe(r.Profile.Name, 40), err)}
	}
	name := safe(d.Name, 60)
	pflag := ""
	if d.Scope == profile.ScopeProject {
		pflag = " --project-dir " + e.q(d.Project)
	}
	id := "profile.health." + idName(d.Name)
	if d.Scope == profile.ScopeProject {
		id += "@project" // a global and a project profile may share a name
	}
	title := "프로필 " + name
	var out []Check
	var broken, disabled []string
	var brokenIDs []int64
	disabledBy := map[string][]string{}
	for _, m := range d.MemberList {
		label := m.Kind + ":" + safe(m.Name, 50)
		switch {
		case m.Broken != "":
			broken = append(broken, fmt.Sprintf("%s멤버 번호 %d (%s)", map[bool]string{true: "", false: label + " "}[m.Name == ""], m.ID, m.Broken))
			brokenIDs = append(brokenIDs, m.ID)
		case !m.Enabled:
			disabled = append(disabled, label)
			disabledBy[m.Kind] = append(disabledBy[m.Kind], m.Name)
		}
	}
	switch {
	case len(broken) > 0:
		out = append(out, mk(id, "profile", title, Fail, fmt.Sprintf("프로필이 가리키는 항목이 삭제되었거나 손상되었습니다 (%d건): %s", len(broken), listNames(broken)),
			"멤버 행이 존재하지 않는 스킬/도구/MCP 서버나 버전을 가리킵니다 (DB 가 손상되었거나 외부에서 수정됨)",
			brokenFix(d.Name, pflag, brokenIDs)...))
	case len(d.MemberList) == 0:
		out = append(out, mk(id, "profile", title, Warn, "활성 프로필이 비어 있습니다: 이 프로필이 적용되는 클라이언트에는 아무것도 노출되지 않습니다",
			"프로필은 허용 목록이라 멤버가 없으면 스킬·도구·MCP 서버가 모두 숨겨집니다",
			append([]string{"forge profile add " + adapter.ShellQuote(d.Name) + pflag + " --skill|--tool|--mcp <이름>"}, e.unuseFix(d.ID, clients)...)...))
	default:
		out = append(out, mk(id, "profile", title, OK, fmt.Sprintf("멤버 %d", len(d.MemberList)), ""))
	}
	if len(disabled) > 0 {
		out = append(out, mk(id+".disabled", "profile", title+" 비활성 멤버", Warn, fmt.Sprintf("프로필 멤버 중 비활성화된 항목이 있어 노출되지 않습니다 (%d건): %s", len(disabled), listNames(disabled)),
			"enabled 플래그가 프로필 멤버십보다 우선합니다", disabledFix(d.Name, pflag, disabledBy)...))
	}
	out = append(out, e.pinHealth(d, id, title, pflag)...)
	if appliesToBoth {
		out = append(out, e.claudeOpenCodeOverlap(d, id, title, pflag)...)
	}
	return out
}

// brokenFix: exact commands for damaged members (no name exists, so by row id).
func brokenFix(profileName, pflag string, ids []int64) []string {
	fix := []string{"forge profile show " + adapter.ShellQuote(profileName) + pflag + "   # 손상된 멤버와 번호 확인"}
	for i, id := range ids {
		if i == 4 {
			fix = append(fix, "# … 나머지 손상 멤버도 같은 방식(--member-id <번호>)으로 제거")
			break
		}
		fix = append(fix, fmt.Sprintf("forge profile remove %s%s --member-id %d   # 손상된 멤버 제거", adapter.ShellQuote(profileName), pflag, id))
	}
	return fix
}

// disabledFix: the exact enable command per member kind and name, plus the way to
// drop the member from the profile instead.
func disabledFix(profileName, pflag string, byKind map[string][]string) []string {
	var fix []string
	for _, kind := range []string{profile.KindSkill, profile.KindMCP, profile.KindTool} {
		cmd := map[string]string{profile.KindSkill: "forge skill enable ", profile.KindMCP: "forge mcp enable ", profile.KindTool: "forge tool enable "}[kind]
		for i, n := range byKind[kind] {
			if i == 2 {
				break
			}
			fix = append(fix, cmd+adapter.ShellQuote(n))
		}
	}
	for _, kind := range []string{profile.KindSkill, profile.KindMCP, profile.KindTool} {
		if ns := byKind[kind]; len(ns) > 0 {
			fix = append(fix, "forge profile remove "+adapter.ShellQuote(profileName)+pflag+" --"+kind+" "+adapter.ShellQuote(ns[0])+"   # 또는 프로필에서 제외")
			break
		}
	}
	return fix
}

// unuseFix: the exact `profile unuse` commands for every place the profile is
// activated: a project activation needs its --project-dir, the global one does not.
func (e *env) unuseFix(profileID int64, allClients []string) []string {
	acts, err := profile.ActivationsOfProfile(e.ctx, e.db, profileID)
	if err != nil || len(acts) == 0 {
		return []string{"forge profile unuse --client all   # 활성 해제(레거시 동작으로 복귀)"}
	}
	byProject := map[string][]string{}
	var order []string
	for _, a := range acts {
		if _, ok := byProject[a.Project]; !ok {
			order = append(order, a.Project)
		}
		byProject[a.Project] = append(byProject[a.Project], a.Client)
	}
	var fix []string
	for _, proj := range order {
		cs := byProject[proj]
		flag := ""
		where := "전역 활성화"
		if proj != "" {
			flag = " --project-dir " + e.q(proj)
			where = "이 프로젝트의 활성화"
		}
		if len(cs) == len(allClients) {
			fix = append(fix, "forge profile unuse --client all"+flag+"   # "+where+" 해제(레거시 동작으로 복귀)")
			continue
		}
		for _, c := range cs {
			fix = append(fix, "forge profile unuse --client "+c+flag+"   # "+where+" 해제")
		}
	}
	return fix
}

// pinHealth: a pinned skill version whose stored files are gone.
func (e *env) pinHealth(d *profile.Detail, id, title, pflag string) []Check {
	rows, err := e.db.QueryContext(e.ctx, `SELECT s.name, v.version, v.store_path
		FROM profile_members m JOIN skills s ON s.id=m.skill_id JOIN skill_versions v ON v.id=m.pinned_version_id
		WHERE m.profile_id=?`, d.ID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var bad []string
	skillsRoot := filepath.Join(e.o.Home, "skills")
	for rows.Next() {
		var n, v, sp string
		if rows.Scan(&n, &v, &sp) != nil {
			continue
		}
		dir := filepath.Join(e.o.Home, filepath.FromSlash(sp))
		if rel, err := filepath.Rel(skillsRoot, dir); err != nil || strings.HasPrefix(rel, "..") {
			bad = append(bad, safe(n, 40)+"@"+safe(v, 30)+" (저장소 밖 경로)")
			continue
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			bad = append(bad, safe(n, 40)+"@"+safe(v, 30))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return []Check{mk(id+".pins", "profile", title+" 핀 버전", Warn, fmt.Sprintf("고정(pin)한 스킬 버전의 저장된 파일이 없습니다 (%d건): %s", len(bad), listNames(bad)),
		"불변 버전 스냅샷(~/.forge/skills/<이름>@<버전>)이 지워졌습니다. sync 와 forge_skill 이 그 스킬을 제공하지 못합니다",
		"forge skill show <이름>   # 등록된 버전 확인", "forge profile add "+adapter.ShellQuote(d.Name)+pflag+" --skill <이름> --pin <다른 버전>", "forge skill add <원본 경로>   # 같은 내용을 다시 등록")}
}

// claudeOpenCodeOverlap: OpenCode also reads .claude/skills, so a skill that is
// direct for claude-code stays visible to OpenCode even when the profile hides it there.
func (e *env) claudeOpenCodeOverlap(d *profile.Detail, id, title, pflag string) []Check {
	var names []string
	for _, m := range d.MemberList {
		if m.Kind != profile.KindSkill || m.Broken != "" || !m.Enabled {
			continue
		}
		if m.ExposureFor("claude-code") == profile.ExposureDirect && m.ExposureFor("opencode") != profile.ExposureDirect {
			names = append(names, safe(m.Name, 50))
		}
	}
	if len(names) == 0 {
		return nil
	}
	fix := []string{}
	for i, n := range names {
		if i == 3 {
			break
		}
		fix = append(fix, "forge profile add "+adapter.ShellQuote(d.Name)+pflag+" --skill "+adapter.ShellQuote(n)+" --exposure hidden --client claude-code   # 양쪽에서 숨김")
	}
	return []Check{mk(id+".overlap", "profile", title+" 스킬 노출 불일치", Warn, fmt.Sprintf("claude-code 에는 direct, opencode 에는 hidden/gateway 인 스킬 %d건: %s", len(names), listNames(names)),
		"OpenCode 는 .claude/skills 도 읽으므로 claude-code 에 렌더링된 스킬은 opencode 에서도 보입니다 (opencode 에서만 숨길 수 없음)", fix...)}
}

// skillStores: enabled skills whose immutable snapshot directory is missing.
func (e *env) skillStores() []Check {
	rows, err := e.db.QueryContext(e.ctx, `SELECT s.name, v.store_path FROM skills s JOIN skill_versions v ON v.id=s.current_version_id WHERE s.enabled=1 ORDER BY s.name LIMIT 500`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	skillsRoot := filepath.Join(e.o.Home, "skills")
	var bad []string
	for rows.Next() {
		var n, sp string
		if rows.Scan(&n, &sp) != nil {
			continue
		}
		dir := filepath.Join(e.o.Home, filepath.FromSlash(sp))
		if rel, err := filepath.Rel(skillsRoot, dir); err != nil || strings.HasPrefix(rel, "..") {
			bad = append(bad, safe(n, 40)+" (저장소 밖 경로)")
			continue
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			bad = append(bad, safe(n, 40))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return []Check{mk("paths.skillstore", "paths", "스킬 저장소", Warn, fmt.Sprintf("활성 스킬의 저장된 파일이 없습니다 (%d건): %s", len(bad), listNames(bad)),
		"레지스트리에는 있지만 ~/.forge/skills 의 스냅샷 디렉터리가 지워졌습니다", "forge skill show <이름>", "forge skill add <원본 경로>   # 다시 등록")}
}
