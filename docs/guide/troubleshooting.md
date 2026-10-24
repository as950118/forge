# 문제 해결

먼저 `forge doctor` 를 실행하세요. 읽기 전용이라 아무것도 고치지 않고, 각 항목마다 **원인** 과 **해결**(정확한 명령)을 출력합니다. 해결 명령은 안내일 뿐 doctor 가 실행하지 않으니 읽어 본 뒤 직접 실행합니다.

```bash
forge doctor --project-dir . --scope all
forge doctor --json
forge doctor --strict
```

종료 코드: `fail` 이 있으면 1, 아니면 0 입니다(`--strict` 는 `warn` 도 1). `--json` 은 `schema: 1` 입니다. 모든 항목의 상세 규칙은 [`docs/doctor.md`](../doctor.md).

## doctor 출력 → 원인 → 해결

해결 열의 `<...>` 는 doctor 가 출력하는 실제 값(경로, 서버 이름 등)으로 바뀝니다. 제목이 `<target>/<scope>` 인 행은 실제로는 `claude-code/project` 처럼 출력됩니다. 하드링크·스킬 디렉터리·게이트웨이 항목·MCP 명령·보안 행의 문구는 실제 출력으로 확인했고, 나머지 행은 `docs/doctor.md` 를 근거로 한 것이라 출력 제목이 조금 다를 수 있습니다.

