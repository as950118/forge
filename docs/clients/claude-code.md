# Claude Code 설정 형식 (TASK-0001 스파이크)

- 확인 버전: **Claude Code 2.1.305** (`claude --version`, `/Users/heonjinjeong/.local/bin/claude`)
- 확인일: 2026-10-21
- 근거 표기: `[doc]` 공식 문서(code.claude.com/docs/en/{skills,mcp,hooks}), `[help]` `claude ... --help` 출력, `[local]` 로컬 파일 관찰(홈은 읽기만, 비밀값 미기록), `[미확인]` 확인 못함.
- 공식 문서는 최신판을 가져온 것이라 2.1.305와 정확히 같다는 보장이 없다. `[doc]` 항목은 doctor/픽스처 테스트로 재검증이 필요하다(R1).

## 1. 스킬

| 항목 | 내용 | 근거 |
|------|------|------|
| 전역 경로 | `~/.claude/skills/<name>/SKILL.md` | [doc], [local] `~/.claude/skills/` 존재(내용은 `synced/` 하위, claude.ai 동기화분) |
| 프로젝트 경로 | `<repo>/.claude/skills/<name>/SKILL.md` (작업 디렉터리~저장소 루트 상위까지 탐색) | [doc] |
| 중첩 | `<subdir>/.claude/skills/...` (해당 하위 파일을 처음 만질 때 로드), `--add-dir` 디렉터리의 `.claude/skills/` | [doc] |
| 엔터프라이즈 / 플러그인 | managed settings 디렉터리의 `.claude/skills/`; 플러그인 `<plugin>/skills/<name>/` (`/plugin:skill` 네임스페이스) | [doc] |
| 우선순위 | 동일 이름: enterprise > personal > project. 플러그인은 네임스페이스로 충돌 없음 | [doc] |
| frontmatter | 모든 필드 선택. `description` 권장. `name`(기본=디렉터리명, 소문자·숫자·하이픈), `description`, `when_to_use`, `argument-hint`, `arguments`, `disable-model-invocation`, `user-invocable`, `allowed-tools`, `disallowed-tools`, `model`, `effort`, `context`(fork), `agent`, `background`, `hooks`, `paths`, `shell` | [doc] |
| 제약 | 여는 `---`가 파일 첫 줄이어야 함. YAML 파싱 실패 시 필드 없이 로드. 모르는 필드는 조용히 무시. claude.ai/Skills API 업로드는 `name, description, license, compatibility, metadata, allowed-tools`만 허용 | [doc] |
| 로딩 방식 | 설명만 상시 컨텍스트, 본문은 호출 시 로드. 모델이 `Skill` 도구를 호출하거나 사용자가 `/skill-name` 입력 | [doc], [local] transcript에 `tool_use name="Skill"` 관찰(§4) |
| 변경 감지 | 세션 중 `SKILL.md` 추가/수정/삭제는 재시작 없이 반영. 새 최상위 skills 디렉터리는 `/reload-skills` | [doc] |
| 가져오기 호환 | agentskills.io 규격 SKILL.md(name+description)는 그대로 사용 가능 | [doc] (name/description 공통 필드 기준) |

## 2. MCP 설정

