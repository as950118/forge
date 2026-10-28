# `forge doctor` — 점검, 버전 게이팅, 안전 설계 (TASK-0012)

```bash
forge doctor [--scope project|user|all] [--project-dir <dir>] [--json] [--strict] [--hook-days 7] [--timeout 30s]
```

Forge 홈·DB, 클라이언트 설치, sync 드리프트, 깨진 경로, 이력, 위생, 프로필을 점검한다. **읽기 전용**이다: 아무것도 만들거나 고치거나 지우지 않고, 고칠 방법은 정확한 명령 문자열로 안내만 한다.
이 문서의 모든 문장은 코드(`internal/doctor`, `internal/clientver`, `internal/cli/doctor.go`, `internal/cli/sync.go`)와 대조했다.

## 심각도와 종료 코드

| 심각도 | 의미 |
|--------|------|
| `ok` | 문제 없음 |
| `info` | 참고(정상일 수 있음): 설치하지 않은 클라이언트, 미검증 패치, 프로필 없음, 초기화 전 |
| `warn` | 주의: 드리프트, 권한, 깨진 경로, 미검증 버전, **점검 불가**(점검 자체가 실패) |
| `fail` | 문제: DB 손상·다운그레이드·checksum 불일치, 손상된 프로필 멤버, 디렉터리인 forge.db 등 |

종료 코드: `fail` 이 하나라도 있으면 **1**, 아니면 0. `--strict` 는 `warn` 이 있어도 1. 출력은 먼저 나오고(텍스트 또는 `--json`) 종료 코드 사유가 한 줄 오류로 stderr 에 나온다.
점검 하나가 실패하거나 패닉해도 다른 점검은 계속한다(실패는 fail 이 아니라 `점검 불가` warn). 전체 시간 제한 `--timeout`(기본 30초)을 넘으면 남은 그룹은 `*.skipped` warn 으로 건너뛴다.
항목마다 `원인`과 `해결`(정확한 명령, 항목당 최대 6개)을 낸다. 경로는 POSIX 작은따옴표로 인용되고 제어/비가시 문자는 보이는 이스케이프(`\x1b` 등)로 바뀐다. 이스케이프는 출력에서 터미널을 보호하지만 셸에서는 글자 그대로라 **그대로 붙여 넣으면 실제 경로와 다른 경로를 가리킨다**: 이런 경로가 해결 명령에 들어가면 그 항목의 `fix` 끝에 `# 주의: …` 줄(`ls -b` 등으로 실제 이름을 확인해 직접 입력)을 덧붙이고, 클라이언트 위치 경고 메시지에도 같은 안내를 붙인다(TASK-0024 AC-3. 인용 방식을 바꾸지 않은 이유: `$'…'` 인용은 셸마다 지원이 달라 `sh`/`dash` 에서 더 위험한 오동작이 될 수 있다). 300자를 넘어 잘린 경로도 같다.
**DB 를 쓸 수 없을 때**(TASK-0024 AC-4): 이유(읽을 수 없음·손상·Forge DB 아님)는 `db.integrity` 한 곳에서만 말한다. DB 가 필요한 그룹(`drift`, `history`, `profile`, `security`)은 `*.skipped` 로 "점검 불가: DB 를 사용할 수 없어 … 점검을 건너뜁니다" **warn** 을 내며 이유를 되풀이하지 않는다(점검하지 못했으니 정상으로 읽히면 안 되고 `--strict` 에서 종료 코드에 반영된다). `forge.db` 가 **아직 없는** 경우(`forge init` 전)만 info 다. 목록은 8개까지만 이름을 나열하고 "외 N건"으로 줄인다.

## `--json` (schema 1)

```json
{"schema":1,"home":"…","project":"…","scope":"project","strict":false,"exit_code":0,
 "summary":{"ok":0,"info":0,"warn":0,"fail":0},
 "checks":[{"id":"drift.claude-code.project","group":"drift","title":"…","status":"warn","message":"…","cause":"…","fix":["forge sync …"]}],
 "duration_ms":12}
```

