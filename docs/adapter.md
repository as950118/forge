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

- `adapter.LoadInputsFor(ctx, forgeHome, client, project)`(`LoadInputs` 는 project 없이 부르는 얇은 래퍼)가 레지스트리를 **읽기 전용** DB 핸들로 읽어 `Inputs{Client, Skills, Servers, Profile}` 를 만든다. 활성(enabled) 스킬(`SkillInput{Name, Version, Hash, StoreDir}`)과 활성 MCP 서버(`*mcp.Info`)만 들어가며, (client, project)에 적용되는 프로필이 있으면(`docs/profile.md` §1 우선순위) 프로필 멤버만 남고(허용 목록), 스킬은 `direct` 노출 멤버가 핀 버전(없으면 현재 버전)으로, MCP 서버는 `Exposure[client]` 가 프로필의 노출로 바뀐다. 프로필이 없으면 기존처럼 현재 버전과 MCP 노출 설정(`mcp.exposure.<서버>.<client>`; 기본 direct)을 쓴다. `Client` 는 노출을 찾는 키라서 **어댑터 이름이 레지스트리 `clients` 테이블의 id 와 같아야 한다**(`claude-code`, `opencode`). 어댑터는 `Inputs.Profile` 을 읽지 않는다(Inputs 가 이미 반영돼 있다); CLI 가 적용 규칙을 계획 참고로 표시한다.
- `Request.ClaimedSkills`: 같은 실행에서 다른 타깃(claude-code)이 계획한 스킬 이름. opencode 어댑터는 디스크의 claude-code 상태 파일이 알려 주는 것에 더해 이것도 "claude-code 가 관리"로 취급해 다시 렌더링하지 않는다(`forge sync --target all` 의 통합 계획용).
- `Request`: `Scope`(project|user), `ProjectDir`, `Home`, `ForgeBin`, `NoHooks`/`RemoveHooks`, `BackupDir`, `SyncInProgress`, `ForceManaged`, `Now`. 타깃이 지원하지 않는 요청 필드는 **조용히 무시하지 말고 오류**로 거부한다(opencode 는 `NoHooks`/`RemoveHooks` 를 거부한다).

### 출력 `Plan`

`Plan` 은 순수 데이터다: `Items`(표에 보이는 항목: `Kind` skill|mcp|hook|command, `Action` add|update|remove|conflict|skip|unchanged|info, `Reason`, `Path`, `Overridable`), `Ops`(파일 단위 작업: `write-file`, `delete-file`, `replace-tree`, `delete-tree`; 각 작업은 계획 시점의 `Before` 스냅샷을 가진다), `Commands`(사용자가 실행할 명령, 출력만), `Notes`, 그리고 크래시 복구·상태 파일 정보(`NewState`, `StatePath`, `JournalPath/JournalBase`, `Interrupted`, `RecoverIDs`, `StaleTemps`, `TempDirs`). 사용자 소유 내용은 `Ops` 에 넣지 않고 `conflict` 항목으로만 표시한다.

## 타깃이 구현해야 하는 것

