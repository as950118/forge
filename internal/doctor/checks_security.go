package doctor

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/heonjinjeong/forge/internal/skill"
	"github.com/heonjinjeong/forge/internal/tool"
)

const securityGroup = "security"

// checkSecurity (TASK-0013, read-only):
//   - skill store integrity: the files of every enabled skill's current version
//     are re-hashed and compared with the registered content_hash (the same
//     check as `forge skill check`, bounded by the import limits);
//   - MCP tool definitions: servers whose cached definitions differ from the
//     approved ones (changed) or were never approved, and whether enforcement
//     (forge mcp pin --enforce) is on.
func checkSecurity(e *env) []Check {
	conn, err := e.database()
	if err != nil {
		return []Check{e.dbSkipped("security.skipped", securityGroup, "보안", "보안")}
	}
	_ = conn
	var out []Check
	out = append(out, e.skillIntegrity()...)
	out = append(out, e.mcpApprovals()...)
	out = append(out, e.legacyCodeExecTools()...)
	return out
}

// legacyCodeExecTools (TASK-0032 AC-9, ADR-F0006 D5): a code-exec tool that was
// registered with confirm=none before the classification existed is NOT
// grandfathered: the gateway asks anyway (and refuses where the client cannot
// confirm). Doctor tells the user, with the way to re-register it, so a tool that
// "stopped working" is explained.
func (e *env) legacyCodeExecTools() []Check {
	reg := &tool.Registry{DB: e.db, Home: e.o.Home}
	defs, err := reg.List(e.ctx)
	if err != nil {
		if isCtx(err) {
			return nil
		}
		return []Check{unavailable("security.tools.codeexec", securityGroup, "코드 실행형 도구", err)}
	}
	var names []string
	for i := range defs {
		d := &defs[i]
		if d.Enabled && d.ForcedAsk() {
			names = append(names, safe(d.Name, 50)+" ("+safe(d.CodeExecBinary(), 30)+")")
		}
	}
	if len(names) == 0 {
		return nil
	}
	first := strings.Fields(names[0])[0]
	return []Check{mk("security.tools.codeexec", securityGroup, "코드 실행형 도구의 확인 강제", Info,
		fmt.Sprintf("confirm=none 인 코드 실행형 도구 %d개는 게이트웨이가 확인(ask)을 강제합니다: %s", len(names), listNames(names)),
		"인자를 코드로 해석하는 프로그램(sqlite3, npm, uv, go run 등)은 기존에 none 으로 등록했어도 그랜드파더링하지 않습니다. 확인(elicitation)을 지원하지 않는 클라이언트에서는 재등록 전까지 호출이 거부됩니다 (ADR-F0006)",
		"forge tool list   # CONFIRM 열의 none->ask(forced) 확인",
		"forge tool show "+e.q(first)+"   # 현재 정의 확인",
		"forge tool add "+e.q(first)+" --replace ... --confirm none --yes-code-exec   # 위험을 이해하고 무확인으로 쓸 때만 (기존 정의 옵션을 모두 다시 지정)")}
}