`id` 는 안정적이다(`home.dir`, `db.perm`, `db.integrity`, `db.migrations`, `db.wal`, `client.<id>`, `drift.<target>.<scope>[.links|.interrupted|.journal|.temps|.backup|.stale]`,
`paths.<target>.<scope>.<skilldirs|toolcache|gateway|gatewaypath|mcpcommand>`, `paths.skillstore`, `history.hook|size|unknown`, `hygiene.backups[.perm]`, `hygiene.perm` (이상 없음) 또는 `hygiene.perm.<표시 이름>`(경고; 표시 이름은 `.mcp.json`, `.claude/settings.json`, `opencode.json`, `~/.claude/settings.json` 이므로 실제 id 는 `hygiene.perm..mcp.json`, `hygiene.perm..claude/settings.json`, `hygiene.perm.opencode.json`, `hygiene.perm.~/.claude/settings.json`),
`profile.rule.<client>`, `profile.legacy.<client>`, `profile.health.<이름>[.disabled|.pins|.overlap]`, `profile.stale`). `fix` 는 비어 있을 수 있다(ok/info). 정렬은 그룹 순서(home, client, drift, paths, history, hygiene, profile).
**프로필 id 의 이름 부분**은 구분자 충돌을 막기 위해 이스케이프한다: 프로필 이름의 `.` 은 `%2E`, `%` 는 `%25` 로 쓴다(이름이 `x.disabled` 인 프로필의 id 는 `profile.health.x%2Edisabled` 이고 프로필 `x` 의 비활성 멤버 점검 `profile.health.x.disabled` 와 겹치지 않는다). 프로젝트 프로필은 id 끝에 `@project` 가 붙는다(같은 이름의 전역 프로필과 구분: `profile.health.x@project`, `profile.health.x@project.disabled`).

## 점검 항목

