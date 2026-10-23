# `forge sync` — 레지스트리를 클라이언트 설정으로 렌더링 (TASK-0010)

`forge sync` 는 Forge 레지스트리(스킬·MCP 서버·호출 원장 hook)를 클라이언트 설정 파일로 내보낸다.
Claude Code(`--target claude-code`)와 OpenCode(`--target opencode`, 이 문서의 "OpenCode" 절)를 지원한다.
두 타깃은 같은 공통 계층(`internal/adapter`)을 쓰며 인터페이스는 `docs/adapter.md` 에 있다. 아래 설명은 별도 표시가 없으면 두 타깃에 공통이고,
`.mcp.json`·`settings.json`·hook 이야기는 claude-code, `opencode.json` 이야기는 opencode 에 해당한다.

> 이 기능은 **사용자의 실제 설정 파일을 쓴다.** 그래서 기본 동작은 계획(plan) 출력뿐이고, 파일은 `--apply` 를 명시했을 때만 쓴다.

## 사용법

```bash
# 계획만 출력 (아무것도 쓰지 않음; --dry-run 은 기본 동작과 같다)
forge sync --target claude-code
forge sync --target claude-code --dry-run --diff        # 파일별 통합 diff 포함 (비밀 값은 *** 로 가림)
forge sync --target claude-code --json                  # 안정 스키마 JSON

# 실제 적용 (영향받는 모든 파일을 먼저 백업한 뒤 원자적으로 씀)
forge sync --target claude-code --apply
forge sync --target claude-code --apply --skip-conflicts   # 충돌 항목만 빼고 나머지 적용
forge sync --target claude-code --apply --force-managed    # 손으로 고친 "관리 항목"만 덮어씀 (백업됨)
forge sync --target claude-code --apply --no-hooks         # hook 은 설치/갱신도 제거도 하지 않음
forge sync --target claude-code --apply --remove-hooks     # 설치된 관리 hook 만 제거
# 이전 sync 가 중단됐다면 계획이 알려 주고, --apply 가 이어서 완료한다 (7절)

# 되돌리기
forge sync --list-backups [--json]
forge sync --restore latest|<id> [--dry-run] [--force]
forge sync --prune-backups [--keep 20] [--dry-run]       # 오래된 백업 삭제: 이 명령으로만 지워진다
```

| 플래그 | 의미 |
|--------|------|
| `--target claude-code\|opencode\|all` | 대상 클라이언트 (필수). `all` 은 두 클라이언트를 하나의 통합 계획으로 처리한다("`--target all`" 절). 다른 값은 지원 목록과 함께 오류 |
| `--check-version` | (계획용) 대상 클라이언트의 `<binary> --version` 을 실행해 검증된 버전인지 계획에 표시한다. 기본 계획은 실행하지 않는다. `--apply` 와 함께 쓸 수 없다 |
| `--allow-untested-version` | `--apply` 전용: 검증되지 않은 클라이언트 버전(warn)이어도 계속 진행한다 ("버전 게이트" 절) |
| `--scope project\|user` | 기본 `project`. 아래 "스코프별 대상" 참고 |
| `--project-dir <경로>` | 프로젝트 스코프의 시작 디렉터리(기본 현재 디렉터리). `.git` 이 있는 상위 디렉터리를 저장소 루트로 쓴다. `.git` 이 없으면 지정한 디렉터리를 쓰고 계획에 참고를 남긴다. **홈 디렉터리, Forge 홈(`$FORGE_HOME`), `~/.claude` 자체는 프로젝트 루트로 쓸 수 없다** (아래 "금지 루트") |
| `--dry-run` / `--apply` | 동시 지정은 오류. 둘 다 없으면 계획만 출력 |
| `--diff` | 계획에 파일별 통합 diff 추가 |
| `--bin <이름\|절대경로>` | 설정 파일에 기록할 forge 명령 (기본 `forge`). 상대 경로(`./forge`)는 거부 |
| `--no-hooks` | (claude-code 전용; opencode 에서는 오류) hook 을 **설치·갱신하지 않는다. 이미 설치된 관리 hook 은 건드리지 않는다**(제거하지 않음, `settings.json` 도 열지 않음). 계획에 참고가 한 줄 남는다 |
| `--remove-hooks` | (claude-code 전용; opencode 에서는 오류) 이미 설치된 **관리 hook 만 제거**한다(handler 단위, 계획에 `삭제` 로 보임; 사용자 hook 은 그대로). `--no-hooks` 와 동시 지정은 오류 |
| `--skip-conflicts` | 충돌 항목을 제외하고 적용 |
| `--force-managed` | "Forge 가 관리하지만 사람이 수정한" 항목을 덮어쓴다. 사용자 소유 항목에는 효과가 없다 |
| `--restore`, `--force` | 복원. `--force` 는 백업 이후 사용자가 수정한 파일이 있어도 복원 |

종료 코드: 변경 없음 0, 계획 출력 0(충돌이 있어도), `--apply` 에서 충돌이 있고 `--skip-conflicts` 가 없으면 비0
(이 경우 아무것도 쓰지 않는다), 오류(파싱 불가 JSON 등) 비0.

## 스코프별 대상

(claude-code. opencode 는 아래 "OpenCode" 절의 표)

| 항목 | `--scope project` (기본) | `--scope user` |
|------|--------------------------|----------------|
| 스킬 | `<repo>/.claude/skills/<name>/` | `~/.claude/skills/<name>/` |
| MCP 서버 | `<repo>/.mcp.json` 의 `mcpServers` | **파일을 쓰지 않음** — 사용자가 실행할 `claude mcp add --scope user ...` 명령을 출력만 한다 |
| hook | `<repo>/.claude/settings.json` 의 `hooks` | `~/.claude/settings.json` 의 `hooks` |
| 상태 파일 | `<repo>/.forge/sync-state.json` | `$FORGE_HOME/sync-state/claude-code-user.json` (기본 `~/.forge/sync-state/`) |
| 백업·잠금 | `$FORGE_HOME/backups/`, `$FORGE_HOME/locks/` | 같음 |

### 무엇이 렌더링되는가

