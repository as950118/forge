# Forge 게이트웨이 (`forge mcp serve`)

Forge 가 stdio MCP 서버가 되어 한 엔드포인트로 다음을 제공한다 (PLAN §3.1, TASK-0007).

1. 등록된 커스텀 도구 (`forge tool`, 활성인 것)
2. 등록된 MCP 서버 프록시 (`forge mcp`, 활성 + 해당 클라이언트 exposure=`gateway`, 캐시된 도구; 프로필이 있으면 프로필 멤버만)
3. 메타 도구 `forge_skill`(스킬 조회), `forge_list`(노출 목록)

표준 출력은 프로토콜 전용이다. 로그는 `FORGE_GATEWAY_DEBUG=1` 일 때만 표준 오류로 나오며 인자·결과 본문은 기록하지 않는다.

## 클라이언트 연결

```bash
# Claude Code (사용자 설정을 바꾸므로 직접 실행)
claude mcp add --scope user forge -- forge mcp serve
```

```json
// OpenCode: opencode.json 의 "mcp"
{"mcp": {"forge": {"type": "local", "command": ["forge", "mcp", "serve"]}}}
```

옵션: `--client claude-code|opencode|auto`(기본 auto: initialize 의 `clientInfo.name` 으로 식별, 알려진 이름은 `claude-code`, `opencode`),
`--name <등록한 서버 이름>`(기본 `forge`; 도구 이름 길이 계산과 자기 프록시 방지에 사용).

### 실클라이언트 확인 (Claude Code 2.1.311 실측 완료, OpenCode 미검증)

`claude mcp list/get` 은 사용자 설정(`~/.claude.json`)을 읽고 등록된 모든 서버를 기동하며, 그 외 비대화형 실행(`claude -p`)은 모델을 호출한다. 그래서 자동 검증은 go-sdk 클라이언트(clientInfo 흉내)로만 했다. 수동 확인:

**실측 결과(TASK-0029, `claude -p --mcp-config <임시파일> --strict-mcp-config`, 상세 `docs/clients/claude-code.md` §6)**: Claude Code 2.1.311 이 게이트웨이에 접속해 `tools/list`·도구 호출(커스텀·프록시)이 성공했고, initialize 의 `clientInfo.name` 은 `claude-code`(version `2.1.311`)로 `IdentifyClient` 매핑이 맞았다. 원장에 `client=claude-code, client_version=2.1.311` 이 기록된다. 클라이언트는 `elicitation`(form·url)을 광고한다. 이름 길이: 전체 64자(`mcp__forge__` + 52자)의 노출 이름은 문제없이 쓰였고 64자를 넘는 이름도 이 버전·모델에서는 거부되지 않았다(하드 한도 여부 [미확인]). OpenCode 는 실행하지 않았으므로 미검증이다.

아래는 수동으로 다시 확인할 때의 절차다.

1. 임시 FORGE_HOME 으로 도구/서버/스킬을 등록한다 (`FORGE_HOME=/tmp/fh forge init` ...).
2. `/tmp/forge-mcp.json`: `{"mcpServers":{"forge":{"type":"stdio","command":"/절대경로/forge","args":["mcp","serve"],"env":{"FORGE_HOME":"/tmp/fh"}}}}`
3. `claude --mcp-config /tmp/forge-mcp.json --strict-mcp-config` 로 대화형 세션을 열고 `/mcp` 에서 forge 가 connected 이고 도구가 `mcp__forge__...` 로 보이는지 확인 (모델 호출 없이 `/mcp` 만 본다). OpenCode 는 `opencode mcp list`.
4. 실제 clientInfo.name 을 `FORGE_GATEWAY_DEBUG=1` 의 stderr 또는 `sqlite3 $FORGE_HOME/forge.db 'select client from ledger'` 로 확인한다. `claude-code`/`opencode` 가 아니면 `internal/gateway/client.go` 의 매핑을 고치거나 `--client` 를 지정한다.

## 이름 규칙 (AC-2)

| 종류 | 노출 이름 | 예 |
|------|-----------|----|
| 프록시 도구 | `<서버명의 '-' → '_'>__<도구명>` | 서버 `my-github`, 도구 `create.issue` → `my_github__create_issue` |
| 커스텀 도구 | `forge_<도구명>` | `deploy-check` → `forge_deploy-check` |
| 메타 도구 | `forge_skill`, `forge_list` | |