| 그룹 | 점검 | 상태 규칙 |
|------|------|-----------|
| 홈·DB | `FORGE_HOME` 존재·권한 | 없음=info(`forge init`). 0700 아님=warn(`chmod 700 <홈>`; world-readable 이면 문구에 표시). 디렉터리 아님·끊어진 링크=fail |
| | `forge.db` 권한·종류 | 0600 아님=warn(`chmod 600`). **디렉터리면 fail**("손상"이 아니라 "디렉터리"로 구분) |
| | 무결성 | `PRAGMA quick_check`(읽기 전용, 8초 제한). 실패/손상=fail(백업 DB 복원 안내), 시간 초과=warn(점검 불가). Forge DB 가 아니거나 테이블 없음=fail |
| | 마이그레이션 | 바이너리보다 최신=fail(다운그레이드 거부), checksum 불일치=fail, 미적용 있음=info(`forge init`), 일치=ok |
| | WAL | 헤더가 WAL 이 아니면 info, `-wal` 이 64MiB 초과면 warn(`wal_checkpoint`) |
| 클라이언트 | `claude`/`opencode` 탐지·버전 | 아래 "버전 게이팅" |
| 드리프트 | (target, scope) 별 Plan 을 읽기 전용으로 계산 | 충돌=warn(`--diff`/`--force-managed`/`--skip-conflicts` 명령), 미적용 변경=warn(`--apply`), 일치=ok. 링크 거부(심볼릭/하드링크)·해석 불가 JSON·잘못된 상태 파일은 원인과 함께 warn |
| | 중단된 sync | 저널이 남음=warn(`--apply` 로 이어서 완료) · 믿을 수 없는 저널(손상/변조)=warn(`--list-backups`) · 임시 파일=info |
| | **백업 의존성** (TASK-0022 AC-1/8) | 저널 사본(`<repo>/.forge/sync-journal*.json`)은 있는데 `~/.forge/backups/<id>` 가 없음=warn. `pending` 인 사본만 대상(완료·롤백된 사본은 무시). 복구가 `$FORGE_HOME/backups` 의 백업에 의존한다는 한계(백업을 지우거나 `--prune-backups` 로 정리하면 중단 복구가 불가능)와 계획의 "충돌"이 오도할 수 있음을 알리고 `--diff`, `--skip-conflicts`, 직접 확인 후 삭제·이름 변경, 저널 사본 삭제를 제시한다. `--force-managed` 는 이 충돌에 효과가 없어(`LostBackupReason`) 권하지 않는다. 계획(`forge sync`)의 같은 사유(`lost_backups`)와 짝이다. 저널의 id 는 형식 검증 후에만 경로로 쓴다 |
| 경로·연결 | 관리 중인 스킬 디렉터리 누락 | warn (`--apply` 로 재생성) |
| | MCP 항목의 `command` 존재 | 관리 항목만 대상. `exec.LookPath`/stat 으로 **존재만** 확인(실행 금지), 없음·상대 경로=warn |
| | 게이트웨이 항목의 forge 경로 | 없는 명령·상대 경로·현재 forge 바이너리와 다른 바이너리=warn(`--apply --bin <현재 바이너리>`) |
| | tools/list 캐시 없음 | exposure=gateway 서버=warn(`forge mcp refresh <서버>`) |
| | 게이트웨이 항목 누락 | gateway 노출 서버(또는 프로필의 도구)가 있는데 설정에 `forge` 항목이 없음=warn |
| | 잔존 항목 | 비활성/삭제/프로필 제외된 관리 항목이 설정에 남음=warn |
| | 스킬 저장소 | 활성 스킬의 `~/.forge/skills/<이름>@<버전>` 누락=warn |
| 이력 | hook 활동 | claude-code hook 이 관리 중인데 최근 N일(`--hook-days`, 기본 7) `source=hook` 이벤트 0건=warn. 설치한 지 N일 미만이면 info |
| | 원장 크기 | 500만 행 또는 DB 1GiB 초과=warn(내보내기 명령. 이력을 지우는 forge 명령은 아직 없음) |
| | 미등록 이름 상한 | 한 게이트웨이 세션이 미등록 이름 100행(상한)을 기록=warn(`forge history misses`) |
| 위생 | 백업 | sync 백업 20개 초과=warn(`--prune-backups --dry-run`), 다른 사용자가 읽을 수 있음=warn(`chmod -R go-rwx`) |
| | 설정 파일 권한 | `.mcp.json`, `.claude/settings.json`, `opencode.json`(project), `~/.claude/settings.json`(user)이 그룹/다른 사용자 쓰기 가능(0022 비트)=warn(`chmod go-w`). 링크는 따라가지 않는다 |
| 프로필 | 적용 규칙 | 클라이언트별 `project`/`global`=ok, `legacy`=info(`profile create --from-current`) |
| | 활성 프로필 건강 | 손상/삭제된 멤버=fail, 빈 프로필=warn, 비활성 멤버=warn, 핀 버전 파일 누락=warn, claude-code direct + opencode hidden 불일치=warn, 무시되는 레거시 노출 설정=info |
| | 클라이언트별 프로필의 스킬 노출 불일치 (`profile.crossclient`, TASK-0024 AC-6) | claude-code 와 opencode 에 **서로 다른** 프로필이 적용되고, claude-code 프로필에서 `direct` 인 스킬이 opencode 프로필에서는 `hidden`/`gateway` 이거나 멤버가 아님=warn. OpenCode 는 `.claude/skills` 도 읽으므로 그 스킬이 OpenCode 에 보인다. 같은 프로필 안의 불일치는 `profile.health.<이름>.overlap` 이 보고한다. 대칭 처리(opencode 쪽 숨김을 실제로 구현)는 하지 않았다: OpenCode 가 `.claude/skills` 를 읽는 것을 Forge 가 끌 수 없으므로 경고가 정직한 최대치다 |
| | 중복 프로젝트 행 (`profile.projects.duplicate`, TASK-0024 AC-11) | `projects` 의 서로 다른 행이 같은 디렉터리(`os.SameFile`)를 가리킴(대소문자 변형 등 `FindProject` 이전에 기록된 행)=info. 행마다 활성화·프로젝트 프로필이 따로라 **자동으로 합치지 않는다**. `stat` 할 수 없는 경로는 "같은지 알 수 없음"이라 건너뛴다(최대 5000행) |
| | 없어진 프로젝트의 활성화 (`profile.stale`) | `projects.path` 가 더 이상 존재하지 않는(`stat` 이 ENOENT/ENOTDIR) 프로젝트에 활성화가 남아 있음=warn. 경로·마지막 활성화 날짜를 보이고 해결로 `forge profile prune-stale --dry-run`(목록 확인)과 `forge profile prune-stale --yes`(실제 정리)를 제시. 점검은 경로를 `stat` 할 뿐 아무것도 바꾸지 않는다 |
| 보안 (TASK-0013, `docs/security.md`) | 스킬 저장소 무결성 (`security.skills`) | 활성 스킬 현재 버전의 `~/.forge/skills/<이름>@<버전>` 파일을 가져오기 한도 안에서 다시 해시해 등록된 `content_hash` 와 비교(`forge skill check` 와 같은 `skill.VerifyStore`). 다르거나 심볼릭 링크·추가 파일이 있음=fail(`forge skill check <이름>`, 삭제 후 재등록 안내), 읽을 수 없음=warn, 누락은 위 "스킬 저장소" 점검이 보고 |
| | MCP 도구 정의 승인 (`security.mcp`) | 도구 캐시가 있는 활성 서버 중 승인된 정의와 다른 도구가 있거나 승인 이후 새로 생긴 도구가 있음=warn(`forge mcp show <서버>`, `forge mcp approve <서버>`), 없음=ok. 승인 기록 테이블이 없는 옛 DB=info |
| | 승인 기록 없음 (`security.mcp.unapproved`) | 한 번도 승인하지 않은 서버=info(승인하면 이후 변경을 감지) |
| | 도구 고정 (`security.mcp.enforce`) | enforce 가 꺼져 있음=info(`forge mcp pin <서버> --enforce`), 켜져 있고 게이트웨이가 숨기는 도구가 있음=warn, 숨김 없음=ok. 해결 명령의 서버 이름은 셸 인용 규칙을 따른다 |

