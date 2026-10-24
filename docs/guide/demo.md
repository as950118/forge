# 안전한 로컬 시연

`forge` 의 핵심 흐름(스킬·MCP·커스텀 도구 등록 → 프로필 → 클라이언트 동기화 → 게이트웨이 → 이력 → doctor)을 **내 환경을 건드리지 않고** 처음부터 끝까지 한 번에 눈으로 확인합니다. 안전 장치(설치 미리보기와 승인 정책, 해시 고정, MCP 도구 정의 고정, 평문 비밀 거부, 악성 zip 거부, 인젝션 방어, 사용자 파일 보존, 충돌 시 쓰기 중단, 버전 게이트)가 실제로 동작하는 모습을 함께 보여 줍니다.

모든 것은 `mktemp -d` 로 만든 임시 디렉터리 하나 아래에서 일어납니다. 실제 `~/.forge`, `~/.claude`, `~/.claude.json`, `~/.config/opencode` 는 읽지도 쓰지도 않습니다(`~/.forge` 는 수정 시각을 `stat` 으로 시작과 끝에 비교만 합니다). 모델 호출·패키지 관리자·외부 네트워크도 쓰지 않습니다.

> **한계(미검증)**: 이 시연은 가짜 클라이언트 바이너리(`--version` 만 응답)와 JSON-RPC 클라이언트(`scripts/demo/gw_call.py`)로 합니다. **실제 Claude Code·OpenCode 가 Forge 가 만든 `.mcp.json`·`opencode.json`·hook 설정을 올바르게 읽는지는 이 시연이 증명하지 않습니다.**

## 사전 요구

- macOS(검증) 또는 Linux, bash 3.2 이상, git, python3(표준 라이브러리만 사용), Go(바이너리를 빌드하므로 `go.mod` 의 `go` 지시자 버전 이상), make
- 실제 `claude`/`opencode` 는 필요 없고 **실행되지도 않습니다.** 스크립트가 PATH 에서 실제 설치 위치(`~/.local/bin`, `~/.opencode/bin` 등 `claude`/`opencode` 가 든 항목)를 빼고, 맨 앞에 가짜 스크립트를 둡니다. 가짜는 `--version` 하나만 받고 호출 인자를 로그에 남깁니다.

## 실행법

저장소 루트에서 실행합니다. `make demo` 는 `dist/forge` 를 빌드한 뒤 `FORGE_BIN` 으로 넘겨 `scripts/demo/demo.sh` 를 실행합니다.

```bash
make demo
```

옵션은 환경 변수로 줍니다(스크립트를 직접 실행할 때).

```bash
DEMO_PAUSE=1 bash scripts/demo/demo.sh
KEEP=1 bash scripts/demo/demo.sh
FORGE_BIN=dist/forge bash scripts/demo/demo.sh
```

- `DEMO_PAUSE=1`: 단계마다 Enter 를 기다림(설명하며 시연할 때).
- `KEEP=1`: 끝나도 임시 디렉터리를 남겨 직접 살펴봄(경로가 출력됨). 직접 지워야 합니다.
- `FORGE_BIN=...`: 이 바이너리를 씁니다(없으면 저장소를 일반 `go build` 로 빌드합니다. `go.mod` 는 바꾸지 않습니다).

종료 코드는 모든 단계가 통과하면 0, 하나라도 실패하면 1 입니다. 약 10초 안팎이 걸립니다(비대화형). 임시 디렉터리는 종료 시(trap) 정확한 경로만 `rm -rf` 로 지웁니다(읽기 전용 스킬 스냅샷이 있어 먼저 `chmod -R u+w` 를 합니다).

구성 파일: `scripts/demo/demo.sh`(시연 본체), `scripts/demo/mock_mcp.py`(echo/add 도구가 있는 stdio MCP 서버, 도구 정의 변경 시연용 변형 파일 지원), `scripts/demo/gw_call.py`(게이트웨이를 JSON-RPC 로 호출하는 클라이언트).

## 단계별 설명

"기대 출력"의 핵심 줄은 실제 실행에서 가져왔습니다(경로는 `<DEMO>` 로 치환됨, 해시·시각·ID 는 매번 다릅니다). 시연은 12단계(0~11)이며 마지막에 `단계 12개 중 통과 12, 실패 0` 을 출력합니다.

