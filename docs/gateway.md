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
- 마스킹: 값 패턴(알려진 토큰 형태·고엔트로피)뿐 아니라 키 이름 기준으로도 가린다. `token`/`password`/`secret`/`key`/`auth` 등이 든 인자 이름은 값 길이와 관계없이 `***`, 값 안의 `token=abcd1234`·`password: hunter2`·`--opt=token=...` 같은 `key=value` 도 길이와 무관하게 가린다(원장 payload 와 같은 규칙, `ledger.MaskText`). 스크립트 `입력:` 의 JSON 은 키 이름 기준으로 가린다. 한계: 키 이름이 비밀스럽지 않고 값도 토큰처럼 보이지 않는 짧은 비밀(`q="hunter2"`)은 알 수 없다. **TASK-0020 보강(ledger 규칙 버전 5)**: 값 안에 이스케이프된 따옴표가 있는 `api_key="a\"b"`, 문자열 안에 든 JSON 텍스트 `{\"api_key\":\"x\"}`·`export API_KEY=\"x\"`, 따옴표가 붙은 키 `"password": "x y"` 도 가리고, 라벨을 전각 문자(`ｔoken=...` 등), 키릴/그리스 비슷한 문자, 제로폭 문자, 결합 기호로 쓴 경우도 가린다(접은 텍스트에서 값 구간을 찾아 원문의 같은 구간을 `***` 로 바꾸므로 나머지 글자는 그대로다). 남는 한계: confusables 표(키릴·그리스 일부)에 없는 비슷한 글자, 따옴표를 쪼갠 값(`'it''s'`).
- 클라이언트가 elicitation 을 광고하지 않으면 안전하게 실행을 거부하고 "사용자 확인이 필요한 도구입니다 ... forge tool test 로 직접 실행하거나 정책을 변경하세요" 를 돌려준다. 근거: 확인 수단이 없는데 실행하면 정책이 무력화된다. Claude Code 2.1.311 은 elicitation 을 광고하고 `-p` 에서도 요청을 받지만 사용자 UI 가 없어 기본으로 `cancel` 로 응답하므로 `confirm=ask` 도구는 거부된다(원장 `confirm_refused`). `Elicitation` hook 이 `accept`(`approve=true`)를 돌려주면 실행됐다(실측, docs/clients/claude-code.md §6; 대화형 승인은 이 실행에서 시험하지 않음). OpenCode 지원은 미확인이다. **기본값 결정(TASK-0020 AC-6)**: `forge tool add` 의 기본 `confirm=ask` 는 안전 우선으로 유지한다(확인할 수 없는 도구가 조용히 실행되는 쪽보다 항상 거부되는 쪽이 낫다; 코드 실행형은 ADR-F0006 에 따라 어차피 ask 강제). 대신 해결 방법을 알린다: (1) 등록 직후 `forge tool add` 가 stderr 로 안내를 출력한다(확인 요청 미지원/UI 없는 환경에서는 항상 거부됨, Claude Code 는 Elicitation hook 으로 승인, 신뢰하는 도구는 `--confirm none`, 코드 실행형은 `--yes-code-exec` 도 필요, `forge tool test` 로 직접 실행). (2) 클라이언트가 확인 요청에 `cancel` 로 답한 경우(Claude Code `-p` 의 자동 응답)의 거부 메시지가 같은 해결 방법을 담는다(`decline` 은 사람이 거부한 것이므로 짧은 문구 그대로, 원장은 둘 다 `confirm_refused`). 테스트: `TestConfirmCancelExplainsHowToApprove`, `TestToolAddExplainsConfirmAsk`.
- 기본 `forge tool add` 의 confirm 은 `ask` 이므로, 비대화형/미지원 클라이언트에서는 `--confirm none` 으로 등록한 도구만 실행된다.
- **코드 실행형 프로그램(`risk=code-exec`, TASK-0018)**: `sqlite3`, `npm`/`npx`/`pnpm`/`yarn`, `uv`/`uvx`/`pipx`, `swift`, `java`/`jshell`, `irb`, `go run`, `cargo run`, `open` 등 인자를 코드로 해석할 수 있는 프로그램(이름 기반 휴리스틱, 목록과 한계는 `docs/security.md`)을 실행하는 command 도구는 저장된 confirm 이 `none` 이어도 게이트웨이가 `ask` 로 강제한다(`effectiveConfirm`). 승인 메시지에 `위험:` 줄이 붙고 `forge_list` 설명에 "(실행 전 사용자 확인 필요)" 가 붙는다. 등록 시 `--confirm none` 을 유지하려면 `--yes-code-exec` 로 명시 확인해야 하며(`ack_code_exec` 로 기록), 확인한 도구만 무확인으로 실행된다. 이미 `none` 으로 등록된 code-exec 도구는 막히지 않고 확인을 요구받는다(재등록으로 해제). elicitation 이 없는 클라이언트에서는 거부되며 안내문이 재등록 방법을 알려 준다.