### 프로필 해결 명령은 (클라이언트, 프로젝트)와 실제 이름을 포함한다

- **빈 프로필** 해결의 `unuse` 는 그 프로필이 실제로 활성인 위치마다 낸다: 프로젝트 활성화는 `forge profile unuse --client <c|all> --project-dir <정규화된 경로>`(셸 인용), 전역 활성화는 `--project-dir` 없이. 모든 클라이언트에서 활성이면 `--client all`, 아니면 클라이언트별로 따로.
- **레거시(프로필 없음)** 안내의 `forge profile use <이름> --client <c>` 는 전역용이고, 점검한 프로젝트가 있으면 `--project-dir <경로>` 를 붙인 "이 프로젝트에서만" 형태를 함께 낸다. 클라이언트 버전·hook·미등록 이름 안내의 `forge sync` 명령도 점검한 프로젝트면 `--project-dir <경로>` 를 포함한다.
- **손상된 멤버**(행이 가리키는 스킬/도구/MCP 서버가 없음)는 이름이 없으므로 `forge profile remove <프로필> [--project-dir …] --member-id <번호>` 를 번호별로 낸다(번호는 메시지와 `forge profile show` 에 나옴).
- **비활성 멤버**는 종류·이름을 실제 값으로 치환한 `forge skill enable <이름>` / `forge mcp enable <이름>` / `forge tool enable <이름>`(종류마다 최대 2개)과, 프로필에서 빼는 `forge profile remove <프로필> --<종류> <이름>` 를 낸다.
- 위 명령들은 테스트가 실제로 실행해 효과를 확인한다(`TestDoctorFixCommandsCarryClientAndProject`, `TestBrokenAndDisabledMemberFixCommandsRun`).

## 버전 게이팅 정책

**승인된 결정**: Forge 가 클라이언트 바이너리에 대해 실행할 수 있는 것은 **`<binary> --version` 하나뿐**이다. 다른 서브명령·플래그는 모델 호출이나 설정 변경의 가능성이 있어 어떤 경우에도 실행하지 않는다(`clientver.VersionArg`, 테스트는 가짜 스크립트가 argv 를 기록해 정확히 `--version` 하나임을 단언한다). 테스트는 실제 `claude`/`opencode` 를 실행하지 않는다.

지원 버전 표(코드: `clientver.Specs`, 테스트가 `docs/clients/*.md` 의 "확인 버전"과 대조): claude-code **2.1.305**, opencode **2.0.24**(문서의 `v2.0.24`).

| 결과 | 심각도 | 의미 |
|------|--------|------|
| 정확히 검증된 버전 | ok | |
| 같은 major.minor 의 다른 patch | info | "미검증 패치" — `sync --apply` 는 계속 진행 |
| 그 밖의 버전 | warn | "미검증 버전: 생성한 설정이 올바르게 로드되지 않을 수 있습니다" |
| 바이너리 없음 | info | 설치하지 않은 클라이언트는 정상 |
| 프로젝트 디렉터리 안의 바이너리만 있음 | warn | 실행하지 않음 |
| 다른 사용자가 수정할 수 있는 위치의 바이너리 (`unsafe-location`) | warn | 실행하지 않음: "<경로> 는 다른 사용자가 수정할 수 있는 위치라 실행하지 않았습니다 (<문제 경로>: <이유>): chmod go-w …". `sync --apply` 는 **`--allow-untested-version` 으로도 우회할 수 없다**(아래) |
| 실행 실패(종료 코드≠0, 시간 초과, 출력 초과, 실행 불가) / 버전 형식 인식 불가 | warn | 검증되지 않은 것으로 취급 |