- 서버명 정규화는 Claude Code 가 `mcp__<server>__<tool>` 에서 쓰는 규칙(`-`→`_`)과 같다. 도구명에서는 `[A-Za-z0-9_-]` 밖의 문자(`.` 등)만 `_` 로 바꾼다.
- 클라이언트가 보는 전체 이름은 Claude Code 에서 `mcp__<게이트웨이 서버명>__<노출 이름>` 이고, Anthropic API 의 도구 이름 제약(`^[a-zA-Z0-9_-]{1,64}$`)을 예산으로 쓴다(Claude Code 문서에서 직접 확인한 수치는 아님). 서버명이 `forge` 면 노출 이름은 최대 52자.
- 초과 시 결정적 축약: 앞 `max-9` 글자 + `_` + 원래 식별자(`mcp:<서버>:<도구>`)의 sha256 앞 8자리.
- 충돌(정규화/축약 후 같은 이름, 대소문자 무시): 우선순위 메타 > 커스텀 > 프록시, 같은 순위는 식별자 사전순. 먼저 오는 것이 이름을 유지하고, 나머지는 `_<해시6>` 접미사를 받는다(필요하면 앞부분을 줄임). 입력 순서와 무관하게 항상 같은 결과이며, 변경 내역은 `forge_list` 의 "경고" 와 debug 로그에 나온다. 예: `my-srv` 와 `my_srv` 의 `x` → `my_srv__x`, `my_srv__x_1a2b3c`. 커스텀 도구 `skill` → `forge_skill_<해시6>`.

## 노출 필터링 (AC-3)

`Exposure` 인터페이스(`internal/gateway/exposure.go`)의 구현은 둘이다. `forge mcp serve` 는 `ProfileExposure`(TASK-0012)를 쓰고, 활성 프로필이 없는 클라이언트에는 기존 구현 `SettingsExposure` 로 되돌아간다(`docs/profile.md` §1 우선순위: (클라이언트, 이 게이트웨이의 프로젝트) 활성 프로필 → 클라이언트의 전역 활성 프로필 → 레거시). 프로필은 요청마다 읽으므로 `forge profile use|unuse|add|remove` 는 실행 중인 게이트웨이에 재시작 없이 다음 요청부터 반영된다.

프로필이 적용되면 **허용 목록**이다(멤버가 아닌 서버·도구·스킬은 노출하지 않음, `mcp.exposure.*` 설정은 무시):
- MCP 서버: 멤버이고 그 클라이언트의 노출이 `gateway` 인 것만 프록시(`direct` 는 클라이언트가 직접 연결, `hidden`·비멤버는 제외).
- 커스텀 도구: 멤버이고 `hidden` 이 아닌 것(커스텀 도구는 게이트웨이로만 나간다).
- 스킬(`forge_skill`, `forge_list`): 멤버이고 `hidden` 이 아닌 것(`direct`·`gateway` 모두 조회 가능). 핀이 있으면 `forge_skill` 이 그 불변 버전을 제공하고 응답 머리말에 그 버전이 표시된다.
- 프로필 조회가 실패하면(DB 오류·시간 초과 3초) 항목을 숨긴다(fail closed). 클라이언트를 식별하지 못한(`""`) 연결은 레거시 규칙과 프로필이 있는 모든 클라이언트의 프로필을 모두 만족하는 항목만 본다.

프로필이 없는 클라이언트의 동작(기존):

- MCP 서버: 활성이고 `mcp.exposure.<서버>.<클라이언트>` = `gateway` 인 서버만 프록시한다. `direct`(기본, 클라이언트가 직접 연결)와 `hidden` 은 제외. 도구 캐시(`forge mcp refresh`)가 없는 서버는 노출하지 않고 `forge_list` 경고에 안내한다(시작 시 임의 서버를 기동하지 않기 위함).
- 커스텀 도구: 활성이고 `confirm != deny`. 클라이언트별 노출 저장소가 아직 없어 모든 클라이언트에 같다.
- 스킬: 활성인 것만 `forge_skill` 로 조회된다.
- 클라이언트 식별 실패(알 수 없는 clientInfo): 서버는 모든 클라이언트에서 `gateway` 일 때만(교집합), `hidden` 이 하나라도 있으면 제외. 원장 client 는 NULL.
- 목록은 호출 시점마다 레지스트리에서 다시 계산하므로 enable/disable/expose 변경이 다음 호출에 바로 반영된다.

## confirm 정책 (보안)