| doctor 출력(제목, 심각도) | 원인 | 해결 |
|---------------------------|------|------|
| `Forge 홈` info: FORGE_HOME 이 아직 없음 | 초기화 전 | `forge init` |
| `Forge 홈 권한` warn | 홈 디렉터리 권한이 0700 이 아님(다른 사용자가 읽을 수 있음) | `chmod 700 <홈>` |
| `forge.db 권한` warn | DB 파일 권한이 0600 이 아님 | `chmod 600 <홈>/forge.db` |
| `DB 무결성` fail | DB 손상, Forge DB 가 아님, 또는 디렉터리 | doctor 가 안내하는 백업 DB 복원 명령. 복원할 백업이 없다면 레지스트리를 다시 만들어야 할 수 있음 |
| `마이그레이션` fail | DB 가 이 forge 바이너리보다 최신이거나 checksum 불일치 | 더 새로운 forge 로 실행(다운그레이드는 거부됨). 미적용 마이그레이션이 있으면 info 이며 `forge init` |
| `클라이언트 <id>` info | 클라이언트 바이너리를 PATH 에서 찾지 못함, 또는 같은 major.minor 의 다른 patch(미검증 패치) | 설치하지 않았다면 무시. 미검증 patch 는 계속 진행됨 |
| `클라이언트 <id>` warn: 미검증 버전 | 확인된 버전(Claude Code 2.1.305, OpenCode 2.0.24)이 아니거나 `--version` 실행/파싱 실패 | 생성한 설정이 로드되지 않을 수 있음. 알고 쓰려면 `forge sync ... --apply --allow-untested-version`. 아래 "버전 게이트 중단" |
| `sync 드리프트 <target>/<scope>` warn: 미적용 변경 | 레지스트리·프로필이 바뀌었지만 sync 하지 않음 | `forge sync --target <target> ...`(계획 확인) → `--apply` |
| `sync 드리프트` warn: 충돌 | Forge 가 관리하는 항목을 손으로 수정했거나 같은 이름의 사용자 항목이 이미 있음 | `--diff` 로 확인 후 수정을 되돌리거나 `--force-managed`(관리 항목만, 백업됨) / `--skip-conflicts`. 아래 "충돌" |
| `sync 드리프트` warn: 중단된 sync | 이전 `--apply` 가 도중에 죽음(저널이 남음) | `forge sync ... --apply` 가 이어서 완료. 아래 "중단된 sync" |
| `sync 드리프트` warn: 믿을 수 없는 저널 | 저널이 손상·변조됨(무시됨) | `forge sync --list-backups` 로 확인, 필요하면 `--restore` |
| `sync 드리프트 <target>/<scope>` warn: `점검 불가: <파일> 를 읽을 수 없습니다: 하드링크된 설정 파일은 지원하지 않습니다(...)` (원인: `계획을 세우지 못했습니다`) | 설정 파일(`.mcp.json` 등)이 다른 파일과 하드링크(`nlink > 1`)로 연결됨. 링크가 풀릴 때까지 sync 계획도 같은 이유로 실패함 | 하드링크를 없애 `nlink == 1` 로 만든 뒤(`ln` 으로 만든 다른 이름 삭제 등) 다시 실행. 아래 "하드링크·심볼릭 링크 거부". (심볼릭 링크는 계획에서 충돌로 보고됨) |
| `sync 드리프트` warn: 백업 의존성 | 프로젝트에 저널 사본은 있는데 `<FORGE_HOME>/backups/<id>` 가 없음(`FORGE_HOME` 이 바뀌었거나 백업을 지움) | doctor 가 안내하는 `--diff` / `--apply --force-managed` / `--skip-conflicts` 중 선택, 또는 저널 사본 삭제 명령 |
| `스킬 디렉터리 <target>/<scope>` warn: 관리 중인 스킬 디렉터리가 없습니다 | 관리하던 `.claude/skills/<이름>` 등이 지워짐 | `forge sync ... --apply`(재생성) |
| `MCP 명령 <target>/<scope>` warn: 관리 중인 MCP 항목의 실행 파일을 찾을 수 없습니다 | 설정 파일의 관리 MCP 항목이 가리키는 실행 파일이 없음 | 실행 파일을 설치하거나 서버를 다시 등록한 뒤 sync |
| `게이트웨이 항목 <target>/<scope>` warn: `.mcp.json`/`opencode.json` 에 게이트웨이 항목(forge)이 없습니다 | 게이트웨이로 노출하는 서버·도구가 있는데 설정에 `forge` 항목이 없음 | `forge sync ... --apply` |
| `경로·연결`: 게이트웨이 forge 경로가 다름/없음 | 설정에 적힌 forge 경로가 현재 바이너리와 다르거나 상대 경로 | `forge sync ... --apply --bin <현재 바이너리 절대 경로>` |
| `경로·연결`: tools/list 캐시 없음 | 게이트웨이 노출 서버를 `refresh` 하지 않음 | `forge mcp refresh <서버>` |
| `경로·연결`: 잔존 항목 | 비활성·삭제·프로필 제외된 관리 항목이 설정에 남음 | `forge sync ... --apply` |
| `이력`: hook 활동 없음 warn | 관리 중인 hook 이 있는데 최근 7일(`--hook-days`) hook 이벤트가 0건 | 클라이언트를 한 번 사용한 뒤 `forge history --source hook`. 계속 없으면 hook 설치 상태 확인(`forge sync --target claude-code`) |
| `이력`: 원장 크기 warn | 500만 행 또는 DB 1GiB 초과 | doctor 가 안내하는 내보내기 명령(`forge history --format csv --output ...`). 이력을 지우는 명령은 아직 없음 |
| `이력`: 미등록 이름 상한 warn | 한 게이트웨이 세션이 미등록 이름 100행(상한)을 기록 | `forge history misses` 로 확인 |
| `백업` warn | sync 백업이 20개 초과 | `forge sync --prune-backups --dry-run` 확인 후 `--prune-backups` |
| `백업 권한` warn | 다른 사용자가 읽을 수 있는 백업 항목 | doctor 가 안내하는 `chmod -R go-rwx ...` |
| `설정 파일 권한` warn | `.mcp.json`, `.claude/settings.json`, `opencode.json` 등이 그룹/다른 사용자 쓰기 가능 | `chmod go-w <파일>` |
| `프로필 규칙 <client>` info: 프로필 없음 | 활성 프로필이 없어 레거시 동작(활성 항목 전체 노출) | 원하면 `forge profile create <이름> --from-current` → `forge profile use <이름> --client <client>` |
| `프로필 <이름>` fail/warn | 손상·삭제된 멤버(fail), 빈 프로필·비활성 멤버·핀 버전 누락(warn) | doctor 가 안내하는 `forge profile` 하위 명령(`show`, `remove`, `add`) |
| `스킬 저장소 무결성` fail | 설치된 스킬 파일이 등록된 해시와 다름(변조 또는 손상 의심). sync 는 이 스킬을 렌더링하지 않고 게이트웨이 `forge_skill` 도 제공하지 않음 | `forge skill check <이름>` 으로 확인. 변조가 확인되면 `forge skill remove <이름> --yes` 후 신뢰하는 소스에서 다시 `forge skill add` |
| `MCP 도구 정의 승인` warn | 서버가 도구 설명·스키마를 바꿨거나(변경됨) 새 도구가 생김(미승인) | `forge mcp show <서버>` 로 바뀐 설명을 읽고, 신뢰하면 `forge mcp approve <서버>` |
| `MCP 도구 고정(enforce)` warn | enforce 가 켜져 있어 게이트웨이가 승인되지 않은 도구를 숨김 | 같은 해결(`show` → `approve`). 끄려면 `forge mcp pin <서버> --no-enforce` |

