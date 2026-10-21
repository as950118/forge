# OpenCode 설정 형식 (TASK-0001 스파이크)

- 확인 버전: **OpenCode v2.0.24** (`opencode --version`, `/Users/heonjinjeong/.opencode/bin/opencode`, 단일 Mach-O 바이너리)
- 확인일: 2026-10-21
- 근거 표기: `[doc]` 공식 문서(opencode.ai/docs/{mcp-servers,plugins,skills}), `[help]` `opencode ... --help`, `[local]` 로컬 파일/DB 관찰(홈은 읽기만, 비밀값 미기록), `[bin]` 설치 바이너리에 내장된 번들 텍스트(`strings`) 관찰, `[미확인]`.
- **중요 주의:** 설치본은 v2.x 로 **SQLite 기반 세션 저장·백그라운드 서비스 구조**(`opencode service`, `opencode serve`, `session_v2` 테이블)이다. 공식 문서 페이지는 어느 버전 기준인지 명시가 없고 `[doc]` 항목 중 v2.0.24 에서 `[bin]`/`[help]`/`[local]` 로 교차 확인되지 않은 것은 "문서 기준(미교차확인)"으로 표시한다. 로컬에는 사용자 설정(`opencode.json`)·플러그인·MCP·스킬이 하나도 없고(`opencode mcp list`=없음, `plugin list`=없음), 유일한 세션은 모델 엔드포인트 오류로 실패한 것이라 **도구 호출 기록의 실제 형태는 관찰하지 못했다.**

## 0. 경로와 설정 탐색

- `opencode debug paths` [help+local]: config `~/.config/opencode`, data `~/.local/share/opencode`, cache `~/.cache/opencode`, state `~/.local/state/opencode`, db `~/.local/share/opencode/opencode.db`, log `~/.local/share/opencode/log`.
- `opencode debug config` 는 "설정 소스 목록"을 보여주며 로컬에서는 `~/.config/opencode` 디렉터리만 나옴. [local]
- 설정 파일명 [bin]: 전역 `~/.config/opencode/{config.json,opencode.json,opencode.jsonc}`, 프로젝트(상위 디렉터리 탐색) `opencode.json`, `opencode.jsonc`, `.opencode/opencode.json(c)`.
- 탐색 루트 [bin]: 전역 `~/.config/opencode`, `~/.claude`, `~/.agents`; 프로젝트는 현재 디렉터리에서 위로 올라가며 `.claude`, `.agents`, `.opencode`, `opencode.json(c)`.
- 값 치환 [bin]: `{env:NAME}`, `{file:path}` 정규식 확인. [doc]도 `{env:VAR}` 를 예시.
- 조직 기본값: `/.well-known/opencode` 원격 엔드포인트(WellKnown 코드 존재 [bin], [doc]).
- 로컬 `service.json`(`~/.config/opencode`, `~/.local/state/opencode`)은 백그라운드 서비스 접속 정보이며 비밀값(password) 포함 -> 값은 기록하지 않음. Forge 는 건드리지 말 것. [local]

## 1. 스킬