**sync 연동**: `forge sync --apply` 는 파일을 쓰기 **전에** 대상 클라이언트의 버전을 확인하고, warn 이면 아무것도 쓰지 않고 중단한다. `--allow-untested-version` 이 있어야 진행한다(계획의 `참고:` 와 JSON `notes` 에 버전이 남는다). 단 **바이너리 위치가 안전하지 않은 경우(`unsafe-location`)는 이 플래그로 우회할 수 없다**: 그 플래그는 "아직 검증하지 못한 *버전*"을 받아들이는 것이지 다른 계정이 심었을 수 있는 *프로그램*을 실행해도 된다는 뜻이 아니고, 해결(`chmod go-w`)이 한 줄이라 우회로를 둘 이유가 없다. `--target all` 은 두 클라이언트를 모두 확인하고 하나라도 warn 이면 둘 다 쓰지 않는다.
**계획(plan)은 기본적으로 `--version` 을 실행하지 않는다**: 계획은 읽기 전용·빈번한 명령이라 외부 바이너리 실행이라는 부작용을 옵트인(`--check-version`)으로 두었다. 쓰기 직전(`--apply`)에는 정확성이 중요하므로 항상 확인한다. `forge doctor` 는 점검 목적이므로 항상 실행한다. **결과는 캐시하지 않는다**(매번 실행). `--allow-untested-version` 은 `--apply` 와, `--check-version` 은 계획과만 쓸 수 있다.

### `--version` 실행의 안전 설계 (`clientver.Exec`)