## 자주 겪는 문제

### 충돌: `충돌이 있어 아무것도 쓰지 않았습니다`

Forge 가 쓴 항목은 상태 파일(`<프로젝트>/.forge/sync-state*.json`)에 해시로 기록되고, 상태 파일에 없는 항목은 사용자 소유입니다. 관리 항목을 손으로 고쳤거나 같은 이름의 사용자 항목이 있으면 `충돌` 이며 `--apply` 는 **아무것도 쓰지 않습니다**(백업도 만들지 않음).

```bash
forge sync --target claude-code --dry-run --diff
forge sync --target claude-code --apply --skip-conflicts
forge sync --target claude-code --apply --force-managed
```

- 충돌 이유 표(`docs/sync.md` 의 "충돌 해소 방법")를 보세요. 사용자 항목과 이름이 겹치면 사용자 항목을 지우거나 이름을 바꿉니다(Forge 는 덮어쓰지 않습니다).
- `--skip-conflicts` 는 충돌 항목만 빼고 나머지를 적용합니다. `--force-managed` 는 "Forge 가 관리하지만 사람이 고친" 항목만 덮어쓰며 사용자 항목에는 효과가 없습니다(덮어쓰기 전 백업).
- 상태 파일을 커밋하지 않은 다른 클론에서는 이미 sync 된 항목이 사용자 소유로 보여 충돌로 보고됩니다. 충돌 항목을 지우고 다시 sync 하거나 상태 파일을 함께 커밋하세요.

### 중단된 sync (`이전 sync 가 중단되었습니다`)

`--apply` 도중 프로세스가 죽으면(SIGKILL, 정전) 다음 계획 맨 위에 `경고: 이전 sync 가 중단되었습니다 (백업 <id>, ...)` 와 파일별 진행이 나옵니다. 같은 `--apply` 를 다시 실행하면 **앞으로 완료**합니다(그 사이 고친 파일은 보존하고 충돌로 알립니다). 되돌리려면 `forge sync --restore <중단된 백업 id>` 또는 `--restore latest` 를 직접 쓰세요. 중단된 `--restore` 는 같은 `--restore <id> --force` 를 다시 실행하면 됩니다. 저널은 `$FORGE_HOME/backups` 의 것만 신뢰하므로, 백업을 지웠거나 `FORGE_HOME` 을 바꿨다면 중단 사실을 모릅니다(위 "백업 의존성" 행).

### 버전 게이트 중단: `아무것도 쓰지 않고 중단합니다`

`--apply` 는 쓰기 전에 대상 클라이언트의 `<binary> --version`(그 외 인자 없음)을 실행합니다. 확인된 버전이 아니거나 실행 실패·인식 불가이면 warn 으로 **아무것도 쓰지 않고 중단**하고 `--allow-untested-version` 을 안내합니다. 계획은 기본적으로 클라이언트를 실행하지 않으며 `--check-version` 으로 확인할 수 있습니다.

```bash
forge sync --target claude-code --check-version
forge sync --target claude-code --apply --allow-untested-version
forge doctor
```

`--allow-untested-version` 은 "생성한 설정이 올바르게 로드되지 않을 수 있음" 을 감수한다는 뜻입니다(실제 클라이언트 로드는 미검증). 안전하지 않은 위치(다른 사용자가 쓸 수 있는 디렉터리)의 바이너리, 프로젝트 안의 바이너리는 실행하지 않고 건너뜁니다.

### 승인 거부(비대화형): `승인이 필요하지만 대화형 터미널이 아니고 --yes 도 지정되지 않았습니다`

`forge skill add` 는 git URL 과 `.zip` 에 승인을 요구합니다. 터미널이 아니면(스크립트, CI, 파이프) `--yes` 가 없는 한 아무것도 설치하지 않고 종료 코드 1 입니다. 먼저 `--preview` 로 내용을 읽고, 확인했다면 같은 명령에 `--yes` 를 붙입니다.

```bash
forge skill add --preview ./some-skill.zip
forge skill add ./some-skill.zip --yes
```

내용을 고정하려면 미리보기의 `트리 sha256` 을 `--sha256` 으로 주세요. 다르면 `--yes` 가 있어도 설치하지 않습니다. `forge skill update <이름>` 도 같은 승인 정책입니다.

### 하드링크·심볼릭 링크 거부