1. **Name / Scopes**: `Name()` 은 안정적인 id. `ScopeProject`(저장소 루트 아래)와 `ScopeUser`(홈 아래)를 모두 처리하거나 한쪽을 명확히 거부한다. 사용자 스코프에서 비밀이 섞였거나 구조가 미확인인 설정 파일은 열지 않고 **명령만 출력**(`Plan.Commands`)한다.
2. **루트 해석과 금지 루트**: `adapter.ResolveRoot` 가 `.git` 이 있는 상위 디렉터리를 프로젝트 루트로 쓰고, 홈·Forge 홈과 **같은 디렉터리**(`EvalSymlinks` 후 `os.SameFile`)는 거부한다. 타깃은 클라이언트의 사용자 디렉터리 목록(`adapter.ForbiddenDir`)만 넘긴다.
3. **Inputs 수집**: 위 `LoadInputs` 를 쓴다. 어댑터가 자체적으로 레지스트리나 DB 를 열지 않는다.
4. **Plan 렌더**: 항목마다 현재 파일 상태를 읽어 `add/update/remove/unchanged/conflict/skip` 을 정하고, 바꿀 것만 `Ops` 에 담는다. 렌더 직전에 `mcp.Spec.Validate` 와 비밀·치환 문법 검사를 다시 한다(값뿐 아니라 env/헤더 **이름**도 `adapter.CheckValueKey` 로 모든 참조 종류에 대해 검사: 치환 문법·제어 문자·공백·역슬래시·비ASCII·128자 초과·`__proto__` 거부; env 는 식별자, 헤더는 HTTP token — 따옴표 `'` 는 RFC 9110 token 문자라 헤더 이름에서는 허용되고(env 이름은 식별자 규칙이 거부), 큰따옴표는 어느 쪽에서도 거부된다)(레지스트리 검증은 첫 번째 방어선일 뿐). JSON 설정은 `jsonedit`(바이트 오프셋 편집)로만 고친다. 상태 파일 쓰기는 **마지막 작업**이어야 한다(`Planner.FinishState`).
5. **허용 경로 `AllowedPath`**: 이 어댑터가 쓸 수 있는 경로의 **전체 목록**(설정 파일, 상태 파일, 스킬 디렉터리 1단계)만 통과시킨다. `adapter.AllowedPath` 가 첫 줄에서 `adapter.GuardPath` 를 호출하므로 타깃은 설정 파일 경로 목록만 넘긴다. 엔진은 Apply·Restore·저널 검증 때 이 함수로 경로를 다시 검사하므로 변조된 manifest/저널이 다른 경로를 가리킬 수 없다. 반환하는 `base` 아래의 경로 구성요소가 심볼릭 링크이면 엔진이 거부한다.
6. **금지 경로 가드**: 모든 파일 입출력은 `GuardPath` 를 거친다. 판정은 이름이 아니라 **(디렉터리, 파일 이름) 조합과 파일 동일성**(`os.SameFile`; 하드링크·심볼릭 링크)이다 — 같은 이름이라도 클라이언트의 설정·데이터 디렉터리 밖의 파일은 정상이다(`credential-helper` 스킬). 추가로 `ReadFileSafe` 는 `st_nlink > 1` 인 파일을 `ErrHardLinked`("하드링크된 설정 파일은 지원하지 않습니다")로 거부하고, `TreeHash`/`copyTree` 는 트리 안 각 파일에 동일성 검사와 같은 `nlink > 1` 검사(`ErrHardLinked`)를 하고, diff 의 트리 읽기도 하드링크된 파일은 읽지 않는다. `ErrHardLinked` 메시지는 조치("링크를 풀고 복사본으로 교체하세요")가 앞, 경로는 한 번만 뒤에 붙는다. 새 클라이언트의 비밀 파일은 `internal/adapter/guard.go` 에만 추가한다(다른 소스에 이름을 적지 않는다; 테스트가 확인한다).
7. **상태 파일 이름 규칙**: 타깃마다 **서로 다른 파일**을 쓴다. 프로젝트는 `<repo>/.forge/sync-state[.<target>].json`·`sync-journal[.<target>].json`(claude-code 는 호환을 위해 접미사 없음, opencode 는 `.opencode`), 사용자는 `$FORGE_HOME/sync-state/<target>-user.json`·`<target>-user.journal.json`. 내용은 `adapter.State`(종류·이름·해시·시각, 비밀 없음). `ParseState` 는 target/scope 가 다르면 거부한다. 임시 파일 이름(`.forge-tmp-*`/`.forge-old-*`)과 백업 manifest·저널의 `target` 은 엔진이 기록한다.
8. **충돌 의미**: 상태 파일에 없는 항목 = 사용자 소유 → `conflict`(덮어쓰기 불가, `Overridable=false`). 상태에 있으나 현재 내용 해시가 다르면(손 수정) `conflict` + `Overridable=true`(`--force-managed` 로만 덮어씀, 백업됨). 스킬은 마커 `.forge-managed` + 상태 + 트리 해시가 모두 맞을 때만 관리 항목이다. 충돌 항목은 어떤 경우에도 `Ops` 에 들어가지 않고, 충돌이 있으면 Apply 는 `--skip-conflicts` 없이는 아무것도 쓰지 않는다(`ErrConflicts`).
9. **중복 노출 방지(해당 시)**: 클라이언트가 다른 클라이언트의 설정 경로도 읽는다면(OpenCode 는 `.claude/skills` 를 읽음) 다른 타깃의 **상태 파일을 읽기만** 해서 이미 노출된 항목을 건너뛴다(`opencode/opencode.go` `loadClaims`, 읽기는 `adapter.ReadForeignState`). 상태 파일 경로는 그 어댑터가 공개한 함수로만 얻는다(`claudecode.Adapter.StatePath`).