### 0. 격리와 안전 검사
임시 루트 아래에 `home/`(HOME), `fh/`(FORGE_HOME), `proj/`(git init 한 프로젝트), `bin/fakebin/`(가짜 클라이언트)을 만들고 HOME·FORGE_HOME 이 그 아래인지 검사합니다(아니면 중단). 미끼 `~/.claude.json`(0600, 가짜 비밀)도 이때 만듭니다.
- 기대: `[OK]   가짜 claude --version`, `[OK]   가짜 claude 는 --version 외 인자에 오류 종료`
- 가치: 시연이 어떤 경우에도 실제 환경을 건드리지 않는다는 전제.

### 1. init, 버전
`forge init` 이 `<DEMO>/fh/forge.db` 를 만듭니다. 기대: `Forge 레지스트리를 초기화했습니다`.

### 2. 스킬: 등록·조회·검증, 설치 미리보기와 승인 정책, 악성 zip 거부
`forge skill validate|add|list|show` 후 다음을 차례로 보여 줍니다.

1. **악성 zip**(`../evil.txt` 엔트리)은 거부됩니다. 기대: `zip 항목 거부: 상위 경로(..)는 허용되지 않습니다: "../evil.txt"`, `[OK]   evil.txt 가 어디에도 생기지 않음`. 거부 후 임시 루트 전체에 `evil.txt` 가 없음을 `find` 로 확인합니다(zip slip 방어).
2. **설치 미리보기**: `forge skill add --preview <dir>` 는 `allowed-tools` 같은 위험 frontmatter, 실행 가능한 파일(`scripts/run.sh` 의 크기·해시·인터프리터), `SKILL.md` 의 `curl ... | sh` 위험 패턴을 보여 주되 **아무것도 설치하지 않습니다.** 기대: `트리 sha256`, `! allowed-tools: Bash`, `[실행 가능한 파일]`, `미리보기만 출력했습니다. 아무것도 설치하지 않았습니다.`, 이어지는 `skill list` 에 없음.
3. **zip 승인 정책**: 비대화형에서 `--yes` 없이 zip 을 추가하면 거부됩니다(종료 코드 1). 기대: `승인이 필요하지만 대화형 터미널이 아니고 --yes 도 지정되지 않았습니다. 아무것도 설치하지 않았습니다.`
4. **해시 고정**: `--yes --sha256 0000...` 은 `가져온 트리의 sha256 이 고정한 값과 다릅니다 ... 아무것도 설치하지 않았습니다` 로 거부됩니다.
5. 미리보기를 확인했다면 `--yes` 로 승인해 설치합니다(`추가됨: zip-skill@h-...`). `forge skill check` 는 `설치 파일이 등록된 해시와 같습니다`(저장소 변조 탐지, 읽기 전용).
- 가치: 외부 소스는 실행 전에 무엇이 들어오는지 보고, 승인하고, 해시로 고정합니다. 미리보기의 위험 패턴은 휴리스틱이라 "발견 없음" 이 안전을 뜻하지 않습니다.

### 3. MCP: 등록·refresh·도구 캐시, 평문 비밀 거부, 도구 정의 고정
`forge mcp add demo-mcp -- python3 scripts/demo/mock_mcp.py` → `refresh` → `show`.
- 기대: `OK   demo-mcp: 도구 2개 (신규 2, 변경 0, 사라짐 0)`, `show` 의 도구 표에 `add`, `echo`
- 보안 포인트: `--env API_KEY=sk-live-...` 는 `비밀로 보이는 키에는 literal 값을 저장할 수 없습니다` 로 거부되고, `--env API_KEY='${MY_KEY}'` 는 허용되며 `show` 에 `API_KEY = ${MY_KEY}  [env]` 참조만 보이고 DB 파일에도 `sk-live` 문자열이 없습니다.
- **도구 정의 고정(rug pull 탐지)**: 서버 `pin-mcp` 를 `--preview`(등록 안 함)로 보고 등록한 뒤 `refresh` 하면 `미승인(신규) 도구 2개` 가 나옵니다. 설명을 읽고 `forge mcp approve pin-mcp`(`승인됨: pin-mcp`) 합니다. 서버가 `echo` 설명에 문장을 덧붙이면(변형 파일을 `v2` 로 바꿈) 다음 `refresh` 가 `변경됨(승인된 정의와 다름): echo` 와 바뀐 부분(`설명 변경 (프롬프트 인젝션 위험: 바뀐 부분을 읽어 보세요)`)을 보여 줍니다. `forge mcp pin pin-mcp --enforce` 를 켜면 `도구 고정 켜짐: pin-mcp — 노출 1개, 숨김 1개(변경됨/미승인)` 입니다. 게이트웨이에서 숨겨지는 모습은 7단계에서 확인합니다.
- 한계: `enforce` 는 게이트웨이를 거치는 노출에만 적용되고 기본은 꺼져 있으며, 변경은 `refresh` 를 실행했을 때만 발견됩니다.

