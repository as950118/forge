# 동기화 어댑터 인터페이스 (TASK-0011)

`forge sync` 는 클라이언트마다 **어댑터**가 "무엇을 바꿀지"(Plan)만 정하고, 파일을 쓰는 모든 일(백업·저널·크래시 복구·잠금·원자적 쓰기·롤백·복원)은 공통 엔진(`internal/adapter`)이 한다.
그래서 안전 규칙(docs/sync.md)은 어댑터가 늘어나도 그대로 유지된다. 구현된 어댑터: `claude-code`(`internal/adapter/claudecode`), `opencode`(`internal/adapter/opencode`).
이 문서의 모든 문장은 코드와 대조했다(파일·심볼 이름을 함께 적음).

## Adapter 인터페이스 (`internal/adapter/adapter.go`)

```go
type Adapter interface {
    Name() string                                                      // --target 값, 백업 manifest/저널/상태 파일의 target
    Plan(ctx, in Inputs, req Request) (*Plan, error)                   // 읽기만 한다. 아무것도 쓰지 않는다
    AllowedPath(scope Scope, root, path string) (base string, err error) // 쓸 수 있는 경로인가, 그 아래 심볼릭 링크 검사의 기준 디렉터리
}
```

CLI(`internal/cli/sync.go`)는 추가로 `ResolveRoot(adapter.Request) (root string, notes []string, err error)` 메서드를 요구한다(`lockRoot`): 계획 전에 `target+scope+root` 로 잠금 이름을 정하기 위해서다. 없으면 `--apply` 가 오류로 끝난다. 어댑터 목록은 `syncAdapters()` 한 곳에서 등록한다(복원도 manifest 의 `target` 으로 이 목록에서 어댑터를 고른다).

### 입력 `Inputs` / `Request`

- `adapter.LoadInputs(ctx, forgeHome, client)` 가 레지스트리를 **읽기 전용** DB 핸들로 읽어 `Inputs{Client, Skills, Servers}` 를 만든다. 활성(enabled) 스킬의 현재 버전(`SkillInput{Name, Version, Hash, StoreDir}`)과 활성 MCP 서버(`*mcp.Info`)만 들어간다. `Client` 는 MCP 노출 설정(`mcp.exposure.<서버>.<client>`; 기본 direct)을 찾는 키라서 **어댑터 이름이 레지스트리 `clients` 테이블의 id 와 같아야 한다**(`claude-code`, `opencode`).
- `Request`: `Scope`(project|user), `ProjectDir`, `Home`, `ForgeBin`, `NoHooks`/`RemoveHooks`, `BackupDir`, `SyncInProgress`, `ForceManaged`, `Now`. 타깃이 지원하지 않는 요청 필드는 **조용히 무시하지 말고 오류**로 거부한다(opencode 는 `NoHooks`/`RemoveHooks` 를 거부한다).

### 출력 `Plan`

`Plan` 은 순수 데이터다: `Items`(표에 보이는 항목: `Kind` skill|mcp|hook|command, `Action` add|update|remove|conflict|skip|unchanged|info, `Reason`, `Path`, `Overridable`), `Ops`(파일 단위 작업: `write-file`, `delete-file`, `replace-tree`, `delete-tree`; 각 작업은 계획 시점의 `Before` 스냅샷을 가진다), `Commands`(사용자가 실행할 명령, 출력만), `Notes`, 그리고 크래시 복구·상태 파일 정보(`NewState`, `StatePath`, `JournalPath/JournalBase`, `Interrupted`, `RecoverIDs`, `StaleTemps`, `TempDirs`). 사용자 소유 내용은 `Ops` 에 넣지 않고 `conflict` 항목으로만 표시한다.

## 타깃이 구현해야 하는 것