설정 파일(`.mcp.json`, `.claude/settings.json`, `opencode.json`, 상태 파일)과 스킬 대상 디렉터리가 **심볼릭 링크**이면 따라가지 않고 충돌로 보고하며, 설정 파일이 **하드링크가 둘 이상**(`nlink > 1`)이면 `하드링크된 설정 파일은 지원하지 않습니다` 로 읽지 않고 중단합니다(다른 파일을 링크로 바꾸어 읽거나 덮어쓰게 만드는 공격을 막기 위함). 링크를 실제 파일/디렉터리로 바꾸세요. dotfiles 저장소로 `~/.claude` 를 링크했다면 `--scope user` 는 충돌이 되므로 링크 대상 쪽을 직접 관리합니다. 알려진 한계: 스킬 트리 **안의** 하드링크 검사는 아직 없습니다.

### 백업 복원

모든 `--apply` 는 쓰기 전에 `$FORGE_HOME/backups/<UTC시각>-<id>/` 에 백업합니다.

```bash
forge sync --list-backups
forge sync --restore latest --dry-run
forge sync --restore latest
forge sync --restore latest --force
```

백업 이후 사용자가 파일을 고쳤으면 `--force` 가 있어야 복원합니다. 복원 직전 상태도 새 백업으로 남으므로 `--restore latest` 를 한 번 더 하면 복원을 되돌릴 수 있습니다. 다른 타깃의 백업은 거부합니다. 백업은 자동으로 지워지지 않으며 `--prune-backups [--keep N]` 으로만 지웁니다.

### 읽기 전용 파일

읽기 전용 파일/디렉터리는 "건드리지 마" 신호로 보고 충돌(`읽기 전용이라 변경하지 않습니다`)로 보고합니다. 쓰기 권한을 주거나(`chmod u+w <파일>`) 그 항목을 sync 대상에서 뺍니다. 스킬 레지스트리 스냅샷(`<FORGE_HOME>/skills/...`)은 일부러 읽기 전용입니다. 연습 환경을 지울 때는 먼저 `chmod -R u+w` 를 하세요.

### `opencode.jsonc` 만 있는 프로젝트

`JSONC 는 아직 지원하지 않습니다(주석 보존 불가). 아무것도 쓰지 않고 중단합니다`. 설정을 `opencode.json` 으로 옮기거나(주석이 없어야 함) 그 프로젝트는 직접 관리하세요. 자세한 내용은 [OpenCode 빠른 시작](quickstart-opencode.md).

### `confirm=ask` 도구가 거부됨

`사용자 확인이 필요한 도구입니다 (confirm=ask). 이 클라이언트는 확인 요청(elicitation)을 지원하지 않아 실행하지 않았습니다` 는 클라이언트가 elicitation 을 광고하지 않아 안전하게 거부한 것입니다. `forge tool test <이름>` 으로 직접 실행하거나, 위험을 이해한 도구에 한해 `forge tool add ... --confirm none --replace` 로 정책을 바꿉니다. OpenCode 의 elicitation 지원은 확인하지 못했습니다(미검증).

### 평문 비밀 거부, MCP 서버에 환경변수가 안 보임

- `비밀로 보이는 키에는 literal 값을 저장할 수 없습니다`: `--env API_KEY='${API_KEY}'` 처럼 환경변수 참조로 등록하세요(셸이 확장하지 않도록 작은따옴표). 우회 옵션은 없습니다.
- 서버가 종료되며 환경변수 누락 안내가 나옴: MCP 서버는 부모 셸 전체가 아니라 최소 환경과 `--env` 로 선언한 변수만 받습니다. 필요한 변수를 `--env KEY='${KEY}'` 로 선언하고, `forge mcp refresh` 를 실행하는 셸(또는 게이트웨이를 띄우는 클라이언트)의 환경에 그 변수가 있어야 합니다.

### 알려진 한계: 경로 인자가 비밀로 오탐됨 (`인자 #N: 비밀 값처럼 보이는 문자열이 있습니다`)

| 증상 | 원인 | 해결 |
|------|------|------|
| `forge mcp add ... -- python3 <경로>` 또는 `forge tool add ...` 가 "비밀 값처럼 보이는 문자열이 있습니다" 로 거부됨 | `--` 뒤 인자에도 비밀 탐지 휴리스틱(고엔트로피 문자열)을 적용하는데, 경로 구성요소 중 **24자 이상의 무작위 문자열**(예: macOS 기본 `TMPDIR` 인 `/var/folders/xx/<24자 무작위>/T/` 아래 경로)을 비밀로 오탐할 수 있음. **제품의 알려진 한계**이며 이슈로 기록됨 | 인자 경로를 짧거나 상대 경로로 바꾸기(`/tmp/...` 같은 짧은 경로로 복사, 또는 저장소 안의 상대 경로), 긴 경로를 감싸는 짧은 이름의 래퍼 스크립트를 만들어 그 스크립트를 등록하기. 비밀 검사를 끄는 옵션은 없음 |