| 항목 | 내용 | 근거 |
|------|------|------|
| 프로젝트 경로 | `.opencode/skills/<name>/SKILL.md`, `.claude/skills/<name>/SKILL.md`, `.agents/skills/<name>/SKILL.md` (현재 디렉터리~git 루트 탐색) | [doc]; 탐색 루트 `.claude/.agents/.opencode` 는 [bin] 교차확인 |
| 전역 경로 | `~/.config/opencode/skills/`, `~/.claude/skills/`, `~/.agents/skills/` | [doc]; 전역 루트 3종은 [bin] 교차확인 |
| 스캔 패턴 | `{*.md,**/SKILL.md}` (디렉터리 하위 `SKILL.md`, 그리고 `<name>.md` 단일 파일 형태도 허용되는 코드 존재) | [bin] |
| frontmatter | `name`(필수), `description`(필수, 1~1024자), `license`, `compatibility`, `metadata`(문자열 맵). OpenCode 전용: `metadata."opencode/slash": "true"`(TUI `/name` 노출), `metadata."opencode/autoinvoke": "false"`(모델 자동선택 목록에서 숨김, 이름 지정 로드는 가능) | [doc]; `opencode/autoinvoke` 문자열은 [bin] 확인, `opencode/slash` 는 [bin] 에서 미발견 |
| name 규칙 | `^[a-z0-9]+(-[a-z0-9]+)*$`, 1~64자, 디렉터리명과 일치 | [doc] (미교차확인) |
| 로딩 방식 | `skill` 도구: 사용 가능 스킬 목록이 도구 설명의 `<available_skills>` 에 들어가고 모델이 `skill({name})` 호출로 본문 로드 | [doc]; `opencode.tool.skill` 플러그인 존재 [bin] |
| 권한 | `opencode.json` `permission.skill` 패턴(`allow|ask|deny`), 에이전트별 오버라이드, `tools.skill:false` 로 도구 비활성 | [doc] (미교차확인) |
| 호환 | Claude Code 형식 `.claude/skills` 를 그대로 읽음 -> Forge 가 `.claude/skills` 에 렌더링하면 두 클라이언트 모두 노출 가능(단 name=디렉터리명·소문자 규칙 준수 필요) | [doc]+[bin] |
| 새 `opencode/` 메타 외 Claude 전용 필드 | `allowed-tools`, `disable-model-invocation` 등의 처리는 [미확인] | [미확인] |

## 2. MCP 설정

| 항목 | 내용 | 근거 |
|------|------|------|
| 위치 | 전역: `~/.config/opencode/opencode.json(c)`; 프로젝트: `opencode.json(c)`. 키: 최상위 `mcp` (서버 이름 -> 객체) | [doc], [bin] (`"mcp": {"my-server": {"type":"remote","url":...}}` 예시가 CLI 안내문에 내장) |
| local(stdio) | `{"type":"local","command":["cmd","arg",...],"environment":{K:V},"cwd":"...","enabled":true,"timeout":5000}` — `command` 는 **배열**, 환경변수 키는 `environment` | [doc] (미교차확인) |
| remote(http/sse) | `{"type":"remote","url":"...","headers":{...},"oauth":{clientId,clientSecret,scope}|false,"enabled":true,"timeout":5000}` — `type` 은 `local|remote` 두 가지뿐이며 http/sse 구분 키는 문서에 없음(전송 방식 자동 선택 여부 [미확인]) | [doc] |
| 값 치환 | `"Bearer {env:MY_API_KEY}"` 형식 | [doc], [bin] |
| OAuth | 원격 서버 기본 OAuth 지원. 토큰 저장 `~/.local/share/opencode/mcp-auth.json`(문서). v2.0.24 에서는 [local] 파일 없음, `opencode mcp auth`/`logout` 존재 | [doc], [help] |
| 도구 필터 | `tools` 키에 `"servername_*": false` 등 글롭. 서버 도구는 서버명 접두 | [doc] |
| CLI | `opencode mcp list`, `opencode mcp add [--url U] [--header k=v] [--env k=v] [--global] <name> [-- cmd args...]` (`--global` 없으면 프로젝트 설정에 기록), `opencode mcp auth`, `opencode mcp logout`. **`remove` 서브명령 없음** | [help] |
| 반영 | `opencode reload` (백그라운드 서비스 설정 재로드) | [help] |

## 3. Hook / 플러그인 이벤트