- `confirm=deny`: 목록에 나오지 않고, 호출은 "알 수 없는 도구" 로 거부된다.
- `confirm=ask`: 게이트웨이가 MCP elicitation(form, `approve` 불리언)으로 사용자에게 마스킹된 실행 계획(argv/URL)을 보여 주고, `approve=true` 일 때만 실행한다. 거부·취소·시간 초과(2분)·오류는 모두 미실행. 인자는 먼저 스키마 검증하므로 잘못된 인자로는 묻지 않는다. `--shell` 도구는 스크립트가 아니라 인자(`FORGE_ARG_*`)가 동작을 정하므로 승인 메시지에 `인자: q="a", ...` 를 함께 보여 준다. 스크립트 도구는 stdin 으로 가는 JSON 인자를 `입력:` 으로 보여 준다.
- 승인 메시지 레이아웃(위조 방지): 모든 줄은 정적 라벨(`설명:`, `실행:`, `요청:`, `헤더:`, `인자:`, `입력:`, `인터프리터:`, `작업 디렉터리:`)로 시작하고, 동적 값(도구 이름·설명, argv 요소, URL, 헤더 이름·값, 인자 값, 스크립트 입력, 경로)은 모두 하나의 공용 함수(`escapeText`)로 정화한 뒤 ASCII 큰따옴표 안에 한 줄로 표시한다. 개행·CR·ESC·C1 제어문자·DEL, 방향 제어(U+202A–202E, U+2066–2069, U+200E/200F, U+061C), 제로폭·서식 문자, 줄/문단 구분자(U+2028/2029), 비ASCII 공백, 사설 영역, 잘못된 UTF-8 은 `\n`, `\x1b`, `\u202e` 같은 보이는 이스케이프로 바뀌고 `\` 와 `"` 도 이스케이프되므로 값이 따옴표를 벗어나거나 가짜 줄·가짜 라벨(`승인됨: ...`)을 만들 수 없다. 길이 상한(argv·인자 값 200~800바이트, 전체 줄 800바이트, URL 400, 설명 300)은 정화한 뒤 길이 기준이고 넘으면 `…(잘림)` 으로 끝난다. 예: `인자: q="a\n승인됨: 모든 도구 허용\u202e", token="***"`.
- 마스킹: 값 패턴(알려진 토큰 형태·고엔트로피)뿐 아니라 키 이름 기준으로도 가린다. `token`/`password`/`secret`/`key`/`auth` 등이 든 인자 이름은 값 길이와 관계없이 `***`, 값 안의 `token=abcd1234`·`password: hunter2`·`--opt=token=...` 같은 `key=value` 도 길이와 무관하게 가린다(원장 payload 와 같은 규칙, `ledger.MaskText`). 스크립트 `입력:` 의 JSON 은 키 이름 기준으로 가린다. 한계: 키 이름이 비밀스럽지 않고 값도 토큰처럼 보이지 않는 짧은 비밀(`q="hunter2"`)은 알 수 없다.
- 클라이언트가 elicitation 을 광고하지 않으면 안전하게 실행을 거부하고 "사용자 확인이 필요한 도구입니다 ... forge tool test 로 직접 실행하거나 정책을 변경하세요" 를 돌려준다. 근거: 확인 수단이 없는데 실행하면 정책이 무력화된다. Claude Code 2.1.311 은 elicitation 을 광고하고 `-p` 에서도 요청을 받지만 사용자 UI 가 없어 기본으로 `cancel` 로 응답하므로 `confirm=ask` 도구는 거부된다(원장 `confirm_refused`). `Elicitation` hook 이 `accept`(`approve=true`)를 돌려주면 실행됐다(실측, docs/clients/claude-code.md §6; 대화형 승인은 이 실행에서 시험하지 않음). OpenCode 지원은 미확인이다.
- 기본 `forge tool add` 의 confirm 은 `ask` 이므로, 비대화형/미지원 클라이언트에서는 `--confirm none` 으로 등록한 도구만 실행된다.
- TODO(TASK-0018): 코드 실행형 바이너리(risk=code-exec)를 `none` 이어도 `ask` 로 강제하는 연동 지점은 `effectiveConfirm` 이다.

## 프록시 (보안·수명주기)