1. **Name / Scopes**: `Name()` 은 안정적인 id. `ScopeProject`(저장소 루트 아래)와 `ScopeUser`(홈 아래)를 모두 처리하거나 한쪽을 명확히 거부한다. 사용자 스코프에서 비밀이 섞였거나 구조가 미확인인 설정 파일은 열지 않고 **명령만 출력**(`Plan.Commands`)한다.
2. **루트 해석과 금지 루트**: `.git` 이 있는 상위 디렉터리를 프로젝트 루트로 쓰되, 홈·Forge 홈·클라이언트의 사용자 디렉터리와 **같은 디렉터리**(`EvalSymlinks` 후 `os.SameFile`)는 거부한다.
3. **Inputs 수집**: 위 `LoadInputs` 를 쓴다. 어댑터가 자체적으로 레지스트리나 DB 를 열지 않는다.
4. **Plan 렌더**: 항목마다 현재 파일 상태를 읽어 `add/update/remove/unchanged/conflict/skip` 을 정하고, 바꿀 것만 `Ops` 에 담는다. 렌더 직전에 `mcp.Spec.Validate` 와 비밀·치환 문법 검사를 다시 한다(값뿐 아니라 env/헤더 **이름**도 `adapter.CheckValueKey` 로 모든 참조 종류에 대해 검사: 치환 문법·제어 문자·공백·따옴표·역슬래시·비ASCII·128자 초과 거부; env 는 식별자, 헤더는 HTTP token)(레지스트리 검증은 첫 번째 방어선일 뿐). JSON 설정은 `jsonedit`(바이트 오프셋 편집)로만 고친다. 상태 파일 쓰기는 **마지막 작업**이어야 한다(`finishState`).
5. **허용 경로 `AllowedPath`**: 이 어댑터가 쓸 수 있는 경로의 **전체 목록**(설정 파일, 상태 파일, 스킬 디렉터리 1단계)만 통과시키고, 첫 줄에서 `adapter.GuardPath` 를 호출한다. 엔진은 Apply·Restore·저널 검증 때 이 함수로 경로를 다시 검사하므로 변조된 manifest/저널이 다른 경로를 가리킬 수 없다. 반환하는 `base` 아래의 경로 구성요소가 심볼릭 링크이면 엔진이 거부한다.
6. **금지 경로 가드**: 모든 파일 입출력은 `GuardPath` 를 거친다. 판정은 이름이 아니라 **(디렉터리, 파일 이름) 조합과 파일 동일성**(`os.SameFile`; 하드링크·심볼릭 링크)이다 — 같은 이름이라도 클라이언트의 설정·데이터 디렉터리 밖의 파일은 정상이다(`credential-helper` 스킬). 추가로 `ReadFileSafe` 는 `st_nlink > 1` 인 파일을 `ErrHardLinked`("하드링크된 설정 파일은 지원하지 않습니다")로 거부하고, `TreeHash`/`copyTree` 는 트리 안 각 파일에 동일성 검사를 한다. 새 클라이언트의 비밀 파일은 `internal/adapter/guard.go` 에만 추가한다(다른 소스에 이름을 적지 않는다; 테스트가 확인한다).
7. **상태 파일 이름 규칙**: 타깃마다 **서로 다른 파일**을 쓴다. 프로젝트는 `<repo>/.forge/sync-state[.<target>].json`·`sync-journal[.<target>].json`(claude-code 는 호환을 위해 접미사 없음, opencode 는 `.opencode`), 사용자는 `$FORGE_HOME/sync-state/<target>-user.json`·`<target>-user.journal.json`. 내용은 `adapter.State`(종류·이름·해시·시각, 비밀 없음). `ParseState` 는 target/scope 가 다르면 거부한다. 임시 파일 이름(`.forge-tmp-*`/`.forge-old-*`)과 백업 manifest·저널의 `target` 은 엔진이 기록한다.
8. **충돌 의미**: 상태 파일에 없는 항목 = 사용자 소유 → `conflict`(덮어쓰기 불가, `Overridable=false`). 상태에 있으나 현재 내용 해시가 다르면(손 수정) `conflict` + `Overridable=true`(`--force-managed` 로만 덮어씀, 백업됨). 스킬은 마커 `.forge-managed` + 상태 + 트리 해시가 모두 맞을 때만 관리 항목이다. 충돌 항목은 어떤 경우에도 `Ops` 에 들어가지 않고, 충돌이 있으면 Apply 는 `--skip-conflicts` 없이는 아무것도 쓰지 않는다(`ErrConflicts`).
9. **중복 노출 방지(해당 시)**: 클라이언트가 다른 클라이언트의 설정 경로도 읽는다면(OpenCode 는 `.claude/skills` 를 읽음) 다른 타깃의 **상태 파일을 읽기만** 해서 이미 노출된 항목을 건너뛴다(`opencode/opencode.go` `loadClaims`). 상태 파일 경로는 그 어댑터가 공개한 함수로만 얻는다(`claudecode.Adapter.StatePath`).