## 공통 계층: 보안 로직은 한 곳에 (TASK-0023)

TASK-0011 은 claudecode 어댑터를 opencode 로 복사해(약 800줄) 한쪽의 보안 수정이 다른 쪽에 빠질 위험이 있었다. 지금은 아래 보안 로직이 `internal/adapter` 에 **한 번만** 있고, 타깃 패키지는 형식 변환(스키마·경로·치환 문법)과 훅(데이터/함수)만 가진다.

| 보안 로직 | 공통 위치 | 타깃이 넘기는 것(형식) |
|-----------|-----------|------------------------|
| JSON 설정 로더(크기 제한 `ReadFileSafe`·`GuardPath`·심볼릭 링크·읽기 전용·엄격 JSON) | `docfile.go` `LoadDoc`/`DocFile` | 파일 경로 |
| 스킬 마커·트리 해시·관리 판정(`planSkill`/`planStaleSkill`), 레지스트리 스냅샷 해시 검증, 스킬 `GuardPath` | `skills.go` `Planner.PlanSkills` | `SkillPolicy`(링크 안내 문구, 다른 타깃이 이미 노출한 스킬 `Claimed` 와 그 사유) |
| 저널 인정(`Adopt`·`JournalListed`·`JournalOrphans`)과 관리 항목 해시 충돌 판정(제거·갱신: `ManagedModified`) | `planner.go` | 없음(항목 종류·이름·현재 해시) |
| MCP `renderServer` 재검증(`Spec.Validate`)·`CheckValueKey`·`envNameRe`·서버 이름 규칙·비밀 literal·치환 문법·제어 문자 거부 | `mcpfile.go` `RenderServer` | `MCPFormat`(스키마 `Build`, 참조 문법 `Ref`, 치환 정규식 `Subst`, `SubstInCommand`, cwd 사유) |
| MCP 파일 계획(소유권·해시 충돌·추가/갱신/제거·`jsonedit` 편집) 과 user 스코프 명령 출력 | `mcpfile.go` `PlanMCPFile`/`PlanMCPCommands` | `MCPFile`(경로·최상위 키·파일 라벨), `MCPTarget`(게이트웨이 항목·렌더 함수) |
| `AllowedPath`·금지 루트·`ResolveRoot`·`--bin` 검사 | `planner.go` `AllowedPath`/`ResolveRoot`/`ValidateBin` | 쓸 수 있는 설정 파일 목록, `ForbiddenDir` 목록, 추가 `--bin` 검사 |
| 상태 읽기·`FinishState`·임시 디렉터리 심볼릭 검사·중단 sync 탐지(`Preflight`) | `planner.go` `Planner` | `Paths`(루트·스킬 디렉터리·상태·저널 경로), 임시 파일 디렉터리 목록 |
| 다른 타깃 상태 파일 읽기(중복 노출 방지) | `planner.go` `ReadForeignState` | 상태 파일 경로 |