- 서버별 지연 연결: 첫 호출 때 연결하고 재사용한다. 죽은 연결은 다음 호출 전에 감지해 재연결하고, 연결 실패는 최대 2회(300ms 간격) 시도하되 command 없음/환경변수 누락/인증 필요는 재시도하지 않는다. 호출 중 서버가 죽으면 그 호출은 한국어 오류로 끝나고(부작용 중복 방지로 자동 재전송 없음) 다음 호출이 재연결한다.
- `${ENV}` 참조는 연결 시점에 게이트웨이 프로세스 환경에서 풀어 자식 프로세스/HTTP 헤더에만 넘긴다. 오류·stderr·응답에 비밀 값이 있으면 `***` 로 가린다.
- 외부 유래 텍스트 정화: 프록시 서버가 돌려준 도구 설명(과 커스텀 도구·스킬의 설명)은 `tools/list` 의 description 과 `forge_list` 에 내보내기 전에 `listText` 로 정화한다. 공백·개행·CR·탭·U+2028/2029·비ASCII 공백은 공백 하나로 접어 한 줄로 만들고, 남는 제어문자·방향 제어·제로폭·서식·사설 영역 문자와 잘못된 UTF-8 은 `\u202e`, `\x1b` 같은 보이는 이스케이프로 바꾸며, 길이는 정화 후 기준으로 `tools/list` 1024바이트, `forge_list` 도구 줄 160바이트(스킬 목록 줄 160, `forge_skill` 설명 속 스킬 줄 80)로 자르고 `…` 로 끝낸다. 따옴표 안에 넣는 값이 아니므로 `"`·`\` 는 이스케이프하지 않는다. 노출 이름은 `[A-Za-z0-9_-]` 로 정규화된 것만 나간다.
- `forge mcp refresh` 캐시 마스킹: 서버가 돌려준 도구 이름·설명·입력 스키마의 모든 문자열(키 포함)과 서버 정보에서 그 서버의 해석된 비밀 값(`--env KEY=${KEY}` 로 선언해 해석된 값과 그 base64·URL 이스케이프 형태, 긴 값 먼저; 한계는 `docs/security.md` "MCP 등록 후속")을 `***` 로 치환한 뒤에만 캐시 DB(`mcp_tools`)에 저장하므로 `forge mcp show --json` 에도 비밀이 나오지 않는다. 변경 감지 해시는 마스킹 후 값으로 계산하므로 비밀 값을 바꿔도 도구 변경으로 보고되지 않는다(반대로 서버가 설명에 비밀이 아닌 값을 바꿔 에코하면 변경으로 보인다). 리터럴로 선언한 env 값은 비밀로 취급하지 않아 마스킹하지 않는다.
- 자식 환경은 부모 환경 전체가 아니라 허용 목록(`mcp.MinimalEnv`: PATH, HOME, USER, LOGNAME, SHELL, LANG, LC_*, XDG_*, TMPDIR, TZ, TERM, 프록시/CA 설정(HTTP(S)_PROXY, NO_PROXY, SSL_CERT_*, NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE), 버전 관리자 경로(NVM_DIR, VOLTA_HOME, PYENV_ROOT, ASDF_DIR, ASDF_DATA_DIR), 패키지 캐시/레지스트리 위치(npm_config_cache/registry, UV_CACHE_DIR), Windows 기본 변수)와 서버에 등록한 env 값, `FORGE_GATEWAY=1` 뿐이다. 이 규칙은 `forge mcp refresh`(탐색)와 게이트웨이 프록시에 똑같이 적용되므로 refresh 에서 되는 서버가 게이트웨이에서만 실패하지 않는다. 허용 목록 변수라도 값에 `://userinfo@`(자격 증명이 든 URL, 예 `HTTPS_PROXY=http://user:pw@host:8080`, `npm_config_registry=https://tok@registry/`)가 있으면 그 변수는 값 전체를 자식에게 전달하지 않는다(자격 증명만 지우면 변조된 URL 이 의도치 않게 동작할 수 있어 일부만 고치지 않는다). 필요하면 서버에 `--env HTTPS_PROXY='${HTTPS_PROXY}'` 로 명시 선언한다(선언한 값은 전달되고 출력에서는 `***` 로 가려진다). 제외된 변수 이름(값 제외)은 `FORGE_GATEWAY_DEBUG=1` 의 stderr 에 나오고, `forge mcp refresh` 실패 안내(process_exit/connection)에도 같은 설명이 붙는다. 의도적으로 제외: NODE_OPTIONS(코드 주입), NPM_TOKEN·`npm_config_*authToken`·UV_INDEX_URL/PIP_INDEX_URL(자격 증명 가능), PYTHONPATH, 그 밖의 모든 변수. 서버가 필요한 변수(API 키 등)는 `forge mcp add --env KEY='${KEY}'` 로 선언해야 하며, 선언하지 않아 서버가 종료하면 refresh 오류에 "서버에 필요한 환경변수는 --env KEY=${KEY} 로 선언하세요" 안내가 붙는다. `FORGE_GATEWAY=1` 이 설정된 곳에서 `forge mcp serve` 는 실행을 거부한다.
- `forge mcp serve` 는 저장소(`$FORGE_HOME` 또는 `~/.forge` 의 forge.db)가 없으면 홈을 만들거나 마이그레이션하지 않고 "forge init 을 먼저 실행하세요" 를 stderr 에 출력한 뒤 종료 코드 1 로 끝난다. forge.db 가 있으면 디렉터리 생성·마이그레이션보다 먼저 읽기 전용으로 열어 확인 쿼리 한 번을 실행하고, 실패하면(손상·읽기 전용 저장소 등) `forge.db 를 열 수 없습니다(손상?): <원인>. 백업이 있으면 복원하거나 forge doctor 를 확인하세요` 를 출력하고 종료 코드 1 로 끝난다. 이때 `skills/`, `tmp/` 등 아무것도 만들지 않는다. 원장 기록과 WAL 때문에 쓰기 가능한 저장소가 필요하므로, 읽기 전용 디렉터리/파일에서는 같은 메시지로 시작하지 않는다. 게이트웨이는 클라이언트가 사람이 구성한 설정으로 띄우는 프로세스라 상태를 몰래 만들지 않는 편이 안전하다. 프록시 자식은 FORGE_HOME 을 받지 않지만 `FORGE_GATEWAY=1` 때문에 자식이 `forge mcp serve` 를 실행해도 홈 접근 전에 거부된다.
- 자기 프록시 방지: 서버명이 게이트웨이 서버명(또는 `forge`)이거나 command 가 `forge mcp serve`(`sh -c` 래퍼 포함) 이면 제외하고 경고한다.
- 한도: 호출당 60초(서버 `timeout_ms` 가 있으면 그 값; 초과 시 연결을 끊음), 응답 256KiB(텍스트 절단, 큰 이미지 생략, 안내 문구 추가), 동시 8건, 대상 서버 스키마로 최소 검증 후 전달.
- 종료: stdin 이 닫히거나 SIGINT/SIGTERM 을 받으면 모든 자식에게 stdin EOF → 1초 후 프로세스 그룹 SIGTERM → 1초 후 SIGKILL. 게이트웨이가 SIGKILL 로 죽으면 자식은 stdin EOF 로 스스로 끝나는 것에 의존한다(보장 아님).
- 한계(코드로 막을 수 없음): 사용자가 래퍼 스크립트로 `unset FORGE_GATEWAY; exec forge mcp serve` 를 실행하면 재귀 방지(`FORGE_GATEWAY=1`)를 우회할 수 있다(사람이 명시적으로 만든 구성이며 자기 프록시 방지의 command 검사는 `sh -c` 래퍼까지만 본다). 게이트웨이가 SIGKILL 로 죽으면 종료 절차가 돌지 못해, stdin EOF 로 끝나지 않는 서버나 그 서버가 띄운 손자 프로세스는 고아로 남을 수 있다.
- HTTP/SSE 서버의 응답 본문은 수신 후에 절단한다(수신 중 메모리 상한은 없음, 알려진 한계).