- **탐색**: `$PATH` 의 **절대 경로 항목만** 쓴다(빈 항목, `.`, 상대 경로 항목은 무시). 각 후보는 `exec.LookPath` 로 실행 가능 여부를 확인하고, 심볼릭 링크는 해석한 경로를 얻어 그 경로를 실행한다(표시는 둘 다). 링크 또는 해석 경로가 **프로젝트 디렉터리 안**이면 실행하지 않고 다음 PATH 항목을 찾는다.
- **쓰기 가능한 위치(실행하지 않음)** (코드: `clientver/location.go`, `perm_unix.go`): 탐색된 바이너리를 실행하기 전에 심볼릭 링크를 해석한 경로로 다음을 본다. 먼저 "다른 사용자가 쓸 수 있음"의 정의: **other 쓰기 비트(0002)는 항상** 해당하고, **group 쓰기 비트(0020)는 그 그룹이 신뢰 그룹이 아닐 때만** 해당한다. 신뢰 그룹은 gid 0(wheel/root), macOS 의 gid 80(`admin`, 관리자 계정 그룹), 그리고 소유자의 **user-private group**(소유자의 기본 그룹이면서 이름이 사용자 이름과 같은 그룹, 리눅스 UPG 관례)뿐이다. `staff`(gid 20, 모든 로컬 일반 사용자)·`users`·`everyone` 같은 광범위 그룹과, `wheel`/`sudo`/`adm` 같은 이름은 **이름으로 신뢰하지 않으며**(gid 로만 판정), 조회가 실패하면 신뢰하지 않는다(warn). 그룹 구성원은 열거하지 않는다(UPG 는 이름 관례로, admin 은 gid 로 가정). 이렇게 해서 Apple Silicon Homebrew 의 `drwxrwxr-x user:admin` 접두사(`/opt/homebrew/bin` → `Caskroom/...` 링크 체인 포함)는 허용하고 `staff` 그룹 쓰기 디렉터리는 막는다. 규칙: (1) 바이너리 파일: 다른 사용자가 쓸 수 있음 또는 소유자가 현재 사용자도 root 도 아님. (2) 바이너리가 든 디렉터리(그리고 링크가 든 PATH 항목 디렉터리): 다른 사용자가 쓸 수 있으면 **sticky 비트가 있어도 안전하지 않음**(`/tmp` 같은 곳에서는 실행하지 않는다), 소유자가 현재 사용자도 root 도 아님. (3) 그 위의 모든 상위 디렉터리: 다른 사용자가 쓸 수 있으면서 sticky 가 **아니면** 안전하지 않음(sticky 디렉터리는 파일 소유자만 이름을 바꿀 수 있어 본인 소유 디렉터리 위의 `/tmp` 는 허용), 소유자가 현재 사용자도 root 도 아님. **경로는 정규화하지 않고 구성요소 순서대로 따라간다**(`unsafeLocation`의 lexical walk): `/` 에서 시작해 PATH 후보(적힌 그대로, `..` 가 있어도 정리하지 않음)와 모든 링크 대상을 한 구성요소씩 걷는다. 들어간 디렉터리마다 위 규칙으로 `/` 까지의 상위 디렉터리와 함께 검사하고(지나가기만 하는 디렉터리는 sticky 면 면제, 링크나 바이너리를 담은 디렉터리는 면제 없음), `..` 는 지금까지 도달한 실제 디렉터리의 부모로 올라가며 올라오기 전에 지난 모든 중간 디렉터리(`sub/..` 의 `sub` 포함)를 이미 검사했다. 심볼릭 링크 구성요소(중간 디렉터리가 링크인 경우 포함)는 홉이다: 링크를 담은 디렉터리를 엄격히 검사하고, 링크 자신은 소유자(모드 비트를 존중하는 시스템, 즉 리눅스가 아닐 때는 모드도)를 검사한 뒤, 상대 대상은 링크가 든 디렉터리에서, 절대 대상은 `/` 에서 다시 걷는다(전체 최대 40홉; 초과·순환은 안전하지 않음). 마지막으로 실제 파일의 모드·소유자와 그것을 담은 디렉터리를 검사한다. 정규화를 하지 않는 이유: `<0777 디렉터리>/sub/../../safe/claude` 를 정리하면 안전한 경로로 보이지만, 공격자가 0777 디렉터리 안의 `sub` 를 심볼릭 링크로 바꾸면 `..` 가 다른 곳을 가리키게 된다(테스트 `TestDotDotInALinkTargetIsWalkedNotNormalized`). `..` 를 무조건 거부하지 않는 이유: Homebrew 링크는 `../Cellar/...` 처럼 `..` 를 쓰는 상대 대상이 일반적이다(이 개발 기계의 `/opt/homebrew/bin` 에서 확인; 테스트 `TestDotDotWalkAcceptsNormalChainsAndOddNames`). `a..b`, `...` 같은 이름은 평범한 이름이다. 한계: 검사와 실행 사이의 경쟁(TOCTOU)은 막지 못한다(안전한 위치에서만 실행하는 것이 방어선); 커널의 링크 깊이 한도(예: macOS 32)를 넘는 체인은 그 전에 "찾을 수 없음"이 된다.
- **실행**: 인자는 정확히 `--version`, stdin 은 `/dev/null`, 작업 디렉터리는 빈 임시 디렉터리, 타임아웃 5초, stdout·stderr 각 4KiB 상한(초과하면 프로세스를 멈춤), 별도 프로세스 그룹으로 실행하고 끝난 뒤(성공해도) 그룹 전체를 SIGKILL 해 남은 자식을 정리한다.
- **환경**: `mcp.MinimalEnv` 의 허용 목록(PATH, 로케일, 프록시·CA 설정 등. 토큰·API 키·`://사용자:비밀@` 가 든 값은 제외) 중 `HOME`/`XDG_*`/`TMPDIR` 은 **제외**하고, `HOME`·`TMPDIR`·`XDG_*` 를 실행마다 만든 **임시 디렉터리**로 지정한다. 이유: 클라이언트가 `--version` 에서 사용자 홈의 설정·자격증명을 읽거나(홈 격리) 홈에 쓰는 부작용(첫 실행 마이그레이션, 업데이트 확인, 캐시)을 막는다. 임시 디렉터리는 실행 후 삭제된다. 대가: 실제 `HOME` 으로 런타임을 찾는 nvm/asdf 류 shim 은 실패할 수 있고 그 경우 "실행 실패"(warn)로 보고된다(검증된 버전으로 오인하지 않는다). 종료 코드≠0·실행 불가 메시지에는 이 원인 후보와 "터미널에서 `<바이너리> --version` 으로 직접 확인하세요"를 덧붙이고, 경로가 `.nvm`/`.asdf`/`.volta`/`.fnm`/`.pyenv`/`.rbenv`/`mise`/`.proto` 아래면 shim 으로 보인다는 구체적 문구와 `--allow-untested-version` 안내를 쓴다(TASK-0024 AC-7. 시간 초과·출력 초과에는 붙이지 않는다). `FORGE_HOME` 도 전달하지 않는다.
- **파싱**: 출력의 처음 5줄 중 엄격한 정규식(선택적 짧은 단어 접두사, 선택적 `v`, `x.y.z`, 뒤는 공백·`(`·줄 끝)에 맞는 첫 줄의 `x.y.z` 만 쓰고 나머지는 버린다. 프리릴리스(`2.1.305-beta`)와 버전 바로 뒤의 제어 문자는 인식하지 않는다. 각 숫자의 선행 0 은 버리고 비교한다(`02.1.0305` = `2.1.305`, TASK-0024 AC-5; 한 숫자는 최대 4자리). 메시지에는 원본 출력이 아니라 파싱한 버전과 정화된 종료 코드/stderr 첫 줄(120자)만 들어간다.
- 실제 설치본으로 확인한 것: 위 파서가 설치된 `claude --version` 출력에서 `2.1.307`, `opencode --version` 출력에서 `2.0.24` 를 인식했다(Forge 가 격리된 HOME 으로 실행한 결과이며 원문 출력은 저장하지 않아 정확한 원문 형식은 기록하지 않았다. 설치된 claude 는 확인 버전 2.1.305 보다 새 patch 였다). 원문 형식이 바뀌어 인식하지 못하면 "인식 불가" warn 이다.

