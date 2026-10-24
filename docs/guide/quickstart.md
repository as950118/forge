# 빠른 시작 (공통)

Forge 를 빌드하고, 연습용 레지스트리에 스킬·MCP 서버·커스텀 도구를 등록하고, 프로필로 묶고, 클라이언트 설정으로 내려보낼 계획을 확인하고, `forge doctor` 와 이력 조회까지 해 봅니다.

- **격리**: 이 문서의 명령은 연습용 임시 디렉터리(`FORGE_HOME`, 연습 프로젝트)만 씁니다. 실제 `~/.forge`, `~/.claude`, `~/.claude.json`, `~/.config/opencode` 는 읽지도 쓰지도 않고, 클라이언트 설정 파일도 쓰지 않습니다(계획 출력만). 모델 호출도 없습니다.
- **상태**: 알파입니다. 이 문서의 모든 명령은 `make quickstart-check` 로 격리된 임시 환경에서 실제 실행해 검증합니다. **실제 Claude Code·OpenCode 가 Forge 가 만든 설정을 읽는지는 아직 검증하지 못했습니다.** 클라이언트별 이어서 하기는 [Claude Code](quickstart-claude-code.md), [OpenCode](quickstart-opencode.md).
- **소요 시간(두 수치)**: 명령 실행 자체는 수 초입니다(`make quickstart-check` 가 실제 실행해 합계를 출력합니다. 첫 빌드 시간은 Go 캐시 상태에 따라 다릅니다). 문서를 처음 읽으며 따라 하면 보통 10분 안팎입니다(읽고 붙여 넣는 시간 포함, 추정). 5분 안에 끝난다고 주장하지 않습니다.

준비물: Go(`go.mod` 의 `go` 지시자 버전 이상), git, make, python3(예제 MCP 서버 `scripts/demo/mock_mcp.py` 용, 표준 라이브러리만 사용). **처음 빌드할 때 Go 모듈 캐시가 비어 있으면 모듈 다운로드에 네트워크가 필요**하고, 이후에는 오프라인으로 빌드할 수 있습니다. macOS 에서만 검증했고 Linux·Windows 는 검증하지 않았습니다.

> 이 문서의 코드 블록은 **저장소 루트에서 같은 셸로 위에서 아래로** 실행하는 것을 전제로 합니다(환경 변수를 이어 씁니다).

## 1. 받기와 빌드

공개 저장소 URL 이 아직 없습니다(프로젝트 이름은 가칭이며 `go install` 은 지원하지 않습니다). 이 저장소의 소스 디렉터리를 이미 가지고 있다면 그 루트로 이동해 아래 빌드부터 시작하세요. 저장소가 공개되면 `git clone <저장소 URL>` 로 받으면 됩니다(`<저장소 URL>` 자리표시자를 그대로 붙여넣으면 셸 오류가 납니다).

```bash
make build
export PATH="$PWD/dist:$PATH"
export FORGE_REPO="$PWD"
export FORGE_EXE="$PWD/dist/forge"
forge --version
```

`make build` 는 `dist/forge` 를 만듭니다. 개발 빌드는 버전이 `dev` 로 나옵니다.

## 2. 연습용 환경 만들기 (`forge init`)

실제 `~/.forge` 를 건드리지 않도록 `FORGE_HOME` 을 임시 디렉터리로 지정하고, 연습용 git 프로젝트를 만듭니다. 실제로 쓸 때는 `FORGE_HOME` 줄을 빼면 `~/.forge` 가 기본값입니다.

```bash
export FORGE_HOME="${FORGE_QS_TMP:-/tmp}/forge-quickstart-home"
export SRC="${FORGE_QS_TMP:-/tmp}/forge-quickstart-src"
export PRACTICE="${FORGE_QS_TMP:-/tmp}/forge-quickstart-project"
mkdir -p "$SRC" "$PRACTICE"
git -C "$PRACTICE" init -q
forge init
```

## 3. 스킬 추가 (미리보기와 승인 정책)

스킬은 `SKILL.md`(frontmatter 의 `name`, `description`)가 있는 디렉터리입니다.

```bash
mkdir -p "$SRC/hello-skill"
cat > "$SRC/hello-skill/SKILL.md" <<'EOF'
---
name: hello-skill
description: 인사하는 방법을 알려 주는 연습용 스킬
---
# hello-skill

사용자가 인사를 요청하면 한국어로 짧게 인사합니다.
EOF
forge skill validate "$SRC/hello-skill"
forge skill add --preview "$SRC/hello-skill"
forge skill add "$SRC/hello-skill"
forge skill list
forge skill show hello-skill
forge skill check
```