func (e *env) skillIntegrity() []Check {
	rows, err := e.db.QueryContext(e.ctx, `SELECT s.name, v.version, v.store_path, v.content_hash FROM skills s JOIN skill_versions v ON v.id=s.current_version_id WHERE s.enabled=1 ORDER BY s.name LIMIT 200`)
	if err != nil {
		return []Check{unavailable("security.skills", securityGroup, "스킬 저장소 무결성", err)}
	}
	defer rows.Close()
	skillsRoot := filepath.Join(e.o.Home, "skills")
	var bad, unread []string
	n := 0
	var fix []string
	for rows.Next() {
		var name, ver, sp, hash string
		if rows.Scan(&name, &ver, &sp, &hash) != nil {
			continue
		}
		if e.ctx.Err() != nil {
			break
		}
		n++
		dir := filepath.Join(e.o.Home, filepath.FromSlash(sp))
		if rel, err := filepath.Rel(skillsRoot, dir); err != nil || strings.HasPrefix(rel, "..") {
			bad = append(bad, safe(name, 40)+" (저장소 밖 경로)")
			continue
		}
		st, _, _ := skill.VerifyStore(dir, hash)
		switch st {
		case skill.StoreOK:
		case skill.StoreMissing:
			// reported by the paths check (paths.skillstore)
		case skill.StoreUnreadable:
			unread = append(unread, safe(name, 40))
		default:
			bad = append(bad, safe(name, 40)+"@"+safe(ver, 30))
			if len(fix) < 3 {
				fix = append(fix, "forge skill check "+e.q(name))
			}
		}
	}
	if len(bad) > 0 {
		fix = append(fix, "forge skill remove <이름> --yes   # 변조가 확인되면 삭제 후 신뢰하는 소스에서 다시 추가")
		return []Check{mk("security.skills", securityGroup, "스킬 저장소 무결성", Fail,
			fmt.Sprintf("설치된 스킬 파일이 등록된 해시와 다릅니다 (변조 또는 손상 의심, %d건): %s", len(bad), listNames(bad)),
			"~/.forge/skills 의 스냅샷은 등록 후 바뀌면 안 됩니다. sync 는 이 스킬을 렌더링하지 않고 게이트웨이 forge_skill 도 제공하지 않습니다", fix...)}
	}
	if len(unread) > 0 {
		return []Check{mk("security.skills", securityGroup, "스킬 저장소 무결성", Warn, "점검 불가(읽기 권한 없음): "+listNames(unread), "스냅샷 디렉터리를 읽을 수 없습니다", "ls -ld "+e.q(skillsRoot))}
	}
	return []Check{mk("security.skills", securityGroup, "스킬 저장소 무결성", OK, fmt.Sprintf("활성 스킬 %d개의 설치 파일이 등록된 해시와 같습니다", n), "")}
}