타깃에 남은 것: `claudecode` — `.mcp.json`(`mcpServers`, `${VAR}`)·`settings.json` 훅(`hooks.go`; 훅은 claude-code 에만 있어 타깃에 둔다, 해시 충돌 판정은 공통 `ManagedModified` 를 쓴다)·경로 표; `opencode` — `opencode.json`(`mcp`, `{env:VAR}`, local/remote)·`opencode.jsonc` 감지·XDG 스킬 경로·`loadClaims`. 이 둘은 해당 클라이언트의 사실이라 공통화하지 않았다.

### 코드량·보안 로직 보유 (AC-2; 테스트 제외 `.go` 줄 수, `wc -l`)

| | 이전(TASK-0026 머지 직후) | 이후 |
|---|---|---|
| `internal/adapter/claudecode` | 1592줄 (claudecode.go 445, skills.go 252, mcp.go 407, hooks.go 423, docfile.go 65) | 658줄 (claudecode.go 146, mcp.go 91, hooks.go 421) |
| `internal/adapter/opencode` | 1364줄 (opencode.go 581, skills.go 275, mcp.go 443, docfile.go 65) | 418줄 (opencode.go 282, mcp.go 136) |
| `internal/adapter` 공통(엔진 포함) | 3691줄 | 4848줄 (신규 `planner.go` 393, `skills.go` 281, `mcpfile.go` 418, `docfile.go` 65) |
| 합계 | 6647줄 | 5924줄 |

보안 로직 보유: 이전에는 두 어댑터가 위 표의 모든 항목을 **각자** 구현했다(8개 중 8개 복제). 이후 두 어댑터는 그 항목을 구현하지 않고 호출만 한다. 어댑터에 남은 판단은 형식 사실(어느 파일, 어느 키, 어느 치환 문법을 거부할지의 정규식)뿐이다. 새 보안 수정은 공통 파일 한 곳에 들어가며, 변이 시험(아래)으로 두 타깃 테스트가 모두 잡는지 확인했다.

### 변이 시험 (AC-3)

공통 로직을 한 번에 하나씩 망가뜨리고(적용 → `go test ./internal/adapter/... -short -count=1 -p 1` → 원복) 두 타깃의 테스트가 **모두** 실패하는지 확인했다. 기존 테스트는 일부 변이를 한 타깃만, 또는 어느 쪽도 잡지 못했다(공통 가드 호출이 `ReadFileSafe`·`Snapshot` 의 가드와 겹쳐 있어서). 그래서 같은 시나리오를 모든 타깃에 돌리는 `internal/adapter/conformance_test.go`(`TestShared*`)를 추가했다. 새 타깃은 `conformTargets` 에 한 줄을 더해 같은 시험을 통과해야 한다.