- `skill add` 는 설치 전에 항상 **미리보기**(트리 sha256, frontmatter 의 위험 필드, 실행 가능한 파일, `SKILL.md` 의 위험 패턴 휴리스틱)를 stderr 로 출력합니다. `--preview` 는 미리보기만 하고 아무것도 설치하지 않습니다.
- **승인 정책**: 로컬 디렉터리는 미리보기만 보여 주고 질문 없이 진행합니다(`--strict-local` 이면 승인 필수). **git URL 과 `.zip` 은 승인이 필수**입니다: 터미널에서는 y/N 질문, 비대화형에서는 `--yes` 가 없으면 아무것도 설치하지 않고 종료 코드 1 로 거부합니다. 내용을 고정하려면 미리보기의 `트리 sha256` 을 `--sha256` 으로 지정합니다.
- 위험 패턴 점검은 단순 휴리스틱입니다. "발견 없음" 이 안전하다는 뜻이 아닙니다. 자세한 위협 모델은 [`docs/security.md`](../security.md).

## 4. MCP 서버 등록 (비밀은 `${ENV}` 참조)

예제 서버는 저장소의 `scripts/demo/mock_mcp.py`(`echo`, `add` 도구)입니다. 비밀 값은 평문으로 저장되지 않고 `${환경변수 이름}` 참조만 저장됩니다. 셸이 값을 확장하지 않도록 작은따옴표로 감쌉니다.

```bash
export QS_API_KEY=example-not-a-real-key
forge mcp add demo-mcp --env QS_API_KEY='${QS_API_KEY}' -- python3 "$FORGE_REPO/scripts/demo/mock_mcp.py"
forge mcp refresh demo-mcp
forge mcp show demo-mcp
forge mcp approve demo-mcp
```

- `mcp add` 는 등록 전에 실행될 정확한 명령줄과 점검(버전 미고정 패키지, 평문 http 등)을 stderr 로 보여 줍니다. 기본은 질문 없이 등록하며 등록만으로는 서버를 실행하지 않습니다(`--preview` 는 미리보기만, `--strict` 는 승인 필수). 서버는 `forge mcp refresh` 때 처음 실행되어 `tools/list` 가 캐시됩니다.
- `--env API_KEY=sk-...` 처럼 비밀로 보이는 값을 직접 쓰면 저장을 거부합니다(우회 옵션 없음). 서버가 필요로 하는 환경변수는 `--env KEY='${KEY}'` 로 선언해야 전달됩니다(부모 셸의 나머지 변수는 전달되지 않습니다).
- `mcp approve` 는 "지금 읽은 도구 설명과 스키마를 신뢰한다" 는 기록입니다. 이후 `refresh` 에서 정의가 바뀌면 경고하고, `forge mcp pin <이름> --enforce` 를 켜면 게이트웨이가 승인된 정의만 노출합니다. 먼저 `forge mcp show` 로 설명을 읽으세요.

## 5. 커스텀 도구 (`confirm` 정책)

커스텀 도구는 셸 없이 argv 로 실행되고, `{{name}}` 은 한 인자 안에서만 치환됩니다.

```bash
forge tool add greet --description '인사 도구' \
  --schema '{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}' \
  --confirm none -- echo hello '{{name}}'
forge tool test greet --args '{"name":"world"}'
forge tool test greet --args '{"name":"world"}' --dry-run
forge tool list
```

- `--confirm none|ask|deny` 는 실행 확인 정책입니다. **기본은 `ask`**: 게이트웨이가 클라이언트에 사용자 확인(MCP elicitation)을 요청하고 승인될 때만 실행합니다. 클라이언트가 elicitation 을 지원하지 않으면 `ask` 도구는 안전하게 실행을 거부합니다(Claude Code 는 문서상 지원, OpenCode 는 미확인). 여기서는 연습이라 `none` 으로 등록했습니다. `deny` 는 목록에 나오지 않고 호출도 거부됩니다.
- 커스텀 도구는 클라이언트 설정에 직접 렌더링되지 않고 **게이트웨이(`forge mcp serve`)로만** `forge_<이름>` 으로 노출됩니다. 게이트웨이 연결은 클라이언트별 문서를 보세요.
- 위험한 명령(`env`, `sh`, `awk` 등)은 등록이 거부되고, `-` 로 시작하는 인자 값은 옵션 주입 방지로 거부됩니다. deny 목록은 샌드박스가 아닙니다([`docs/security.md`](../security.md)).