## 읽기 전용 보장과 악의적 저장소

- **DB**: `mode=ro` + `query_only`. `-wal` 에 미반영 내용이 없으면 `immutable=1` 로 열어 `-wal`/`-shm` 보조 파일도 만들지 않는다. `-wal` 에 내용이 있으면 함께 읽어야 하므로 immutable 이 아니며 `-shm` 이 이미 있어야 연다(없으면 파일이 만들어질 수 있어 "점검 불가"). 쓰기 가능한 `store.Open`(마이그레이션)은 쓰지 않는다. 테스트는 FORGE_HOME·프로젝트·HOME 디렉터리 스냅샷 해시와 DB·WAL 바이트가 전후 같음을 확인한다.
- **설정 파일**: sync 와 같은 경로 가드(`adapter.GuardPath`, 심볼릭 링크·하드링크·크기 상한 `ReadFileSafe`) 뒤에서만 읽는다. 위조된 상태 파일·저널·링크는 계획의 기존 검증(저널은 형식·경로·해시 재검증 후에만 신뢰, 저널의 내용을 경로로 쓰지 않음)을 따르고, 위조는 warn 으로 보고될 뿐 아무것도 쓰거나 지우지 않는다. 저널 사본의 id 는 백업 id 형식일 때만 경로에 쓴다.
- **금지 파일**: `~/.claude.json` 과 OpenCode 비밀·전역 설정 파일(`service.json`, `auth.json`, `opencode.db`, `credential*`, 데이터 디렉터리)은 doctor 코드가 열거나 이름을 붙이지 않는다. sync 의 `GuardPath` 가 다른 경로의 동일 파일(링크) 여부를 판정하려고 그 파일들을 `stat` 만 한다(내용을 열지 않으며, 이 stat 은 sync 계획이 이미 하던 것이다). 위생 점검은 `Lstat` 만 쓰며 사용자 수준에서는 `~/.claude/settings.json` 하나만 본다.
- **한계**: 설정 파일 크기 상한(16MiB) 안의 매우 큰 JSON 은 계획 계산에 수 초가 걸릴 수 있다(20,000개 서버 항목·30만 행 원장에서 시험). 시간 제한을 넘으면 남은 점검을 건너뛴다. 클라이언트의 `--version` 출력이 실제 형식과 다른 새 버전은 "인식 불가" warn 이 된다. 원장 정리 명령이 없어 크기 경고의 해결은 내보내기까지만 안내한다. 디렉터리 권한은 POSIX 비트만 본다(ACL 은 보지 않음).

## 후속 보강과 알려진 한계 (TASK-0024)

