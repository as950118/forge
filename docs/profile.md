# 프로필 (`forge profile`) — 노출 묶음과 활성화 (TASK-0012)

프로필은 "어떤 스킬·커스텀 도구·MCP 서버를, 어떤 클라이언트에, 어떤 방식으로 노출할지"의 이름 붙은 묶음이다(PLAN §3.3, ADR-F0001 D2·D3).
저장소는 마이그레이션 0001 의 `profiles`, `profile_members`, `profile_member_exposure`, `profile_activations` 테이블이다. **스키마 변경(마이그레이션 0002)은 필요 없었다**:
멤버(스킬/도구/MCP 정확히 하나), 클라이언트별 노출(`direct|gateway|hidden`), 스킬 핀(`pinned_version_id`), (클라이언트, 프로젝트)별 활성화가 이미 모두 표현된다.
이 문서의 모든 문장은 코드(`internal/profile`, `internal/adapter/inputs.go`, `internal/gateway/exposure.go`, `internal/cli/profile.go`, `internal/cli/sync.go`)와 대조했다.

## 1. 우선순위 규칙 (코드: `profile.Resolve`)

한 클라이언트가 어떤 프로젝트에서 무엇을 보는지는 **하나의 규칙**으로 정한다. `forge sync`(plan·apply), 게이트웨이(`forge mcp serve`), `forge profile current`, `forge doctor` 가 모두 이 함수를 쓴다.

1. **(클라이언트, 이 프로젝트)에 활성화된 프로필** → 규칙 `project`
2. 없으면 그 **클라이언트의 전역 활성 프로필**(모든 프로젝트용, `project_id` NULL) → 규칙 `global`
3. 그것도 없으면 **프로필 없음** → 규칙 `legacy`: 기존 동작(활성화된 스킬·도구·MCP 서버 전체 + `mcp.exposure.<서버>.<클라이언트>` 설정)

"이 프로젝트"는 `--project-dir`(기본 현재 디렉터리)에서 `.git` 이 있는 가장 가까운 상위 디렉터리(심볼릭 링크 해석 후; 없으면 그 디렉터리 자신)다. 게이트웨이는 자기 작업 디렉터리로 같은 계산을 한다.

**프로젝트 경로 동일성** (코드: `profile.FindProject`, `canonicalCase`): `projects` 테이블의 행 하나가 프로젝트 하나다. 경로 문자열이 아니라 **실제 디렉터리**로 같은 프로젝트인지 정한다.
조회·활성화·프로필 생성은 먼저 경로 문자열이 정확히 같은 행을 찾고, 없으면 기존 행들의 경로를 `stat` 해서 `os.SameFile`(같은 장치·inode)로 같은 디렉터리인 행을 재사용한다(없어진 경로의 행은 건너뜀; 행 수가 적어 비용은 무시할 만하다).
그래서 대소문자 구분 없는 파일 시스템(macOS 기본)에서 `x/proj`, `x/PROJ`, `x/Proj` 가 한 프로젝트이고(셸은 입력한 대소문자를 cwd 로 유지한다), 심볼릭 링크·`..`·`.`·후행 슬래시 통합은 기존대로 `ProjectRoot` 가 먼저 한다.
새 행을 만들 때는 디스크에 저장된 실제 대소문자로 경로를 복원해 저장한다(각 구성요소를 대소문자만 바꿔 보아 같은 파일을 가리킬 때만 디렉터리를 열거해 실제 이름을 찾는다; `F_GETPATH` 같은 플랫폼 전용 호출은 쓰지 않았다). 복원하지 못하면 입력한 철자로 저장해도 위 `SameFile` 재사용이 같은 프로젝트로 묶는다.
대소문자를 구분하는 파일 시스템에서는 `x/proj` 와 `x/PROJ` 가 서로 다른 inode 이므로 갈라진 채로 둔다(오탐 없음). 서로 다른 프로젝트는 합쳐지지 않는다(하드링크된 디렉터리는 없다고 본다). 동시에 서로 다른 철자로 `profile use` 를 해도 쓰기는 `BEGIN IMMEDIATE` 로 직렬화되므로 행은 하나다(테스트 `TestConcurrentUseWithTwoSpellingsMakesOneRow`).
이 규칙 이전에 이미 갈라진 행이 있으면 자동으로 합치지 않는다(`forge profile prune-stale` 이 경로가 없는 행만 정리한다).
`forge sync --scope user` 는 모든 프로젝트에 적용되므로 **전역 활성 프로필만** 따른다(계획 참고에 그렇게 적힌다).
규칙은 계획 출력의 `참고:` 줄(예: `프로필 "work" 적용 (전역 활성화, 클라이언트 opencode): 프로필에 없는 항목은 노출되지 않습니다(허용 목록)`), `forge profile current`, `forge doctor` 의 `프로필 규칙` 항목에 표시된다.