| 항목 | 내용 | 근거 |
|------|------|------|
| 스코프/파일 | local(기본): `~/.claude.json` 의 해당 프로젝트 경로 하위. project: `<repo>/.mcp.json` (버전관리 공유). user: `~/.claude.json` | [doc]. `~/.claude.json` 내부 키 구조는 비밀값이 섞여 있어 열람하지 않음 -> 정확한 키 경로 [미확인] |
| 최상위 키 | `.mcp.json`: `mcpServers` | [doc] |
| 우선순위 | local > project > user > 플러그인 > claude.ai 커넥터. 이름 충돌 시 항목 전체 교체(필드 병합 없음) | [doc] |
| stdio | `{"type":"stdio","command":"...","args":[...],"env":{K:V}}` | [doc] |
| http | `{"type":"http","url":"...","headers":{...}}` (`streamable-http` 별칭) | [doc] |
| sse | `{"type":"sse","url":"...","headers":{...}}` (deprecated) | [doc] |
| 기타 | `ws` 타입, `headersHelper`, `timeout`, `alwaysLoad`; OAuth는 `--client-id/--client-secret/--callback-port` | [doc], [help] |
| 주의 | `url`만 있고 `type`이 없으면 stdio로 취급되어 설정 오류로 스킵 -> 어댑터는 `type`을 항상 명시 | [doc] |
| env/헤더 | `${VAR}`, `${VAR:-default}` 확장. 적용 대상: `command, args, env, url, headers`. 미설정+기본값 없음이면 경고 후 리터럴 유지. 원격 url/headers에서 `ANTHROPIC_API_KEY` 등은 빈 값으로 읽힘 | [doc] |
| CLI | `claude mcp add [-t stdio\|sse\|http] [-s local\|user\|project] [-e K=V] [-H "K: V"] <name> <cmdOrUrl> [args...]`, `add-json <name> <json>`, `get`, `list`, `remove`, `login/logout`, `reset-project-choices`, `add-from-claude-desktop` | [help] |
| 승인 | `.mcp.json` 서버는 최초 사용자 승인 필요(`⏸ Pending approval`). `-p`/SDK에서는 승인 없이 로드. 차단: `disabledMcpjsonServers`, `--strict-mcp-config` | [help], [doc] |
| 1회성 주입 | `--mcp-config <json파일|문자열>`, `--strict-mcp-config` | [help] |
| 재적용 | MCP 설정 변경은 `/reload-plugins` 필요 | [doc] |

## 3. Hook 이벤트

설정 위치: `settings.json` 의 `hooks` (전역 `~/.claude/settings.json`, 프로젝트 `.claude/settings.json`, 로컬 `.claude/settings.local.json`; 플러그인·스킬 frontmatter `hooks`도 가능). [doc], [local] 전역 settings.json 에 `Notification` hook 1건이 실제로 존재(구조 확인).

스키마: 이벤트 -> matcher 그룹 -> handler 배열. [doc], [local]
```json
{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"...","timeout":30}]}]}}
```
- handler `type`: `command`(`command`, `args`, `async`, `shell`...), `http`(`url`, `headers`, `allowedEnvVars`), `mcp_tool`, `prompt`, `agent`(실험). 공통 `if`(권한 규칙 문법), `timeout`, `statusMessage`, `once`. [doc]
- matcher: `*`/빈 값/생략=전체. 영숫자·`_`·`-`·공백·`,`·`|`만이면 정확 일치 목록, 그 외는 JS 정규식. [doc]
- command hook 은 stdin 으로 JSON 수신. [doc]; 로컬 hook 예시(`cat | jq -r '.message'`)도 stdin JSON 사용. [local]

이벤트 (문서 기준 33종) [doc]: SessionStart, Setup, UserPromptSubmit, UserPromptExpansion, PreToolUse, PermissionRequest, PermissionDenied, PostToolUse, PostToolUseFailure, PostToolBatch, Notification, MessageDisplay, SubagentStart, SubagentStop, TaskCreated, TaskCompleted, Stop, StopFailure, TeammateIdle, InstructionsLoaded, ConfigChange, CwdChanged, DirectoryAdded, FileChanged, WorktreeCreate, WorktreeRemove, PreCompact, PostCompact, PreModelSwitch, PostModelSwitch, Elicitation, ElicitationResult, SessionEnd. (2.1.305에서의 실제 발화 여부는 [미확인])

공통 입력 필드 [doc]: `session_id, prompt_id, transcript_path, cwd, scratchpad_dir, permission_mode, effort{level}, hook_event_name`, 서브에이전트 내부에서는 `agent_id, agent_type`.

