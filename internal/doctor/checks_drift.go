package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/adapter/claudecode"
	"github.com/heonjinjeong/forge/internal/adapter/opencode"
)

// planned is one (target, scope) plan computed for the drift and path checks.
type planned struct {
	target string
	scope  adapter.Scope
	root   string
	plan   *adapter.Plan
	in     adapter.Inputs
	state  *adapter.State
	err    error
	// hooks is true when claude-code hooks are managed in this scope.
	hooks bool
}

func (p *planned) flags(e *env) string {
	s := "--target " + p.target + " --scope " + string(p.scope)
	if p.scope == adapter.ScopeProject {
		s += " --project-dir " + e.q(p.root)
	}
	return s
}

func targets(home string) []adapter.Adapter {
	return []adapter.Adapter{claudecode.New(home), opencode.New(home)}
}

type rootResolver interface {
	ResolveRoot(adapter.Request) (string, []string, error)
}

// configFile returns the client config file that holds MCP entries (project scope).
func configFile(target, root string) (path, key string) {
	if target == claudecode.Name {
		return filepath.Join(root, ".mcp.json"), "mcpServers"
	}
	return filepath.Join(root, opencode.ConfigFile), "mcp"
}

// readServers reads the server map of a client config without executing or
// writing anything. A missing file is (nil, false, nil).
func readServers(path, key string) (map[string]json.RawMessage, bool, error) {
	b, _, ok, err := adapter.ReadFileSafe(path)
	if err != nil || !ok {
		return nil, ok, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, true, errors.New("JSON 으로 해석할 수 없습니다")
	}
	raw, has := top[key]
	if !has {
		return map[string]json.RawMessage{}, true, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, true, fmt.Errorf("%s 가 객체가 아닙니다", key)
	}
	return m, true, nil
}

func entryCommand(target string, raw json.RawMessage) string {
	if target == claudecode.Name {
		var v struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(raw, &v) == nil {
			return v.Command
		}
		return ""
	}
	var v struct {
		Command []string `json:"command"`
	}
	if json.Unmarshal(raw, &v) == nil && len(v.Command) > 0 {
		return v.Command[0]
	}
	return ""
}

var plainBinRe = regexp.MustCompile(`^[A-Za-z0-9._/+:@%=,~ -]{1,300}$`)

// forgeBin returns the forge command already written into the gateway entry, so
// that planning with it only reports real drift (the path check compares it with
// the running binary separately). It falls back to "forge".
func forgeBin(target string, scope adapter.Scope, root string) string {
	if scope != adapter.ScopeProject {
		return "forge"
	}
	path, key := configFile(target, root)
	m, _, err := readServers(path, key)
	if err != nil || m == nil {
		return "forge"
	}
	raw, ok := m[claudecode.GatewayServerName]
	if !ok {
		return "forge"
	}
	c := entryCommand(target, raw)
	if c == "" || !plainBinRe.MatchString(c) || (strings.ContainsRune(c, '/') && !filepath.IsAbs(c)) {
		return "forge"
	}
	return c
}