## 프록시 (보안·수명주기)

- 서버별 지연 연결: 첫 호출 때 연결하고 재사용한다. 죽은 연결은 다음 호출 전에 감지해 재연결하고, 연결 실패는 최대 2회(300ms 간격) 시도하되 command 없음/환경변수 누락/인증 필요는 재시도하지 않는다. 호출 중 서버가 죽으면 그 호출은 한국어 오류로 끝나고(부작용 중복 방지로 자동 재전송 없음) 다음 호출이 재연결한다.
- `${ENV}` 참조는 연결 시점에 게이트웨이 프로세스 환경에서 풀어 자식 프로세스/HTTP 헤더에만 넘긴다. 오류·stderr·응답에 비밀 값이 있으면 `***` 로 가린다.
- 외부 유래 텍스트 정화: 프록시 서버가 돌려준 도구 설명(과 커스텀 도구·스킬의 설명)은 `tools/list` 의 description 과 `forge_list` 에 내보내기 전에 `listText` 로 정화한다. 공백·개행·CR·탭·U+2028/2029·비ASCII 공백은 공백 하나로 접어 한 줄로 만들고, 남는 제어문자·방향 제어·제로폭·서식·사설 영역 문자와 잘못된 UTF-8 은 `\u202e`, `\x1b` 같은 보이는 이스케이프로 바꾸며, 길이는 정화 후 기준으로 `tools/list` 1024바이트, `forge_list` 도구 줄 160바이트(스킬 목록 줄 160, `forge_skill` 설명 속 스킬 줄 80)로 자르고 `…` 로 끝낸다. 따옴표 안에 넣는 값이 아니므로 `"`·`\` 는 일반적으로 이스케이프하지 않는다. **표기 규칙(TASK-0020 AC-11)**: 정화가 쓰는 이스케이프는 `\xNN`·`\uNNNN`·`\UNNNNNNNN` 뿐이며, 원문에 글자 그대로 있던 `\x1b` 처럼 그 모양으로 읽히는 `\` 와 이스케이프될 문자 바로 앞의 `\` 는 `\\` 로 두 번 쓴다. 따라서 출력의 `\x1b`·`\u202e` 는 항상 실제 숨은 문자의 이스케이프이고 글자 그대로의 것은 `\\x1b` 로 구별된다(경로·정규식의 일반 `\` 는 건드리지 않는다). `forge_list` 의 줄 자르기(`cutClean`)는 이미 정화된 설명을 다시 정화하지 않고 이스케이프 중간을 자르지 않는다. 테스트: `TestListTextEscapeNotationIsUnambiguous`, `TestCutCleanDoesNotDoubleOrSplitEscapes`. 노출 이름은 `[A-Za-z0-9_-]` 로 정규화된 것만 나간다.
- `forge mcp refresh` 캐시 마스킹: 서버가 돌려준 도구 이름·설명·입력 스키마의 모든 문자열(키 포함)과 서버 정보에서 그 서버의 해석된 비밀 값(`--env KEY=${KEY}` 로 선언해 해석된 값과 그 base64·URL 이스케이프 형태, 긴 값 먼저; 한계는 `docs/security.md` "MCP 등록 후속")을 `***` 로 치환한 뒤에만 캐시 DB(`mcp_tools`)에 저장하므로 `forge mcp show --json` 에도 비밀이 나오지 않는다. 변경 감지 해시는 마스킹 후 값으로 계산하므로 비밀 값을 바꿔도 도구 변경으로 보고되지 않는다(반대로 서버가 설명에 비밀이 아닌 값을 바꿔 에코하면 변경으로 보인다). 리터럴로 선언한 env 값은 비밀로 취급하지 않아 마스킹하지 않는다.
- 자식 환경은 부모 환경 전체가 아니라 허용 목록(`mcp.MinimalEnv`: PATH, HOME, USER, LOGNAME, SHELL, LANG, LC_*, XDG_*, TMPDIR, TZ, TERM, 프록시/CA 설정(HTTP(S)_PROXY, NO_PROXY, SSL_CERT_*, NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE), 버전 관리자 경로(NVM_DIR, VOLTA_HOME, PYENV_ROOT, ASDF_DIR, ASDF_DATA_DIR), 패키지 캐시/레지스트리 위치(npm_config_cache/registry, UV_CACHE_DIR), Windows 기본 변수)와 서버에 등록한 env 값, `FORGE_GATEWAY=1` 뿐이다. 이 규칙은 `forge mcp refresh`(탐색)와 게이트웨이 프록시에 똑같이 적용되므로 refresh 에서 되는 서버가 게이트웨이에서만 실패하지 않는다. 허용 목록 변수라도 값에 `://userinfo@`(자격 증명이 든 URL, 예 `HTTPS_PROXY=http://user:pw@host:8080`, `npm_config_registry=https://tok@registry/`)가 있으면 그 변수는 값 전체를 자식에게 전달하지 않는다(자격 증명만 지우면 변조된 URL 이 의도치 않게 동작할 수 있어 일부만 고치지 않는다). 프록시/레지스트리 변수(이름에 `proxy`·`registry` 가 든 변수; `NO_PROXY` 제외)는 스킴 없는 `user:pw@proxy:1`·`user@proxy:1` 도 자격 증명으로 보고 같은 방식으로 제외한다(TASK-0020 AC-1; 정상 `host:port` 와 경로 값은 통과, 다른 변수의 맨 `@` 는 자격 증명이 아니라고 본다). 필요하면 서버에 `--env HTTPS_PROXY='${HTTPS_PROXY}'` 로 명시 선언한다(선언한 값은 전달되고 출력에서는 `***` 로 가려진다). 제외된 변수 이름(값 제외)은 `FORGE_GATEWAY_DEBUG=1` 의 stderr 에 나오고, `forge mcp refresh` 실패 안내(process_exit/connection)에도 같은 설명이 붙는다. 의도적으로 제외: NODE_OPTIONS(코드 주입), NPM_TOKEN·`npm_config_*authToken`·UV_INDEX_URL/PIP_INDEX_URL(자격 증명 가능), PYTHONPATH, 그 밖의 모든 변수. 서버가 필요한 변수(API 키 등)는 `forge mcp add --env KEY='${KEY}'` 로 선언해야 하며, 선언하지 않아 서버가 종료하면 refresh 오류에 "서버에 필요한 환경변수는 --env KEY=${KEY} 로 선언하세요" 안내가 붙는다. `FORGE_GATEWAY=1` 이 설정된 곳에서 `forge mcp serve` 는 실행을 거부한다.
- `forge mcp serve` 는 저장소(`$FORGE_HOME` 또는 `~/.forge` 의 forge.db)가 없으면 홈을 만들거나 마이그레이션하지 않고 "forge init 을 먼저 실행하세요" 를 stderr 에 출력한 뒤 종료 코드 1 로 끝난다. forge.db 가 있으면 디렉터리 생성·마이그레이션보다 먼저 읽기 전용으로 열어 확인 쿼리 한 번을 실행하고, 실패하면(손상·읽기 전용 저장소 등) `forge.db 를 열 수 없습니다(손상?): <원인>. 백업이 있으면 복원하거나 forge doctor 를 확인하세요` 를 출력하고 종료 코드 1 로 끝난다. 이때 `skills/`, `tmp/` 등 아무것도 만들지 않는다. `forge.db` 위치에 파일이 아니라 디렉터리가 있으면 "손상?" 이나 "초기화되지 않았습니다" 대신 "forge.db 위치에 파일이 아니라 디렉터리가 있습니다 (손상이 아닙니다): <경로>. 디렉터리를 다른 이름으로 옮긴 뒤 forge init 을 실행하세요 (mv ... && forge init)" 를 모든 CLI 경로(`store.CheckDBLocation`: store.Open/OpenRaw/OpenReadOnly/CheckReadable, history, mcp serve)가 똑같이 출력한다(`forge doctor` 는 같은 상황을 `db.file` Fail 로 따로 알린다). 테스트: `TestDBIsDirectoryIsReportedDistinctly`. 원장 기록과 WAL 때문에 쓰기 가능한 저장소가 필요하므로, 읽기 전용 디렉터리/파일에서는 같은 메시지로 시작하지 않는다. 게이트웨이는 클라이언트가 사람이 구성한 설정으로 띄우는 프로세스라 상태를 몰래 만들지 않는 편이 안전하다. 프록시 자식은 FORGE_HOME 을 받지 않지만 `FORGE_GATEWAY=1` 때문에 자식이 `forge mcp serve` 를 실행해도 홈 접근 전에 거부된다.
- **원격 MCP(http/sse) 연결 시점 주소 검사(TASK-0020 AC-12)**: 게이트웨이 프록시와 `forge mcp refresh` 의 원격 연결은 커스텀 http 도구와 같은 정책(`internal/netsafe`)으로 DNS 해석 뒤 주소를 검사하고(검사한 주소로 접속; 리다이렉트 대상도 같은 다이얼러를 거침) 링크 로컬·클라우드 메타데이터·멀티캐스트·미지정 주소는 항상, 이름이 루프백/사설/CGNAT 주소로 해석되는 연결은 기본으로 차단한다. 영향 평가: 로컬 개발 서버(`http://localhost:3000/mcp`, `http://127.0.0.1:...`, 사설 IP 리터럴 URL)는 URL 에 직접 쓴 것이므로 **플래그 없이 계속 동작**한다. 이름이 사설 주소로 해석되는 사내망 서버(`mcp.corp.internal` 등)는 처음으로 차단되며(`blocked_address` 오류와 안내), 신뢰하는 경우 `forge mcp allow-private <서버>`(설정 `mcp.allow_private.<서버>`, `--revoke` 로 해제, `forge mcp show` 에 표시)로 허용한다. 허용해도 링크 로컬·메타데이터는 계속 차단된다. 환경변수 프록시(`HTTPS_PROXY` 등)가 요청에 적용되면 프록시가 대상에 접속하므로 대상 주소를 검사할 수 없다(한계, 이전 동작 유지). 함께 바뀐 것: 서버 정의의 헤더(`Authorization` 등)는 등록한 URL 과 같은 출처(스킴·호스트·포트; 기본 포트 80/443 은 생략해도 같은 출처)에만 붙고 다른 호스트·포트로의 리다이렉트나 https 에서 http 로의 다운그레이드에는 붙지 않는다. 테스트: `TestDefinitionHeadersFollowOriginSchemeHostAndPort`. 테스트: `TestRemoteTransportBlocksInternalAddressesByName`, `TestRemoteRedirectIsCheckedAndHeadersStayOnTheirHost`, `TestRefreshRemoteServerBlockedThenAllowed`, `TestRemoteLocalServerStillConnectsWithoutFlag`, `TestMCPAllowPrivateCommand`.
- 자기 프록시 방지: 서버명이 게이트웨이 서버명(또는 `forge`)이거나 command 가 `forge mcp serve`(`sh -c` 래퍼 포함) 이면 제외하고 경고한다.
- 한도: 호출당 60초(서버 `timeout_ms` 가 있으면 그 값; 초과 시 연결을 끊음), 응답 256KiB(텍스트 절단, 큰 이미지 생략, 안내 문구 추가), 동시 8건, 대상 서버 스키마로 최소 검증 후 전달.
- 종료: stdin 이 닫히거나 SIGINT/SIGTERM/SIGHUP(TASK-0020: 터미널이 사라질 때; 이전에는 Go 기본 동작으로 정리 없이 죽었다) 을 받으면 모든 자식에게 stdin EOF → 1초 후 프로세스 그룹 SIGTERM → 1초 후 SIGKILL. 게이트웨이가 SIGKILL 로 죽으면 자식은 stdin EOF 로 스스로 끝나는 것에 의존한다(보장 아님).
- 한계(코드로 막을 수 없음): 사용자가 래퍼 스크립트로 `unset FORGE_GATEWAY; exec forge mcp serve` 를 실행하면 재귀 방지(`FORGE_GATEWAY=1`)를 우회할 수 있다(사람이 명시적으로 만든 구성이며 자기 프록시 방지의 command 검사는 `sh -c` 래퍼까지만 본다). 게이트웨이가 SIGKILL 로 죽으면 종료 절차가 돌지 못해, stdin EOF 로 끝나지 않는 서버나 그 서버가 띄운 손자 프로세스는 고아로 남을 수 있다. **TASK-0020 AC-4 검토 결과**: SIGKILL·OOM kill·패닉처럼 게이트웨이가 정리 코드를 실행하지 못하는 종료에는 확실한 완화책이 없다. (a) Linux 의 `Pdeathsig` 는 부모 "스레드" 가 끝날 때 신호를 보내므로 Go 런타임이 유휴 스레드를 정리하면 멀쩡한 자식이 죽을 수 있고(`LockOSThread` 가 필요) macOS 에는 같은 기능이 없다. (b) 별도 감시 프로세스(워치독)는 그 자체가 고아가 될 수 있고 숨은 서브커맨드·프로세스를 늘린다. 둘 다 구현하지 않았다. 구현한 완화: 각 자식을 별도 프로세스 그룹으로 띄워 정상 종료 때 그룹 전체를 정리하고, SIGHUP 을 정상 종료 경로로 처리한다(`TestE2ESighupStopsGatewayAndChild`: SIGHUP 후 게이트웨이가 종료 코드 0 으로 끝나고 자식도 사라짐). 서버가 stdin EOF 를 무시하고 손자를 남기는 경우는 서버 쪽 결함으로 보고 `pgrep`/`ps` 로 사용자가 정리한다. **래퍼 우회 재평가**: `unset FORGE_GATEWAY` 로 재귀 방지를 우회하는 래퍼는 코드로 막을 수 없다고 재확인했다(환경변수는 신뢰 경계가 아니고, 막으려면 부모 프로세스 추적이 필요한데 이식성·오탐 비용이 크다). 우회해도 자기 프록시가 되어 자원 낭비가 생기는 정도이며 권한 상승은 없다(같은 사용자 권한, 사람이 만든 구성).
- HTTP/SSE 서버의 응답 본문은 수신 후에 절단한다(수신 중 메모리 상한은 없음, 알려진 한계).