## 엔진이 제공하는 것 (`internal/adapter`)

| 기능 | 위치 | 내용 |
|------|------|------|
| 검증 후 백업 | `engine.go` `applyOps` | 쓰기 전에 모든 `Before` 해시를 재검증(`ErrStale`), 바꿀 기존 파일을 `$FORGE_HOME/backups/<id>/` 에 복사하고 manifest(target·scope·root·항목별 before/after)를 쓴다. 백업 실패 시 아무것도 바꾸지 않는다 |
| 저널·크래시 복구 | `journal.go`, `engine.go` | 첫 쓰기 전에 저널(`pending`)을 백업 디렉터리와 상태 파일 옆에 쓰고, 다음 plan 이 `DetectInterrupted`(같은 target·scope·root)로 중단을 감지, apply 가 "앞으로 완료". 위조 저널은 `AllowedPath` 검증과 해시 일관성 검사로 무시 |
| 롤백 | `engine.go` `rollback` | 중간 실패 시 백업으로 되돌리고 저널을 `rolled_back` 으로 표시 |
| 복원 | `restore.go` | `--restore`: manifest 를 `ValidateManifest`(target 일치, 절대·정규 경로, `GuardPath`, `AllowedPath`)로 검증하고 해시 드리프트는 `--force` 요구, 복원 직전 상태를 새 백업으로 보존. **다른 target 의 백업은 거부**(`백업 대상("…")과 어댑터("…")가 다릅니다`) |
| 잠금 | `lock*.go` | `AcquireLock(<forge home>/locks, LockKey(target, scope, root))` — 같은 target+root 만 직렬화 |
| 원자적 쓰기 | `fsx.go` | 같은 디렉터리 임시 파일 + fsync + rename, 스킬 디렉터리는 임시 디렉터리 복사 후 rename. 심볼릭 링크 검사(`CheckNoSymlinks`)·읽기 전용 신호(`WriteBlocker`) |
| 임시 파일 정리 | `temps.go` | 죽은 pid 가 남긴 정확한 이름 형식만 정리(링크는 따라가지 않음) |
| diff·마스킹 | `diff.go`, `render.go` | 파일별 통합 diff. 키 이름이 TOKEN/KEY/SECRET/PASSWORD/AUTH/COOKIE/CREDENTIAL… 를 포함하는 키의 문자열 값(`apiKey`, `api_key`, `Authorization` 포함)과 그 키 아래 하위 트리 전체(중첩 객체·배열)의 문자열·숫자 값을 JSON 값 단위로 `***` 처리. `${VAR}`·`{env:VAR}` 참조는 비밀이 아니므로 그대로 보임. 표시용일 뿐 쓰는 내용은 바뀌지 않음 |
| JSON 편집 | `jsonedit/` | 키 순서·들여쓰기(2/4/탭)·CRLF·마지막 줄바꿈을 보존하며 멤버 삽입/교체/삭제. 엄격한 JSON 이 아니면 오류(아무것도 쓰지 않음) |
| 출력 | `render.go` | 계획 표, JSON 리포트(`schema: 1`), 백업 목록, 복원 결과 |