- **스킬**: 레지스트리에서 활성(enabled)인 스킬의 현재 버전(`~/.forge/skills/<name>@<ver>/`)의 파일 트리를 복사한다. 파일 0644(실행 비트 있으면 0755), 디렉터리 0755 — 읽기 전용 스냅샷 모드는 물려받지 않는다. 심볼릭 링크가 있거나 내용 해시가 등록값과 다르면(변조) 복사하지 않고 충돌로 보고한다. 디렉터리 안에 관리 마커 `.forge-managed`(name, version, tree_hash)를 둔다. SKILL.md 에는 아무것도 추가하지 않는다.
- **MCP(project)**: `.mcp.json` 에 (a) 게이트웨이 항목 `forge`(`type: stdio`, `command` = `--bin`, `args` = `mcp serve --client claude-code`)와 (b) exposure=`direct` 인 활성 서버를 쓴다. `type` 은 항상 명시한다(Claude Code 는 `type` 없는 `url` 항목을 stdio 로 보아 설정 오류로 건너뜀). 값은 `${VAR}` 참조 그대로이며, 렌더 직전에 레지스트리 검증(`mcp.Spec.Validate`)을 다시 거쳐 리터럴 비밀이 있으면 렌더를 거부한다. `exposure=gateway` 서버는 게이트웨이가 프록시하므로 쓰지 않고, `hidden` 은 쓰지 않는다. `cwd` 가 있는 서버와 keychain 참조 서버는 `.mcp.json` 형식으로 표현할 수 없어 건너뛴다(`gateway` 사용). `timeout_ms` 는 Claude Code 쪽 단위가 미확인이라 렌더하지 않는다.
- **hook**: `ledger.ClaudeCodeSnippet` 과 같은 내용(`PreToolUse`/`PostToolUse`/`PostToolUseFailure`, matcher `Skill|mcp__.*`, `forge hook ingest ...`)을 `hooks` 에 **handler 단위로** 병합한다. 같은 matcher 그룹에 사용자가 넣은 handler 가 있으면 그 그룹은 그대로 두고 Forge handler 만 뒤에 추가한다. 제거도 handler 단위(우리가 만든 빈 그룹/이벤트/`hooks` 만 정리).

## OpenCode (`--target opencode`, TASK-0011)

```bash
forge sync --target opencode --dry-run --diff      # 계획 (아무것도 쓰지 않음)
forge sync --target opencode --apply               # 쓰기 (백업 후)
forge sync --target opencode --scope user          # 스킬은 ~/.config/opencode/skills, MCP 는 명령만 출력
```

`--scope`, `--project-dir`, `--dry-run`, `--diff`, `--apply`, `--skip-conflicts`, `--force-managed`, `--json`, `--bin`, `--restore`, `--list-backups`, `--prune-backups` 는 claude-code 와 같이 동작한다.
`--no-hooks`/`--remove-hooks` 는 지원하지 않는다(오류): OpenCode 의 hook/플러그인 수집은 이번 범위에 없다. **Forge 는 `opencode`·`claude` 를 `--version`(버전 게이트, "버전 게이트" 절) 말고는 어떤 서브명령·플래그로도 실행하지 않는다.**
적용 뒤 OpenCode 에 반영하려면 사용자가 직접 `opencode reload` 를 실행한다(계획이 안내만 한다).

### 스코프별 대상

| 항목 | `--scope project` (기본) | `--scope user` |
|------|--------------------------|----------------|
| 스킬 | `<repo>/.opencode/skills/<name>/` | `~/.config/opencode/skills/<name>/` |
| MCP 서버 | `<repo>/opencode.json` 의 최상위 `mcp` | **파일을 쓰지 않음** — 사용자가 실행할 `opencode mcp add <이름> --global ...` 명령을 출력만 한다 |
| hook | 없음 | 없음 |
| 상태 파일 | `<repo>/.forge/sync-state.opencode.json` (저널 `.forge/sync-journal.opencode.json`) | `$FORGE_HOME/sync-state/opencode-user.json` (저널 `opencode-user.journal.json`) |
| 백업·잠금 | `$FORGE_HOME/backups/`(manifest 의 `target: opencode`), `locks/opencode-project-<해시>.lock` | 같음 |

claude-code 의 상태 파일 이름(`sync-state.json`, `claude-code-user.json`)은 바뀌지 않았다. 두 타깃은 서로의 상태·저널·백업·잠금을 공유하지 않으며, `--restore` 는 다른 타깃의 백업을 거부한다.

### 무엇이 렌더링되는가