## 2. 허용 목록 의미 — "프로필 있음"과 "프로필 없음"의 차이

| | 프로필 없음 (`legacy`) | 프로필 적용 (`project`/`global`) |
|--|--|--|
| 스킬 | 활성화된 스킬 전부, 현재 버전 | **멤버만**. 핀이 있으면 그 버전 |
| MCP 서버 | 활성화된 서버 전부, `mcp.exposure.*` 설정(기본 direct) | **멤버만**. 노출은 프로필의 멤버 노출(기본 direct). `mcp.exposure.*` 는 무시된다 |
| 커스텀 도구 | 활성화된 도구 전부(게이트웨이) | **멤버만** (게이트웨이) |
| 새로 등록한 항목 | 곧바로 노출 대상 | 프로필에 추가하기 전까지 노출되지 않음 |

- `enabled` 플래그는 멤버십보다 **우선**한다: 비활성 항목은 프로필 멤버여도 노출되지 않는다(`forge doctor` 가 경고).
- 프로필이 비어 있으면 아무것도 노출되지 않는다(`forge doctor` 가 경고).
- 레지스트리에서 스킬·도구·MCP 서버를 지우면 모든 프로필의 해당 멤버도 함께 사라진다(스키마의 `ON DELETE CASCADE`). `forge skill|mcp|tool remove` 는 확인 문구 앞(그리고 `--yes` 일 때도, 막지는 않고)에 **"다음 프로필에서 제외됩니다: …"** 로 영향받는 프로필을 알린다(프로젝트 프로필은 경로, 어딘가에서 활성이면 `[활성 프로필: 클라이언트@위치]` 표시; 8개를 넘으면 "외 N건"; 표준 오류로 출력). 이 명령들에는 `--json` 이 없어 JSON 필드는 추가하지 않았다.
- 기존 사용자의 `mcp.exposure.*` 설정은 지워지지 않는다. 프로필을 해제하면(`unuse`) 다시 그 설정이 적용된다. 이관: `forge profile create <이름> --from-current`.

### 멤버 노출 값

| 멤버 | `direct` (기본) | `gateway` | `hidden` |
|------|-----------------|-----------|----------|
| mcp | sync 가 클라이언트 설정(`.mcp.json`/`opencode.json`)에 직접 렌더링. 게이트웨이는 프록시하지 않음 | 게이트웨이(`forge mcp serve`)만 프록시. 설정에는 쓰지 않음 | 노출 안 함 |
| skill | sync 가 클라이언트 스킬 디렉터리에 렌더링 + `forge_skill` 로 조회 | `forge_skill` 로만 조회(디렉터리에 쓰지 않음) | 노출 안 함 |
| tool | (사용 불가: 커스텀 도구는 게이트웨이로만 노출됨) | 게이트웨이(`forge_<이름>`) — 행이 없을 때의 기본 | 노출 안 함 |