테스트 훅: `ApplyOptions.Inject`(단계별 실패 주입), `ApplyOptions.Crash`(모든 단계 경계에서 SIGKILL 모사). 새 어댑터의 크래시 매트릭스 테스트는 이 훅을 그대로 쓴다(`opencode/recovery_test.go`).

## 새 타깃(Codex, Gemini CLI 등) 추가 체크리스트

1. **형식 검증 스파이크 문서**: `docs/clients/<client>.md` 에 경로, 설정 키, 값 치환 문법, CLI 옵션, 비밀이 들어 있는 파일을 `[doc]`/`[help]`/`[local]`/`[bin]`/`[미확인]` 으로 구분해 적는다. **클라이언트 바이너리는 실행하지 않는다**(필요하면 `strings` 로 읽기만).
2. **레지스트리 클라이언트 id**: `db` 의 `clients` 테이블에 id 가 있어야 노출 설정(`mcp expose`)이 동작한다(스키마 변경은 별도 태스크).
3. **어댑터 구현**: `internal/adapter/<client>/` — `Name`, `Plan`, `AllowedPath`, `ResolveRoot`; 상태·저널 파일 이름을 다른 타깃과 겹치지 않게 정한다. `[미확인]` 항목은 추정하지 않고 렌더하지 않는다(계획의 참고로 알린다).
4. **가드**: 비밀 파일의 (디렉터리, 이름)을 `guard.go` 에 추가하고(동일 파일 판정 포함; 이름만으로 막지 않는다) 금지 루트를 어댑터 `forbiddenRoot` 에 추가한다. 프로젝트 설정 파일이 오탐으로 막히지 않는지 시험한다.
5. **등록**: `internal/cli/sync.go` 의 `syncAdapters`, `supportedTargets`, 도움말, 지원하지 않는 플래그 오류.
6. **픽스처**: `testdata/<버전>/` 에 기존 사용자 설정(가짜 비밀 포함)과 골든 결과, `NOTES.txt` 에 근거와 `[미확인]`. `.gitignore` 에 걸리는 이름은 `dot-` 접두.
7. **테스트(최소)**: plan 무쓰기, apply 멱등, 사용자 항목 보존(키 순서·들여쓰기·CRLF), 충돌 5종, 파싱 불가 무변경, 심볼릭 링크 거부, 백업·복원·롤백, 크래시 매트릭스, 저널 위조, 금지 루트·가드(하드링크 포함), diff 마스킹, 변환 정확성(특수 문자 인자), 사용자 스코프 명령 `sh -n`, 다른 타깃과의 교차 실행·중복 노출 방지, 실제 HOME 비접근.
8. **위협 점검**: 비밀이 파일·diff·명령·오류·상태 파일에 새지 않는지, 클라이언트의 치환 문법(`{env:}`/`{file:}`/`${}`)으로 값이 바뀌어 읽히지 않는지, 설정 파일 우선순위·확장자(JSONC)로 엉뚱한 파일을 쓰지 않는지, 사용자 디렉터리가 프로젝트 루트로 쓰이지 않는지.
9. **문서**: `docs/sync.md` 에 스코프별 대상 표·근거 표·한계를 추가한다.

## claude-code 와 opencode 의 차이

