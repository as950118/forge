# 빠른 시작: OpenCode

[공통 빠른 시작](quickstart.md)의 1~7단계를 끝낸 **같은 셸**에서 이어 갑니다(`FORGE_REPO`, `FORGE_HOME`, `SRC`, `PRACTICE` 환경 변수를 씁니다). Claude Code 도 같이 쓴다면 [Claude Code 문서](quickstart-claude-code.md)를 먼저 하세요(권장 순서: claude-code 먼저). 이 문서의 코드 블록은 `make quickstart-check` 가 격리된 임시 환경에서 **가짜 `opencode`**(`--version` 만 응답)로 실제 실행합니다.

> **미검증**: Forge 는 OpenCode 바이너리를 **한 번도 실행하지 않고**(`--version` 제외) 공식 문서와 설치본 번들 텍스트(`strings`)·`--help` 출력만 근거로 어댑터를 만들었습니다([`docs/clients/opencode.md`](../clients/opencode.md)). 생성한 `opencode.json`·스킬을 **실제 OpenCode 가 로드하는지는 검증하지 못했습니다.** 확인 근거가 있는 버전은 OpenCode 2.0.24 입니다.

## 무엇이 어디에 쓰이나

`--scope project`(기본)는 프로젝트 저장소 루트 아래에만 씁니다.

| 항목 | 위치 |
|------|------|
| 스킬 | `<프로젝트>/.opencode/skills/<이름>/` |
| MCP 서버 | `<프로젝트>/opencode.json` 의 최상위 `mcp` (게이트웨이 `forge` + 노출이 `direct` 인 서버) |
| hook | 없음(OpenCode 의 hook·플러그인 수집은 지원하지 않습니다) |
| Forge 상태 | `<프로젝트>/.forge/sync-state.opencode.json` |

- **스킬 중복 방지**: OpenCode 는 `.opencode/skills` 뿐 아니라 `.claude/skills` 도 읽습니다. 같은 스킬이 두 곳에 있을 때의 동작이 미확인이라, claude-code 타깃이 이미 관리하는 스킬은 opencode 쪽에서 건너뜁니다. 그래서 claude-code 를 먼저 적용했다면 아래 결과에 `.opencode/skills` 가 생기지 않는 것이 정상입니다.
- 이력 수집은 hook 이 없으므로 게이트웨이(`forge mcp serve`) 경로로만 쌓입니다.

## 1. 계획 → diff → 적용

```bash
forge sync --target opencode --project-dir "$PRACTICE" --bin "$FORGE_EXE"
forge sync --target opencode --dry-run --diff --project-dir "$PRACTICE" --bin "$FORGE_EXE"
forge sync --target opencode --project-dir "$PRACTICE" --bin "$FORGE_EXE" --apply
cat "$PRACTICE/opencode.json"
```

두 클라이언트를 모두 적용했다면(Claude Code 문서를 먼저 했다면) `forge doctor` 로 점검합니다. 미적용 변경과 게이트웨이 항목 경고가 사라지고 경고가 없어야 합니다(`--strict` 는 경고가 있으면 종료 코드 1).

```bash
forge doctor --project-dir "$PRACTICE" --strict
```

`opencode.json` 의 `mcp` 는 다음 형태로 렌더링됩니다: stdio 서버는 `{"type":"local","command":[명령, 인자...],"enabled":true}`, 환경변수 참조는 `"environment":{"K":"{env:V}"}`, http/sse 는 `{"type":"remote","url":"..."}`. 다른 키(`$schema`, provider, model, permission 등)는 바이트 그대로 둡니다. `--apply` 는 쓰기 직전에 `opencode --version` 한 번만 실행해 버전을 확인하고, 쓰기 전에 백업합니다. `--target all` 은 두 클라이언트를 하나의 통합 계획으로 처리합니다.

## 2. 한계: `opencode.jsonc` 만 있는 프로젝트

Forge 는 `opencode.json` 만 편집합니다. 프로젝트에 `opencode.jsonc` 만 있고 `opencode.json` 이 없으면 **주석을 보존할 수 없어** "JSONC 는 아직 지원하지 않습니다" 로 **아무것도 쓰지 않고 중단**합니다(스킬도 쓰지 않습니다). 둘 다 있으면 `opencode.json` 만 대상으로 하고, 두 파일의 우선순위가 미확인이라는 경고를 계획에 남깁니다. `opencode.json` 이 엄격한 JSON 이 아니거나 `mcp` 가 객체가 아니어도 중단합니다.