### MCP 도구 정의 고정 (`forge mcp pin --enforce`, TASK-0013)

서버별 설정(`settings` 키 `mcp.enforce.<서버>`, 기본 꺼짐). 켜면 `build` 가 그 서버의 캐시된 도구 중 **승인된 정의와 해시가 같은 것만** 노출하고(`forge mcp approve` 로 기록, `mcp_tool_approvals`), 변경됐거나 미승인인 도구는 `tools/list` 에서 빠지며 호출해도 알 수 없는 도구로 거부된다. `forge_list` 의 경고에 "도구 정의 고정(enforce)으로 도구 N개를 숨겼습니다 (승인된 정의와 다름 a, 미승인 b): <이름 최대 5개>. ... `forge mcp approve <서버>`" 가 나온다(이름은 정화, 숨겨진 도구의 설명은 싣지 않음). 승인 기록이 없으면 모든 도구가 숨겨진다(안전하게 닫힘). 적용 범위는 게이트웨이를 거치는(`gateway`) 노출뿐이며, 게이트웨이는 호출 때 업스트림의 `tools/list` 를 다시 비교하지 않고 마지막 `forge mcp refresh` 의 캐시 기준으로 판정한다. 테스트: `TestEnforceHidesChangedAndUnapprovedTools`, `TestEnforceIsPerServer`, `TestEnforceNoteEscapesToolNames`.