- **스킬**: claude-code 와 같은 마커(`.forge-managed`)·충돌 규칙으로 레지스트리 스킬 트리를 복사한다. `--skills-dir` 같은 경로 선택 옵션은 두지 않았다: `.claude/skills` 에 쓰면 claude-code 타깃과 소유권(상태 파일)이 둘로 갈라지고, 아래 중복 방지로 이미 같은 목적을 달성하며, 옵션이 늘면 안전 검증 표면만 커진다.
- **MCP(project)**: `opencode.json` 의 `mcp` 에 (a) 게이트웨이 `forge` = `{"type":"local","command":["<bin>","mcp","serve","--client","opencode"],"enabled":true}` 와 (b) exposure=`direct` 인 서버를 쓴다(노출 설정 키 `mcp.exposure.<서버>.opencode`).

  | 레지스트리(Spec) | opencode.json |
  |------------------|---------------|
  | stdio, command + args | `{"type":"local","command":[command, ...args],"enabled":true}` (공백·따옴표·메타문자가 있어도 JSON 문자열이라 안전) |
  | stdio env `K=${V}` | `"environment":{"K":"{env:V}"}` |
  | http / sse, url | `{"type":"remote","url":"…","enabled":true}` (둘 다 remote) |
  | header `Authorization=${V}` | `"headers":{"Authorization":"{env:V}"}` |
  | cwd 있는 서버 / keychain 참조 | 건너뜀 (`gateway` 사용) |
  | `timeout_ms` | 렌더하지 않음 (단위 [미확인], 계획에 참고) |

  렌더 직전 `mcp.Spec.Validate` 를 다시 거치고, 비밀로 보이는 literal 은 거부한다. OpenCode 는 설정 텍스트의 `{env:NAME}`/`{file:경로}` 를 치환하므로([bin]) **명령·인자·URL·literal 값에 `{env:`, `{file:`, `${` 가 들어 있으면 렌더를 거부(충돌)** 한다(Forge 가 직접 쓰는 `{env:NAME}` 참조만 허용). 환경변수·헤더 **이름**도 참조/literal 구분 없이 렌더 직전에 검사한다(`adapter.CheckValueKey`: env 는 `^[A-Za-z_][A-Za-z0-9_]*$`, 헤더는 HTTP token, 치환 문법·제어 문자·공백·`"`·`\`·비ASCII·128자 초과 거부; claude-code 도 같음). DB 를 직접 고쳐 악성 이름을 넣어도 충돌로 거부된다. 사용자가 같은 이름을 직접 정의했으면 충돌이다.
- **user 스코프 MCP 명령**: `opencode mcp add <이름> --global [--env K={env:V}]... -- <command> <args>...`, 원격은 `opencode mcp add <이름> --global --url <URL> [--header K={env:V}]...`. 모든 단어는 POSIX 작은따옴표 이스케이프되며 비밀 값은 출력하지 않고 환경변수 **이름 참조**만 넣는다. 옵션은 [help] 에 기록된 `--url`, `--header k=v`, `--env k=v`, `--global`, `-- cmd args...` 만 쓴다. 옵션 순서와 add 가 `{env:..}` 값을 그대로 저장하는지는 [미확인]이라 이름을 먼저 두고 `--` 를 썼다. `opencode` 에는 `mcp remove` 가 없다. 명령은 실행하지 않는다.

### 스킬 중복 노출 방지

OpenCode 는 `.opencode/skills` 뿐 아니라 `.claude/skills` 도 읽는다([doc]+[bin]). 같은 스킬이 두 경로에 있을 때의 동작은 [미확인]이므로:

- opencode 타깃은 같은 scope 의 **claude-code 상태 파일을 읽기만** 해서(`sync-state.json` / `claude-code-user.json`; 수정·생성 없음) 거기에 기록된 스킬을 **건너뜀**으로 계획한다(이유 ".claude/skills 로 이미 노출됨 — 두 경로 동시 노출 시 동작 [미확인]").
- opencode 가 이미 `.opencode/skills` 에 쓴 스킬을 나중에 claude-code 가 관리하게 되면, 다음 opencode sync 가 자기 사본을 **관리 항목일 때만**(마커·상태·해시 일치) 백업 후 제거한다. 손으로 고친 사본은 지우지 않고 충돌로 알린다. claude-code 상태 파일을 해석할 수 없으면 검사를 건너뛰고 계획에 알린다.
- 반대 방향(claude-code 쪽 방지)은 이번에 구현하지 않았다: opencode → claude-code 순서로 sync 하면 잠시 두 경로에 있다가 opencode 를 한 번 더 sync 하면 수렴한다(권장 순서: claude-code 먼저).

### `opencode.json` / `opencode.jsonc`

- `opencode.json` 만 편집한다(최상위 `mcp` 멤버만; `$schema`, provider, model, permission, tools 등 다른 키는 순서·들여쓰기 포함 바이트 그대로). 엄격한 JSON 이 아니거나 `mcp` 가 객체가 아니면 아무것도 쓰지 않고 중단한다.
- 프로젝트에 `opencode.jsonc` 만 있고 `opencode.json` 이 없으면 **JSONC 는 아직 지원하지 않습니다(주석 보존 불가)** 로 중단한다(아무것도 쓰지 않음; 스킬도 쓰지 않는다). 둘 다 있으면 `opencode.json` 만 대상으로 하고 "opencode.jsonc 도 존재함: 우선순위 [미확인]" 경고를 계획에 남긴다. `opencode.jsonc` 는 열지 않는다(존재만 확인). `.opencode/opencode.json` 은 대상이 아니다.
- diff 의 비밀 마스킹은 `apiKey`, `api_key`, `options.apiKey`, `headers.Authorization` 등 키 이름 규칙으로 provider 키를 가린다(`{env:NAME}` 참조는 그대로 보임).

### 읽지도 쓰지도 않는 파일

`adapter.GuardPath` 는 **(디렉터리, 파일 이름) 조합**과 **파일 동일성(`os.SameFile`)** 으로 거부한다. 이름만으로는 거부하지 않는다: 프로젝트의 `credential-helper`, `service`, `auth` 같은 스킬·파일 이름은 정상이다.

- OpenCode **설정 디렉터리**(`~/.config/opencode`, 그리고 `XDG_CONFIG_HOME` 이 절대 경로로 설정되어 있으면 `$XDG_CONFIG_HOME/opencode`) 바로 아래의 `service.json`, `mcp-auth.json`, `auth.json`, `opencode.db`(+`-wal`/`-shm`), `credential*`, 그리고 전역 설정 `opencode.json`, `opencode.jsonc`, `config.json`(provider API 키가 literal 로 들어갈 수 있음). 이름 비교는 대소문자·보이지 않는 문자·끝의 `.`/공백을 무시한다.
- OpenCode **데이터·상태 디렉터리**(`~/.local/share/opencode`, `~/.local/state/opencode`, `$XDG_DATA_HOME/opencode`, `$XDG_STATE_HOME/opencode`) 아래 전부.
- **동일성**: 위 파일들(데이터·상태 디렉터리는 바로 아래 모든 파일)과 같은 파일(하드링크·심볼릭 링크, 어떤 이름·위치든)이면 거부한다. 예를 들어 프로젝트의 `opencode.json` 을 `mcp-auth.json` 이나 `auth.json` 에 `ln` 으로 연결해도 읽지 않는다. 판정은 `stat` 만 쓰며 파일을 열지 않는다. 스킬 트리를 해시·복사할 때도 트리 안의 각 파일에 같은 동일성 검사를 한다.
- **하드링크 정책(일반 방어)**: 어댑터가 읽는 파일(`opencode.json`, `.mcp.json`, `settings.json`, 상태 파일 등)이 **하드링크가 둘 이상**(`st_nlink > 1`)이면 내용을 읽지 않고 "하드링크된 설정 파일은 지원하지 않습니다(다른 파일과 같은 내용을 공유함)" 로 중단한다. plan·apply(적용 직전 재검증)·restore 가 모두 같은 읽기 함수를 거치므로 일관되며 두 타깃 모두에 적용된다. 정당한 하드링크 사용은 드물어 안전 쪽을 택했다. 링크를 지우면(`nlink == 1`) 다시 동작한다.
- **XDG**: `XDG_CONFIG_HOME` 이 설정되어 있으면 user 스코프 스킬 대상은 `$XDG_CONFIG_HOME/opencode/skills`(미설정·빈 값이면 `~/.config/opencode/skills`)이고 계획의 참고에 실제 대상 경로가 표시된다. OpenCode 가 XDG 를 따르는지는 `docs/clients/opencode.md` 에 근거가 없어 [미확인]이므로 보수적으로 처리한다. 상대 경로, 제어 문자가 든 값, 홈 디렉터리 밖, 심볼릭 링크인 값은 무시하고 기본 경로를 쓰며 계획에 경고를 남긴다. 가드와 금지 루트는 XDG 값이 절대 경로이면 홈 안팎을 가리지 않고 적용한다. (복원은 복원 시점의 환경변수로 허용 경로를 다시 검사하므로, sync 때와 `XDG_CONFIG_HOME` 이 다르면 그 백업의 user 스킬 경로는 거부된다.)

**프로젝트의 `opencode.json` 은 정상 대상이다.** 프로젝트 루트로 쓸 수 없는 디렉터리: 홈, `$FORGE_HOME`, `~/.claude`, `~/.config/opencode`, `~/.local/{share,state}/opencode`, `~/.cache/opencode`, 위의 XDG 설정·데이터·상태 디렉터리. 비밀 파일 이름을 가진 스킬도(예: `credential-helper`) 일반 스킬처럼 추가·갱신·제거된다.

### 형식 근거

픽스처: `internal/adapter/opencode/testdata/v2.0.24/` (`project_before/` 입력, `golden/` 기대값, `NOTES.txt` 에 근거와 `[미확인]`; `UPDATE_GOLDEN=1 go test ./internal/adapter/opencode`). 근거는 `docs/clients/opencode.md` 의 `[doc]`/`[bin]`/`[help]`/`[local]` 항목뿐이다. `[미확인]`(렌더하지 않거나 경고만 함): `cwd`, `timeout` 단위, `oauth`, http/sse 구분, `tools` 글롭, 플러그인, `opencode.json` 과 `opencode.jsonc` 의 우선순위, 한 스킬이 두 경로에 있을 때의 동작, OpenCode 의 `XDG_CONFIG_HOME` 준수 여부(보수적으로 처리), `mcp add` 의 옵션 순서와 값 저장 방식.

## 안전 설계

### 1. 기본은 계획만
`--apply` 가 없으면 어떤 파일도(프로젝트·홈·`$FORGE_HOME`) 만들거나 바꾸지 않는다. 레지스트리 DB 도 읽기 전용으로 연다.

### 2. 사용자 항목은 절대 건드리지 않는다 — 사이드카 상태 파일
`.mcp.json`·`settings.json` 에는 Forge 전용 필드를 **추가하지 않는다**(Claude Code 가 모르는 필드를 경고/거부할 수 있음). 대신 상태 파일이 "Forge 가 쓴 항목"을 기록한다.

```json
{"version": 1, "target": "claude-code", "scope": "project",
 "items": [{"kind": "mcp", "name": "forge", "hash": "<canonical JSON sha256>", "applied_at": "2026-10-22T00:00:00Z"}]}
```

종류(`skill`/`mcp`/`hook`)·이름·내용 해시·적용 시각만 있고 비밀은 없다(프로젝트 상태 파일은 git 에 커밋해도 된다. hook 항목 이름에는 `--bin` 값이 들어간다). 규칙:

- 상태 파일에 없는 항목 = 사용자 소유. 같은 이름이어도 수정·삭제하지 않고 **충돌**로 보고한다.
- 상태 파일에 있어도 현재 내용의 해시가 기록과 다르면(손으로 고침) 덮어쓰지 않고 **충돌**(수정됨)로 보고한다. `--force-managed` 로만 덮어쓰며 이때도 백업한다.
- JSON 해시는 키를 정렬한 정규형(canonical)이라 공백·들여쓰기·키 순서를 바꿔도 "수정"으로 보지 않는다.
- 스킬: 대상 디렉터리에 `.forge-managed` 마커가 없으면 사용자 스킬로 보고 건너뛴다. 마커가 있어도 상태 파일과 일치하고 트리 해시(마커 제외)가 같을 때만 갱신한다. 관리 디렉터리에 파일을 추가/수정하면 충돌이다.
- 상태 파일이 손상되었거나 버전/대상이 다르면 아무것도 쓰지 않고 중단한다(관리 항목을 추측하지 않는다). 상태 파일에서 읽은 이름은 경로로 쓰기 전에 다시 검증한다(`../` 방어).
- 레지스트리에서 비활성/삭제된 스킬, gateway/hidden 으로 바뀐 서버, 더 이상 필요 없는 hook 은 **관리 항목일 때만** 제거한다(백업 후). 스킬은 상태 파일에 있어도 마커가 없거나 마커 이름이 다르면(사용자 디렉터리로 대체됨) 지우지 않고 상태 항목만 정리한다.
- 마커가 없거나(또는 이름·소유자가 다른, 깨진 마커) 같은 이름의 디렉터리는 상태 파일에 기록이 있어도 사용자 소유이며 `--force-managed` 로도 덮거나 지우지 않는다(덮을 수 있는 것은 "마커·상태·이름이 맞는데 내용 해시만 다른" 관리 항목뿐).

### 3. JSON 편집은 순서·들여쓰기 보존 방식
`.mcp.json`·`settings.json` 은 구조체로 재직렬화하지 않고, 바이트 오프셋으로 필요한 멤버만 삽입/교체/삭제한다
(`internal/adapter/jsonedit`). 사용자 항목, 키 순서, 들여쓰기(2칸/4칸/탭), 줄바꿈(LF/CRLF), 마지막 줄바꿈 유무가 그대로 남는다.
새로 넣는 값은 파일에서 감지한 들여쓰기 단위로 쓴다. **파일이 엄격한 JSON 이 아니면(주석, 후행 쉼표, BOM, 빈 파일, 최상위가 객체 아님, `mcpServers`/`hooks` 가 객체 아님) 아무것도 쓰지 않고 오류로 중단한다.** 오류 메시지에는 파일 내용을 싣지 않는다.

**중복 키**: Forge 가 조회·편집하지 않는 경로(예: 최상위 `theme` 이 두 번)의 중복 키는 허용하고 **바이트 그대로 보존**한다. Forge 가 찾아야 하는 키(`mcpServers`/`hooks`, 그 아래 서버 이름·이벤트 이름)가 중복이면 어느 쪽을 편집할지 알 수 없으므로 아무것도 쓰지 않고 중단한다.

### 4. `~/.claude.json` 은 읽지도 쓰지도 않는다
이 파일은 비밀이 섞여 있고 구조가 미확인이다. 어떤 코드 경로도 이 이름을 만들지 않으며, 모든 파일 입출력은 `adapter.GuardPath` 를 거친다. `GuardPath` 는 **이름**으로(대소문자 무시 - APFS/NTFS 는 대소문자를 구분하지 않음 -, 보이지 않는 서식 문자·결합 부호 무시, 끝의 `.`/공백 무시) 그리고 **파일 동일성**으로(경로가 이미 존재하고 `$HOME` 의 그 파일과 `os.SameFile` 이면 - 하드링크·심볼릭 링크·다른 철자 모두 - 이름이 무엇이든) 거부한다. 동일성 판정은 `stat` 만 쓰며 파일을 열지 않는다. (한계: 동일성은 `$HOME` 아래의 그 한 파일 기준이다. `CLAUDE_CONFIG_DIR` 로 옮긴 다른 위치의 파일은 이름 규칙으로만 막는다.)

**금지 루트**: 프로젝트 루트는 문자열이 아니라 실제 디렉터리 동일성으로 판정한다(`EvalSymlinks` 후 `os.SameFile`). 홈 디렉터리, Forge 홈(`$FORGE_HOME`), `~/.claude` 와 같은 디렉터리이면(대소문자만 다른 철자, 심볼릭 링크 포함) 거부한다. 그 아래 하위 디렉터리는 정상 프로젝트다. 같은 규칙이 `.git` 상위 탐색 결과에도 적용된다. `--scope user` 의 MCP 서버는 파일 대신 명령을 출력한다(비밀 값 없이 `${ENV}` 참조, POSIX 작은따옴표 이스케이프). Forge 는 이 명령을 **실행하지 않는다**. 이미 등록된 이름은 `claude mcp add` 가 실패하므로 갱신하려면 `claude mcp remove --scope user <이름>` 후 다시 add 한다(user 스코프 MCP 는 상태 추적 대상이 아니라 자동 제거도 없다).
테스트: HOME 을 임시 디렉터리로 두고 접근 불가(모드 0) 미끼 파일을 둬 해시·mtime·atime 불변을 확인하고, 소스에 이 파일명이 가드 외에 등장하지 않음을 검사한다.

### 5. 백업과 되돌리기
`--apply` 는 **파일을 바꾸거나 지우기 전에** 영향받는 모든 기존 파일(`.mcp.json`, `settings.json`, 스킬 디렉터리 트리, 상태 파일)을 `$FORGE_HOME/backups/<UTC시각>-<6자리 id>/` 에 원래 경로 구조로 복사한다(디렉터리 0700, 파일 0600). 백업에 실패하면 아무것도 바꾸지 않는다.

`manifest.json`: 대상·scope·루트·상태(`pending`/`applied`/`rolled_back`/`recovered`)와 항목별 `before`(존재 여부, 종류, sha256/트리 해시, 모드, 파일 모드 목록)·`after`(적용 직후 해시), 이번 apply 가 새로 만든 디렉터리 목록. **새로 만든 파일은 "없었음"으로 기록**하며 복원 시 삭제한다(그 때문에 비게 된, apply 가 만든 디렉터리도 정리).

복원(`--restore <id|latest>`):
1. 현재 파일 해시가 "백업 직전 상태"도 "백업 직후 상태"도 아니면 사용자가 그 뒤 수정한 것이므로 경고하고 `--force` 가 있어야 진행한다.
2. 복원 직전의 현재 상태를 **새 백업**으로 보존한다(`--restore latest` 를 한 번 더 하면 복원을 되돌릴 수 있다).
3. manifest 의 경로만 대상으로 하며, 경로는 절대·정규형이어야 하고 어댑터가 쓰는 경로(해당 스코프의 `.mcp.json`, `settings.json`, 상태 파일, 스킬 디렉터리 1단계)여야 하고 그 아래 구성요소가 심볼릭 링크가 아니어야 한다. 위반하면 아무것도 하지 않고 거부한다. 백업 파일은 복원 전에 해시로 검증한다.
- `latest` 는 롤백된 백업을 건너뛴 최신 백업이다. id 는 정확히 일치하거나 유일한 접두어면 된다.
- 중단된 sync 를 이어서 완료한 apply 의 백업 manifest 는 `supersedes`(완료한 중단 백업 id)를 기록한다. 이 백업을 복원하면 중단된 sync 가 바꾼 파일까지 **그 sync 이전(가장 오래된 백업의 원본) 상태**로 되돌린다(중단 백업도 같은 방식으로 검증하며 root/scope/대상이 같아야 한다). 중단됐던 백업의 상태는 `recovered` 로 표시된다.
- 백업은 자동으로 지우지 않는다. 20개를 넘으면 안내만 하며 `--prune-backups [--keep N]` 으로만 지운다(Forge 백업 id 형식의 디렉터리만 대상).

### 6. 원자성·동시성·멱등
- 모든 쓰기는 같은 디렉터리의 임시 파일 + fsync + rename 이다. 기존 파일 권한은 보존한다(새 파일: 프로젝트 0644, user 스코프 설정 0600). 스킬 디렉터리는 임시 디렉터리에 복사한 뒤 rename 으로 교체한다.
- 계획 시점의 파일 상태(해시)를 apply 직전에 재검증한다. 그 사이 파일이 바뀌었거나 심볼릭 링크가 생겼으면 아무것도 쓰지 않고 중단한다.
- 중간에 실패하면 이미 바꾼 것을 백업으로 자동 롤백한다(백업은 `rolled_back` 으로 표시되어 남는다). 프로세스가 죽어 롤백조차 못 한 경우는 7절(저널)이 다룬다.
- 같은 apply 를 두 번 실행하면 두 번째는 "변경 없음"이며 백업을 만들지 않고 파일 바이트·mtime 이 그대로다(상태 파일의 `applied_at` 도 해시가 바뀔 때만 갱신).
- `--apply`/`--restore` 는 `$FORGE_HOME/locks/<target>-<scope>-<루트 해시>.lock`(flock)로 직렬화한다. 계획과 적용이 같은 잠금 안에서 실행되므로 동시에 실행한 두 번째 sync 는 기다렸다가 "변경 없음"을 본다. 30초 안에 잠금을 얻지 못하면 오류.

### 7. 크래시 복구: 저널, 자동 복구, 임시 파일 정리
`--apply` 가 중간에 죽어도(SIGKILL, 정전) 다음 sync 가 알아채고 일관 상태로 수렴하도록 apply 는 **변경 전에 저널**을 쓴다.

- **저널**: 백업을 만들고 manifest 를 쓴 직후, 첫 파일을 바꾸기 전에 `<백업>/journal.json`(상태 `pending`)과, 프로젝트 스코프는 `<repo>/.forge/sync-journal.json`, user 스코프는 `$FORGE_HOME/sync-state/claude-code-user.journal.json` 에 같은 내용의 사본을 쓴다. 내용: 백업 id, 쓸 모든 경로와 각 경로의 변경 전/후 해시(스킬 트리는 렌더 결과로 미리 계산한 트리 해시), apply 가 마지막에 쓸 **상태 파일의 새 항목 목록과 그 직렬화 해시**.
- **상태 파일은 apply 의 마지막 쓰기**다(계획이 항상 끝에 둔다). 모든 쓰기가 끝나면 manifest 를 `applied` 로 바꾸고, 사본을 지우고, 저널을 `complete` 로 표시한다. 실패해 롤백하면 저널은 `rolled_back`(롤백까지 실패하면 `pending` 유지).
- **감지**: 잠금을 얻은 `--apply` 는 계획을 세울 때 `$FORGE_HOME/backups/*/journal.json` 중 같은 대상·scope·루트의 `pending` 저널을 찾는다. 읽기 전용 계획(`plan`)은 잠금을 잡지 않으므로 잠금 파일을 만들지 않고 "지금 잠겨 있는지"만 본다. 다른 sync 가 실행 중이면 그 저널을 중단으로 보지 않고 참고만 남기고, 아니면 같은 방식으로 감지해 계획 맨 위에 `경고: 이전 sync 가 중단되었습니다 (백업 <id>, ...)` 와 파일별 진행(완료·대기·수정/중간 상태)을 표시한다(`--json` 의 `interrupted`). 신뢰하는 것은 **백업 디렉터리의 저널뿐**이다. 프로젝트의 사본은 읽지 않는다(저장소가 위조할 수 있음).
- **복구 규칙(결정 규칙)**: 자동 롤백은 하지 않고 **언제나 앞으로 완료**한다. `--apply` 는 디스크의 현재 파일로 평소처럼 다시 계획한다(그 사이 사용자가 고친 내용이 그대로 반영됨). 단 상태 파일이 아직 기록하지 못한 항목에 한해, **저널이 그 항목을 나열하고 디스크의 내용 해시가 저널의 값과 정확히 같을 때만**(스킬은 마커도 맞아야 함) 관리 항목으로 인정한다. 이미 쓰인 항목은 그대로 두고 남은 항목만 쓰며 상태 파일을 마지막에 쓴다. 이미 쓰였지만 이후 수정된 항목, 저널에 없는 항목은 평소 규칙대로 **충돌**이며(이유에 "중단된 이전 sync 가 만든 항목"이라고 표시) 충돌이 있으면 아무것도 쓰지 않는다(`--skip-conflicts` 로 나머지만 적용). 복구 apply 도 바꾸는 파일을 평소처럼 **새 백업**에 먼저 보존한다(복구 전 현재 상태 보존). 성공하면 중단된 저널을 `recovered` 로 표시한다. 롤백이 필요하면 사용자가 직접 `--restore <중단된 백업 id>` 또는 복구 백업(`latest`)을 쓴다(5절: 복구 백업의 복원은 중단된 sync 의 변경까지 되돌린다).
- **중단된 `--restore`**: 복원도 같은 저널을 쓴다. 중단된 복원은 계획에 `이전 restore 가 중단되었습니다` 로 표시되며 자동으로 이어 하지 않는다(같은 `--restore <id> --force` 를 다시 실행하면 된다: 이미 복원된 파일은 `변경 없음`). 그 사이 `--apply` 를 하면 현재 파일 기준의 평소 sync 로 처리되고 그 저널은 `recovered` 로 표시된다.
- **위조된 저널**: 저널의 모든 경로는 어댑터가 쓸 수 있는 경로(해당 스코프의 `.mcp.json`, `settings.json`, 상태 파일, 스킬 디렉터리 1단계)이고 심볼릭 링크가 아닌지 다시 검증하고, 버전·id·대상·scope·root·해시 형식과 항목 목록 대 상태 해시의 일관성도 검증한다. 하나라도 어긋나면 그 저널은 **무시**하고 계획에 알린다. 어느 경우에도 저널의 내용을 경로로 삼아 파일을 지우거나 쓰지 않는다(비교에만 쓴다). 형식이 맞는 위조 저널이 할 수 있는 최대치는 "내용 해시가 정확히 같은 항목을 관리 항목으로 취급"이며, 스킬은 마커까지 맞아야 한다. 저널에서만 알려진 스킬이 더는 필요 없으면 마커+해시가 맞을 때만 제거 대상이 된다(MCP 서버·hook 은 불필요해져도 자동 제거하지 않는다).
- **임시 파일**: 쓰기용 임시 파일/디렉터리는 `.forge-tmp-<pid>-<8hex>-<대상 이름>`, 옮겨 둔 이전 트리는 `.forge-old-<pid>-<8hex>-<대상 이름>` 이다. 잠금을 얻은 `--apply` 는 시작 시(충돌이 없을 때) 관리 대상 디렉터리(프로젝트 루트, `.claude`, `.claude/skills`, `.forge` / user: `~/.claude`, `~/.claude/skills`, `$FORGE_HOME/sync-state`)에서 **정확히 이 이름 형식이고 그 pid 의 프로세스가 살아 있지 않은 것만** 지운다. 심볼릭 링크는 따라가지도 지우지도 않고, 상위 경로가 링크인 디렉터리는 열지 않으며, 형식에 맞지 않는 사용자 파일(`.forge-tmp-notes.txt` 등)은 건드리지 않는다. 계획은 이를 `정리 예정` 으로 표시만 한다(`--json` 의 `stale_temp_files`, 적용 결과의 `cleaned_temp_files`).
- **크래시 시험**: 엔진은 모든 단계 경계와 파일 기본 동작(임시 파일 기록 직후, 트리 이동 사이 등)에서 강제 종료를 흉내 내는 훅(`ApplyOptions.Crash`)을 갖고, 테스트(`recovery_test.go`)는 그 모든 지점에서 죽인 뒤 다음 계획이 중단을 표시하고 `--apply` 가 깨끗한 실행과 같은 바이트로 수렴하며 임시 파일·저널 사본이 남지 않고 `--restore latest` 후 다시 수렴하는지 확인한다.

### 8. 심볼릭 링크 정책
- 설정 파일(`.mcp.json`, `settings.json`, 상태 파일)과 스킬 대상 디렉터리, 그리고 그 사이의 `.claude`, `.claude/skills`, `.forge` 가 **심볼릭 링크이면 따라가지 않고 거부**한다(해당 항목은 충돌로 보고). 링크를 통해 다른 곳의 파일을 바꾸지 않기 위해서다. dotfiles 저장소로 `~/.claude` 를 링크해 쓰는 경우 `--scope user` 는 충돌로 보고되므로 링크 대상 쪽을 직접 관리한다.
- **부모 경로**: 프로젝트 루트(`.git` 이 있는 디렉터리)와 HOME 은 한 번 `EvalSymlinks` 로 실경로를 구한 뒤 그 **아래** 구성요소만 검사한다. 따라서 저장소가 링크된 작업 디렉터리 아래(`/tmp`, 링크된 workspace)에 있어도 동작한다. 루트 자체나 그 위의 링크는 사용자가 선택한 위치로 존중한다.
- 레지스트리 스냅샷에 링크/특수 파일이 있으면 복사하지 않는다(충돌).
- 읽기 전용 파일/디렉터리는 사용자의 "건드리지 마" 신호로 보고 충돌로 보고한다.

## 출력

계획은 항목별로 `추가/변경/삭제/충돌/건너뜀` 과 이유를 표로 보여 준다(`변경 없음` 항목은 요약에만 센다). 모든 동적 문자열(이름, 경로, 이유, 명령)은 `internal/textsafe` 로 정화하고, diff 는 줄 길이·줄 수를 제한한다. diff 의 비밀 마스킹은 **줄이 아니라 JSON 값 단위**다. diff 를 계산하기 전에 표시용 사본에서 키 이름이 TOKEN/SECRET/PASSWORD/KEY/AUTH/COOKIE/CREDENTIAL 등을 포함하는(대소문자·camelCase 무관) 문자열 값, **그런 키 아래 하위 트리 전체**(중첩 객체·배열·배열 안 객체)의 문자열과 숫자 값, `--token` 류 플래그 다음 요소를 토큰 단위로 찾아 `***` 로 바꾼다(`credentials`/`secrets`/`auth` 아래 객체의 값도 가려진다. 값 전체가 `${VAR}`/`{env:VAR}` 인 참조는 비밀이 아니므로 그대로 보인다). 한 줄로 압축된 파일, 여러 줄 파일, 중첩, 배열 안 객체를 똑같이 처리하며, 마스킹은 문자열 리터럴 안만 바꾸므로 줄 수가 변하지 않는다(전후 줄이 1:1 대응). 그 뒤에 줄 단위 값 패턴(알려진 자격 증명 형태, 인라인 `--token X`) 마스킹이 한 번 더 적용된다. 마스킹은 **표시용**일 뿐 쓰는 내용은 바꾸지 않는다. 알려진 한계: 비밀 값만 바뀐 줄은 양쪽이 `***` 가 되어 diff 에 변경으로 보이지 않고(파일은 `쓸 파일` 목록에 나온다), 평범한 형태의 비밀이 비밀스럽지 않은 키 이름 아래 있으면 휴리스틱이 놓칠 수 있다.

표의 이름·이유 칸은 표시용으로 각각 80자·120자에서 `…` 로 줄인다(줄였다는 안내가 붙는다). `--json` 은 줄이지 않는다(단 모든 동적 문자열은 300자에서 정화·절단한다).

`--json` 스키마(`schema: 1`): `target, scope, root, mode(plan|apply), applied, conflicts, summary{add,update,remove,conflict,skip,unchanged}, items[{kind,name,action,reason,path}], files[{path,change(create|modify|delete|replace),before_hash,after_hash}], commands[], notes[], diff[{path,diff}](--diff 시), backup{id,path}(적용 시)`. 크래시 복구용 선택 필드(있을 때만): `interrupted[{backup_id,kind,created_at,ops,done,todo,changed}]`, `stale_temp_files[]`, 적용 시 `cleaned_temp_files[]`, `recovered[]`(완료 표시한 중단 백업 id).
`--list-backups --json`: `{schema, keep_limit, backups[{id,path,created_at,kind,target,scope,root,status,files}]}`.

## 충돌 해소 방법

| 충돌 이유 | 해소 |
|-----------|------|
| 같은 이름의 사용자 스킬(마커 없음) | 사용자 디렉터리를 다른 이름으로 옮기거나 레지스트리 스킬 이름을 바꾼다. Forge 는 덮어쓰지 않는다 |
| `.mcp.json` 에 같은 이름의 사용자 서버 | 사용자 항목을 지우거나 이름을 바꾸면 다음 sync 가 추가한다 (내용이 같아도 관리 대상이 아니라서 충돌) |
| 같은 command 의 사용자 hook | 사용자 hook 을 지운 뒤 sync (또는 `--no-hooks` 로 hook 을 건너뜀) |
| 관리 항목이 수정됨 | 수정을 되돌리거나, Forge 정의로 덮으려면 `--force-managed` (백업됨) |
| 마커는 있는데 상태 파일에 없음 | 다른 곳에서 복사된 디렉터리다. 지우거나 이름을 바꾼 뒤 sync. (이유에 "중단된 이전 sync 가 만든 디렉터리"가 보이면 중단된 sync 가 만든 항목을 이후 수정한 것이다: 수정을 되돌리거나 지운 뒤 sync) |
| 심볼릭 링크 / 읽기 전용 | 링크를 실제 디렉터리로 바꾸거나 쓰기 권한을 준다 |
| 레지스트리 서버 이름이 `forge` | 게이트웨이 항목 이름과 충돌하므로 다른 이름으로 다시 등록 |
| 레지스트리 스냅샷 검증 실패 | 변조/손상이므로 `forge skill` 로 다시 등록 |
| 렌더 거부(비밀 의심 값) | 값을 환경변수에 넣고 `${ENV}` 참조로 다시 등록 |

충돌이 있으면 `--apply` 는 아무것도 쓰지 않는다. 충돌 항목만 건너뛰고 나머지를 적용하려면 `--skip-conflicts`.

## 지원 버전과 형식 근거

지원 버전 목록: **Claude Code 2.1.305** (`claudecode.SupportedVersions`). 픽스처: `internal/adapter/claudecode/testdata/2.1.305/`
(`project_before/` 입력, `golden/` 기대 출력; `UPDATE_GOLDEN=1 go test ./internal/adapter/claudecode` 로 갱신). 설치 버전은 `forge sync --apply`·`forge doctor` 의 버전 게이트가 `claude --version` 으로만 확인한다("버전 게이트" 절, 지원 버전 표는 `clientver.Specs`). 다른 버전 형식 변화가 확인되면 이 절에 픽스처 추가 절차(TASK-0022 AC-5)를 문서화한다.

| 항목 | 근거 |
|------|------|
| 스킬 경로 `<repo>/.claude/skills/<name>/`, `~/.claude/skills/<name>/` | [doc] |
| `.mcp.json` 최상위 `mcpServers`, stdio/http/sse 형식, `type` 명시 필수, `${VAR}` 확장 | [doc] |
| `settings.json` `hooks` 스키마(이벤트 → matcher 그룹 → handler 배열) | [doc], [local] 사용자 전역 settings.json 의 Notification hook 구조 |
| `~/.claude.json` 의 키 구조 | [미확인] — 읽지 않으므로 필요 없음 |
| `claude mcp add` 옵션 순서(가변 인자 `--env`/`--header` 와 위치 인자 처리), user 스코프에서의 `${VAR}` 처리 | [미확인] — 출력만 하며, 이름을 먼저 두고 옵션을 뒤에 둬 가변 인자가 이름을 삼키지 않게 했다 |
| `.mcp.json`/`settings.json` 의 모르는 필드 거부 여부 | [미확인] — 어댑터가 추가하지 않는다 |
| `timeout_ms`, `cwd` 의 `.mcp.json` 표현 | [미확인] — 렌더하지 않는다 |
| 최초 사용 시 `.mcp.json` 서버 승인 | [doc] — 계획에 참고로 표시 |

## 한계

- (opencode) `opencode.jsonc` 는 지원하지 않는다(TASK-0022 후속 후보: 주석 보존 편집기). 중복 노출 방지는 opencode → claude-code 방향만이다. 어댑터 코드가 claudecode 에서 복사되어 있어 세 번째 타깃 전에 공통화가 필요하다(`docs/adapter.md`).
- 프로젝트 스코프 레지스트리(`skill --project`)가 없어 전역 레지스트리만 내보낸다. 프로필별 노출(TASK-0012)은 아직 없고 MCP 노출은 `mcp expose`/`enable --client` 설정을 쓴다. 스킬·도구의 클라이언트별 노출 설정은 없다(활성이면 모두 내보냄). 사용자 정의 도구(`forge tool`)는 게이트웨이로만 제공되어 별도 렌더링이 없다.
- user 스코프 MCP 는 파일을 쓰지 않아 상태 추적·자동 제거·되돌리기가 없다(명령 출력만).
- 사용자가 `.mcp.json` 서버를 직접 정의한 이름과 겹치면 항상 충돌이다(동일 내용이어도 채택하지 않음).
- 상태 파일이 없는 저장소(예: 상태 파일을 커밋하지 않은 다른 클론)에서는 이미 sync 된 항목이 사용자 소유로 보여 충돌로 보고된다. 상태 파일을 같이 커밋하거나, 충돌 항목을 지우고 다시 sync 한다.
- hook command 는 `--bin` 값에 따라 문자열이 달라진다. `--bin` 을 바꾸면 이전 관리 handler 를 제거하고 새 handler 를 추가한다.
- 동시에 같은 파일을 편집하는 다른 프로그램(편집기)과는 apply 직전 해시 재검증으로만 충돌을 감지한다(검증~rename 사이의 짧은 틈은 남는다).
- 크래시 복구: 저널은 백업 디렉터리(`$FORGE_HOME/backups`)에 있어야 인식된다(백업을 지웠거나 `FORGE_HOME` 을 바꾸면 중단 사실을 모르고 평소 규칙대로 충돌로 보고한다). 복구는 "앞으로 완료"만 자동이며, 중단 후 사용자가 같은 항목을 수정하면 충돌로 남는다. 중단된 sync 가 쓴 MCP 서버·hook 이 복구 전에 레지스트리에서 사라졌다면 자동으로 제거하지 않는다. pid 생존 검사는 pid 가 재사용되면 임시 파일을 오래 남길 수 있다(지우지 않는 쪽으로만 틀린다). 복구 백업의 `--restore` 는 중단된 sync 의 변경까지 되돌리지만, 중간 상태 트리는 직후 해시를 기록하지 못해 `--force` 가 필요할 수 있다. 전원 차단 시 파일시스템이 쓰기 순서를 바꾸는 환경은 가정하지 않는다(`fsync` 사용).
- plan 이 레지스트리 DB 를 읽기 전용으로 열 때 빈 `-wal`/`-shm` 파일이 남을 수 있다(TASK-0021 에서 처리).
- Windows 는 검증하지 않았다(잠금은 `.held` 파일 방식 폴백).

## 프로필, `--target all`, 버전 게이트 (TASK-0012)

### 프로필 반영

계획·적용은 (클라이언트, 프로젝트)에 적용되는 프로필을 따른다(`docs/profile.md` §1: 프로젝트 활성 프로필 → 클라이언트 전역 활성 프로필 → 프로필 없음=기존 동작). 프로필이 있으면 **허용 목록**이다: 프로필에 없는 스킬·MCP 서버는 렌더링되지 않고, 이전에 Forge 가 쓴 것이면 관리 항목 삭제로 계획된다. 스킬은 `direct` 노출인 멤버만 클라이언트 스킬 디렉터리에 렌더링되며 핀이 있으면 그 불변 버전을 복사한다. MCP 서버는 프로필의 멤버 노출(`direct|gateway|hidden`)을 `mcp.exposure.*` 대신 쓴다. 계획에는 `참고: 프로필 "<이름>" 적용 (…)` 또는 `참고: 프로필 없음: 레거시 동작…` 줄이 항상 나온다. `--scope user` 는 전역 활성 프로필만 따른다. hook 은 프로필과 무관하다. `forge profile use` 는 활성화만 기록하고 파일을 쓰지 않으므로 반영하려면 sync 를 실행해야 한다.

### `--target all`

```bash
forge sync --target all [--diff] [--json]        # claude-code 와 opencode 를 하나의 통합 계획으로 (쓰지 않음)
forge sync --target all --apply [--skip-conflicts] [--allow-untested-version]
```

- 두 어댑터의 계획을 **모두 계산한 뒤** 판단한다. 출력은 `=== claude-code ===`, `=== opencode ===` 계획과 `=== 통합 요약 ===`(충돌 합계, `--apply` 에서는 대상별 적용 상태). `--json` 은 `{"schema":1,"target":"all","mode","conflicts","applied":[…],"partial","targets":[{"target","status","error","report":<단일 대상 report>}],"restore":[…]}` 이며 `status` 는 `planned|applied|failed|not-applied`.
- **충돌이 둘 중 어디에든 하나라도 있으면 `--apply` 는 아무것도 쓰지 않는다**(백업도 만들지 않는다). `--skip-conflicts` 는 양쪽의 충돌 항목만 제외하고 적용한다.
- **중복 노출 방지**: OpenCode 는 `.claude/skills` 도 읽는다. claude-code 계획이 관리하게 될 스킬(계획 후 상태 파일 기준)은 opencode 계획에서 "claude-code 가 관리"로 건너뛴다(`Request.ClaimedSkills`). 적용 순서는 항상 claude-code → opencode.
- **적용은 순차이며 하나의 트랜잭션이 아니다.** 각 어댑터는 자체적으로 원자적이다(백업·저널·롤백). 두 대상의 잠금을 고정 순서로 모두 잡은 채 계획하고 적용한다. claude-code 가 실패하면 opencode 는 적용하지 않는다(`not-applied`). claude-code 가 적용된 뒤 opencode 가 실패하면 **부분 성공**이다: 출력이 `부분 성공: claude-code 만 적용되었고…`, 대상별 상태, 적용된 쪽을 되돌리는 명령(`forge sync --restore <백업 id>`)과 재시도 안내를 보여 주고 종료 코드는 비0(`일부만 적용되었습니다: …`)이다. 같은 명령을 다시 실행하면 이미 적용된 쪽은 변경 없음으로 수렴한다.
- `--no-hooks`/`--remove-hooks` 는 claude-code 에만 적용된다. 백업·복원 명령(`--list-backups/--restore/--prune-backups`)은 대상 하나씩 다룬다.

### 버전 게이트

`forge sync --apply` 는 파일을 쓰기 전에 대상 클라이언트마다 `<binary> --version`(그 외 인자 없음)을 실행해 분류한다(정확히 검증된 버전 ok, 같은 major.minor 의 다른 patch info "미검증 패치", 그 밖의 버전·실행 실패·인식 불가 warn "미검증 버전: 생성한 설정이 올바르게 로드되지 않을 수 있습니다", 바이너리 없음 info).
warn 이 하나라도 있으면 **아무것도 쓰지 않고 중단**하며 `--allow-untested-version` 을 안내한다. 결과는 계획의 `참고:` 줄에 남고 캐시하지 않는다. 계획(`--apply` 없음)은 기본적으로 클라이언트를 실행하지 않는다(`--check-version` 으로 옵트인). 실행 안전 설계(PATH 탐색, 프로젝트 안 바이너리 거부, 환경 격리, 타임아웃·출력 상한)와 지원 버전 표는 `docs/doctor.md`.