func (e *env) mcpApprovals() []Check {
	rows, err := e.db.QueryContext(e.ctx, `
		SELECT m.name, m.enabled,
		  COALESCE((SELECT value FROM settings WHERE key='mcp.enforce.' || m.name),'0'),
		  (SELECT count(*) FROM mcp_tools t WHERE t.server_id=m.id AND t.stale=0),
		  (SELECT count(*) FROM mcp_tools t JOIN mcp_tool_approvals a ON a.server_id=t.server_id AND a.tool_name=t.name WHERE t.server_id=m.id AND t.stale=0 AND a.approved_hash=t.schema_hash),
		  (SELECT count(*) FROM mcp_tools t JOIN mcp_tool_approvals a ON a.server_id=t.server_id AND a.tool_name=t.name WHERE t.server_id=m.id AND t.stale=0 AND a.approved_hash<>t.schema_hash),
		  (SELECT count(*) FROM mcp_tool_approvals a WHERE a.server_id=m.id)
		FROM mcp_servers m WHERE m.scope='global' AND m.project_id IS NULL ORDER BY m.name LIMIT 200`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return []Check{mk("security.mcp", securityGroup, "MCP 도구 정의 승인", Info, "이 DB 에는 승인 기록 테이블이 아직 없습니다 (마이그레이션 0002 미적용)", "forge 명령을 한 번 실행하면 자동으로 마이그레이션됩니다", "forge init")}
		}
		return []Check{unavailable("security.mcp", securityGroup, "MCP 도구 정의 승인", err)}
	}
	defer rows.Close()
	var changed, hiddenEnf, neverApproved, enforceOn []string
	var fix []string
	total := 0
	for rows.Next() {
		var name, enf string
		var enabled, tools, approved, chg, anyApproval int
		if rows.Scan(&name, &enabled, &enf, &tools, &approved, &chg, &anyApproval) != nil {
			continue
		}
		if enabled != 1 || tools == 0 {
			continue
		}
		total++
		unapproved := tools - approved - chg
		n := safe(name, 40)
		if enf == "1" {
			enforceOn = append(enforceOn, n)
		}
		switch {
		case chg > 0:
			changed = append(changed, fmt.Sprintf("%s (변경 %d, 미승인 %d)", n, chg, unapproved))
			if len(fix) < 4 {
				fix = append(fix, "forge mcp show "+e.q(name)+"   # 바뀐 설명·스키마를 읽어 보세요", "forge mcp approve "+e.q(name)+"   # 읽은 뒤 신뢰하면 승인")
			}
		case anyApproval > 0 && unapproved > 0:
			changed = append(changed, fmt.Sprintf("%s (승인 이후 새로 생긴 도구 %d)", n, unapproved))
			if len(fix) < 4 {
				fix = append(fix, "forge mcp show "+e.q(name), "forge mcp approve "+e.q(name))
			}
		case anyApproval == 0:
			neverApproved = append(neverApproved, n)
		}
		if enf == "1" && approved < tools {
			hiddenEnf = append(hiddenEnf, fmt.Sprintf("%s (숨김 %d)", n, tools-approved))
		}
	}
	if err := rows.Err(); err != nil {
		return []Check{unavailable("security.mcp", securityGroup, "MCP 도구 정의 승인", err)}
	}
	var out []Check
	switch {
	case len(changed) > 0:
		out = append(out, mk("security.mcp", securityGroup, "MCP 도구 정의 승인", Warn,
			fmt.Sprintf("승인된 정의와 다르거나 새로 생긴 도구가 있는 MCP 서버 %d개: %s", len(changed), listNames(changed)),
			"서버가 도구 설명·입력 스키마를 바꿨을 수 있습니다(설명 변경은 프롬프트 인젝션 경로). 읽어 본 뒤 승인하세요", fix...))
	case total == 0:
		out = append(out, mk("security.mcp", securityGroup, "MCP 도구 정의 승인", OK, "도구 캐시가 있는 활성 MCP 서버가 없습니다", ""))
	default:
		out = append(out, mk("security.mcp", securityGroup, "MCP 도구 정의 승인", OK, "승인된 정의와 다른 도구가 없습니다", ""))
	}
	if len(neverApproved) > 0 {
		out = append(out, mk("security.mcp.unapproved", securityGroup, "MCP 도구 승인 기록", Info,
			fmt.Sprintf("도구 정의를 한 번도 승인하지 않은 MCP 서버 %d개: %s", len(neverApproved), listNames(neverApproved)),
			"승인하면 이후 변경(rug pull)을 감지할 수 있습니다", "forge mcp show "+e.q(strings.Fields(neverApproved[0])[0]), "forge mcp approve "+e.q(strings.Fields(neverApproved[0])[0])))
	}
	if len(enforceOn) > 0 {
		msg := "도구 정의 고정(enforce) 켜짐: " + listNames(enforceOn)
		if len(hiddenEnf) > 0 {
			out = append(out, mk("security.mcp.enforce", securityGroup, "MCP 도구 고정(enforce)", Warn, msg+" — 게이트웨이가 숨기는 도구가 있음: "+listNames(hiddenEnf),
				"승인되지 않았거나 변경된 도구는 노출되지 않습니다", "forge mcp show "+e.q(strings.Fields(hiddenEnf[0])[0]), "forge mcp approve "+e.q(strings.Fields(hiddenEnf[0])[0])))
		} else {
			out = append(out, mk("security.mcp.enforce", securityGroup, "MCP 도구 고정(enforce)", OK, msg+" — 숨겨진 도구 없음", ""))
		}
	} else if total > 0 {
		out = append(out, mk("security.mcp.enforce", securityGroup, "MCP 도구 고정(enforce)", Info, "도구 정의 고정(enforce)이 꺼져 있습니다 (기본). 변경은 경고만 하고 노출은 막지 않습니다",
			"켜면 게이트웨이는 승인된 정의와 같은 도구만 노출합니다 (게이트웨이 경유 노출에만 적용)", "forge mcp pin <서버> --enforce"))
	}
	return out
}
