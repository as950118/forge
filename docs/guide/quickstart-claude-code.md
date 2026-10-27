# 빠른 시작: Claude Code

[공통 빠른 시작](quickstart.md)의 1~7단계를 끝낸 **같은 셸**에서 이어 갑니다(`FORGE_REPO`, `FORGE_HOME`, `SRC`, `PRACTICE` 환경 변수를 씁니다). 이 문서의 코드 블록은 `make quickstart-check` 가 격리된 임시 환경에서 **가짜 `claude`**(`--version` 만 응답)로 실제 실행합니다.

> **미검증**: Forge 가 만든 `.mcp.json`, `.claude/skills`, hook 설정을 **실제 Claude Code 가 로드하는지는 아직 검증하지 못했습니다.** 형식은 공식 문서와 `claude --help` 출력 기준이며([`docs/clients/claude-code.md`](../clients/claude-code.md)), 지원이 확인된 버전은 Claude Code 2.1.305 입니다. 이 문서는 사용자 설정(`~/.claude.json`)을 읽지도 쓰지도 않는 방법만 안내합니다.

## 무엇이 어디에 쓰이나

`--scope project`(기본)는 프로젝트 저장소 루트 아래에만 씁니다.

| 항목 | 위치 |
|------|------|
| 스킬 | `<프로젝트>/.claude/skills/<이름>/` (관리 마커 `.forge-managed` 포함) |
| MCP 서버 | `<프로젝트>/.mcp.json` 의 `mcpServers` (게이트웨이 항목 `forge` + 노출이 `direct` 인 서버) |
| 이력 hook | `<프로젝트>/.claude/settings.json` 의 `hooks` (`PreToolUse`·`PostToolUse`·`PostToolUseFailure`·`UserPromptExpansion`) |
| Forge 상태 | `<프로젝트>/.forge/sync-state.json` (Forge 가 쓴 항목의 해시 기록) |

사용자가 직접 쓴 항목(다른 MCP 서버, `permissions`, 사용자 hook)은 수정하지 않습니다. Forge 가 쓴 항목은 상태 파일에 해시로 기록되고, 내용이 사람 손으로 바뀌었으면 덮어쓰지 않고 **충돌**로 멈춥니다.

## 1. 계획 → diff → 적용

```bash
forge sync --target claude-code --project-dir "$PRACTICE" --bin "$FORGE_EXE"
forge sync --target claude-code --dry-run --diff --project-dir "$PRACTICE" --bin "$FORGE_EXE"
```

계획은 아무것도 쓰지 않고 클라이언트도 실행하지 않습니다. `--diff` 는 파일별 통합 diff 를 보여 주며 비밀 값은 `***` 로 가립니다.

적용 전에 프로젝트에 사용자가 직접 쓴 `.mcp.json` 이 있는 상황을 만들어 보고, 그대로 보존되는지 확인합니다. `--apply` 는 영향받는 모든 파일을 `$FORGE_HOME/backups/` 에 먼저 백업한 뒤 원자적으로 씁니다. 쓰기 직전에 대상 클라이언트에 `claude --version`(그 외 인자 없음)을 한 번 실행해 검증된 버전인지 확인합니다.

```bash
cat > "$PRACTICE/.mcp.json" <<'EOF'
{"mcpServers":{"my-own":{"type":"stdio","command":"echo","args":["mine"]}}}
EOF
forge sync --target claude-code --project-dir "$PRACTICE" --bin "$FORGE_EXE" --apply
cat "$PRACTICE/.mcp.json"
ls "$PRACTICE/.claude/skills"
forge sync --target claude-code --project-dir "$PRACTICE" --bin "$FORGE_EXE"
```

- `--bin` 은 설정 파일에 기록할 forge 명령입니다(기본 `forge`). hook 과 게이트웨이를 Claude Code 가 실행하므로 PATH 에 `forge` 가 없다면 절대 경로를 주세요(상대 경로는 거부됩니다). 계획·적용에 **같은 값**을 쓰세요: hook command 문자열에 이 값이 들어가므로 값을 바꾸면 이전 관리 hook 을 지우고 새로 추가하는 변경이 계획에 나옵니다.
- 마지막 명령은 다시 계획을 보는 것으로, `변경 없음` 이어야 합니다(멱등).
- `.mcp.json` 에는 `my-own`(사용자), `forge`(게이트웨이), `demo-mcp`(관리 서버)가 함께 들어 있어야 합니다.

## 2. hook 설치 스니펫

`forge sync --target claude-code --apply` 는 이력 hook 을 `.claude/settings.json` 에 handler 단위로 병합합니다(같은 matcher 의 사용자 handler 는 그대로 두고 Forge handler 만 추가). hook 을 건드리고 싶지 않으면 `--no-hooks`, 설치된 관리 hook 만 제거하려면 `--remove-hooks` 를 씁니다. 병합 전에 내용을 보고 싶거나 직접 붙여 넣으려면 스니펫만 출력합니다(어떤 파일도 수정하지 않습니다).

```bash
forge hook snippet --client claude-code --bin "$FORGE_EXE"
cat "$PRACTICE/.claude/settings.json"
```

`forge hook ingest` 는 어떤 실패에도 종료 코드 0 으로 끝나고 출력이 없어 Claude Code 의 동작을 막지 않도록 설계했습니다. Skill 도구가 hook payload 에 어떻게 나타나는지는 문서상 확인하지 못했습니다(미확인).

## 3. user 스코프: 명령 출력만