노출은 클라이언트별로 따로 정한다(`--client claude-code|opencode|all`). 행이 없으면 위 "기본"이다.
커스텀 도구는 게이트웨이로만 나가므로 프로필 멤버십이 곧 게이트웨이 노출을 정한다. 어댑터가 쓰는 설정에 게이트웨이 항목(`forge`)은 프로필과 무관하게 항상 들어간다.
**한계**: OpenCode 는 `.claude/skills` 도 읽는다. claude-code 에 `direct` 로 렌더링된 스킬은 opencode 에서 `hidden`/`gateway` 로 해도 OpenCode 에 보인다(`forge doctor` 가 `스킬 노출 불일치` 로 경고). hook 은 프로필과 무관하다(claude-code 이력 수집, `--no-hooks`/`--remove-hooks` 로 따로 다룬다).

## 3. 명령

```text
forge profile create <name> [--project-dir <dir> | --global] [--from-current] [--description <text>] [--json]
forge profile list [--json]
forge profile show <name> [--project-dir <dir> | --global] [--json]
forge profile delete <name> [--project-dir <dir> | --global]
forge profile add <name> (--skill|--tool|--mcp) <item> [--exposure direct|gateway|hidden --client <id>|all]... [--pin <skill-version>]
forge profile remove <name> (--skill|--tool|--mcp) <item> | --member-id <id>
forge profile use <name> --client claude-code|opencode|all [--project-dir <dir>]
forge profile unuse --client claude-code|opencode|all [--project-dir <dir>]
forge profile current [--client <id>|all] [--project-dir <dir>] [--json]
forge profile prune-stale [--dry-run] [--yes] [--json]
```

- **범위**: 프로필은 전역(기본) 또는 프로젝트 소속(`--project-dir`)이다. 이름은 범위(와 프로젝트)마다 유일하다. `create/show/delete/add/remove` 는 지정한 범위만 보며 다른 범위로 대체하지 않는다. `--global` 과 `--project-dir` 은 함께 쓸 수 없다.
- **이름**: `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`. 설명은 500자 이하, 제어/비가시 문자 금지.
- **`use`**: 활성화 기록만 바꾼다. **클라이언트 설정 파일은 쓰지 않는다**(출력이 그렇게 알려 준다). `--project-dir` 없이 쓰면 그 클라이언트의 전역 활성화, 있으면 그 프로젝트에서만. 이름은 프로젝트 프로필 → 전역 프로필 순으로 찾는다(`use` 만 대체 허용). 프로젝트 프로필은 자기 프로젝트에서만 활성화할 수 있다. 같은 위치의 이전 활성화는 교체된다.
- **`delete`**: 어딘가에서 활성인 프로필은 거부한다(조용히 레거시 동작으로 돌아가는 것을 막음). 먼저 `unuse`(없어진 프로젝트의 활성화는 `prune-stale`).
- **디렉터리가 없는 `--project-dir`**: 새 행을 만들 수 있는 `create`·`use` 는 디렉터리가 있어야 한다. 행을 **찾기만** 하는 `show/delete/add/remove/unuse/current` 는 디렉터리가 없어도 경로 문자열 정규화(`profile.LoosePath`: 절대 경로·정리·가장 깊은 기존 상위 디렉터리의 심볼릭 링크 해석)로 DB 의 행을 찾아 동작한다. 존재하지 않는 경로에서 새 행을 만들지는 않는다. `current` 는 그 경로가 없다는 안내를 덧붙인다.
- **`remove --member-id <id>`**: 이름으로 지정할 수 없는 **손상된 멤버**(레지스트리에서 사라진 항목을 가리키는 행, 외부에서 DB 를 고쳤을 때만 생긴다)를 번호로 제거한다. 번호는 `profile show`(손상 표시 옆)와 `profile show --json` 의 `member_list[].id`, `forge doctor` 의 해결 명령에 나온다. 번호는 그 프로필의 멤버여야 하며 `--skill|--tool|--mcp` 와 함께 쓸 수 없다.
- **`prune-stale`**: 프로젝트 디렉터리가 삭제·이동되어 `projects.path` 가 더 이상 존재하지 않을 때 남는 활성화를 정리한다(ADR-F0001 O-4). **기본은 목록만 보여 주는 dry-run**이고(`--dry-run` 은 같은 동작), `--yes` 를 줄 때만 지운다(`--dry-run --yes` 는 오류). 목록은 프로젝트마다 경로, 활성화(클라이언트 -> 프로필), **마지막 활성화 시각**, 유지될 프로젝트 프로필 수를 보여 준다. 삭제는 한 트랜잭션이고 트랜잭션 안에서 "경로 없음"을 다시 판정한다(그 사이 다시 만든 디렉터리는 지우지 않는다).
  지우는 것: 경로가 없는 프로젝트의 `profile_activations`, 그리고 그 뒤 활성화·프로젝트 프로필·프로젝트 범위 스킬/도구/MCP 어디서도 참조하지 않는 `projects` 행. 지우지 않는 것: **프로필**(프로젝트 프로필 포함; 활성화가 사라지면 `profile delete --project-dir <없는 경로>` 로 지울 수 있다), 경로가 있는 프로젝트, `stat` 이 "없음"(ENOENT/ENOTDIR)이 아닌 오류(권한·I/O)를 낸 경로, 프로젝트 범위 항목이 참조하는 `projects` 행(지우면 `ON DELETE CASCADE` 로 그 항목이 함께 사라지므로).
  **이동한 프로젝트와 삭제한 프로젝트는 같게 보인다.** 옮겼다면 지우기 전에 새 위치에서 `forge profile use <프로필> --client <c> --project-dir <새 경로>` 로 다시 활성화하라(그래서 목록에 경로와 시각을 보여 주고 기본이 dry-run 이다). `forge doctor` 의 `profile.stale` 이 이 명령을 안내한다(점검은 읽기 전용).
  `unuse`/`current` 로 한 프로젝트씩 해제할 수도 있다.