### 4. 커스텀 도구
`forge tool add greet ... --confirm none -- echo hello '{{name}}'` → `tool test`, `--dry-run`.
- 기대: `hello world`, `argv: ["echo" "hello" "world"]`
- 인젝션: `{"name":"; touch PWNED"}` → `hello ; touch PWNED` 가 인자 하나로만 전달되고 `PWNED` 파일은 생기지 않습니다. `{"name":"-n"}` 은 `값이 '-' 로 시작해 옵션으로 해석될 수 있어 거부` 됩니다.
- 위험 도구: `env`, `sh`, `awk` 는 `... command 도구로 쓸 수 없습니다` 로 등록 거부.
- 7단계용으로 `confirm=ask` 도구 `asker` 도 등록합니다.

### 5. 프로필
`profile create work` → `add`(스킬, MCP direct, 도구 greet·asker) → `use work --client all --project-dir <proj>` → `current`.
- 기대: `설정 파일은 아직 바뀌지 않았습니다`(use 는 기록만 바꿈), `claude-code  프로필 work (이 프로젝트에 활성화; 멤버 4, 허용 목록)`
- 가치: 프로필에 없는 것은 노출되지 않는 허용 목록.

### 6. 동기화 (가장 긴 단계)
사전: 프로젝트에 사용자 소유 `.mcp.json`(서버 `my-own`)과 `.claude/settings.json`(permissions, 사용자 hook)을 둡니다.

| 소단계 | 내용 | 기대 출력의 핵심 줄 |
|--------|------|-------------------|
| 6-1 | 계획만 | `모드: 계획만 출력합니다`, `[OK]   계획 전후 프로젝트 파일 해시 동일(아무것도 쓰지 않음)`, `[OK]   계획은 클라이언트 바이너리를 실행하지 않음` |
| 6-2 | `--dry-run --diff` | 파일별 통합 diff, 해시 동일 |
| 6-3 | `--apply` | `claude-code: 적용됨`, `opencode: 적용됨`, 가짜 클라이언트 로그가 각각 `argc=1 argv=--version` 한 줄 |
| 6-4 | 결과 파일 | `.claude/skills/demo-skill/`, `.mcp.json` 에 `my-own`·`forge`·`demo-mcp`, `settings.json` 에 `Bash(ls:*)`·`echo user-hook`·`hook ingest`, `opencode.json`, `.opencode`(스킬은 claude-code 가 이미 노출해 건너뛰므로 없음) |
| 6-5 | 다시 계획 | `변경 없음: 쓸 파일이 없습니다` |
| 6-6 | 버전 게이트 | 가짜 claude 가 `9.9.9` 를 출력하면 `클라이언트 버전을 확인하지 못해 아무것도 쓰지 않고 중단합니다`, `그래도 적용하려면 --allow-untested-version 을 지정하세요`, 종료 코드 1, 해시 불변. 버전을 되돌리면 적용됨 |
| 6-7 | 백업·restore | `forge sync --list-backups` 표, `--restore latest` 로 `opencode.json` 이 이전 상태로 돌아가고 한 번 더 `--restore latest` 로 원래대로 복원(`복원 직전 상태를 백업했습니다`) |
| 6-8 | 충돌 | 관리 스킬에 `# hand edit by user` 를 추가하면 `충돌  관리 디렉터리가 수정되었습니다 (해시 불일치)`, apply 는 `충돌이 있어 아무것도 쓰지 않았습니다`(종료 코드 1, 해시 불변). `--apply --force-managed` 만 백업 후 덮어씀 |

- 가치: 사용자가 쓴 항목은 절대 수정하지 않고(`my-own`, permissions, 사용자 hook 보존), 쓰기 전에 항상 백업하며, 애매하면 쓰지 않습니다.
- 보안 포인트: Forge 가 클라이언트 바이너리에 실행하는 것은 `--version` 하나뿐이고(로그로 증명), 버전이 검증되지 않았으면 쓰기 전에 멈춥니다.