`--scope user` 의 스킬·hook 은 `~/.claude/skills`, `~/.claude/settings.json` 에 쓰지만, **MCP 서버는 파일을 쓰지 않고** 사용자가 직접 실행할 `claude mcp add --scope user ...` 명령을 출력만 합니다(`~/.claude.json` 은 읽지도 쓰지도 않습니다). 먼저 계획만 보세요(계획은 쓰지 않습니다).

```bash
forge sync --target claude-code --scope user --bin "$FORGE_EXE"
```

출력된 명령을 확인한 뒤, 원할 때 **직접** 실행하세요. Forge 는 이 명령을 실행하지 않습니다. 이 명령은 사용자 설정을 바꿉니다. user 스코프 MCP 는 상태 추적·자동 제거·되돌리기가 없습니다.

## 4. 버전 게이팅

`--apply` 는 `claude --version` 결과를 분류합니다: 정확히 검증된 버전(2.1.305)은 ok, 같은 major.minor 의 다른 patch 는 info(미검증 패치, 계속 진행), 그 밖의 버전·실행 실패·인식 불가는 warn 이며 **아무것도 쓰지 않고 중단**합니다. 알고 감수한다면 `--allow-untested-version` 을 붙입니다. 계획은 기본적으로 클라이언트를 실행하지 않으며, `--check-version` 으로 확인을 요청할 수 있습니다.

```bash
forge sync --target claude-code --project-dir "$PRACTICE" --bin "$FORGE_EXE" --check-version
```

## 5. 백업과 복원

모든 `--apply` 는 쓰기 전에 백업을 만듭니다. 복원은 백업 이후 사용자가 파일을 고쳤으면 `--force` 가 있어야 하고, 복원 직전 상태를 새 백업으로 보존합니다(`--restore latest` 를 한 번 더 하면 되돌릴 수 있습니다).

```bash
forge sync --list-backups
forge sync --restore latest --project-dir "$PRACTICE" --dry-run
forge sync --restore latest --project-dir "$PRACTICE"
cat "$PRACTICE/.mcp.json"
forge sync --target claude-code --project-dir "$PRACTICE" --bin "$FORGE_EXE" --apply
```

복원하면 `.mcp.json` 은 사용자 항목(`my-own`)만 남은 적용 전 상태로 돌아가고, 마지막 명령이 다시 적용합니다. 백업은 자동으로 지워지지 않으며 `forge sync --prune-backups` 로만 지웁니다.

## 6. 게이트웨이 연결 (`forge mcp serve`)

게이트웨이는 Forge 하나를 MCP 서버로 등록해 커스텀 도구(`forge_<이름>`), 게이트웨이로 노출한 MCP 서버의 도구(`<서버>__<도구>`), 스킬 조회(`forge_skill`, `forge_list`)를 한 엔드포인트로 제공합니다.

- 프로젝트에 `forge sync --target claude-code --apply` 를 했다면 `.mcp.json` 에 `forge` 항목(`mcp serve --client claude-code`)이 이미 들어 있습니다.
- 사용자 전체에 등록하려면 아래 명령을 **직접** 실행하세요(사용자 설정을 바꾸므로 이 문서의 검증에서도 실행하지 않습니다).

<!-- qc:skip -->
```bash
claude mcp add --scope user forge -- forge mcp serve
```

- `confirm=ask` 도구는 클라이언트가 elicitation 을 지원할 때만 실행됩니다. Claude Code 는 문서상 지원하지만 **실제로 확인하지 못했습니다**. 확인 수단이 없으면 안전하게 실행을 거부합니다.
- 게이트웨이의 실제 `clientInfo.name`(`claude-code` 로 식별되는지)도 미확인이라, 필요하면 `forge mcp serve --client claude-code` 로 지정합니다.

## 부록: 실제 Claude Code 로 연결 확인 (선택, 직접 실행, 미검증)

**이 부록의 명령은 `make quickstart-check` 도 시연도 실행하지 않았고, 한 번도 실행해 본 적이 없습니다.** 사용자 설정(`~/.claude.json`)을 건드리지 않는 방식(`--mcp-config <임시 파일> --strict-mcp-config`)이지만, 대화형 세션을 열면 **모델 호출(토큰 사용)이 발생할 수 있으므로** 원할 때 직접 실행하세요. 절차는 [`docs/gateway.md`](../gateway.md) 의 "실클라이언트 확인 절차"와 같습니다.

1. 위 연습 환경의 `FORGE_HOME` 을 그대로 쓰거나 새 임시 `FORGE_HOME` 에 도구·서버·스킬을 등록합니다.
2. 임시 설정 파일을 만듭니다(경로는 본인 환경에 맞게). 예: `/tmp/forge-mcp.json`

<!-- qc:skip -->
```json
{"mcpServers":{"forge":{"type":"stdio","command":"/절대경로/forge","args":["mcp","serve"],"env":{"FORGE_HOME":"/연습용/FORGE_HOME"}}}}
```

3. 사용자의 다른 MCP 설정을 무시하고 엽니다.

<!-- qc:skip -->
```bash
claude --mcp-config /tmp/forge-mcp.json --strict-mcp-config
```

4. 대화형 세션에서 `/mcp` 만 열어 `forge` 가 connected 이고 도구가 `mcp__forge__...` 로 보이는지 확인합니다(질문은 입력하지 않습니다). 확인 후 종료합니다.
5. 실제 `clientInfo.name` 은 `FORGE_GATEWAY_DEBUG=1` 로 띄운 stderr 로 확인합니다. `claude-code` 가 아니면 `--client claude-code` 로 지정합니다.

문제가 생기면 [문제 해결](troubleshooting.md), 동작 원리는 [`docs/sync.md`](../sync.md), [`docs/gateway.md`](../gateway.md).
