package doctor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/adapter/claudecode"
	"github.com/as950118/forge/internal/mcp"
)

func (e *env) orderedPlans() []*planned {
	var out []*planned
	for _, sc := range e.scope {
		for _, a := range targets(e.o.Home) {
			if p := e.plans[a.Name()+"/"+string(sc)]; p != nil && p.plan != nil {
				out = append(out, p)
			}
		}
	}
	return out
}

// commandExists only LOOKS for the executable (exec.LookPath / stat); it never runs it.
func commandExists(cmd string) (found bool, relative bool) {
	if cmd == "" {
		return false, false
	}
	if strings.ContainsRune(cmd, '/') {
		if !filepath.IsAbs(cmd) {
			return false, true
		}
		_, err := exec.LookPath(cmd)
		return err == nil, false
	}
	_, err := exec.LookPath(cmd)
	return err == nil, false
}

// checkPaths: broken paths and unconnected pieces of the managed configuration.
func checkPaths(e *env) []Check {
	var out []Check
	if e.db == nil {
		return nil
	}
	for _, p := range e.orderedPlans() {
		prefix := "paths." + p.target + "." + string(p.scope)
		label := p.target + "/" + string(p.scope)
		sync := "forge sync " + p.flags(e)
		out = append(out, e.missingSkillDirs(p, prefix, label, sync)...)
		out = append(out, e.cachelessGatewayServers(p, prefix, label)...)
		if p.scope == adapter.ScopeProject {
			out = append(out, e.configEntryChecks(p, prefix, label, sync)...)
		}
	}
	out = append(out, e.skillStores()...)
	if len(out) == 0 {
		out = append(out, mk("paths.ok", "paths", "경로·연결", OK, "관리 중인 스킬 디렉터리, MCP 명령, 게이트웨이 항목에 문제가 없습니다", ""))
	}
	return out
}

func (e *env) missingSkillDirs(p *planned, prefix, label, sync string) []Check {
	if p.state == nil {
		return nil
	}
	var missing []string
	for _, it := range p.plan.Items {
		if it.Kind != adapter.KindSkill || it.Path == "" || (it.Action != adapter.ActionAdd && it.Action != adapter.ActionUpdate) {
			continue
		}
		if p.state.Find(adapter.KindSkill, it.Name) == nil {
			continue
		}
		if _, err := os.Lstat(it.Path); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, safe(it.Name, 60))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Check{mk(prefix+".skilldirs", "paths", "스킬 디렉터리 "+label, Warn, fmt.Sprintf("관리 중인 스킬 디렉터리가 없습니다 (%d건): %s", len(missing), listNames(missing)),
		"Forge 가 쓴 스킬 디렉터리를 누가 지웠습니다 (클라이언트가 스킬을 못 찾음)", sync+"   # 계획 확인", sync+" --apply   # 다시 만듦")}
}

// cachelessGatewayServers: exposure=gateway servers without a tools/list cache.
func (e *env) cachelessGatewayServers(p *planned, prefix, label string) []Check {
	var names []string
	for _, s := range p.in.Servers {
		if s.Exposure[p.in.Client] == mcp.ExposureGateway && s.Enabled && s.ToolCount == 0 {
			names = append(names, safe(s.Name, 60))
		}
	}
	if len(names) == 0 {
		return nil
	}
	fix := []string{}
	for i, n := range names {
		if i == 3 {
			fix = append(fix, "forge mcp refresh   # 전체 서버")
			break
		}
		fix = append(fix, "forge mcp refresh "+adapter.ShellQuote(n))
	}
	return []Check{mk(prefix+".toolcache", "paths", "MCP 도구 캐시 "+label, Warn, fmt.Sprintf("게이트웨이로 노출하는데 tools/list 캐시가 없는 MCP 서버 %d건: %s", len(names), listNames(names)),
		"캐시가 없으면 게이트웨이가 그 서버의 도구를 노출하지 못합니다 (아직 forge mcp refresh 를 하지 않았거나 실패)", fix...)}
}