- **레지스트리 삭제와 프로필(AC-1)**: `forge skill|tool|mcp remove` 는 삭제 확인(`[y/N]`) **앞에서**, `--yes` 일 때도, 항목을 가진 프로필과 그 프로필이 활성인 위치를 "다음 프로필에서 제외됩니다: …" 로 보여 준다(`warnProfileMembers`). 삭제가 끝난 뒤에 doctor 가 "프로필 멤버가 삭제로 사라졌다"고 알리는 것은 **구현하지 못한다**: 삭제는 `ON DELETE CASCADE` 로 멤버 행을 지우고 삭제 이력(감사 행)을 남기지 않아 나중에는 무엇이 사라졌는지 알 근거가 없다. 근거가 남는 경우만 알린다: 멤버가 하나도 없는 활성 프로필은 `profile.health.<이름>` 이 "비어 있음" warn 으로 알린다. 삭제 감사 기록을 남기는 것은 스키마 변경이 필요한 별도 과제다.
- **PATH 바이너리 정책(AC-2)**: world-writable(0002) 디렉터리 안 클라이언트 바이너리는 이미 **실행하지 않고 warn** 한다(`unsafe-location`; sticky 비트가 있어도 같다. 테스트 `TestBinaryDirectoryModes` 등). 안전 쪽 기본값이며 `--allow-untested-version` 으로도 우회할 수 없다.
- **PATH 정책 한계(AC-10)**: 검사와 실행 사이의 교체(TOCTOU)는 막지 못한다 · macOS 커널 링크 깊이 한도(32홉)를 넘는 체인은 "찾을 수 없음"이 된다 · 그룹 구성원은 열거하지 않고 gid 0·macOS gid 80·소유자 단독 그룹(이름이 사용자 이름과 같음) 관례를 가정한다 · 소유자가 다른 일반 사용자인 경우(파일 소유자 규칙)는 테스트가 소유자를 바꿀 수 없어 **실제 환경에서 검증하지 못했다**(현재 uid 와의 비교로만 구현) · 리눅스에서 링크 자신의 모드 검사 예외(리눅스는 링크 모드를 무시)는 이 개발 기계(macOS)에서 **실행해 보지 못했다** · `<위험 디렉터리>/../<안전>` 처럼 위험 디렉터리에 들어갔다 곧바로 나오는 PATH 항목도 차단한다(`..` 의 의미가 그 디렉터리 안의 링크로 바뀔 수 있어 정규화하지 않는 설계의 안전 쪽 과잉 차단).
- **원장 정리(AC-5)**: 원장을 지우는 forge 명령은 아직 없다. `history.size` 경고의 해결은 CSV 내보내기·기간 좁히기까지만 안내한다. `forge history prune --before` 류는 삭제 의미론(호출 연결·dedup, ADR-F0002/F0003)을 먼저 정해야 해서 **이 태스크 범위 밖**이다. 사용자가 DB 파일을 직접 편집하라고 안내하지 않는다.
- **프로젝트 삭제와 활성화(AC-5, ADR-F0001 O-4)**: 정책은 "자동 삭제 없음, 명시적 정리 명령"이다(`forge profile prune-stale`, 위 `profile.stale`). 이동일 수 있어 자동으로 지우지 않는다.
- **`forge-ver-*` 임시 디렉터리(AC-12)**: `--version` 실행은 `TMPDIR` 아래 `forge-ver-<난수>` 디렉터리를 만들고 정상 종료·시간 초과·출력 초과에서는 항상 지운다. 확인 도중 forge 프로세스가 `SIGKILL`/정전으로 죽으면 그 디렉터리가 남을 수 있다(빈 디렉터리 또는 클라이언트가 쓴 캐시). **시작 시 자동 정리는 구현하지 않았다**: `TMPDIR` 는 공유될 수 있고(다른 사용자·다른 forge 프로세스의 실행 중인 디렉터리), 이름 접두사만으로는 "죽은 프로세스의 것"을 증명할 수 없어 잘못 지우면 진행 중인 `--version` 이 깨진다. 남은 것은 직접 확인 후 지운다: `ls -ld "${TMPDIR:-/tmp}"/forge-ver-*` 로 소유자·시각을 보고, 본인 소유이며 오래된 것만 `rmdir`/`rm -r` 한다.
- **대소문자 변형 프로젝트 행(AC-11)**: `profile.projects.duplicate` 는 알리기만 한다. 합치려면 행마다 활성화·프로젝트 프로필·스킬/도구/MCP 의 `project_id` 를 옮겨야 하고 충돌 규칙이 필요해 자동으로 하지 않는다. `stat` 할 수 없는 경로(권한 없음, 끊긴 마운트)는 같은 프로젝트인지 판정하지 못한다.