| 이벤트 | 추가 필드 | 근거 |
|--------|-----------|------|
| PreToolUse | `tool_name, tool_input, tool_use_id` | [doc] |
| PostToolUse | `tool_name, tool_input, tool_response, tool_use_id, duration_ms(선택)`; MCP 도구는 `mcp_server{name,source}` 추가 | [doc] |
| PostToolUseFailure | `tool_name, tool_input, tool_use_id, error, is_interrupt, duration_ms` (사전 거부/권한 거부 시 미발화) | [doc] |
| Stop | `stop_hook_active, last_assistant_message, background_tasks, session_crons` | [doc] |
| SubagentStop | 위 + `agent_id, agent_type, agent_transcript_path` | [doc] |
| UserPromptExpansion | matcher=명령/스킬 이름(`command_name`) | [doc] |
| SessionStart/SessionEnd, Notification(`message`) 등 | SessionStart/End 필드는 [미확인]. Notification 은 `.message` 사용이 [local] 로 관찰 | [local] |

- 모델/토큰: 확인한 Stop/Pre/PostToolUse 페이로드에는 모델·토큰 필드 없음(Agent 도구의 PostToolUse `tool_response` 에만 `resolvedModel, modelsUsed, totalTokens, usage`). [doc]. `PreModelSwitch/PostModelSwitch` 페이로드는 [미확인].
- 헤드리스: `--output-format stream-json --include-hook-events` 로 hook 생명주기 이벤트를 스트림에 포함 가능. [help]
- Forge가 쓸 hook 설정은 `settings.json`에 병합하되 Forge 마커로 구분 필요(사용자 hook 보존).

## 4. Transcript / 세션 로그 (R2)

| 항목 | 내용 | 근거 |
|------|------|------|
| 위치 | `~/.claude/projects/<cwd를 '-'로 치환한 디렉터리>/<session-uuid>.jsonl` (예: `-Users-heonjinjeong-IdeaProjects-toyproject-forge/`). hook 입력의 `transcript_path` 로도 제공 | [local], [doc] |
| 서브에이전트 | `<session-uuid>/subagents/agent-<id>.jsonl` + `.meta.json`; hook 의 `agent_transcript_path` | [local], [doc] |
| 형식 | JSONL, 1줄=1레코드. `type` 값 관찰: `user, assistant, attachment, system, ai-title, last-prompt, mode, permission-mode, file-history-snapshot, file-history-delta, queue-operation, cost-state, pr-link, bridge-session` 등 | [local] (한 세션 파일 집계) |
| 공통 필드 | `uuid, parentUuid, sessionId, timestamp, cwd, gitBranch, version, entrypoint, isSidechain, userType` | [local] |
| 모델 | `assistant` 레코드의 `message.model` (예: `claude-sonnet-5`) | [local] |
| 토큰 | `assistant` 레코드의 `message.usage`: `input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens, cache_creation, service_tier, speed, iterations, server_tool_use, output_tokens_details` | [local] |
| 비용 | `cost-state` 레코드 존재(내부 필드는 [미확인]) | [local] |
| 도구 호출 | `assistant.message.content[]` 의 `{type:"tool_use", name, input, id}` + 이어지는 `user` 레코드의 `tool_result` 및 최상위 `toolUseResult` | [local] |
| 주의 | 스트리밍 중 같은 응답이 여러 레코드로 나뉘어 usage 가 중복 집계될 수 있는지는 [미확인] (집계 시 `requestId`/`message.id` 기준 중복 제거 필요 여부 검증 필요) | [미확인] |

결론(R2): Claude Code 는 **모델·토큰(입력/출력/캐시)을 transcript 에서 응답 단위로 수집 가능**. hook payload 자체에는 모델·토큰이 없으므로 hook 으로는 `transcript_path`/`session_id` 를 받아 transcript 를 후처리하는 구성이 필요.

## 5. 스킬 호출 식별 (R3)