## 6. 프로필로 묶기

프로필은 **허용 목록**입니다. 멤버로 넣은 것만 클라이언트에 노출됩니다. `profile use` 는 활성화 기록만 바꾸며 설정 파일은 쓰지 않습니다.

```bash
forge profile create work --description '연습용'
forge profile add work --skill hello-skill
forge profile add work --mcp demo-mcp --exposure direct --client all
forge profile add work --tool greet
forge profile use work --client all --project-dir "$PRACTICE"
forge profile current --project-dir "$PRACTICE"
forge profile show work
```

## 7. 클라이언트 설정으로 내릴 계획 보기 (아무것도 쓰지 않음)

`forge sync` 는 기본이 **계획만 출력**입니다. `--bin` 은 설정 파일에 기록할 forge 명령입니다(hook 과 게이트웨이를 클라이언트가 실행하므로 절대 경로를 줍니다. 계획·적용에 같은 값을 써야 다음 계획에 불필요한 변경이 나오지 않습니다). 파일은 `--apply` 를 줄 때만 쓰며, 쓰기 전에 백업합니다. 여기서는 계획과 diff 만 봅니다.

```bash
forge sync --target all --project-dir "$PRACTICE" --bin "$FORGE_EXE"
forge sync --target claude-code --dry-run --diff --project-dir "$PRACTICE" --bin "$FORGE_EXE"
```

실제 반영(`--apply`), 백업 복원, 버전 게이팅은 클라이언트별 문서에서 이어 갑니다: [Claude Code](quickstart-claude-code.md), [OpenCode](quickstart-opencode.md).

## 8. 상태 점검 (`forge doctor`)

`forge doctor` 는 읽기 전용입니다. 아무것도 고치지 않고, 고칠 방법을 정확한 명령으로 안내만 합니다. 아직 `--apply` 를 하지 않았으므로 "미적용 변경" 경고(warn)가 보이는 것이 정상입니다. 종료 코드는 `fail` 이 있을 때만 1 입니다(`--strict` 는 warn 도 1).

```bash
forge doctor --project-dir "$PRACTICE"
```

항목별 의미와 해결은 [문제 해결](troubleshooting.md).

## 9. 이력 조회

호출 이력은 클라이언트 hook 과 게이트웨이가 쌓습니다. 연습에서는 Claude Code 의 `PostToolUse` 이벤트 모양을 흉내 낸 **가짜 payload** 를 `forge hook ingest` 로 한 건 보냅니다(`hook ingest` 는 항상 종료 코드 0, 출력 없음). 이 payload 는 [`docs/clients/claude-code.md`](../clients/claude-code.md) 의 확인된 형태를 따른 예시이며, `Skill` 도구의 실제 payload 는 문서상 미확인입니다.

```bash
printf '{"session_id":"qs-1","cwd":"%s","hook_event_name":"PostToolUse","tool_name":"Skill","tool_input":{"skill":"hello-skill"},"tool_response":{"success":true},"tool_use_id":"toolu_qs_1","duration_ms":12}' "$PRACTICE" | forge hook ingest --client claude-code --event post
forge history
forge history summary
forge history unused
forge skill misses
```

이력은 기본적으로 메타데이터(시각, 종류, 이름, 클라이언트, 성공 여부, 지연)만 저장합니다. `forge history --format csv --output <새 파일>` 로 내보낼 수 있습니다.

## 10. 정리

연습이 끝나면 연습용 디렉터리를 지웁니다(스킬 스냅샷이 읽기 전용이라 먼저 쓰기 권한을 줍니다). 변수가 비어 있으면 중단되도록 `:?` 를 썼고, 와일드카드는 쓰지 않습니다.

<!-- qc:skip -->
```bash
chmod -R u+w "${FORGE_HOME:?}" "${SRC:?}" "${PRACTICE:?}"
rm -r -- "${FORGE_HOME:?}" "${SRC:?}" "${PRACTICE:?}"
```

## 다음 단계

- 클라이언트에 실제로 반영하기: [Claude Code](quickstart-claude-code.md), [OpenCode](quickstart-opencode.md)
- 안전한 전체 시연(스킬 미리보기·MCP 도구 정의 고정·게이트웨이·doctor 까지): [시연](demo.md)
- 문제가 생기면: [문제 해결](troubleshooting.md)
- 알려진 한계: 실제 클라이언트 로드 미검증, `confirm=ask` 도구는 elicitation 미지원 클라이언트에서 거부됨, Linux·Windows 미검증([README](../../README.md) 의 "알려진 한계").