- **`add`**: 이미 있는 멤버에 다시 쓰면 노출·핀을 갱신한다(핀은 `--pin` 을 다시 주지 않아도 유지). 레지스트리에 없는 항목, 알 수 없는 클라이언트, `--exposure` 없는 `--client`(그 반대도), 도구의 `direct` 는 거부한다.
- **`--pin <버전>`**: 스킬의 **등록된 불변 버전**(`forge skill show <이름>` 의 버전 문자열)에 고정한다. 그 스킬에 등록되지 않은 버전(다른 스킬의 버전 포함)은 거부한다. 핀이 있는 스킬은 sync 도, 게이트웨이의 `forge_skill` 도 그 버전을 쓴다.
- **`--from-current`**: 지금 활성화된 스킬(현재 버전 추종)·도구·MCP 서버를 멤버로 복사하고, `mcp.exposure.*` 의 `gateway`/`hidden` 설정을 멤버 노출로 복사한다(기본값 direct 는 행을 만들지 않음). 결과: 이관 직후 sync 계획이 이관 전과 같다(테스트 `TestFromCurrentReproducesLegacyPlanAndLegacyCLIPolicy`).
- **동시성**: 모든 쓰기는 하나의 `BEGIN IMMEDIATE` 트랜잭션이고 이름·멤버·활성화 유일성은 DB 인덱스가 보장한다(동시 `add/create/use` 테스트).
- 읽기 명령(`list/show/current`)은 읽기 전용 핸들로 열며 아무것도 만들지 않는다(`forge init` 전이면 안내 후 실패).

### 기존 `forge mcp expose|enable|disable --client` 와의 관계 (안전한 쪽: 거부)

프로필이 그 클라이언트에 (이 프로젝트 또는 전역으로) 적용 중이면 `mcp.exposure.*` 는 무시되므로, 이 명령들은 **아무것도 바꾸지 않고 거부**하며 정확한 대체 명령을 알려 준다:
`forge profile add <프로필> --mcp <서버> --exposure <모드> --client <클라이언트> [--project-dir <루트>]`.
조용히 무시되는 설정을 쓰게 두거나, 사용자가 의도하지 않은 프로필 멤버를 몰래 고치는 것보다 안전하기 때문이다. 프로필이 없으면 기존 동작 그대로다. `--client` 없는 `enable/disable` 은 전역 `enabled` 플래그를 바꾸며(허용), 프로필이 활성이면 "비활성 서버는 프로필 멤버여도 노출되지 않는다"는 참고를 덧붙인다.