| 변이(공통 코드) | 기존 테스트가 잡은 타깃 | 변이 시험 후 |
|-----------------|------------------------|--------------|
| 스킬 대상 `GuardPath` 제거 (`skills.go`) | 없음 | 둘 다: `TestSharedGuardedSkillTarget` |
| 스킬 `StoreDir` `GuardPath` 제거 | 없음 | 둘 다: `TestSharedGuardedStoreDir` |
| `AllowedPath` 의 `GuardPath` 제거 | 없음 | 둘 다: `TestSharedGuardedSkillTarget` |
| `AllowedPath` 스킬 이름 규칙 제거 | 없음 | 둘 다: `TestSharedAllowedPathList` |
| `renderServer` 재검증(`Spec.Validate`) 제거 | 없음 | 둘 다: `TestSharedRenderRefusals` |
| `CheckValueKey` 제거 | 둘 다 (`TestRenderRefusesMaliciousValueKeysFromTheDatabase`) | 둘 다 |
| 서버 이름 규칙·`envNameRe` 제거 | 없음 | 둘 다: `TestSharedRenderRefusals` |
| 제어 문자 거부 제거 | opencode 만 | 둘 다: `TestSharedRenderRefusals` |
| literal 치환 문법 거부 제거 | opencode 만 | 둘 다: `TestSharedRenderRefusals` |
| 관리 항목 해시 충돌 판정(`ManagedModified`) 전부 생략 | 둘 다 (`TestManagedItemsEditedByHandAreConflicts`, `TestConflicts` 등) | 둘 다 |
| 정리 대상 MCP 서버의 해시 충돌 판정 제거(`PlanMCPFile` 제거 경로) | 없음 | 둘 다: `TestSharedStaleServerEditedByHandIsConflict` |
| 마커 `tree_hash` 증거 제거(`ManagedModified`; 갱신·제거 경로 각각) | 없음 | 둘 다: `TestSharedMarkerTreeHashEditIsConflict` |
| `planSkill` 마커 검사 제거 | claude-code 만 | 둘 다: `TestSharedSkillWithMissingOrForeignMarkerIsNeverOverwritten` |
| 저널 보증(`ManagedModified` 의 저널 절) 제거 | 없음 | 둘 다: `TestSharedJournalVouchesForCrashedUpdate` |
| `Adopt` 의 저널 해시 일치 검사 제거(위조 저널) | 둘 다 (`TestRecoveryKeepsUserEdits`) | 둘 다 |
| `LoadDoc` 심볼릭 링크 검사 생략 | 둘 다 (`TestSymlinksAreRefused`) | 둘 다 |
| 스킬 심볼릭 링크 검사 생략 | 없음 | 둘 다: `TestSharedSkillBehindSymlinkIsRefused` |
| 상태 파일 심볼릭 링크 검사 제거 | opencode 만 | 둘 다: `TestSharedStateFileBehindSymlinkIsRefused` |
| 스냅샷 해시 검증 제거 | claude-code 만 | 둘 다: `TestSharedTamperedSnapshotIsRefused` |
| 정리 대상 스킬의 마커 검사 제거 | claude-code 만 | 둘 다: `TestSharedStaleSkillWithoutMarkerIsKept` |
| 금지 프로젝트 루트 검사 제거 | 둘 다 | 둘 다 |
| `--bin` 제어 문자 검사 제거 | 둘 다 | 둘 다 |

TASK-0022 가 추가한 `TestShared*`/단위 시험의 변이 시험(각 변이는 한 번에 하나, 적용 → `go test ./internal/adapter/... -short -count=1 -p 1` → 원복 후 `cmp`). 결과:

| 변이 | 변이 시험 후 실패한 테스트 (adapter 패키지 하나에서 두 타깃이 서브테스트로 실행됨) |
|---|---|
| `detectLostBackup` 호출 제거 (`planner.go`) | 둘 다: `TestSharedLostBackupOfInterruptedSyncIsExplained` |
| `latest` 에서 중단된 복원의 안전 백업 제외 제거 (`backup.go`) | 둘 다: `TestSharedInterruptedRestoreResumesWithTheRightBackup` |
| 저널의 `RestoreOf` 기록 제거 (`journal.go`) | 둘 다: 같은 시험 |
| 복원 완료 시 이전 중단 복원 저널 정리 제거 (`restore.go`) | 둘 다: 같은 시험 |
| diff 줄 단위 값 패턴 마스킹(`masker.line`) 제거 | `TestDiffLineMaskerCatchesValuesWithoutKeyOrJSON` (공통 diff 코드: 타깃 무관) |
| 짧은 키 단어(`pwd`·`pw`·`pass`) 규칙 제거 | `TestDiffMasksPwdKeyNestedPwAndShortFlag`, `TestDiffMasksDirectNumberUnderSecretKey` |
| `-p` 플래그 규칙 제거 / `--pw=값` 규칙 제거 | `TestDiffMasksPwdKeyNestedPwAndShortFlag` |
| 키가 직접 가진 숫자 값 마스킹 제거 | `TestDiffMasksDirectNumberUnderSecretKey` |
| `author(s)` 등 평범한 키 예외 제거 | `TestDiffKeepsOrdinaryWordsReadable` |
| 비밀 값 변경 요약(`hiddenSecretChanges`) 제거 | `TestDiffAnnouncesSecretOnlyChange` |
| `TreeHash` 의 `nlink` 검사 제거 | `TestSkillTreeHardLinkIsRefused`, 둘 다: `TestSharedHardLinkedFileInsideSkillTreeIsRefused` |
| `copyTree` 의 `nlink` 검사 제거 | `TestSkillTreeHardLinkIsRefused` |
| diff `readTreeFiles` 의 `nlink` 검사 제거 | `TestSkillTreeHardLinkIsRefused` |
| `ErrHardLinked` 문구의 조치 앞배치 제거 | `TestHardLinkMessageRemedyFirstPathOnce` |
| 백업 디렉터리 임시 파일 정리 제거(적용 쪽 / 계획 목록 쪽 각각) | 둘 다: `TestSharedBackupDirTempsAreListedAndCleaned` |
| `__proto__` 이름 거부 제거 | `TestCheckValueKeyRefusesProto` |
| 권한 오류를 심볼릭 링크로 오보하지 않는 분기 제거 | 둘 다: `TestSharedUnreadableSkillsDirIsNotCalledASymlink` |
| opencode 의 상태 파일 스킬 주장 검증(`IsManagedSkillDir`) 제거 | `TestOpenCodeForgedClaimDoesNotSkipSkill` |
| `ParseState` 의 target/scope 불일치 거부 제거 | 둘 다: `TestSharedStateFileOfAnotherTargetOrScopeIsRejected` (claude-code 는 기존 `TestStateFileCorruptionAborts` 도 잡음) |
| `JournalOrphans` 의 저널 해시 일치 검사 제거 | 둘 다: `TestSharedHandEditedJournalOrphanIsNeverRemoved` |
| `AllowedPath` 의 알 수 없는 scope 거부 제거 | 둘 다: `TestSharedAllowedPathUnknownScopeAndForgedStateNames/unknown-scope`, `TestSharedAllowedPathList` |
| `planStaleSkill` 의 스킬 이름 규칙 제거 | 둘 다: `…ForgedStateNames/forged-state-names` |
| MCP 정리 경로의 서버 이름 규칙 제거 (`PlanMCPFile`) | 둘 다: `…ForgedStateNames/forged-mcp-state-name` |
| `ReadFileSafe` 의 `MaxJSONFileBytes` 검사 제거 | 둘 다: `TestSharedOversizedConfigFileIsRefused` |
| `Preflight` 의 임시 디렉터리 심볼릭 링크 필터 제거 | 둘 다: `TestSharedPreflightIgnoresTempDirsBehindSymlinks` |
| 스킬 제거 경로의 `WriteBlocker` 제거 | 둘 다: `TestSharedStaleSkillReadOnlyIsNotRemoved` |

잡을 수 없는 변이(동치): (1) `LoadState` 의 `GuardPath` 제거 — 바로 다음 `ReadFileSafe` 가 같은 가드로 같은 오류를 낸다. (2) `RenderServer` 의 비밀 literal 휴리스틱 제거 — 앞선 `Spec.Validate` 가 같은 조건(`IsSecret`·`LooksSecret`·`LooksSecretKV`)을 먼저 거부해 도달하지 못하는 두 번째 방어선이다. 둘 다 계층 방어로 남겼다. 한 타깃에만 의미가 있는 변이: 명령·인자·URL 치환 문법 검사(`SubstInCommand`)는 opencode 만 켠다(Claude Code 는 `${VAR}` 를 명령·인자에서도 확장하는 정상 기능이라 켜지 않음; opencode 테스트가 잡음), 다른 타깃 상태 파일 읽기 가드(`ReadForeignState`)는 opencode 만 쓴다(`TestOpenCodeForeignStateIsGuarded`).

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