// checkDrift computes the plan of every (target, scope) read-only and reports
// pending changes, conflicts, interrupted syncs, leftover temporary files, the
// backup dependency of interrupted syncs and refused links.
func checkDrift(e *env) []Check {
	var out []Check
	homeForPlan := e.o.UserHome
	if _, err := e.database(); err != nil {
		return []Check{e.dbSkipped("drift.skipped", "drift", "sync 드리프트", "드리프트")}
	}
	for _, sc := range e.scope {
		if sc == adapter.ScopeProject && e.projectErr != nil {
			out = append(out, unavailable("drift.project", "drift", "sync 드리프트(project)", e.projectErr))
			continue
		}
		for _, a := range targets(e.o.Home) {
			p := &planned{target: a.Name(), scope: sc}
			e.plans[a.Name()+"/"+string(sc)] = p
			req := adapter.Request{Scope: sc, ProjectDir: e.project, Home: homeForPlan, BackupDir: filepath.Join(e.o.Home, "backups")}
			rr := a.(rootResolver)
			root, _, err := rr.ResolveRoot(req)
			if err != nil {
				p.err = err
				out = append(out, mk("drift."+a.Name()+"."+string(sc), "drift", "sync 드리프트 "+a.Name()+"/"+string(sc), Warn,
					"점검 불가: "+safe(err.Error(), 220), "이 범위의 루트를 정할 수 없습니다",
					"forge doctor --project-dir <프로젝트 디렉터리> --scope project"))
				continue
			}
			p.root = root
			req.ForgeBin = forgeBin(a.Name(), sc, root)
			if a.Name() == claudecode.Name {
				p.hooks = e.hooksManaged(sc, root)
				req.NoHooks = !p.hooks // never installed: not drift (use `forge sync --apply` to install)
			}
			proj := ""
			if sc == adapter.ScopeProject {
				proj = root
			}
			in, err := adapter.LoadInputsDB(e.ctx, e.db, e.o.Home, a.Name(), proj)
			if err != nil {
				p.err = err
				out = append(out, unavailable("drift."+a.Name()+"."+string(sc), "drift", "sync 드리프트 "+a.Name()+"/"+string(sc), err))
				continue
			}
			p.in = in
			plan, err := a.Plan(e.ctx, in, req)
			if err != nil {
				p.err = err
				out = append(out, e.planError(p, err))
				continue
			}
			p.plan = plan
			if plan.StatePath != "" {
				if b, _, ok, rerr := adapter.ReadFileSafe(plan.StatePath); rerr == nil && ok {
					if st, perr := adapter.ParseState(b, a.Name(), sc); perr == nil {
						p.state = st
					}
				}
			}
			out = append(out, e.driftChecks(p)...)
		}
	}
	return out
}

// hooksManaged reports whether the claude-code sidecar lists a managed hook.
func (e *env) hooksManaged(sc adapter.Scope, root string) bool {
	path := claudecode.New(e.o.Home).StatePath(sc, root)
	b, _, ok, err := adapter.ReadFileSafe(path)
	if err != nil || !ok {
		return false
	}
	st, err := adapter.ParseState(b, claudecode.Name, sc)
	if err != nil {
		return false
	}
	for _, it := range st.Items {
		if it.Kind == adapter.KindHook {
			return true
		}
	}
	return false
}

func (e *env) planError(p *planned, err error) Check {
	id := "drift." + p.target + "." + string(p.scope)
	title := "sync 드리프트 " + p.target + "/" + string(p.scope)
	msg := safe(err.Error(), 260)
	switch {
	case errors.Is(err, adapter.ErrHardLinked):
		return mk(id, "drift", title, Warn, "하드링크된 설정 파일이 있어 sync 가 거부합니다: "+msg, "하드링크는 다른 파일(비밀 파일 포함)과 내용을 공유해 sync 가 읽거나 쓰지 않습니다",
			"cp -p FILE FILE.tmp && mv FILE.tmp FILE   # FILE = 위 경로: 링크를 풀고 복사본으로 교체")
	case errors.Is(err, adapter.ErrSymlink):
		return mk(id, "drift", title, Warn, "심볼릭 링크 설정 파일이 있어 sync 가 거부합니다: "+msg, "sync 는 링크를 따라가지 않습니다",
			"ls -l FILE   # FILE = 위 경로", "링크 대상의 내용으로 일반 파일을 만든 뒤 forge sync "+p.flags(e))
	case strings.Contains(msg, "JSON"):
		return mk(id, "drift", title, Warn, "설정 파일을 해석할 수 없어 sync 가 아무것도 쓰지 않고 중단합니다: "+msg, "설정 파일이 올바른 JSON 이 아닙니다 (Forge 는 사용자 파일을 추측해 고치지 않습니다)",
			"python3 -m json.tool FILE   # FILE = 위 경로: 오류 위치를 확인하고 고치세요", "forge sync "+p.flags(e)+" --diff")
	}
	return mk(id, "drift", title, Warn, "점검 불가: "+msg, "계획을 세우지 못했습니다", "forge sync "+p.flags(e)+" --diff")
}