### MCP 도구 정의 고정 (`forge mcp pin --enforce`, TASK-0013)

서버별 설정(`settings` 키 `mcp.enforce.<서버>`, 기본 꺼짐). 켜면 `build` 가 그 서버의 캐시된 도구 중 **승인된 정의와 해시가 같은 것만** 노출하고(`forge mcp approve` 로 기록, `mcp_tool_approvals`), 변경됐거나 미승인인 도구는 `tools/list` 에서 빠지며 호출해도 알 수 없는 도구로 거부된다. `forge_list` 의 경고에 "도구 정의 고정(enforce)으로 도구 N개를 숨겼습니다 (승인된 정의와 다름 a, 미승인 b): <이름 최대 5개>. ... `forge mcp approve <서버>`" 가 나온다(이름은 정화, 숨겨진 도구의 설명은 싣지 않음). 승인 기록이 없으면 모든 도구가 숨겨진다(안전하게 닫힘). 적용 범위는 게이트웨이를 거치는(`gateway`) 노출뿐이며, 게이트웨이는 호출 때 업스트림의 `tools/list` 를 다시 비교하지 않고 마지막 `forge mcp refresh` 의 캐시 기준으로 판정한다. 테스트: `TestEnforceHidesChangedAndUnapprovedTools`, `TestEnforceIsPerServer`, `TestEnforceNoteEscapesToolNames`.