1. **형식 검증 스파이크 문서**: `docs/clients/<client>.md` 에 경로, 설정 키, 값 치환 문법, CLI 옵션, 비밀이 들어 있는 파일을 `[doc]`/`[help]`/`[local]`/`[bin]`/`[미확인]` 으로 구분해 적는다. **클라이언트 바이너리는 어댑터 개발 중에도 실행하지 않는다**(필요하면 `strings` 로 읽기만). 예외는 버전 게이트(`internal/clientver`)가 실행하는 `<binary> --version` 하나뿐이며, 새 타깃을 추가할 때는 `clientver.Specs` 에 바이너리 이름과 검증된 버전을 넣고 `docs/doctor.md` 표를 갱신한다.
2. **레지스트리 클라이언트 id**: `db` 의 `clients` 테이블에 id 가 있어야 노출 설정(`mcp expose`)이 동작한다(스키마 변경은 별도 태스크).
3. **어댑터 구현(작다)**: `internal/adapter/<client>/` 에는 형식 변환만 둔다. 보안 로직(로더·스킬 판정·저널 인정·해시 충돌·재검증·가드·상태)은 **다시 쓰지 않고** 공통 계층을 호출한다.
   - `Name`, `Plan`, `AllowedPath`, `ResolveRoot` 와 `paths`(`adapter.Paths` 를 포함: 루트·스킬 디렉터리·상태·저널; 상태·저널 파일 이름은 다른 타깃과 겹치지 않게 정한다).
   - `Plan` 은 `adapter.ResolveRoot` → `adapter.ValidateBin` → `adapter.NewPlanner` → `Preflight(임시 디렉터리)` → `LoadState` → `PlanSkills(SkillPolicy)` → MCP(`PlanMCPFile` 또는 user 스코프 `PlanMCPCommands`) → `FinishState`(마지막) 순서로 부른다. 템플릿은 `opencode/opencode.go`(훅 없는 쪽).
   - MCP 는 `adapter.MCPFormat`(스키마 `Build`·참조 문법 `Ref`·치환 정규식 `Subst`)과 `adapter.MCPFile`(경로·최상위 키·라벨)만 정의한다. `renderServer`·`envNameRe`·서버 이름 규칙은 새로 만들지 않는다.
   - `AllowedPath` 는 `adapter.AllowedPath` 에 쓸 수 있는 설정 파일 목록만 넘긴다. 금지 루트는 `adapter.ResolveRoot` 에 `ForbiddenDir` 목록으로 넘긴다.
   - `[미확인]` 항목은 추정하지 않고 렌더하지 않는다(계획의 참고로 알린다). 타깃에만 있는 기능(훅, JSONC 감지, 경로 규칙)은 타깃 안에 둔다.
4. **가드**: 비밀 파일의 (디렉터리, 이름)을 `guard.go` 에 추가한다(동일 파일 판정 포함; 이름만으로 막지 않는다). 프로젝트 설정 파일이 오탐으로 막히지 않는지 시험한다. 금지 루트는 3번의 `ForbiddenDir` 로 넘긴다.
5. **등록**: `internal/cli/sync.go` 의 `syncAdapters`, `supportedTargets`, 도움말, 지원하지 않는 플래그 오류.
6. **픽스처**: `testdata/<버전>/` 에 기존 사용자 설정(가짜 비밀 포함)과 골든 결과, `NOTES.txt` 에 근거와 `[미확인]`. `.gitignore` 에 걸리는 이름은 `dot-` 접두.
7. **테스트(최소)**: 공통 로직의 테스트는 공통 패키지와 기존 두 타깃에 있으므로 **새 타깃도 같은 시나리오를 통과**해야 한다(어댑터가 공통 함수를 실제로 호출하는지 확인하는 것이 목적이다; 변이 시험: 공통 가드·재검증·해시 충돌·심볼릭 검사를 하나씩 끄면 새 타깃 테스트도 실패해야 한다). plan 무쓰기, apply 멱등, 사용자 항목 보존(키 순서·들여쓰기·CRLF), 충돌 5종, 파싱 불가 무변경, 심볼릭 링크 거부, 백업·복원·롤백, 크래시 매트릭스, 저널 위조, 금지 루트·가드(하드링크 포함), diff 마스킹, 변환 정확성(특수 문자 인자), 사용자 스코프 명령 `sh -n`, 다른 타깃과의 교차 실행·중복 노출 방지, 실제 HOME 비접근.
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