```bash
mkdir -p "$SRC/jsonc-project"
git -C "$SRC/jsonc-project" init -q
printf '{\n  // my comment\n  "theme": "dark"\n}\n' > "$SRC/jsonc-project/opencode.jsonc"
forge sync --target opencode --project-dir "$SRC/jsonc-project" --bin "$FORGE_EXE" || echo "(예상된 중단: 종료 코드 $?)"
```

해결: 설정을 `opencode.json` 으로 옮기거나(주석이 없어야 함), 이 프로젝트는 Forge 대신 직접 관리합니다.

## 3. 적용 후 `opencode reload`

Forge 는 `opencode` 를 `--version` 외의 어떤 인자로도 실행하지 않습니다. 적용 뒤 OpenCode 에 반영하려면 **사용자가 직접** `opencode reload` 를 실행하세요(계획이 안내만 합니다). 이 문서의 검증도 실행하지 않았습니다.

<!-- qc:skip -->
```bash
opencode reload
```

## 4. user 스코프: 명령 출력만

`--scope user` 의 스킬은 `~/.config/opencode/skills`(`XDG_CONFIG_HOME` 이 절대 경로로 설정돼 있으면 `$XDG_CONFIG_HOME/opencode/skills`)에 쓰지만, **MCP 서버는 파일을 쓰지 않고** `opencode mcp add <이름> --global ...` 명령을 출력만 합니다. 전역 설정 파일(`~/.config/opencode` 아래의 설정·인증 파일)은 읽지도 쓰지도 않습니다. 계획만 보세요.

```bash
forge sync --target opencode --scope user --bin "$FORGE_EXE"
```

## 5. 게이트웨이와 `confirm=ask` 도구

`forge sync --target opencode --apply` 는 `opencode.json` 에 게이트웨이 `forge` 항목을 넣습니다. 직접 등록한다면 다음과 같습니다(`opencode.json` 의 `mcp`).

<!-- qc:skip -->
```json
{"mcp": {"forge": {"type": "local", "command": ["forge", "mcp", "serve"]}}}
```

**주의(미확인)**: 기본 `forge tool add` 의 `confirm` 은 `ask` 입니다. 게이트웨이는 클라이언트의 elicitation(사용자 확인) 지원이 광고되지 않으면 `ask` 도구의 실행을 안전하게 **거부**합니다. **OpenCode 가 elicitation 을 지원하는지는 확인하지 못했으므로**, OpenCode 에서 도구가 "사용자 확인이 필요한 도구입니다 ... 지원하지 않아 실행하지 않았습니다" 로 거부된다면 도구를 `--confirm none` 으로 다시 등록해야 할 수 있습니다(위험을 이해한 도구에만).

<!-- qc:skip -->
```bash
forge tool add greet --description '인사 도구' --schema '{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}' --confirm none --replace -- echo hello '{{name}}'
```

## 부록: 실제 OpenCode 로 연결 확인 (선택, 직접 실행, 미검증)

**이 문서도 시연도 실제 OpenCode 를 실행하지 않았고, 아래 절차는 한 번도 실행해 본 적이 없습니다.** 사용자 설정을 바꾸지 않도록 임시 프로젝트에서만 시도하되, OpenCode 세션을 열면 **모델 호출(토큰 사용)이 발생할 수 있으므로** 원할 때 직접 실행하세요. OpenCode 가 별도 설정 파일 경로 옵션을 제공하는지는 확인하지 못했습니다.

1. 위 연습 프로젝트(`$PRACTICE`)에서 `opencode.json` 이 생성됐는지 확인합니다.
2. 사용자 전역 설정과 섞이지 않도록 연습 프로젝트 디렉터리에서 `opencode mcp list` 를 실행해 `forge` 가 보이는지 확인합니다(이 명령이 사용자 전역 설정도 함께 읽는지는 미확인).
3. 실제 `clientInfo.name`(`opencode` 로 식별되는지)은 `FORGE_GATEWAY_DEBUG=1` 로 띄운 `forge mcp serve` 의 stderr 로 확인합니다. 다르면 `--client opencode` 로 지정합니다.

문제가 생기면 [문제 해결](troubleshooting.md), 동작 원리는 [`docs/sync.md`](../sync.md)(OpenCode 절), [`docs/gateway.md`](../gateway.md).