### 7. 게이트웨이
`gw_call.py` 가 `forge mcp serve --client claude-code` 를 띄워(initialize 의 clientInfo 가 `claude-code`, elicitation 능력 없음) 호출합니다.
- 기대: `tools/list` 에 `forge_asker`, `forge_greet`, `forge_list`, `forge_skill`, `gw_mcp__add`, `gw_mcp__echo`, `pin_mcp__add`(MCP 서버 이름의 `-` 는 `_` 로, 도구는 `<서버>__<도구>`). 직접 노출(direct)인 `demo-mcp` 는 게이트웨이에 나오지 않습니다.
- 호출: `gw_mcp__add {a:2,b:3}` → `5`(mock 서버로 프록시), `forge_greet` → `hello world`, 인젝션 문자열은 데이터로만 출력, `forge_skill demo-skill` → 스킬 본문, 없는 스킬 → `스킬을 찾을 수 없습니다` + isError, `forge_asker` → `사용자 확인이 필요한 도구입니다 (confirm=ask). 이 클라이언트는 확인 요청(elicitation)을 지원하지 않아 실행하지 않았습니다`
- **도구 정의 고정**: 3단계에서 승인 후 설명이 바뀐 `pin_mcp__echo` 는 `tools/list` 에 없고, `forge_list` 에 `도구 정의 고정(enforce)으로 도구 1개를 숨겼습니다 (승인된 정의와 다름 1, 미승인 0): echo` 가 표시되며, 직접 호출해도 거부됩니다. 승인된 `pin_mcp__add` 는 노출됩니다.
- 프로필 전환: 같은 세션 중에 외부 프로세스로 `forge profile use minimal` 을 실행하면 다음 `tools/list` 가 `forge_list`, `forge_skill` 만 남고, 다시 `work` 로 바꾸면 `forge_greet` 가 돌아옵니다(재시작 없음, 사라진 도구의 직접 호출도 거부).
- 가치: 클라이언트에는 서버 하나(`forge`)만 등록하고 프로필로 노출을 제어합니다. 확인 불가한 클라이언트에서는 위험 도구를 실행하지 않는 fail-closed.

### 8. 이력 수집 (hook)
`forge hook snippet --client claude-code` 가 settings.json 조각을 출력합니다. Skill 도구 PostToolUse 샘플 payload(가짜 값)를 `forge hook ingest --client claude-code --event post` 로 보내면 종료 코드 0, stdout 비어 있음. 잘못된 JSON 도 마찬가지.
- 기대: `[OK]   hook ingest 는 항상 종료 코드 0`, `[OK]   stdout 은 비어 있음(클라이언트에 영향을 주지 않음)`
- 가치: 이력 수집이 클라이언트 동작을 절대 방해하지 않습니다.

### 9. 이력 조회
`history`, `history summary`, `history top`, `history unused`, `history misses`, `skill misses`, `history --format csv --output`.
- 기대: 목록에 `hook` 과 `gateway` 소스가 함께 보이고, `unused` 에 `demo-mcp`, `late-mcp`(`never`), `misses` 에 `ghost-skill`, `no-such-skill`.
- 보안 포인트: 이름이 `=cmd|calc` 인 호출은 CSV 에서 `'=cmd|calc` 로 저장되어 스프레드시트 수식 주입이 무력화됩니다. 파일 권한 0600, 이미 있는 파일은 `--force` 없이는 덮어쓰지 않습니다.

### 10. doctor
`.claude/skills/demo-skill` 삭제 + `.mcp.json` 의 `demo-mcp` command 를 없는 경로로 바꾸면(그리고 3단계에서 도구 정의가 바뀐 `pin-mcp` 가 아직 승인되지 않은 채라면):
- 기대: `[warn] sync 드리프트 claude-code/project: 충돌 1건: mcp:demo-mcp`, `[warn] 스킬 디렉터리 ...: 관리 중인 스킬 디렉터리가 없습니다`, `[warn] MCP 명령 ...: 관리 중인 MCP 항목의 실행 파일을 찾을 수 없습니다 ... (/nonexistent/...: 없음)`, 각 항목마다 `원인:` 과 정확한 `해결:` 명령(`forge sync ... --apply --force-managed` 등)
- **security 점검**: `[ok ] 스킬 저장소 무결성: 활성 스킬 ...개의 설치 파일이 등록된 해시와 같습니다`, `[warn] MCP 도구 정의 승인: 승인된 정의와 다르거나 새로 생긴 도구가 있는 MCP 서버 1개: pin-mcp (변경 1, 미승인 0)` + `해결: forge mcp show pin-mcp`, `해결: forge mcp approve pin-mcp`, `[warn] MCP 도구 고정(enforce): ...`
- `--json`: `schema=1 exit_code=0`, 경고만 있으면 종료 코드 0, `--strict` 는 1. 안내된 해결 명령(`forge sync ... --apply --force-managed`, `forge mcp approve pin-mcp`)을 실행하면 `--strict` 도 0.
- 가치/보안: doctor 는 읽기 전용입니다(실행 전후 프로젝트 해시 동일). 고치는 방법은 명령 문자열로만 안내합니다.