| 항목 | 내용 | 근거 |
|------|------|------|
| 플러그인 위치 | 프로젝트 `.opencode/plugins/`, 전역 `~/.config/opencode/plugins/`, npm 패키지는 설정의 `plugin` 배열 | [doc] (v2.0.24 미교차확인) |
| CLI | `opencode plugin list|add <npm/git spec>|check|update|remove` — `add`/`remove` 는 **전역 설정**에 기록 | [help] |
| 함수 형태 | `export const X = async ({project, client, $, directory, worktree}) => ({ ...hooks })` (JS/TS, Bun 사용, `@opencode-ai/plugin` 타입) | [doc] (미교차확인) |
| 이벤트(문서 목록) | `command.executed`, `file.edited`, `file.watcher.updated`, `installation.updated`, `lsp.client.diagnostics`, `lsp.updated`, `message.part.removed|updated`, `message.removed|updated`, `permission.asked|replied`, `server.connected`, `session.created|compacted|deleted|diff|error|idle|status|updated`, `shell.env`, `tool.execute.before|after`, `tui.*` | [doc] |
| 입력 payload | 문서에 일부만 명시: `event.type`; `tool.execute.before` 입력 `input.tool`, 출력 `output.args`; `tool.execute.after` 입력 필드는 문서에 **없음** | [doc] -> after 필드 [미확인] |
| v2.0.24 내부 | 번들에 `tool.hook("execute.before")`, `trigger("tool","execute.before",{tool,sessionID,agent,messageID,id,input})`, `"tool.before"/"tool.after"` 이벤트 이름이 존재하여 도구 전/후 훅 개념은 유지됨. 단 외부 플러그인에 노출되는 payload 형태와 `opencode.json` 의 `plugin` 키 유효성은 실행 검증 못함 | [bin], 외부 노출은 [미확인] |
| 외부 훅 대안 | `opencode serve`/`api`(v2 API·웹 서버), `acp` 로 이벤트를 외부에서 구독할 가능성은 [미확인] | [help] |

참고: Claude Code 식의 "설정 파일에 셸 명령을 적는 hook" 은 문서에 없음 -> 플러그인(JS) 작성이 유일한 확인된 훅 수단.

## 4. Transcript / 세션 로그 (R2)

| 항목 | 내용 | 근거 |
|------|------|------|
| 저장소 | SQLite `~/.local/share/opencode/opencode.db` (WAL). 테이블: `session_v2, session_message, event, event_sequence, session_inbox, session_pending, instruction_*, permission, project, project_directory, workspace, worktree, account*, credential, kv, migration` | [local] |
| 세션 요약 컬럼 | `session_v2`: `id, project_id, directory, title, version, agent, model(JSON), cost, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, parent_id, fork_session_id, time_*, idle_outcome` | [local] 스키마 |
| 메시지 | `session_message(id, session_id, type, seq, time_*, data JSON)`. 관찰한 `type`: `user`(`text, files, agents`), `assistant`(`agent, model{id,providerID,variant}, content[], finish, error`), `idle`(`outcome`) | [local] (실제 행 3종) |
| 내보내기 | `opencode session export [--sanitize] [<id>]` -> JSON(`info{id, projectID, agent, model, cost, tokens{input,output,reasoning,cache{read,write}}, outcome, time, location}`, `messages[]`). `session list/import/delete` 도 존재 | [help], [local] 실행 확인 |
| 이벤트 로그 | `event(aggregate_id, seq, type, data)` 테이블 존재하나 로컬 행 0건 -> 내용 [미확인] | [local] |
| 모델/토큰 | 세션 단위 누적 토큰(입/출/추론/캐시 R/W)·비용·모델이 구조적으로 존재. 메시지 단위 usage 와 도구 호출 part 의 저장 형태는 성공한 응답이 없어 **관찰 못함**(`content: []`) | [local] / [미확인] |
| 파일 로그 | `~/.local/share/opencode/log/opencode.log` (애플리케이션 로그). 도구 이력 용도 여부 [미확인] | [local] |
| 주의 | DB 는 실행 중 서비스가 쓰므로 읽기 전용으로 열고 스키마 버전(`migration`)에 종속 -> 직접 SQL 보다 `session export` JSON 이 안정적. `credential` 테이블은 비밀 포함이므로 절대 접근/복제 금지 | [local] |

결론(R2): OpenCode 는 **모델·토큰·비용을 세션 단위로 수집 가능**(DB 컬럼/`session export`). 메시지·도구 단위 토큰은 [미확인]. 

## 5. 스킬 호출 식별 (R3)