func (e *env) driftChecks(p *planned) []Check {
	var out []Check
	id := "drift." + p.target + "." + string(p.scope)
	title := "sync 드리프트 " + p.target + "/" + string(p.scope)
	var conflicts, links, pending, removals []string
	var overridable, userOwned int
	var tampered []string
	for _, it := range p.plan.Items {
		name := it.Kind + ":" + safe(it.Name, 60)
		switch it.Action {
		case adapter.ActionConflict:
			if strings.Contains(it.Reason, "심볼릭 링크") || strings.Contains(it.Reason, "하드링크") {
				links = append(links, name)
				continue
			}
			conflicts = append(conflicts, name)
			if it.Tampered {
				tampered = append(tampered, safe(it.Name, 60))
				continue
			}
			if it.Overridable {
				overridable++
			} else {
				userOwned++
			}
		case adapter.ActionAdd, adapter.ActionUpdate:
			pending = append(pending, name)
		case adapter.ActionRemove:
			removals = append(removals, name)
		}
	}
	sync := "forge sync " + p.flags(e)
	switch {
	case len(conflicts) > 0:
		fix := []string{sync + " --diff   # 무엇이 다른지 확인"}
		if overridable > 0 {
			fix = append(fix, sync+" --apply --force-managed   # 직접 수정한 관리 항목만 덮어씀(백업됨)")
		}
		fix = append(fix, sync+" --apply --skip-conflicts   # 충돌 항목만 제외하고 적용")
		var cause string
		if overridable+userOwned > 0 {
			cause = "Forge 가 관리하는 항목을 직접 수정했거나, 같은 이름의 사용자 항목이 이미 있습니다"
			if userOwned > 0 {
				cause += " (사용자 항목은 덮어쓰지 않습니다: 이름을 바꾸거나 직접 정리하세요)"
			}
		}
		if len(tampered) > 0 {
			// Registry snapshot hash mismatch: a different cause with a different way out.
			t := "레지스트리 스냅샷이 등록된 해시와 다른 스킬(" + listNames(tampered) + ")이 있습니다: 변조 또는 손상이며 --force-managed 로 해결되지 않습니다. 확인 forge skill check <이름>, 변조가 맞으면 forge skill remove <이름> --yes 후 신뢰하는 소스에서 다시 추가"
			if cause != "" {
				cause += ". "
			}
			cause += t
			fix = append([]string{"forge skill check <이름>   # 변조·손상 확인(읽기 전용)"}, fix...)
		}
		out = append(out, mk(id, "drift", title, Warn, fmt.Sprintf("충돌 %d건: %s", len(conflicts), listNames(conflicts)), cause, fix...))
	case len(pending)+len(removals) > 0:
		out = append(out, mk(id, "drift", title, Warn, fmt.Sprintf("미적용 변경 %d건: %s", len(pending)+len(removals), listNames(append(append([]string{}, pending...), removals...))),
			"레지스트리/프로필이 바뀌었지만 아직 sync 하지 않았습니다", sync+"   # 계획 확인", sync+" --apply"))
	default:
		out = append(out, mk(id, "drift", title, OK, "레지스트리와 설정이 일치합니다 ("+e.ruleNote(p)+")", ""))
	}
	if len(links) > 0 {
		out = append(out, mk(id+".links", "drift", title+" 링크", Warn, fmt.Sprintf("링크 때문에 거부된 항목 %d건: %s", len(links), listNames(links)),
			"sync 는 심볼릭 링크 경로와 하드링크된 파일을 따라가지 않습니다 (다른 파일을 덮어쓰거나 비밀을 읽는 것을 막기 위함)",
			sync+" --diff   # 거부된 경로 확인", "링크를 풀고 일반 파일/디렉터리 복사본으로 교체하세요: cp -pR SRC DST.tmp && rm DST && mv DST.tmp DST"))
	}
	if len(removals) > 0 && len(conflicts) == 0 {
		out = append(out, mk(id+".stale", "paths", title+" 잔존 항목", Warn, fmt.Sprintf("비활성화/삭제/프로필 제외된 항목이 설정에 남아 있습니다 (%d건): %s", len(removals), listNames(removals)),
			"레지스트리에서는 더 이상 노출 대상이 아니지만 클라이언트 설정에는 Forge 가 쓴 항목이 남아 있습니다", sync+" --apply   # 관리 항목만 제거(백업됨)"))
	}
	if len(p.plan.Interrupted) > 0 {
		in := p.plan.Interrupted[0]
		out = append(out, mk(id+".interrupted", "drift", title+" 중단된 sync", Warn,
			fmt.Sprintf("중단된 %s %d건 (백업 %s: 파일 %d개 중 완료 %d · 대기 %d · 수정/중간 상태 %d)", in.Kind, len(p.plan.Interrupted), safe(in.ID, 40), in.Ops, in.Done, in.Todo, in.Changed),
			"이전 forge sync 가 끝나기 전에 중단되었습니다 (저널이 남아 있음)", sync+"   # 계획 확인", sync+" --apply   # 이어서 완료(그 사이 수정한 파일은 충돌로 보고)"))
	}
	if len(p.plan.InvalidJournals) > 0 {
		out = append(out, mk(id+".journal", "drift", title+" 저널", Warn, fmt.Sprintf("믿을 수 없는 저널 %d건 무시: %s", len(p.plan.InvalidJournals), listNames(append([]string{}, p.plan.InvalidJournals...))),
			"저널이 손상되었거나 변조되었습니다 (자동 복구에 쓰지 않습니다)", "forge sync --list-backups", "forge sync --restore <id> --dry-run"))
	}
	if len(p.plan.StaleTemps) > 0 {
		out = append(out, mk(id+".temps", "drift", title+" 임시 파일", Info, fmt.Sprintf("중단된 sync 가 남긴 임시 파일 %d개", len(p.plan.StaleTemps)),
			"sync 가 파일을 쓰다 중단되었습니다", sync+" --apply   # 잠금을 잡고 정리"))
	}
	out = append(out, e.backupDependency(p)...)
	return out
}