## 4. 소비자: sync 와 게이트웨이

- **sync** (`adapter.LoadInputsFor`): 규칙을 적용해 어댑터 입력(`Inputs`)을 만든다. 프로필이 있으면 멤버가 아닌 스킬·MCP 서버는 입력에서 빠지므로, 이전에 Forge 가 쓴 항목이면 일반 규칙대로 **관리 항목 삭제**로 계획된다(사용자 항목은 건드리지 않음). 어댑터 코드는 바꾸지 않았다 — `Inputs` 를 계산하는 지점만 바뀌었다.
- **게이트웨이** (`gateway.ProfileExposure`): `forge mcp serve` 는 요청마다 목록을 다시 계산하므로 **프로필 변경(`use/unuse/add/remove`)은 실행 중인 게이트웨이에 다음 `tools/list`·`tools/call`·`forge_list`·`forge_skill` 부터 재시작 없이 반영된다**(테스트로 확인). 프로필을 읽지 못하면(DB 오류) 항목을 **숨긴다(fail closed)**. 클라이언트를 식별하지 못한 연결(`""`)은 레거시 규칙과, 프로필이 있는 모든 클라이언트의 프로필을 **모두** 만족하는 항목만 본다. `--client` 지정을 권장한다(sync 가 쓰는 항목은 `--client` 를 지정한다).
- 계획·doctor·`profile current` 는 어느 규칙이 적용 중인지 표시한다.

## 5. 프로필 전환과 두 클라이언트의 일관성 (AC-3)

```bash
forge profile use work --client all      # 활성화 기록만
forge sync --target all                  # claude-code + opencode 를 하나의 통합 계획으로 (파일을 쓰지 않음)
forge sync --target all --apply          # 순차 적용
```

`--target all` 의 동작은 `docs/sync.md` 를 보라. 요약: 두 계획을 모두 계산하고, 충돌이 하나라도 있으면 아무것도 쓰지 않으며, 같은 프로필 기준으로 두 설정을 만든다.
claude-code 가 `.claude/skills` 에 관리하는 스킬은 opencode 계획이 이미 "다른 쪽이 관리"하는 것으로 알고 다시 렌더링하지 않는다(`Request.ClaimedSkills`; 통합 계획 시점에도 적용되므로 적용 후 중복 노출이 없다).
클라이언트마다 다른 프로필을 `use` 할 수도 있다. 각 클라이언트는 자기 프로필과 일관된다.

## 6. 알려진 한계

- 프로젝트가 이동·삭제돼도 `profile_activations` 는 **자동으로 지우지 않는다**(이동일 수 있어 사용자 판단이 필요하다). 아무 영향은 없고 `forge doctor` 가 경고하며 `forge profile prune-stale` 로 정리한다(ADR-F0001 O-4 에 대한 구현 정책: 자동 삭제 없음, 명시적 정리 명령. ADR 문서는 바꾸지 않았다).
- 경로 동일성은 `stat`/`SameFile` 에 의존한다: 다른 사용자의 권한 없는 디렉터리·끊긴 마운트는 "같은지 알 수 없음"이라 별개 프로젝트로 남을 수 있다. 하드링크된 디렉터리는 없다고 보고, 유니코드 정규화(NFC/NFD) 철자 차이는 따로 다루지 않는다(같은 디렉터리로 열리는 철자면 `SameFile` 이 묶지만 그 동작은 파일 시스템에 따라 확인하지 않았다).
- `.forge/forge.yaml` 매니페스트로 프로필을 git 으로 공유하는 기능은 없다(M7).
- 스킬 핀은 `forge_skill`·sync 에 적용된다. 클라이언트가 `~/.forge/skills` 를 직접 읽지는 않는다.