`make demo` 와 `make quickstart-check` 는 이 오탐을 피하려고 임시 디렉터리를 `TMPDIR` 와 무관하게 `/tmp` 아래(짧은 경로)에 만들고, 긴 무작위 구성요소가 있는 경로를 주면 시작 시 중단합니다(`FORGE_DEMO_TMPDIR`, `FORGE_QC_TMPDIR` 로 위치 변경 가능). 저장소 체크아웃 경로에 24자 이상 구성요소가 있어도 같은 이유로 `mcp add` 예제가 거부될 수 있습니다.

### `forge mcp serve` 가 바로 종료됨

저장소(`$FORGE_HOME` 또는 `~/.forge` 의 `forge.db`)가 없으면 홈을 만들지 않고 `forge init 을 먼저 실행하세요` 를 stderr 에 출력하고 종료 코드 1 로 끝납니다. 클라이언트가 게이트웨이를 띄울 때 `FORGE_HOME` 이 다른 값이면 같은 일이 생깁니다(설정 파일의 `env` 에 `FORGE_HOME` 지정). 디버그 로그는 `FORGE_GATEWAY_DEBUG=1` 일 때만 stderr 로 나옵니다.

### 관찰된 동작: 프로필에서 숨긴 도구 호출이 `history misses` 에 나타남

프로필 전환 중 프로필에 없는(숨겨진) 등록 도구를 호출하면 그 호출이 `forge history misses` 의 "미등록" 으로 기록됩니다(노출되지 않아 not_found 로 기록되기 때문). 시연에서 관찰한 동작이며 의도된 것인지는 확인하지 않았습니다.

## 알려진 한계

- **실제 Claude Code·OpenCode 클라이언트가 Forge 가 만든 설정을 로드하는지는 검증하지 못했습니다.** 생성 형식은 공식 문서·`--help`·가짜 클라이언트 기준입니다. Linux·Windows 도 검증하지 않았습니다.
- `forge doctor` 는 해결 방법을 안내만 하고 고치지 않습니다. 설정 파일 크기 상한(16MiB) 안의 매우 큰 JSON 은 점검이 느릴 수 있습니다.
- **`make docs-check` 의 한계**: 이 저장소의 `make docs-check` 는 문서의 링크가 실제 파일을 가리키는지, 코드 블록의 `forge ...` 명령의 하위 명령과 `--flag` 가 **존재하는지**(`forge <명령> --help` 로 확인), 비가시 문자와 로컬 전용 경로가 없는지만 검사합니다. **명령을 실행하지 않으므로 실행 결과나 출력까지는 검사하지 않습니다.** 빠른 시작 문서의 명령을 실제로 실행해 보는 것은 `make quickstart-check`(격리된 임시 환경, 가짜 클라이언트)이며, 이것도 문서가 보여 주는 출력 문구까지 대조하지는 않고 종료 코드와 안전 조건(실제 홈 불변 등)을 확인합니다. 여기 표의 `doctor` 출력 문구는 작성 시점의 출력과 `docs/doctor.md` 를 근거로 한 것이라 버전이 바뀌면 달라질 수 있습니다.
- - `make docs-check` 가 검사하는 코드는 `bash`/`sh`/`shell-session`/`powershell`/언어 없는 펜스(bash 로 간주), `text` 펜스의 `forge ` 로 시작하는 줄, 4칸 들여쓴 `forge ` 줄이며, 링크는 인라인·이미지·참조형 정의·HTML `href`/`src` 를 봅니다. 출력 예시는 `$ ` 접두를 붙이거나 앞 줄에 `<!-- docs-check:skip -->` 를 두어 제외합니다. **검사하지 않는 것**: `$(...)` 안, `sudo` 뒤, `bash -c '...'` 안의 명령, 문서가 언급하는 `make` 타깃과 `scripts/*` 경로의 존재.
- `make quickstart-check` 는 `<!-- qc:skip -->` 가 붙은 블록(사용자 설정을 바꾸는 명령, 표준 입력을 기다리는 `forge mcp serve`, 연습 디렉터리를 지우는 정리 블록, URL 자리표시자)은 실행하지 않습니다. 정리 블록은 수동으로 한 번 실행해 오류 없이 동작함을 확인했습니다.
그 밖의 한계와 위협 모델: [`docs/security.md`](../security.md).