관찰 근거 [local] (실제 transcript):
- 모델 호출: `assistant` 의 `tool_use` `{"name":"Skill","input":{"skill":"code-review","args":"medium"}}`. 결과 `tool_result` 텍스트 `Launching skill: <name>`, 최상위 `toolUseResult` 는 `{"success":true,"commandName":"<name>"}` 또는 fork 실행 시 `{"success":true,"commandName","status":"forked","background":true,"agentId",...}`.
- 사용자 `/명령` 입력: `user` 레코드 content 에 `<command-name>/xxx</command-name><command-message>..</command-message><command-args>..</command-args>` 태그 (관찰 사례는 `/model`; 스킬 슬래시 호출이 같은 형태인지는 [미확인]).
- hook 경로 [doc]: `PreToolUse`/`PostToolUse` 를 `Skill` 도구에 매칭하면 모델 호출만 잡힌다(`tool_input.skill` 필드명은 transcript 로 확인했으나 hook 문서에는 명시 없음 -> hook 에서의 실제 payload [미확인]). `/skillname` 직접 입력은 PreToolUse 를 우회하므로 `UserPromptExpansion`(matcher=스킬/명령 이름)으로 잡아야 한다.

결론(R3): **식별 가능.** (a) transcript 의 `tool_use name="Skill"`(+`input.skill`) 파싱, (b) hook `PreToolUse|PostToolUse` matcher `Skill` + `UserPromptExpansion`. 모델이 SKILL.md 를 Read 로 직접 읽는 우회 경로는 별도이며 이 경로는 식별 불가/불확실. "스킬 사용"의 1차 신호는 `Skill` 도구 호출로 정의 가능.

## Forge 어댑터 설계 시사점

1. 스킬 sync: `~/.claude/skills/<name>/` 또는 `.claude/skills/<name>/` 에 디렉터리 렌더링. 변경은 재시작 없이 반영되므로 `forge sync` 후 별도 reload 불필요(새 최상위 디렉터리 최초 생성 시만 `/reload-skills`). frontmatter 는 `name`+`description` 만 보장하고, Forge 고유 메타(버전·해시)는 SKILL.md 밖(별도 manifest)에 두는 편이 안전(알 수 없는 필드는 무시되나 외부 업로드 시 오류).
2. MCP sync: 프로젝트는 `.mcp.json`(`mcpServers`)에 Forge 마커 항목만 병합. 항상 `type` 명시. 비밀값은 `${ENV}` 참조로만 기록(PLAN §3.4 부합). 사용자 전역은 `~/.claude.json` 직접 편집 대신 `claude mcp add/remove -s user` 사용 권장(파일에 비밀값·상태가 섞여 있고 구조 [미확인]). `.mcp.json` 서버는 사용자 승인이 필요함을 `forge doctor` 가 안내해야 함.
3. 이력 수집: hook(`PostToolUse`/`PostToolUseFailure`, `Skill` 및 `mcp__*` 도구, `UserPromptExpansion`)으로 호출 원장(ts, tool, session_id, ok, duration_ms)을 적재. 모델·토큰은 hook 에 없으므로 `transcript_path` 를 읽어 `message.model`/`usage` 를 후처리. 페이로드(`tool_input`/`tool_response`)는 기본 폐기(opt-in).
4. hook 설치: `settings.json` 에 Forge 마커 command 를 병합하고 제거 시 마커로만 삭제. 기존 사용자 hook(예: 전역 Notification) 보존. hook 은 `command` 타입으로 `forge hook ingest` 같은 stdin JSON 소비자를 호출하는 형태가 적합.
5. 게이트웨이 `skill` 메타 도구 경로: MCP 도구 호출은 PostToolUse 에 `mcp_server` 와 함께 잡히므로 hook 과 게이트웨이 원장을 `session_id`+`tool_use_id` 로 교차 검증 가능.
6. R1 대응: 이벤트 33종·필드는 문서 기준이므로 버전별 픽스처(실제 stdin 샘플)를 M4 전에 캡처해 테스트로 고정한다. SessionStart/End 페이로드, Skill 의 hook payload, transcript usage 중복 여부가 후속 확인 대상.