### 11. 안전 확인
미끼 `~/.claude.json` 의 해시·mtime·모드(0600) 불변, 임시 HOME 에 미끼 외 파일 없음(macOS 의 python3 가 남기는 `Library/Caches/com.apple.python` pyc 캐시는 제외), 실제 `~/.forge` 수정 시각 불변, 가짜 클라이언트는 끝까지 `--version` 외 인자를 받지 않음.

마지막에 `[요약]` 으로 `단계 12개 중 통과 12, 실패 0`, 소요 시간을 출력하고 임시 디렉터리를 삭제합니다.

## 직접 해 볼 변형 실험

`KEEP=1` 로 실행한 뒤 출력된 경로(`<루트>`)에서 같은 환경으로 들어갑니다(이 환경 변수는 그 셸에서만 유효합니다).

<!-- qc:skip -->
```bash
export HOME=<루트>/home FORGE_HOME=<루트>/fh PATH=<루트>/bin/fakebin:<루트>/bin:$PATH
```

1. `forge profile remove work --tool greet` 후 게이트웨이를 다시 호출해 `forge_greet` 가 사라지는지 봅니다.
2. `forge profile add work --skill demo-skill --exposure hidden --client claude-code` 후 `forge sync --target all --project-dir <루트>/proj` 계획에서 스킬이 삭제 대상이 되는지 봅니다.
3. `forge tool add shout --description x --schema '{...}' --shell 'echo "$FORGE_ARG_msg"'` 로 셸 도구를 만들어 `forge tool test` 의 환경변수 전달 방식을 봅니다.
4. `.mcp.json` 의 `my-own` 을 지우고 같은 이름의 MCP 서버를 `forge mcp add my-own ...` 로 등록해 사용자 항목과의 이름 충돌 처리를 봅니다.
5. 가짜 클라이언트 버전 파일 `<루트>/bin/fakebin/.claude.version` 을 `2.1.306` 으로 바꾸면 "미검증 패치"(info)로 apply 가 계속 진행되는지 봅니다.
6. `forge doctor --scope all --json` 을 `python3 -m json.tool` 로 열어 봅니다.

## 알려진 한계

- **실제 Claude Code·OpenCode 로 로드해 본 적이 없습니다(미검증).** 가짜 클라이언트는 `claude`/`opencode` 의 실제 동작(승인 프롬프트, 설정 병합 우선순위 등)을 흉내 내지 않습니다. 실제 클라이언트로 연결해 보는 수동 절차(선택, 직접 실행, 모델 호출 가능)는 [Claude Code 문서의 부록](quickstart-claude-code.md)과 [OpenCode 문서의 부록](quickstart-opencode.md)에 있으며, 시연은 실행하지 않습니다.
- hook 샘플 payload 는 [`docs/clients/claude-code.md`](../clients/claude-code.md) 의 확인된 형태를 따른 가짜 값입니다. Skill 도구의 실제 hook payload 는 문서상 미확인입니다.
- `forge --version` 은 개발 빌드면 `dev`(또는 git 설명 문자열)로 나옵니다.
- `history misses` 에는 프로필 전환 중 호출한 등록 도구(`greet`)가 "미등록" 으로 잡힙니다(그 시점에 프로필이 도구를 노출하지 않아 not_found 로 기록되기 때문). 시연에서 관찰한 동작이며 의도 여부는 확인하지 않았습니다.
- 호스트 부하가 매우 높으면 게이트웨이 응답 대기(`gw_call.py` 의 알람 240초)나 가짜 클라이언트 `--version` 의 5초 제한에 걸려 단계가 실패할 수 있습니다. 그 경우 다시 실행하세요.