## 스킬 조회 `forge_skill`

입력 `{name, file?}`. 이름이 없거나 인자가 스키마에 맞지 않는 호출(추가 속성 포함)은 항상 `name=forge_skill, ok=0, error_class=invalid_args` 로 기록하고 요청한 스킬 이름은 남기지 않는다(커스텀 도구·프록시의 인자 오류와 확인 단계의 인자 오류도 각자 이름으로 `invalid_args`, JSON 객체가 아닌 arguments 도 `invalid_args`). 활성 스킬의 현재 버전 스냅샷에서 `SKILL.md`(또는 `file`)와 파일 목록(최대 200개)을 반환한다. 반환 전에 스냅샷을 다시 해시해 등록된 `content_hash` 와 비교하고(`skill.VerifyStore`), 다르면(변조·손상 의심) 아무것도 제공하지 않고 `forge skill check` 를 안내한다(TASK-0013, `TestForgeSkillRefusesTamperedSnapshot`). 128KiB 상한, 절대 경로·`..`·심볼릭 링크 탈출 거부, 텍스트 파일만. 없는 스킬은 `found=0` miss, 비활성은 `found=0, error_class=disabled` 로 기록하고 한국어 오류를 돌려준다.

## 원장 (AC-4)

모든 도구/프록시/스킬 호출이 `source=gateway` 로 기록된다: 커스텀 도구=`tool`, 프록시=`mcp(server, 도구)`, 스킬=`skill` + `invocation=model`. client 는 clientInfo(또는 `--client`), session_id 는 게이트웨이 프로세스마다 생성한 `gw-<hex>`. 노출되지 않은 이름의 호출은 다음처럼 기록한다. (1) 등록된 커스텀 도구 이름(`forge_<도구>`, 비활성·deny 포함)과 맞으면 그 도구로 `kind=tool, found=0, error_class=not_found` miss 를 남긴다. (2) 등록된 서버의 정규화 접두사(`<서버>__<도구>`)와 맞으면 `kind=mcp`, `mcp_server` = 등록된 서버 이름, name = 도구 부분으로 같은 miss 를 남긴다. (3) 등록된 것과 전혀 맞지 않는 `forge_*` 이름과 `<x>__<y>` 모양 이름(`<x>` 가 등록된 서버가 아닌 경우 포함)은 `mcp_server` 없이 정화된 전체 이름을 `kind=tool, found=0, error_class=not_found` 로, 세션당 이름별 1행만, 세션당 최대 100행까지 남긴다(`mcp__x` 가 `mcp_server=mcp` 가 되지 않는다). 100행 상한 슬롯은 실제로 기록에 성공한 행만 차지하므로 기록이 실패한 이름은 슬롯을 쓰지 않고 다음 호출 때 다시 시도되며, 상한을 넘은 이름이나 중복은 기록하지 않고 debug 로그만 남긴다. 그 밖의 모양의 이름은 기록하지 않는다. 기록되는 이름은 `cleanName` 이 정확히 한 번 정화한다: 승인 메시지와 같은 `escapeText` 규칙(방향 제어·ESC 등은 `\u202e` 같은 이스케이프)으로 이스케이프한 뒤 최대 120바이트로 자르고(이스케이프 경계·룬 경계에서 자름) 잘렸으면 `…` 로 끝낸다. 정화된 이름은 `safeName` 타입이라 기록 경로에서 다시 정화되지 않으므로 이중 이스케이프가 없고 원장 한도(128바이트)를 넘지 않는다. **행 수 정책(TASK-0020 AC-3)**: (a) 등록된 것과 맞지 않는 이름과, 등록된 서버 접두사이지만 그 서버가 갖지 않은 도구 이름(`<서버>__<지어낸 이름>`)은 "지어낸 이름" 으로 보고 세션당 (서버, 이름)별 1행·세션당 최대 100행(두 종류가 한 예산을 나눔)만 남긴다. (b) 등록된 커스텀 도구나 서버가 실제로 가진 도구를 숨김·비활성 때문에 호출한 miss 는 ADR-F0002 의 miss 가 "시도 횟수" 이므로 시도마다 1행을 유지하되 세션당 1000행을 안전망 상한으로 둔다. (c) 세션을 가로지르는 보존: 게이트웨이를 반복 재기동하면 세션마다 새 예산이 생기므로, 게이트웨이 프로세스가 지어낸 이름 행을 처음 기록할 때 한 번(바빠서 실패하면 최대 3번) `ledger.SQLite.PruneGatewayMisses` 가 `source=gateway, found=0, error_class=not_found, 세션 gw-...` 이면서 (`kind=tool` 인데 등록된 도구 없음, 또는 `kind=mcp`) 인 행을 최신 2000행만 남기고 지운다. 등록된 커스텀 도구의 miss(`tool_id` 있음), `hidden_by_profile`(found=1), hook 행, 실제 세션의 행은 지우지 않는다. 한계: 비활성 서버 도구의 miss 도 `kind=mcp` 라 오래된 것부터 2000행 밖으로 밀려날 수 있다. 테스트: `TestRegisteredServerPrefixRowsAreBounded`, `TestRegisteredToolMissKeepsOneRowPerAttempt`, `TestRestartLoopIsBoundedByGlobalRetention`. **DB 점유(TASK-0020 AC-9)**: miss 행 기록은 더 이상 락을 쥔 채 쓰지 않는다(슬롯을 먼저 예약하고 쓰기 실패 시 반환) 쓰기 대기는 250ms 로 제한된다(`ledger.SQLite.Record` 는 ctx 마감이 5초 busy_timeout 보다 짧으면 전용 연결의 busy_timeout 을 남은 시간으로 낮췄다가 복구한다; database/sql 의 ctx 취소는 SQLite 의 busy 대기를 끊지 못해 이 방식을 썼다). 20건 동시 호출이 점유된 DB 에서 약 0.3초에 끝난다(이전: 건당 5초씩 직렬화되어 약 100초). 테스트: `TestMissWriteDoesNotSerializeCallsOnBusyDB`. 기록 실패·패닉은 결과를 바꾸지 않는다(debug 로그만). payload 는 `settings.record_payload=1` 일 때만 ledger 의 마스킹 규칙으로 저장된다. **tool_use_id(TASK-0030, ADR-F0004)**: 클라이언트가 identify 결과 `claude-code` 이고 `tools/call` 의 `params._meta["claudecode/toolUseId"]` 가 안전한 문자열이면 그 값을 커스텀 도구·프록시 도구 행의 `tool_use_id` 로 기록한다(값은 hook 의 `tool_use_id` 와 같다: Claude Code 2.1.311 에서 검증자가 독립 재현으로 확인(2.1.311, 2회 호출; 직접 병합과 5초 창 병합의 구분은 단위 테스트로만)). 안전 = 1~128바이트, 문자는 `[A-Za-z0-9_-]` 만(제어·bidi·zero-width·비ASCII·공백·구분자는 모두 거부). 키가 없거나 문자열이 아니거나 형식이 이상하면 오류 없이 무시하고 id 없이 기록한다. **claude-code 일 때만 쓰는 이유**: 키 이름이 Claude Code 전용이라 다른 client(opencode 등)나 미식별 client 가 같은 키를 보내도 그 값에 같은 의미(hook 의 tool_use_id)를 줄 수 없고, 그런 행은 합칠 Claude Code hook 행도 없다. 메타 도구(`forge_skill`, `forge_list`)와 미등록·숨김 이름의 miss 행은 id 를 기록하지 않는다. id 가 있으면 Start 에서 진행 중 행(`ok` NULL)을 쓰고 End 가 같은 행을 완료한다. hook 행과는 이 id 로 직접 병합되고(`ledger/link.go`: 같은 client, 대응하는 이름, 1:1, 시간 창 없음) id 가 없는 게이트웨이 행은 기존 5초 시간 창 연결이 폴백으로 남는다. id 가 있는 게이트웨이 행은 시간 창으로 연결하지 않는다(id 가 다르면 다른 호출). **initialize 전 `server/discover`**: Claude Code 는 initialize 전에 알 수 없는 메서드 `server/discover` 를 보낸다. 게이트웨이(MCP Go SDK)는 이를 JSON-RPC 오류 응답(`error.message` = `method "server/discover" is invalid during session initialization`, code 0)으로 거절하고 연결은 유지되어 이어지는 `initialize`·`tools/list` 가 정상 처리된다(테스트 `TestUnsupportedMethodBeforeInitializeKeepsConnection`; 실제 Claude Code 가 이 응답을 어떻게 취급하는지는 TASK-0029 실측에서 연결에 영향 없음으로만 관찰됨).