## 스킬 조회 `forge_skill`

입력 `{name, file?}`. 이름이 없거나 인자가 스키마에 맞지 않는 호출(추가 속성 포함)은 항상 `name=forge_skill, ok=0, error_class=invalid_args` 로 기록하고 요청한 스킬 이름은 남기지 않는다(커스텀 도구·프록시의 인자 오류와 확인 단계의 인자 오류도 각자 이름으로 `invalid_args`, JSON 객체가 아닌 arguments 도 `invalid_args`). 활성 스킬의 현재 버전 스냅샷에서 `SKILL.md`(또는 `file`)와 파일 목록(최대 200개)을 반환한다. 반환 전에 스냅샷을 다시 해시해 등록된 `content_hash` 와 비교하고(`skill.VerifyStore`), 다르면(변조·손상 의심) 아무것도 제공하지 않고 `forge skill check` 를 안내한다(TASK-0013, `TestForgeSkillRefusesTamperedSnapshot`). 128KiB 상한, 절대 경로·`..`·심볼릭 링크 탈출 거부, 텍스트 파일만. 없는 스킬은 `found=0` miss, 비활성은 `found=0, error_class=disabled` 로 기록하고 한국어 오류를 돌려준다.

## 원장 (AC-4)

모든 도구/프록시/스킬 호출이 `source=gateway` 로 기록된다: 커스텀 도구=`tool`, 프록시=`mcp(server, 도구)`, 스킬=`skill` + `invocation=model`. client 는 clientInfo(또는 `--client`), session_id 는 게이트웨이 프로세스마다 생성한 `gw-<hex>`. 노출되지 않은 이름의 호출은 다음처럼 기록한다. (1) 등록된 커스텀 도구 이름(`forge_<도구>`, 비활성·deny 포함)과 맞으면 그 도구로 `kind=tool, found=0, error_class=not_found` miss 를 남긴다. (2) 등록된 서버의 정규화 접두사(`<서버>__<도구>`)와 맞으면 `kind=mcp`, `mcp_server` = 등록된 서버 이름, name = 도구 부분으로 같은 miss 를 남긴다. (3) 등록된 것과 전혀 맞지 않는 `forge_*` 이름과 `<x>__<y>` 모양 이름(`<x>` 가 등록된 서버가 아닌 경우 포함)은 `mcp_server` 없이 정화된 전체 이름을 `kind=tool, found=0, error_class=not_found` 로, 세션당 이름별 1행만, 세션당 최대 100행까지 남긴다(`mcp__x` 가 `mcp_server=mcp` 가 되지 않는다). 100행 상한 슬롯은 실제로 기록에 성공한 행만 차지하므로 기록이 실패한 이름은 슬롯을 쓰지 않고 다음 호출 때 다시 시도되며, 상한을 넘은 이름이나 중복은 기록하지 않고 debug 로그만 남긴다. 그 밖의 모양의 이름은 기록하지 않는다. 기록되는 이름은 `cleanName` 이 정확히 한 번 정화한다: 승인 메시지와 같은 `escapeText` 규칙(방향 제어·ESC 등은 `\u202e` 같은 이스케이프)으로 이스케이프한 뒤 최대 120바이트로 자르고(이스케이프 경계·룬 경계에서 자름) 잘렸으면 `…` 로 끝낸다. 정화된 이름은 `safeName` 타입이라 기록 경로에서 다시 정화되지 않으므로 이중 이스케이프가 없고 원장 한도(128바이트)를 넘지 않는다. 기록 실패·패닉은 결과를 바꾸지 않는다(debug 로그만). payload 는 `settings.record_payload=1` 일 때만 ledger 의 마스킹 규칙으로 저장된다. 게이트웨이 자신은 `tool_use_id` 를 기록하지 않지만, Claude Code 2.1.311 은 `tools/call` 의 `params._meta["claudecode/toolUseId"]` 로 hook 의 `tool_use_id` 와 같은 값을 보낸다(실측). 지금은 `ledger/link.go` 의 시간 창 기반 연결이 hook 행과 게이트웨이 행을 합치며(실측으로 확인; `_meta["claudecode/toolUseId"]` 전달은 단일 실행 관찰이며 검증자 재현 못함, 병합 일치는 간접 근거), `_meta` 값을 직접 쓰면 더 정확해질 수 있다(후속 후보, 미구현).