- 스킬은 `skill` 도구 호출로 로드된다 [doc], `opencode.tool.skill` 내장 [bin]. 따라서 (a) 플러그인 `tool.execute.before/after` 에서 `input.tool === "skill"` 이고 `args.name` 이 스킬명(필드명은 [doc] 예시에 없음 -> [미확인]), (b) `session_message` assistant `content` 의 tool part 에서 도구명 `skill` 을 조회하는 방식이 이론상 가능하나 **v2.0.24 에서 실제 payload/행을 관찰하지 못했다.**
- `opencode/autoinvoke:"false"`/slash 호출(`/name`)은 `user` 메시지의 `skills` 필드(`Prompt.skills` 스키마 [bin])에 기록될 가능성이 있으나 [미확인].

결론(R3): **잠정 "가능(미검증)"**. 식별 수단 후보는 skill 도구 전/후 훅과 세션 export 이나, 실제 호출 1건을 일으켜 payload 를 캡처하기 전에는 확정 불가. 후속 검증: 사용 가능한 모델 설정 후 임시 디렉터리에서 스킬 1개로 `opencode run --format json` 실행, 결과 JSON·`session export` 확인.

## Forge 어댑터 설계 시사점

1. 스킬: 두 클라이언트가 `.claude/skills/<name>/SKILL.md` 를 모두 읽으므로(OpenCode 는 [doc]+[bin]) Forge 는 하나의 렌더링 경로(`.claude/skills`)로 두 클라이언트를 커버하되, OpenCode 의 name 규칙(`^[a-z0-9]+(-[a-z0-9]+)*$`, 디렉터리명 일치)과 description 1~1024자를 `forge skill validate` 에 포함. Claude 의 `name` 은 선택이므로 name 필수화가 공통 안전선. 전용 경로 필요 시 `.opencode/skills/` 사용(중복 노출 방지 검토 필요: 같은 스킬이 두 경로에 있을 때 동작 [미확인]).
2. MCP: 스키마가 다르다 — Claude `mcpServers.<n>{type:stdio|http|sse, command, args[], env, url, headers}` vs OpenCode `mcp.<n>{type:local|remote, command[](배열), environment, url, headers, enabled}`. 어댑터에서 `command+args` 를 합쳐 배열화, `env`->`environment`, 치환 문법 `${VAR}` -> `{env:VAR}` 변환 필요. http/sse 구분은 OpenCode 에 없음(Forge 모델은 유지하되 렌더링 시 `remote` 로 접음).
3. 설정 쓰기: OpenCode 는 `opencode mcp add --global|프로젝트` CLI 가 있으나 `remove` 가 없으므로 Forge 가 `opencode.json(c)` 를 직접 편집해야 한다. JSONC(주석) 가능성 때문에 파서가 주석 보존/마커 처리를 해야 함. `opencode reload` 로 반영.
4. 이력: OpenCode 는 설정 기반 shell hook 이 없어 JS 플러그인(`.opencode/plugins/` 또는 전역)을 Forge 가 배포하고, 플러그인이 `forge` 로컬 엔드포인트/CLI 에 이벤트를 전달하는 구조가 필요. 단 v2.0.24 에서 플러그인 payload 를 검증하지 못했으므로 M4 전 캡처 스파이크 필요. 보완 경로로 `opencode session export` / DB 읽기전용 폴링(스키마 버전 의존)을 쓴다.
5. 토큰·모델: 세션 합계는 DB/export 에서 취득 가능. Claude(응답 단위)와 해상도가 달라 Ledger 에 `tokens` 는 nullable 세션/메시지 구분 필드로 설계.
6. 게이트웨이(`forge mcp serve`)는 두 클라이언트에 공통 경로이므로 R3 의 보증 경로로 유리: OpenCode 쪽은 훅 payload 가 미확정이어서 게이트웨이 `skill` 메타 도구 기록이 더 확실하다.
7. 버전 리스크(R1): OpenCode 는 v2 에서 저장 구조(SQLite)와 서비스 구조가 바뀐 이력이 있어 어댑터에 `opencode --version` 게이팅과 `doctor` 점검이 필수. 사용자 홈 `service.json` 등은 Forge 가 접근하지 않는다.