func (e *env) ruleNote(p *planned) string {
	if p.in.Profile == nil {
		return "프로필 없음"
	}
	if !p.in.Profile.Active() {
		return "프로필 없음: 레거시 동작"
	}
	return "프로필 " + safe(p.in.Profile.Profile.Name, 40) + " (" + p.in.Profile.Rule + ")"
}

// backupDependency: an interrupted sync is recovered from <forge home>/backups.
// A journal copy next to the state file whose backup directory is gone cannot be
// recovered safely: the plan would show misleading conflicts (TASK-0022 AC-1/8).
func (e *env) backupDependency(p *planned) []Check {
	if p.plan.JournalPath == "" {
		return nil
	}
	b, _, ok, err := adapter.ReadFileSafe(p.plan.JournalPath)
	if err != nil || !ok {
		return nil
	}
	var j struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(b, &j) != nil || !adapter.ValidBackupID(j.ID) {
		return []Check{mk("drift."+p.target+"."+string(p.scope)+".backup", "drift", "sync 저널 사본 "+p.target+"/"+string(p.scope), Warn,
			"저널 사본을 해석할 수 없습니다: "+safe(p.plan.JournalPath, 200), "저널 사본이 손상되었거나 변조되었습니다",
			"ls -l "+e.q(p.plan.JournalPath), "forge sync --list-backups")}
	}
	dir := filepath.Join(e.o.Home, "backups", j.ID)
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		return nil
	}
	sync := "forge sync " + p.flags(e)
	return []Check{mk("drift."+p.target+"."+string(p.scope)+".backup", "drift", "중단된 sync 의 백업 "+p.target+"/"+string(p.scope), Warn,
		fmt.Sprintf("저널 사본(백업 %s)이 있는데 백업 디렉터리가 없습니다", safe(j.ID, 40)),
		"중단된 sync 의 복구는 "+e.o.Home+"/backups 의 백업에 의존합니다. 백업이 지워져 복구할 수 없고, 계획이 \"충돌\"을 잘못 보고할 수 있습니다 (직접 수정한 것이 아닐 수 있음)",
		sync+" --diff   # 실제 차이 확인",
		sync+" --apply --force-managed   # 관리 항목을 레지스트리 기준으로 덮어씀(새 백업 생성)",
		sync+" --apply --skip-conflicts   # 충돌 항목만 제외",
		"rm "+e.q(p.plan.JournalPath)+"   # 복구를 포기하고 저널 사본 삭제(위 방법으로 정리한 뒤)")}
}