func (e *env) configEntryChecks(p *planned, prefix, label, sync string) []Check {
	path, key := configFile(p.target, p.root)
	servers, exists, err := readServers(path, key)
	if err != nil {
		return nil // the drift check reports unreadable/unparsable files
	}
	var out []Check
	// exposure=gateway (or profile tools) but no gateway entry in the config.
	needGateway := false
	for _, s := range p.in.Servers {
		if s.Exposure[p.in.Client] == mcp.ExposureGateway {
			needGateway = true
		}
	}
	if p.in.Profile != nil && p.in.Profile.Active() {
		for _, m := range p.in.Profile.Tools {
			if m != "hidden" {
				needGateway = true
			}
		}
	}
	_, hasGateway := servers[claudecode.GatewayServerName]
	if needGateway && !hasGateway {
		out = append(out, mk(prefix+".gateway", "paths", "게이트웨이 항목 "+label, Warn,
			"게이트웨이(gateway)로 노출하도록 설정했는데 "+filepath.Base(path)+" 에 게이트웨이 항목(forge)이 없습니다",
			"게이트웨이 항목이 없으면 클라이언트가 forge mcp serve 를 시작하지 않아 gateway 노출 항목이 보이지 않습니다", sync+" --apply"))
	}
	if p.state == nil || !exists {
		return out
	}
	var broken []string
	for _, it := range p.state.Items {
		if it.Kind != adapter.KindMCP {
			continue
		}
		raw, ok := servers[it.Name]
		if !ok {
			continue // the drift check reports a missing managed entry
		}
		cmd := entryCommand(p.target, raw)
		if it.Name == claudecode.GatewayServerName {
			out = append(out, e.gatewayPath(p, prefix, label, sync, cmd)...)
			continue
		}
		if cmd == "" {
			continue // http/sse entries have no command
		}
		if found, rel := commandExists(cmd); !found {
			why := "없음"
			if rel {
				why = "상대 경로"
			}
			broken = append(broken, fmt.Sprintf("%s (%s: %s)", safe(it.Name, 40), safe(cmd, 60), why))
		}
	}
	if len(broken) > 0 {
		out = append(out, mk(prefix+".mcpcommand", "paths", "MCP 명령 "+label, Warn, fmt.Sprintf("관리 중인 MCP 항목의 실행 파일을 찾을 수 없습니다 (%d건): %s", len(broken), listNames(broken)),
			"클라이언트가 이 서버를 시작하지 못합니다 (PATH 에 없거나 삭제됨). 존재 여부만 확인했고 실행하지 않았습니다",
			"which <명령>   # 설치 위치 확인 (명령 이름은 위 메시지)", "forge mcp show <서버>   # 등록된 정의", sync+" --apply   # 정의를 바꾼 뒤 설정 갱신"))
	}
	return out
}

func (e *env) gatewayPath(p *planned, prefix, label, sync, cmd string) []Check {
	id, title := prefix+".gatewaypath", "게이트웨이 forge 경로 "+label
	if cmd == "" {
		return nil
	}
	if e.exe == "" {
		return nil
	}
	found, rel := commandExists(cmd)
	fix := sync + " --apply --bin " + e.q(e.exe) + "   # 현재 forge 바이너리로 갱신"
	switch {
	case rel:
		return []Check{mk(id, "paths", title, Warn, "게이트웨이 항목이 상대 경로를 가리킵니다: "+safe(cmd, 120), "상대 경로는 클라이언트의 작업 디렉터리에 따라 달라집니다", fix)}
	case !found:
		return []Check{mk(id, "paths", title, Warn, "게이트웨이 항목의 명령을 찾을 수 없습니다: "+safe(cmd, 120), "클라이언트가 forge mcp serve 를 시작하지 못합니다", fix)}
	}
	res := cmd
	if !filepath.IsAbs(cmd) {
		lp, err := exec.LookPath(cmd)
		if err != nil {
			return nil
		}
		res = lp
	}
	if r, err := filepath.EvalSymlinks(res); err == nil {
		res = r
	}
	if res != e.exe {
		return []Check{mk(id, "paths", title, Warn, fmt.Sprintf("게이트웨이 항목이 현재 forge 와 다른 바이너리를 가리킵니다: %s (현재 %s)", safe(res, 120), safe(e.exe, 120)),
			"여러 forge 가 설치되어 있거나 forge 를 옮겼습니다 (오래된 버전이 게이트웨이를 실행할 수 있음)", fix)}
	}
	return nil
}