| 항목 | claude-code | opencode |
|------|-------------|----------|
| 스킬 경로(project / user) | `<repo>/.claude/skills/<name>/` / `~/.claude/skills/` | `<repo>/.opencode/skills/<name>/` / `$XDG_CONFIG_HOME/opencode/skills/`(설정·유효할 때) 또는 `~/.config/opencode/skills/` (`.claude/skills` 도 읽으므로 이미 claude-code 가 관리하는 스킬은 건너뜀) |
| MCP 파일 | `<repo>/.mcp.json` 의 `mcpServers` | `<repo>/opencode.json` 의 최상위 `mcp` (`opencode.jsonc` 는 미지원) |
| 서버 형식 | `type: stdio\|http\|sse`, `command`(문자열), `args`(배열), `env`, `url`, `headers` | `type: local\|remote`, `command`(**배열** = command+args), `environment`, `url`, `headers`, `enabled: true` |
| 값 참조 | `${VAR}` | `{env:VAR}` (값 전체가 단일 참조) |
| http / sse | 구분해서 기록 | 둘 다 `remote` (구분 키 없음, 전송 자동 선택 여부 [미확인]) |
| cwd / timeout | cwd 있는 서버는 건너뜀, timeout 렌더 안 함 | 둘 다 렌더 안 함(cwd 서버는 건너뜀, timeout 은 참고로 알림) — 문서 기준 미교차확인 |
| 게이트웨이 항목 | `forge` = `{type:stdio, command:<bin>, args:[mcp,serve,--client,claude-code]}` | `forge` = `{type:local, command:[<bin>,mcp,serve,--client,opencode], enabled:true}` |
| hook | `settings.json` 의 `hooks` (handler 단위 병합) | 없음(`--no-hooks`/`--remove-hooks` 는 오류) |
| user 스코프 MCP | `claude mcp add --scope user ...` 명령 출력 | `opencode mcp add <이름> --global ...` 명령 출력 |
| 프로젝트 상태 / 저널 | `.forge/sync-state.json` / `.forge/sync-journal.json` | `.forge/sync-state.opencode.json` / `.forge/sync-journal.opencode.json` |
| user 상태 / 저널 | `$FORGE_HOME/sync-state/claude-code-user.json` / `.journal.json` | `$FORGE_HOME/sync-state/opencode-user.json` / `opencode-user.journal.json` |
| 읽지도 쓰지도 않는 파일 | `~/.claude.json`(이름·동일성) | OpenCode 설정 디렉터리(`~/.config/opencode`, `$XDG_CONFIG_HOME/opencode`) 안의 전역 `opencode.json(c)`·`config.json`, `service.json`, `mcp-auth.json`, `auth.json`, `opencode.db(-wal/-shm)`, `credential*`, 데이터·상태 디렉터리 전체, 그리고 이 파일들과 동일한 파일(하드링크·링크) |
| 반영 | Claude Code 가 최초 사용 시 승인 요청 | 사용자가 `opencode reload` 직접 실행(Forge 는 실행하지 않음) |

## 현재 알려진 한계

- 어댑터 두 개의 계획·스킬·MCP 코드는 구조가 같지만 **복사**되어 있다(`opencode/skills.go`, `mcp.go`, `docfile.go`, `opencode.go` 는 claudecode 의 해당 파일에서 파생). 세 번째 타깃이 생기면 공통 계층(`planner` 의 상태·저널 처리, 스킬 계획, JSON 문서 로더)을 `internal/adapter` 로 올리는 리팩터링이 필요하다.
- 중복 노출 방지는 **opencode → claude-code 방향만** 구현되어 있다. opencode 가 먼저 스킬을 `.opencode/skills` 에 썼다면, 이후 claude-code 를 sync 하면 같은 스킬이 두 경로에 있게 된다. 이어서 opencode 를 다시 sync 하면 중복이 제거된다(수렴). claude-code 어댑터에는 같은 방지가 없다(범위 밖; 후속 과제). 같은 프로젝트의 project 스코프 opencode 와 user 스코프 claude-code(`~/.claude/skills`) 사이의 중복은 검사하지 않는다.
- opencode: `opencode.jsonc`(주석)는 편집하지 않는다(주석 보존 불가) — `opencode.json` 이 없으면 중단, 둘 다 있으면 우선순위 [미확인] 경고. `[미확인]` 항목(cwd, timeout, oauth, tools 글롭, 플러그인)은 다루지 않는다. 버전 게이팅(`opencode --version`)은 하지 않는다(doctor, TASK-0012).
- 프로젝트 스코프 레지스트리·프로필별 노출은 아직 없다(docs/sync.md 한계).