- 표시·방어 세부(TASK-0022 AC-14): diff 마스킹의 키 규칙은 `author(s)`·`keyword(s)` 를 가리지 않는다(표시용; 값 패턴 단계가 실제 비밀을 잡는다). `__proto__` 는 env/헤더 이름으로 거부한다. 권한이 없는 `skills/` 는 "심볼릭 링크"가 아니라 "경로를 확인할 수 없어 거부합니다 (권한 …)" 로 알린다. 키가 직접 가진 숫자 값도 가린다. opencode 가 claude-code 상태 파일의 스킬 목록을 "이미 노출됨"으로 받아들이는 것은 `.claude/skills/<이름>` 이 실제로 있고 Forge 마커(이름 일치)가 있을 때뿐이다 — 낯선 저장소의 위조된 `.forge/sync-state.json` 으로 스킬을 건너뛰게 만드는 DoS 를 막는다(같은 실행의 `--target all` 계획에서 넘어온 `ClaimedSkills` 는 이 확인 없이 믿는다). 남는 한계: 위조된 마커+디렉터리까지 저장소가 가지고 있으면 그 스킬은 `.claude/skills` 로 노출된 것으로 보고 `.opencode` 사본을 만들지 않는다(그 디렉터리는 OpenCode 도 읽는다).
- 보안 로직 공통화(TASK-0023)는 끝났다. 남은 어댑터별 코드는 형식 변환이다. 훅(`claudecode/hooks.go`)은 claude-code 만 있어 타깃에 있으며, 두 번째 훅 타깃이 생기면 같은 방식으로 올린다.
- 중복 노출 방지는 **opencode → claude-code 방향만** 있고 이것이 맞는 설계다(TASK-0023 AC-5 판정). OpenCode 는 `.claude/skills` 와 `.opencode/skills` 를 모두 읽지만(`docs/clients/opencode.md` 스킬 표), Claude Code 가 읽는 스킬 경로는 `~/.claude/skills`, `<repo>/.claude/skills`(및 `--add-dir`·엔터프라이즈·플러그인)뿐이고 `.opencode/skills` 는 없다(`docs/clients/claude-code.md` 스킬 표). 그래서 opencode 가 먼저 `.opencode/skills` 에 쓴 스킬이 있어도 Claude Code 에서는 **중복이 생기지 않는다**: 중복을 보는 쪽은 OpenCode 하나이고, 그쪽은 이미 `claimed`(`opencode/opencode.go` `loadClaims`, `adapter.SkillPolicy.Claimed`)로 막는다. claude-code 가 opencode 가 관리하는 스킬을 건너뛰면 Claude Code 에는 그 스킬이 **아예 없어지므로**(`.claude/skills` 에 쓰는 것이 Claude Code 에 노출하는 유일한 경로) 오히려 해롭다. 따라서 구현하지 않는다. 다만 claude-code 를 나중에 sync 하면 OpenCode 는 잠시 같은 스킬을 두 경로에서 보며, 이어서 opencode 를 다시 sync 하면 `.opencode` 사본이 제거되어 수렴한다. 같은 프로젝트의 project 스코프 opencode 와 user 스코프 claude-code(`~/.claude/skills`) 사이의 중복은 검사하지 않는다.
- opencode: `opencode.jsonc`(주석)는 편집하지 않는다(주석 보존 불가) — `opencode.json` 이 없으면 중단, 둘 다 있으면 우선순위 [미확인] 경고. `[미확인]` 항목(cwd, timeout, oauth, tools 글롭, 플러그인)은 다루지 않는다. 버전 게이팅(`opencode --version`)은 어댑터가 아니라 CLI 의 버전 게이트(`forge sync --apply`, `forge doctor`; `docs/doctor.md`)가 한다.
- 프로젝트 스코프 레지스트리·프로필별 노출은 아직 없다(docs/sync.md 한계).
