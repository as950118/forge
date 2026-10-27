# Forge 위협 모델과 설치 전 점검 (TASK-0013)

이 문서는 Forge(스킬·MCP·도구·이력 관리 평면)의 **위협 모델**, 각 위협의 **완화책(구현 위치와 테스트)**, **남은 위험과 알려진 한계**, 그리고 **사용자가 해야 할 일**을 적는다. 모든 문장은 코드·테스트와 대조했고, 코드로 확인하지 못한 것은 "미검증" 으로 적었다. Forge 는 **샌드박스가 아니다**: 사용자가 등록·승인한 명령은 사용자 권한으로 실행된다. Forge 가 하는 일은 (1) 실행 전에 무엇이 실행될지 보여 주고, (2) 바뀌면 알리고, (3) 위험한 경로를 기본으로 막고, (4) 비밀을 저장·출력하지 않는 것이다.

관련 문서: `docs/gateway.md`(게이트웨이·confirm·프록시), `docs/sync.md`(설정 파일 쓰기), `docs/doctor.md`(읽기 전용 점검), `docs/adapter.md`, `docs/clients/*.md`.

## 1. 자산

| 자산 | 어디에 있나 | 왜 중요한가 |
|------|-------------|-------------|
| 사용자 설정 파일 | `<repo>/.mcp.json`, `.claude/settings.json`, `opencode.json`, `~/.claude/settings.json` 등 sync 대상 | 클라이언트가 읽어 **명령(MCP command, hook)을 실행**하는 파일. 바뀌면 임의 실행으로 이어진다 |
| 비밀 값 | 사용자 환경변수, `~/.claude.json`, OpenCode 인증·서비스 파일 | API 키, 토큰. Forge 는 값을 저장하지 않고 `${ENV}` 참조만 둔다 |
| 레지스트리·이력 | `$FORGE_HOME`(기본 `~/.forge`)의 `forge.db`, `skills/<이름>@<버전>/`, `backups/`, `tmp/` | 스킬 본문(모델이 따르는 지침), MCP 명령·도구 정의, 호출 이력. 변조되면 이후 모든 sync·게이트웨이 출력이 오염된다 |
| 사용자 홈 | `~` 아래 그 밖의 파일 | 악성 도구·스킬·서버가 읽거나 지우면 안 되는 개인 파일 |

## 2. 신뢰 경계

| 주체 | 신뢰 수준 | 비고 |
|------|-----------|------|
| 사용자(로컬 셸의 본인) | 신뢰 | 명령 실행·등록·승인 권한이 있다. `FORGE_HOME` 에 쓸 수 있는 사람(같은 사용자의 다른 프로세스 포함)은 레지스트리를 임의로 바꿀 수 있다 — **DB 쓰기 권한자는 방어 대상이 아니다**(5.3 참조) |
| 모델·에이전트 | **비신뢰 입력 생성자** | 모델이 만든 인자는 데이터 위치에만 들어가고(셸 해석 없음) 스키마 검증을 거친다. 모델이 읽는 텍스트(스킬 본문, MCP 도구 설명)는 모델을 조종하려는 입력일 수 있다 |
| 외부 소스(스킬 git/zip, MCP 서버 패키지, 원격 MCP URL) | **비신뢰** | 설치 전 미리보기·승인·해시 고정의 대상. 가져오기 단계(압축 해제·git 클론)부터 방어한다 |
| 클라이언트(Claude Code, OpenCode) | 부분 신뢰 | Forge 는 클라이언트가 설정 파일을 어떻게 읽고 무엇을 실행하는지 **검증하지 못했다**(실클라이언트 로드는 미검증, TASK-0020 AC-5) |
| 악의적 저장소(내가 연 프로젝트 디렉터리) | **비신뢰** | 그 안의 `.mcp.json`, `.claude/`, `opencode.json`, 스킬 디렉터리, 심볼릭 링크·하드링크, 위조된 상태 파일이 공격 수단이 된다 |

## 3. 위협과 완화 (표)

"테스트" 는 `go test` 로 실행되는 테스트 이름이다. 후속은 로컬에서 관리하는 후속 태스크 번호다(0017 MCP 후속, 0018 도구 위협 모델·code-exec 분류, 0019 이력 수집 후속, 0020 게이트웨이 후속·실클라이언트 검증, 0021 history 후속, 0022 sync 후속, 0023 어댑터 공통 계층, 0024 프로필·doctor 후속). **TASK-0018(code-exec 바이너리 위험 분류)은 이번 범위가 아니다 — 후속.**

### 3.1 외부 스킬 가져오기

| # | 위협 | 완화책 (구현 · 테스트) | 잔여 위험·한계 | 후속 |
|---|------|------------------------|----------------|------|
| S1 | **zip-slip / 압축 폭탄** (`../`, 절대 경로, 심볼릭 링크, 중복·암호화 항목, 거대 파일) | `internal/skill/zip.go` `ExtractZip`: 이름 검사(`checkZipName`)와 경로 포함 검사(`joinWithin`)의 2중 방어, 심볼릭 링크·특수 파일·암호화·Store/Deflate 외 압축 거부, 파일 1000개·총 50MiB·파일당 10MiB(선언 크기와 실제 기록 바이트 모두). 로컬 디렉터리·git 도 같은 한도(`CopyDir`). 테스트: `TestExtractZipRejects`, `TestCheckZipNameRejectsParent`, `TestWithinDir`, `TestExtractZipSecondLayerCatchesEscape`, `TestExtractZipDeclaredSizeRejectsBeforeWriting`, `TestCopyLimitedEnforcesRealBytes`, `TestPreviewManyFilesRejectedNotExploded`(파일 10만 개), `TestPreviewSymlinkRejectedAtImport` | 한도 안의 악성 내용(50MiB 까지)은 막지 않는다. 한도는 고정값이며 설정할 수 없다 | — |
| S2 | **git 인자·URL 주입** (`-u…`, `--upload-pack=`, `ext::`, `file://`, 자격증명 URL, `ssh://-oProxyCommand=x/`) | `internal/skill/git.go`: `ValidateGitURL`(https·ssh 만, 비밀번호·https 사용자 정보·쿼리·공백·`-` 로 시작하는 호스트 거부), 모든 git 호출에 `--` 구분, `GIT_ALLOW_PROTOCOL=https:ssh`, `GIT_TERMINAL_PROMPT=0`, `--ref` 는 `ValidateRef`(영문/숫자로 시작, `..`·`@`·`~`·`^`·`:`·공백·제어 문자·`.lock` 거부). 서브모듈은 가져오지 않는다(`--recurse-submodules` 미사용). 테스트: `TestPrepareGitRejectsBadURLs`, `TestValidateGitURLRejectsDashHost`, `TestExecGitRefusesFileScheme`, `TestPrepareRefValidation`, `TestExecGitCloneRefLocal`. 제품 코드는 `file://` 을 허용하지 않고, 테스트는 `allowFile` 비공개 스위치와 가짜 클로너만 쓴다 | 사용자의 전역 git 설정(`url.<base>.insteadOf`, credential helper, 프록시)은 그대로 적용된다(`GIT_CONFIG_GLOBAL` 을 비우지 않음). 서버가 보낸 내용은 클론 후 `.git` 을 버리고 복사한 트리로만 쓴다 | — |
| S3 | **악성 SKILL.md — 프롬프트 인젝션** (본문이 모델에게 "비밀을 보내라" 등을 지시) | 설치 전 **미리보기**가 SKILL.md 를 읽게 유도하고(이름·설명·frontmatter 전체 표시), 본문의 URL·IP·`curl`/`wget`/`\| sh`/`eval`/`base64 -d`/`chmod +x`/`rm -rf`/`sudo`/`ssh`/`nc` 존재를 **휴리스틱**으로 경고(`internal/skill/preview.go`). git/zip 은 승인 필수, `--sha256` 으로 검토한 트리 고정. 테스트: `TestPreviewContents`, `TestGitAddWithoutApprovalIsRefusedAndInstallsNothing`, `TestSHA256Pin` | **본문 속 지시를 자동으로 판별할 수 없다.** 패턴 목록은 단순 정규식이라 우회가 쉽고 "발견 없음" 은 안전이 아니다. 스크립트·참조 파일 내용은 검사하지 않는다. 승인 후에는 클라이언트의 모델이 본문을 따른다 | — |
| S4 | **frontmatter `hooks` 등 위험 필드** (`hooks`, `allowed-tools`, `disallowed-tools`, `context`, `agent`, `model`, `shell`) | 미리보기가 해당 필드를 `!` 로 강조하고 이유를 붙이며 `hooks` 는 별도 경고문을 낸다. 업데이트·재추가 시 **새로 생기거나 값이 바뀐** 위험 필드를 변경 요약에 표시. 로컬 디렉터리도 경고는 표시하되 막지는 않는다(`--strict-local` 이면 승인 필수). 테스트: `TestPreviewContents`, `TestPreviewChangeSummaryAgainstInstalled`, `TestPreviewChangedRiskyValue`, `TestLocalDirWarnsOnHooksButDoesNotBlock`, `TestStrictLocalRequiresApproval` | Forge 는 frontmatter 를 **바꾸지 않고 그대로 sync** 한다. Claude Code 공식 문서는 스킬 frontmatter 의 `hooks`·`allowed-tools` 를 지원한다고 한다(`docs/clients/claude-code.md`, `[doc]`). 실제 클라이언트가 어떻게 실행하는지는 **미검증**이다 | 0020 |
| S5 | **스킬 스크립트** (`scripts/*.sh`, 실행 비트) | Forge 는 스킬 스크립트를 **실행하지 않는다**: 게이트웨이 `forge_skill` 은 텍스트 파일만 읽어 돌려준다(`readInside`), sync 는 복사만 한다. 미리보기가 실행 가능한 파일(실행 비트·셔뱅·스크립트 확장자)의 경로·크기·sha256·인터프리터를 표시하고, 숨김·바이너리 파일도 표시. 테스트: `TestPreviewContents`, `TestPreviewListsAreBounded` | 클라이언트의 모델이 Bash 도구로 스크립트를 실행할 수 있다(그 실행의 승인은 클라이언트 몫). 스크립트 내용은 미리보기에 나오지 않는다 | 0018 |
| S6 | **승인 메시지·미리보기 위조** (이름·설명·파일명·URL 의 ESC, 줄바꿈, 방향 제어 U+202E) | 미리보기의 모든 동적 문자열은 생성 시 한 번 `textsafe` 로 정화(제어·방향·제로폭 문자는 `\x1b`, `\u202e` 같은 보이는 이스케이프, 길이 상한). 텍스트와 `--json` 모두 같은 값을 쓴다. `mcp show` 의 도구 이름·설명도 정화. 테스트: `TestPreviewSanitizesHostileStrings`, `TestPreviewOutputIsTerminalSafe`, `TestPreviewHugeSkillMDRejectedBeforePreview`, `TestMCPShowSanitizesToolText`, `TestPreviewCommandLineIsShellQuoted` | 정화는 "보이게" 만든다. 사람이 이스케이프된 문자열을 읽고 판단해야 한다. 동형 문자(키릴 `а` 와 라틴 `a`)는 구분하지 않는다 | 0019 |
| S7 | **해시 고정·변경 감지** (같은 URL 이 나중에 다른 내용을 줌) | 미리보기가 항상 **트리 sha256**(경로·실행 비트·크기·내용)을 표시, `--sha256` 불일치는 설치하지 않음(대소문자·공백 정규화, 형식 오류 거부). git 은 `--ref`: 40(64)자 커밋 해시만 "고정됨", 브랜치·태그·미지정은 "고정되지 않음" 경고. 레지스트리에 `source_uri`·`source_ref`(`<요청 ref>@<커밋>`)·`content_hash` 기록. `skill update` 는 다시 가져와 변경 요약 후 승인 시에만 **새 불변 버전** 추가(옛 버전 보존). 테스트: `TestSHA256Pin`, `TestGitRefPinning`, `TestPrepareGitRefStatus`, `TestSkillUpdateShowsChangesAndNeedsApproval`, `TestSkillUpdateGitFollowsRecordedRef`, `TestSkillUpdateRefusesRenamedSource` | 해시는 **내가 본 내용과 같다**는 것만 보장하며 내용이 안전하다는 뜻이 아니다. 해시를 복사해 오는 경로가 신뢰되어야 한다(미리보기 → 사람이 확인 → 재실행). 서버가 커밋 해시 요청에 다른 커밋을 주면 `ExecGit.CloneRef` 가 거부한다. 승인 전 가져오기(클론) 자체는 네트워크 접속이다 | — |
| S8 | **설치 후 저장소 변조** (`~/.forge/skills/<이름>@<버전>/` 파일을 사후에 수정·추가·심볼릭 링크화) | 등록 시 스냅샷은 읽기 전용 권한. 같은 해시 검증(`skill.VerifyStore`: 가져오기 한도 안에서 재해시, 심볼릭 링크·추가 파일·실행 비트 변경도 탐지)을 **`forge skill check`**(읽기 전용, 종료 코드 1), **게이트웨이 `forge_skill`**(변조 시 제공 거부), **`forge doctor`**(`security.skills`, fail) 가 쓰고, **sync** 는 기존 `TreeHash` 교차검증으로 렌더링을 거부한다. 테스트: `TestVerifyStoreStates`, `TestVerifyStoreBoundedByImportLimits`, `TestSkillCheckDetectsStoreTampering`, `TestForgeSkillRefusesTamperedSnapshot`, `TestDoctorDetectsSkillStoreTampering`, `TestTamperedRegistrySnapshotIsRefused` | DB 의 `content_hash` 도 함께 바꾸면 탐지하지 못한다(DB 쓰기 권한자, 5.3). 검사와 사용 사이의 교체(TOCTOU)는 막지 못한다 | — |
| S9 | **미리보기 자체의 폭주** (10만 개 파일, 거대 SKILL.md, 파일 수천 개 스크립트) | 가져오기 한도(S1)가 먼저 거부하므로 미리보기는 최대 1000파일을 다룬다. SKILL.md 는 64KiB 초과 시 거부. 목록은 30개, URL·IP 는 10개로 자르고 "외 N개" 표시. 테스트: `TestPreviewListsAreBounded`, `TestPreviewManyURLsBounded` | — | — |
| S10 | **`--preview` 가 상태를 바꿈** | `skill add --preview` 는 레지스트리를 읽기 전용으로만 열고(`store.OpenReadOnly`, 없으면 열지 않음) FORGE_HOME 을 만들지 않으며 git 클론은 임시 디렉터리에서 삭제. 테스트: `TestPreviewOnlyChangesNothing`(DB 바이트·홈 트리 해시 전후 동일), `TestPreviewOnlyDoesNotCreateHome` | 임시 디렉터리(시스템 임시 경로)에 가져온 내용이 잠시 존재한다 | — |

### 3.2 MCP 서버

| # | 위협 | 완화책 (구현 · 테스트) | 잔여 위험·한계 | 후속 |
|---|------|------------------------|----------------|------|
| M1 | **악성 명령·버전 미고정 패키지** (`npx -y pkg@latest` 이 매번 다른 코드를 받음) | `forge mcp add` 는 등록 전에 **정확한 명령줄**(셸 인용)을 stderr 로 보여 준다. `npx`/`bunx`/`pnpm dlx`/`npm exec`/`uvx`/`pipx run`/`deno run`/`docker|podman run` 의 패키지·이미지가 고정되지 않았으면(@latest·태그 없음·범위·`:latest`·URL) 경고, `npx -y` 의 설치 확인 생략과 `sh -c`, `docker --privileged`/`--network host`/민감 경로 마운트도 경고(`internal/mcp/preview.go`). `--preview` 는 DB 도 열지 않고, `--strict` 는 승인 필수. 등록만으로는 실행되지 않으며 `refresh` 때 실행됨을 미리보기가 알린다. 테스트: `TestPreviewRunnerPinning`, `TestPreviewNpxYesFlag`, `TestPreviewShellAndPipes`, `TestPreviewContainerPrivileges`, `TestMCPAddShowsPreviewOnStderr`, `TestMCPAddPreviewOnlyRegistersNothingAndDoesNotOpenDB`, `TestMCPAddStrict`, `TestPreviewPinGuidancePerRunner`(고정 방법 안내) | **기본은 질문 없이 등록**한다(로컬 설정 작업이라는 정책, 시연 호환). 점검은 명령줄 휴리스틱이며 패키지 **내용**은 검사하지 않는다. 고정된 버전이어도 악성일 수 있다. 서버 프로세스는 샌드박스 없이 사용자 권한으로 실행된다 | 0017 |
| M2 | **악성 도구 설명 (프롬프트 인젝션)** | 게이트웨이는 프록시한 도구 설명을 `listText` 로 정화(한 줄·제어 문자 이스케이프·길이 상한)해서 내보낸다(`internal/gateway/sanitize.go`, 테스트 `TestProxyDescriptionsAreSanitizedInToolsListAndForgeList`). 설명 변경은 아래 M3 로 감지한다. `mcp show` 출력도 정화 | **의미적 인젝션(정상 문장처럼 쓴 악성 지시)은 막지 못한다.** 정화는 터미널·프로토콜 안전만 보장한다 | 0020 |
| M3 | **rug pull (승인 후 도구 정의 변경)** | `forge mcp approve` 가 현재 캐시된 정의(이름+설명+입력 스키마의 해시와 사본)를 승인 기록(`mcp_tool_approvals`, 마이그레이션 0002)에 저장. 이후 `mcp refresh` 는 **승인된 정의와 비교**해 변경(설명·스키마·추가·삭제)을 경고하고 바뀐 부분(공통 앞뒤를 뺀 구간)만 정화해 보여 준다 — 긴 설명 끝에 덧붙인 지시도 잘리지 않는다. 최초 refresh 는 변경이 아니라 "미승인(신규)". `mcp show` 는 상태(승인됨/변경됨/미승인)와 함께 승인 전에 읽을 **설명 전문**(정화, 최대 1000자 — 표는 70자에서 잘린다)을, `doctor`(`security.mcp`)는 상태를 표시. `mcp pin --enforce` 이면 게이트웨이가 **승인된 정의와 해시가 같은 도구만 노출**하고 나머지는 숨김(`forge_list` 에 이유 표시, 호출도 거부). 테스트: `TestFirstRefreshIsUnapprovedNewNotChange`, `TestApproveThenDetectEachKindOfChange`, `TestSchemaChangeIsDetected`, `TestDescriptionDiffIsSanitized`, `TestLongDescriptionAppendIsNotHiddenByTruncation`, `TestEnforceHidesChangedAndUnapprovedTools`, `TestMCPRefreshApproveShowPinFlow`, `TestMCPShowPrintsFullDescriptionOfUnapprovedTools`, `TestDoctorReportsChangedAndUnapprovedMCP`, `TestMigration0002UpgradePath` | (1) **enforce 는 기본 꺼짐**이고, 꺼져 있으면 변경은 경고만 한다. (2) enforce 는 **게이트웨이 경유(`gateway`) 노출에만** 적용된다. `direct` 로 클라이언트 설정에 렌더링된 서버는 클라이언트가 직접 통신하므로 Forge 가 걸러낼 수 없다(`pin` 이 안내한다). (3) 변경은 **사용자가 `refresh` 를 실행했을 때만** 발견한다 — 자동 주기 갱신이 없고, 게이트웨이는 호출 때 업스트림 `tools/list` 를 다시 비교하지 않는다(캐시 기준). 서버가 같은 도구 이름으로 동작만 바꾸는 것은 정의가 같으면 탐지되지 않는다. (4) 승인 기록은 DB 에 있어 DB 쓰기 권한자는 바꿀 수 있다. (5) 비밀 값 마스킹 때문에 비밀만 바뀐 경우는 변경으로 보이지 않는다 | 0017, 0020 |
| M4 | **환경변수 유출** (부모 셸의 API 키가 자식 서버로) | 자식 프로세스 환경 = `mcp.MinimalEnv` 허용 목록 + 서버에 선언한 `--env` 변수만(`internal/mcp/connect.go`). 자격증명이 든 프록시 URL 변수는 통째로 버림. 미리보기가 "최소 환경 + 선언한 변수만" 과 선언 변수 이름을 표시. 테스트: `TestRefreshDoesNotInheritParentEnv`, `TestMinimalEnvDropsCredentialBearingValues`, `TestDefaultProxyEnvDropsCredentialVariables`, `TestPreviewNeverShowsValues` | 선언한 비밀은 **그 서버에 전달된다**(설계). 서버가 다른 곳으로 보내는 것은 막지 못한다. 스킴 없는 `user:pw@host` 프록시 값은 판정하지 못한다 | 0020(AC-1) |
| M5 | **비밀 평문 저장·출력** | env·헤더는 `${ENV}` 참조만 저장, 비밀로 보이는 키 이름·값 패턴·URL 쿼리·`--` 뒤 인자의 literal 은 저장 거부(우회 옵션 없음)(`internal/mcp/spec.go`, `secrets.go`). 발견 결과(도구 이름·설명·스키마·서버 정보)는 해석된 비밀을 `***` 로 치환한 뒤 캐시. 미리보기·승인 기록에는 참조 이름만 나오고 literal 값은 "값 비표시". **오탐 완화(TASK-0026)**: 파일 경로 모양의 값(`/`·`~/`·`./`·`../`·드라이브로 시작, 구성요소 2개 이상, URL 아님)에는 순수 고엔트로피 규칙을 적용하지 않고(macOS `TMPDIR`·npx/pnpm 캐시·해시 디렉터리), 알려진 비밀 접두는 경로의 모든 구성요소에서 계속 검사한다. 문맥이 분명한 식별자(`sha256:<64hex>`, `--ref`/`commit=` 등 뒤의 40·64자 hex, UUID, 짧은 단어로 된 긴 snake/kebab-case)도 비밀로 보지 않는다. URL 의 경로·쿼리·프래그먼트는 파일 경로가 아니므로 예외 없이 엄격하고, **ledger payload 처럼 영구 저장되는 마스킹도 예외 없는 엄격 모드**다(아래 "경로·식별자 예외" 절). 테스트: `TestNoPlaintextSecretInDB`, `TestParseValue`, `TestValidateURLValuePatterns`, `TestCheckArgs`, `TestRefreshMasksSecretsInCache`, `TestPreviewNeverShowsValues`, `TestCorpusRegistration`, `TestCorpusText`, `TestPathExceptionKeepsKnownPrefixChecks`, `TestURLPartsStayStrictEvenWhenPathShaped`, `TestIdentifierContextExceptions`, `TestMCPAddAcceptsMacOSTempDirArgument`, `TestStoredPayloadMasksRandomPathComponent`, `TestPrefixPositionAndCaseRules`, `TestRandomAbsoluteComponentsAreNotAPath`, `TestWordIdentifierShape`, `TestCheckArgsShortFlagVariants`, `TestEnvRefInArgsRegistersWithWarning`, `TestMaskCoversBase64AndURLEscapedEchoes` | 마스킹은 **휴리스틱**이다: 비밀스럽지 않은 이름과 토큰처럼 보이지 않는 짧은 값(`q="hunter2"`), 동형 문자·결합 문자 우회, 비밀 안에 섞여 인코딩된 에코는 놓칠 수 있다(등록한 `${ENV}` 비밀의 전체 base64·URL 인코딩 에코는 가려진다, 아래 "MCP 등록 후속"). 리터럴로 선언한 값은 비밀로 보지 않는다. keychain 참조는 미지원. **경로 구성요소에 접두 없이 박힌 비밀·`commit=<API 키>` 같은 라벨 위장·끝 숫자가 붙은 단어형 암호문구는 통과한다**(아래 절) | 0017, 0019, 0020 |
| M6 | **SSRF** (도구·MCP 가 사설 주소·메타데이터 주소로 요청) | 커스텀 `http` 도구: 사설·루프백·링크 로컬 주소 거부를 등록 시와 **연결 시점**(DNS 해석 결과, 리다이렉트 포함)에 적용(`internal/tool/http.go`). 테스트: `TestHTTPSSRFLiteralIPsRefusedAtAdd`, `TestHTTPSSRFEnforcedAtConnectTime`, `TestHTTPSSRFDNSResolvingToPrivate`, `TestHTTPRedirectToInternalIsBlocked`, `TestRedirectDowngradeStripsDefinitionHeaders`. 원격 MCP URL: 미리보기가 평문 http·사설/링크 로컬 IP·내부망 이름을 경고(`TestPreviewRemote`) | **등록한 원격 MCP URL 에는 연결 시점 SSRF 필터가 없다** — 사용자가 직접 지정한 서버로 보고 미리보기 경고에만 의존한다. 미리보기는 DNS 를 해석하지 않으므로 공개 이름이 사설 주소로 해석되는 경우는 알리지 못한다 | 0020 |

### 3.3 커스텀 도구와 게이트웨이

| # | 위협 | 완화책 (구현 · 테스트) | 잔여 위험·한계 | 후속 |
|---|------|------------------------|----------------|------|
| T1 | **커스텀 도구 명령 주입** (모델 인자가 셸·옵션으로 해석됨) | 도구는 셸 없이 argv 로 실행되고 모델 인자는 변수 위치에만 들어간다. 프로그램·서브명령 위치의 변수, 허용 목록에 없는 명령, 래퍼를 통한 우회, 심볼릭 링크 위장을 거부하고 위조된 DB 행도 실행 시 정책을 다시 검사(`internal/tool`). 인자는 JSON 스키마로 먼저 검증. 테스트: `TestWrapperAllowlistBypassBlocked`, `TestVariableInProgramOrSubcommandSlotRefused`, `TestAllowlistMustContainCommand`, `TestSymlinkDisguiseBlockedByRealBinary`, `TestTamperedDBRowsFailPolicyOnResolve`, `TestScriptStoreIntegrityAndPermissions` | **deny 목록은 심층 방어이지 샌드박스가 아니다.** 목록 밖 바이너리가 데이터를 코드로 해석하면(sqlite3 `.shell`, `npm exec`, `uv run` 등) 임의 실행이 가능하다. `--shell` 본문 해시 없음, TOCTOU, `setsid` 로 타임아웃 회피 가능성. `cat 절대경로` 로 cwd 밖 읽기 | **0018**(code-exec 위험 분류·confirm=ask 강제 — 이번 범위 아님) |
| T2 | **승인(confirm=ask) 메시지 위조** | 모든 동적 값은 하나의 `escapeText` 로 정화해 정적 라벨 뒤 ASCII 따옴표 안에 한 줄로 표시, 비밀은 마스킹(`internal/gateway/confirm.go`, `sanitize.go`). 테스트: `TestConfirmMessageNeutralizesSpoofing`, `TestConfirmMasksSecretKeyNamesRegardlessOfLength`, `TestConfirmScriptInputIsShown` | 키 이름이 평범한 짧은 비밀은 가려지지 않는다. **elicitation 을 광고하지 않는 클라이언트에서는 `confirm=ask` 도구가 항상 거부된다**(안전 쪽 동작; OpenCode 지원은 미확인, 기본값 결정은 후속) | 0020(AC-6) |
| T3 | **hook 의 안정성** (`forge hook ingest` 가 클라이언트를 막거나 깨뜨림) | hook 은 어떤 실패에도 종료 코드 0 으로 끝나고 타임아웃·패닉을 복구하며 홈이 없으면 만들지 않는다. 테스트: `TestHookFailurePathsDoNotCreateOrFail`, `TestHookTimeouts`, `TestIngestHookRecoversPanic` | `duration_ms` 가 비정상(1e999·문자열·null·객체·음수)이어도 payload 는 기록하고 지연 시간만 미상(NULL)이 된다(TASK-0019, `TestHookDurationDecodingIsLenient`). `FORGE_HOOK_TIMEOUT_MS` 는 비숫자·0·음수면 기본값(2초), 10000 초과는 10000 으로 제한한다(`TestHookTimeoutFromEnv`). 다른 필드의 타입 오류나 깨진 JSON 은 여전히 payload 를 버린다. `TestHookConcurrent32` 는 부하에서 가끔 시간 초과한다(알려진 플레이크) | 0019, 0020 |
| T4 | **이력(ledger) 프라이버시** | payload(입력·출력 본문)는 **기본 미저장**이고 `settings.record_payload=1` 일 때만 마스킹 후 별도 테이블에 저장(`internal/ledger/record.go`). 테스트: `TestPayloadDefaultOffAndOptIn`, `TestMaskRules`, `TestMaskKeyAndInlineHardening` | 켜는 CLI 명령은 아직 없고(설정 테이블 값으로만), 켜면 마스킹의 한계(M5)가 그대로 적용된다. 이름·시각·호출 횟수는 항상 기록된다 | 0019 |

### 3.4 sync(설정 파일 쓰기)와 사용자 홈

| # | 위협 | 완화책 (구현 · 테스트) | 잔여 위험·한계 | 후속 |
|---|------|------------------------|----------------|------|
| Y1 | **설정 파일 변조·사용자 항목 훼손** | 기본은 계획만 출력(`--dry-run`, `--diff` 로 마스킹된 diff). 쓰기는 적용 직전 해시 재검증 후 원자적 rename, 사용자 항목은 사이드카 상태 파일·마커로 구분해 건드리지 않음, 충돌은 기본 중단. 테스트: `TestPlanWritesNothing`, `TestUserItemsAreNeverTouched`, `TestManagedItemsEditedByHandAreConflicts`, `TestPlanToApplyChangeIsDetected`, `TestStateTamperedNamesAreIgnored` | 해시 재검증~rename 사이의 짧은 틈(TOCTOU)과 동시에 편집하는 다른 프로그램은 완전히 막지 못한다 | 0022 |
| Y2 | **심볼릭·하드링크로 비밀 파일을 읽거나 덮어씀** | `GuardPath` 가 경로 이름·**파일 신원(inode)** 으로 금지 파일을 판정, 심볼릭 링크·nlink>1 파일 거부(`internal/adapter/guard.go`, `fsx.go`). 테스트: `TestSymlinksAreRefused`(claude-code·opencode 각각), `TestHardLinkedConfigsToSecretFilesAreRefused`, `TestGuardRefusesHardLinksToOpenCodeSecretFiles`, `TestSyncRefusesAliasOfTheUserFileInTheProject` | 스킬 트리 안의 파일도 같은 검사를 받는다(`TreeHash`·`copyTree`·diff 트리 읽기; TASK-0022 AC-9, 테스트 `TestSkillTreeHardLinkIsRefused`, `TestSharedHardLinkedFileInsideSkillTreeIsRefused`). 한계: 검사와 사용 사이의 하드링크 생성(TOCTOU)은 막지 못한다 | — |
| Y3 | **`~/.claude.json`·OpenCode 비밀 파일 접근** | 어떤 코드 경로도 이 파일들을 열지 않는다(`stat` 으로 같은 파일 여부만 판정). 소스에 파일 이름을 적지 않는 테스트까지 있다. 테스트: `TestDecoyUserFileIsNeverTouched`, `TestSourcesNeverMentionTheUserFile`, `TestSourcesDoNotSpellSecretFileNames`, `TestGuardPrimitivesNeverTouchOpenCodeSecretFiles` | 다른 프로그램이 이 파일을 읽는 것은 Forge 범위 밖 | — |
| Y4 | **크래시 일관성·백업** | 쓰기 전 영향받는 모든 파일 백업, 저널로 중단 감지·앞으로 완료, 위조된 저널은 신뢰하지 않음. 테스트: `TestCrashAtEveryPointConverges`, `TestForgedJournalsCannotDeleteOrAdopt`, `TestBackupManifestAndRestore`, `TestRestoreRejectsTamperedManifests`, `TestRollbackOnMidwayFailure` | 백업 디렉터리를 지우면 중단 사실을 모른다. 전원 차단 시 쓰기 순서를 바꾸는 파일시스템은 가정하지 않는다. **백업에는 설정 파일의 옛 사본이 오래 남는다**(`doctor` 가 개수·권한을 점검) | 0022 |
| Y5 | **PATH 바이너리 실행 (`--version`)** | 실행하는 프로그램은 클라이언트 바이너리의 `--version` 하나뿐: argv 고정, 빈 임시 HOME, 최소 환경, 5초 제한·출력 상한·프로세스 그룹 종료, 프로젝트 안 바이너리와 안전하지 않은 디렉터리·링크 체인은 실행하지 않음(`internal/clientver`). 테스트: `TestArgvIsExactlyVersion`, `TestRunEnvironmentIsIsolated`, `TestBinaryInsideProjectIsNeverRun`, `TestSymlinkIntoAnUnsafeDirectoryIsNotRun`, `TestTimeoutKillsProcessGroup` | 검사와 실행 사이의 교체(TOCTOU), world-writable PATH 디렉터리의 일부 경우, 가짜 `claude` 가 PATH 앞에 있는 경우는 모두 막지 못한다. 실제 클라이언트로의 검증은 미검증 | 0024(AC-2, AC-10) |
| Y6 | **doctor 가 상태를 바꿈 / 악의적 저장소에 속음** | `forge doctor` 는 읽기 전용(DB `mode=ro`+`query_only`+조건부 `immutable`, 설정 파일은 sync 와 같은 가드 뒤에서만 읽음), 위조된 저널·링크는 보고만 한다. 새 점검(`security.*`)도 읽기만 한다. 테스트: `TestDoctorDetectsSkillStoreTampering`(doctor 전후 스킬 저장소 동일), `TestDoctorSecurityCheckOnOldSchema`, `docs/doctor.md` 의 읽기 전용 시험 | 수 MiB 이상의 큰 설정은 점검이 느리다(시간 제한 후 건너뜀) | 0024 |
| Y7 | **악의적 저장소 시나리오** (낯선 저장소를 열고 `forge sync --apply`) | 저장소 안의 `.mcp.json`·`opencode.json`·`.claude/` 는 **사용자 항목**으로 취급되어 충돌이면 중단, 심볼릭·하드링크·금지 경로 별칭 거부, 프로젝트 안 저널 사본·상태 파일은 신뢰하지 않음(`TestJournalCopyInTheProjectIsNotTrusted`, `TestForgedJournalCannotAdoptUserItems`). 설치 전에 `forge sync --dry-run --diff` 로 무엇이 바뀌는지 본다 | 저장소가 이미 가진 `.mcp.json` 의 서버 명령을 **클라이언트가 실행하는 것**은 Forge 가 막지 못한다(클라이언트의 프로젝트 서버 신뢰 확인에 의존, 미검증) | — |

## 4. 알려진 한계 (과장 없이)

- **실제 Claude Code·OpenCode 로드는 검증하지 않았다.** 생성한 설정을 클라이언트가 실제로 읽는지, 게이트웨이 `clientInfo.name`, elicitation 지원 여부, 도구 이름 64자 한도, 스킬 frontmatter `hooks` 를 클라이언트가 실제로 어떻게 처리하는지는 문서·가짜 클라이언트 기준이다(TASK-0020 AC-5). Linux·Windows 도 검증하지 않았다(검증은 macOS).
- **deny 목록은 샌드박스가 아니다.** 커스텀 도구 보호는 심층 방어일 뿐이며 코드 실행형 바이너리의 분류는 후속(TASK-0018)이다. MCP 서버·스킬 스크립트·클라이언트가 실행하는 모든 것은 사용자 권한으로 실행된다.
- **미리보기의 위험 패턴은 휴리스틱**이다. 본문의 의미적 인젝션, 난독화된 명령, 스크립트·참조 파일 내용은 탐지하지 못한다.
- **승인은 "내가 본 내용" 에 대한 것**이다. 해시 고정은 같은 내용임을 보장할 뿐 안전을 보장하지 않는다. MCP 도구 정의 승인은 refresh 로 캐시된 정의에 대한 것이며, 서버 동작(같은 정의 아래의 구현)은 알 수 없다.
- **`enforce` 는 게이트웨이 경유 노출에만** 적용되고 기본은 꺼져 있다. 변경 탐지는 `refresh` 시점에만 일어난다.
- **DB 쓰기 권한자는 방어 대상이 아니다.** `forge.db`·`~/.forge` 에 쓸 수 있으면 해시·승인 기록·정책을 함께 바꿀 수 있다. 파일 권한(0600/0700)과 OS 계정 경계에 의존한다.
- **elicitation 미지원 클라이언트에서 `confirm=ask` 도구는 항상 거부**된다(안전 쪽). 기본값 결정은 후속(TASK-0020).
- **휴리스틱 마스킹의 한계**: 비밀스럽지 않은 이름의 짧은 값은 가려지지 않을 수 있다(TASK-0017, 0019, 0020). **키 이름 정규화(TASK-0019)**: 신규 의존성(`x/text` NFKC)은 도입하지 않고 표준 라이브러리만 쓴다. ledger payload 의 JSON 키는 비밀 단어 검사 전에 결합 문자(Mn) 제거, 구분자(`-` `_` 공백 `.` 와 유니코드 Pd 대시·마이너스, Pc 연결 문자, Zs 공백(NBSP 등), Cf 서식·제로폭 문자 범주 전체) 제거, 소문자화, 키릴·그리스 대표 동형문자 소규모 표 접기를 거친다(`pass-word`, `pass_word`, 결합 악센트, 키릴 a 변형이 가려진다. 테스트 `TestMaskKeyAndInlineHardening`). **한계**: 표에 없는 동형문자(예: U+04CF)와 NFKC 전체 호환 분해는 접히지 않는다. 배열 안의 `--token` 형 플래그 요소에는 `foldKey` 만 적용되어 동형문자·결합 문자 변형 플래그는 놓칠 수 있다. 값이 비밀처럼 보이지 않으면 통과한다. 값 쪽 패턴 규칙은 그대로 동작한다. 등록한 `--env KEY=${ENV}` 비밀의 base64·URL 인코딩 에코는 게이트웨이·refresh·도구 출력에서 가려지지만, ledger·diff 의 패턴 마스킹과 비밀 안에 일부만 든 인코딩은 못 가린다(아래 "MCP 등록 후속").
- **이력 마스킹 보강과 남는 한계(TASK-0021, ledger 마스킹 규칙 버전 4)**: 라벨 없는 `sk-…`(20자 이상)·`ghp_/gho_/ghu_/ghs_/ghr_…`·`github_pat_…`·JWT(`eyJ….….…`)·AWS 액세스 키 ID(`AKIA/ASIA`+16자)·`Bearer <토큰>`, 따옴표로 묶인 값(`api_key = "a b"`), `-p <값>`, 대문자 환경변수 형태의 값(`TOKEN=ab cd`: 따옴표·`&`·`;`·`,`·줄 끝까지 전부)을 이제 마스킹한다(테스트 `TestMaskInlineFollowups`, `TestMaskInlineLeavesOrdinaryText`, `TestMaskPayloadCoversFollowups`). **한계**: `-p 22` 처럼 비밀이 아닌 `-p` 값도 가려진다(표시용 과마스킹 선택). `mkdir -p a/b`·`ls -p dir`·`cp -p src dst` 같은 흔한 명령의 인자와, 이름에 TOKEN 이 든 `TOKEN_COUNT=3` 같은 비비밀 환경변수 값도 가려진다. 20자 미만의 `sk-` 값, 소문자 변수명의 공백 포함 값(`token=ab cd` 는 `ab` 만 가려진다), 라벨이 없는 임의 문자열은 못 가린다.
- **hook·게이트웨이 이중 집계 연결과 이력 읽기 한계(TASK-0021, ADR-F0003)**: 같은 호출을 hook 과 게이트웨이가 각각 기록하면 나중 행이 짝을 찾아(같은 등록 client, 대응하는 이름, 5초 이내, 1:1) 같은 키로 만든다(테스트 `TestLinkHookThenGatewayCustomTool`, `TestLinkIsOneToOneAndNearestWins`, `TestLinkRefusals`). **연결하지 못하면 이중 집계(과대 집계)로 남는다**: 메타 도구(`forge_skill`·`forge_list`), 게이트웨이가 이름을 줄이거나 해시를 붙인 도구, PreToolUse hook 없이 5초보다 긴 호출, client 미등록 행, 이전에 기록된 행(소급 연결 없음). 같은 도구를 5초 안에 병렬 호출하면 호출 수는 맞지만 ok·지연 값이 엇갈려 귀속될 수 있다. **오탐 경로**: 게이트웨이에 닿지 않고 차단된 호출의 hook 행(예: 다른 hook 이 거부)이 있는데 1초 뒤 같은 이름의 다른 호출이 게이트웨이에 기록되면 둘이 한 호출로 합쳐질 수 있다(5초 안, 같은 client·같은 이름). 이 경우 실제로는 2건인데 1건으로 센다(과소 집계). 병합된 호출의 성공 여부는 **실패 우선**이다(한 source 라도 실패면 실패, 마이그레이션 0004, 테스트 `TestMergedOKIsFailureFirst`). `forge history` 는 WAL DB 를 `mode=ro` + `query_only` 로 열며(각각 단독 시험 `TestReadOnlyDefensesEachStandAlone`) 이때 SQLite 가 빈 `forge.db-wal`/`-shm` 을 만들 수 있고 닫은 뒤에도 남는다(DB 내용은 불변). 위치가 읽기 전용이라 열 수 없으면 `immutable` 로 한 번 재시도하고 알림을 출력한다(WAL 에 남은 최신 기록은 안 보일 수 있다, 테스트 `TestOpenReadOnlyDirectoryFallsBackToImmutable`). `--output` 은 `-` 이면 표준 출력이고, 부모 디렉터리가 심볼릭 링크면 따라가며(사용자가 고른 경로) 마지막 구성요소가 심볼릭 링크이거나 Forge 저장소 파일이면 거부한다(`TestWriteFileFollowsParentSymlinkButNotTheFinalOne`). 기간 없는 `summary`/`top` 은 원장 전체를 집계해 1M 행에서 약 4초(행 수에 비례)이므로 50만 행 이상이면 `--since` 를 권하는 알림을 stderr 로 낸다(집계 테이블·캐시는 도입하지 않았다). 1M 행 시험은 `FORGE_LARGE_TESTS=1` 일 때만 실행한다.
- **DB 스키마 v4 (마이그레이션 0004)**: 0004 가 적용된 `forge.db` 는 이전 바이너리가 "DB 스키마가 이 forge 바이너리보다 최신입니다" 라며 거부한다. 되돌리지 말고 forge 를 업그레이드한다.
- **프로필에서 숨겨진 도구 호출(TASK-0021 AC-8)**: 등록·활성 상태이지만 프로필·노출 설정에서 숨겨진 도구/MCP 도구를 호출하면 게이트웨이가 `error_class=hidden_by_profile`, `found=1` 로 기록하고(miss 목록에 나오지 않음) 호출자에게 "등록된 도구이지만 노출되지 않는다" 로 안내한다(테스트 `TestHiddenByProfileIsNotAMiss`). 비활성·`confirm=deny` 도구와 숨겨진 서버의 존재하지 않는 도구 이름은 이전처럼 `not_found`/`found=0` 이다.
- **TOCTOU**: 해시·경로 검사와 실제 사용 사이의 교체(스킬 저장소 검증, PATH 바이너리, sync 적용 직전)는 막지 못한다.
- **OS 별 차이**: 하드링크·inode 판정, 파일 잠금, 권한 비트 검사는 POSIX 기준이다. ACL 은 보지 않는다.
- **이번 변경으로 고친 이전 후속 항목**: `forge mcp show` 의 도구 이름·설명 정화(TASK-0020 AC-8 의 해당 부분).

### 비밀 값 규칙의 경로·식별자 예외와 잔여 위험 (TASK-0026)

같은 규칙(`internal/mcp/secrets.go`)을 쓰되 **맥락을 셋으로 나눈다.** 오탐(`***` 로 가려짐)의 비용은 일시 표시에서만 크고, 영구 저장 데이터의 미탐은 영구 노출이기 때문이다.

| 호출 지점 | 맥락 | 정책 |
|-----------|------|------|
| `mcp add` 인자(`CheckArgs`)·`tool add` argv·본문 템플릿 | 등록 거부(평문 저장 방지) | 경로·식별자 예외 적용. `--ref <hex>` 처럼 앞 인자가 라벨이면 그 인자도 예외 |
| `mcp add --env/--header`·`tool add --env/--header`(`ParseValue`, `Validate`) | 등록 거부 | 예외 적용. 키 이름이 commit/rev/ref/sha/hash/digest/checksum 이고 값이 40·64자 hex 면 식별자 |
| 어댑터 렌더(`claude-code`·`opencode` literal 값) | 등록과 같은 판정 | 등록된 값이 렌더에서 거부되지 않도록 같은 키 인식 규칙(`LooksSecretKV`) |
| 스크립트 본문 줄(`tool add --script-file`) | 등록 거부 | 예외 적용(공백으로 나뉜 필드 단위) |
| `mcp add --url`(경로·쿼리·프래그먼트), `://` 가 든 모든 필드 | 등록·표시 | **예외 없음(엄격).** URL 은 파일 경로가 아니다 |
| **ledger payload(hook·게이트웨이 이력, `record_payload=1`)** | **영구 저장** | **엄격 모드(`LooksSecretPersist`)**: 경로·식별자 예외 없음. 읽기 불편해도 경로 구성요소의 고엔트로피 값과 라벨 붙은 hex 도 가린다 |
| 게이트웨이 승인 메시지, 미리보기·diff·doctor 표시, 도구 dry-run 표시(`MaskText`·`MaskJSON`·`adapter.MaskLine`) | 일시 표시 | 경로·식별자 예외 적용(경로를 `***` 로 가리면 읽을 수 없다). 알려진 접두·`token=`·URL userinfo 는 계속 마스킹 |

**경로로 보는 값**: `~/`, `./`, `../`, `node_modules/`, Windows 드라이브(`C:\`)로 시작하거나, `/` 로 시작하면서 **첫 구성요소가 흔한 루트 이름**(`Users home var private tmp opt usr etc Library Applications System Volumes mnt srv root proc dev run nix snap bin sbin lib lib64 cache data workspace app src build work`)인 값. 구성요소 2개 이상, 경로에 허용되는 문자만, `://` 없음. `/<무작위>/<무작위>`·`/v1/<무작위>/mcp` 는 경로가 아니므로 엄격하게 검사한다. 공백이 든 경로(`/Library/Application Support/...`)는 필드가 나뉘어 경로로 인식되지 않는다(남은 오탐).

**알려진 비밀 접두(모든 맥락·경로의 모든 구성요소, 대소문자 무시)**: 경계(구성요소 시작 또는 `._-/:=@` 뒤)에서 `sk-`/`sk_`·`ghp_ gho_ ghu_ ghs_ ghr_`·`AKIA`/`ASIA`·`AIza`·`eyJ`·`npm_`·`hf_`·`ya29.`·SendGrid `SG.<..>.<..>`·Twilio `SK<32hex>`·Bearer/Basic. 고유한 접두는 **구성요소 중간 어디서나**(`prefixghp_...`): `github_pat_`·`gh?_`(꼬리 30자 이상)·`xox[abprs]-`·`xapp-`·`glpat-`·`glsa_`·`sk-ant-`·`sk-proj-`·`sk_live_`/`rk_live_`/`rk_test_`/`sk_test_`/`pk_live_`/`pk_test_`·`whsec_`·`shpat_ shpss_ shpca_ shppa_`·`dop_v1_`. 짧은 접두(`npm_`, `hf_`, `sk-`, `SG.`)는 경계 뒤에서만 본다(`pnpm_store` 같은 이름이 걸리지 않게). `token=`·`password=` 같은 키-값 구성요소도 경로 안에서 검사한다.

예외 규칙과 자체 공격 평가:

- **라벨 예외**: 키/플래그 이름의 마지막 단어가 commit/rev/ref/sha/sha1/sha256/hash/digest/checksum 이고 값이 정확히 40·64자 hex 이거나 `sha256:<64hex>` 일 때만. 32자·그 외 길이·무라벨 hex·`COMMIT_TOKEN` 같은 비밀 단어 키는 계속 거부.
- **식별자 예외**: `_`·`-` 로 나눈 3개 이상, 2~12자 소문자 단어(모음 있고 자음 4연속 없음) 3개 이상. **숫자는 마지막 부분의 버전 꼴(`v2`, `2024`, `rc1`)이거나 마지막 단어 바로 앞의 `v<N>`(`..._v2_final`)에만** 허용하고, 그 밖의 위치에 숫자가 섞이면 식별자로 보지 않는다(단어에 숫자를 섞은 암호문구는 거부).
- 32자 이상 hex(철자·숫자 혼합)는 엔트로피와 무관하게 비밀로 본다. 40자 base64 모양(AWS 비밀 키 형태)도 잡는다.
- 코퍼스(`internal/mcp/secretcorpus`, 시드 고정 가짜 값)에서 **예외로 새로 통과하는 가짜 비밀은 0건**이고, 아래 잔여 위험은 코퍼스에서 따로 집계해 시험으로 고정했다(`TestResidualRisksOfTheExceptions`).

**잔여 위험(미탐 가능성, 등록 맥락)** — 미탐이 0 이라는 뜻이 아니다. 정직하게 적는다:

1. **경로 구성요소의 접두 없는 비밀은 통과**한다. 대상은 알려진 루트 31개(`Users home var private tmp opt usr etc Library Applications System Volumes mnt srv root proc dev run nix snap bin sbin lib lib64 cache data workspace app src build work`) 아래뿐 아니라 `~/`, `./`, `../`, `node_modules/`, 드라이브(`C:\`, `D:/`) 아래도 포함한다. 독립 시험에서 `/tmp/<무작위 40자>` 는 3000/3000 통과했다. 루트 집합이 넓어 사실상 대부분의 실제 절대 경로가 해당한다. **영구 저장 데이터(ledger payload)는 엄격 모드라 이런 값도 가려진다.**
2. **라벨 위장**: `commit=<API 키 40자 hex>`, `--ref <hex>`, `GIT_COMMIT=<64자 hex>`, `sha256:<64자 hex>`, `my-sha=<hex>`, `--api-hash <hex>`, `digest=<hex>`, `--sha256 <hex>` 처럼 라벨이 붙은 40·64자 hex 비밀은 통과한다.
3. **단어형 암호문구**: 숫자 없는 단어 암호문구(`correct-horse-battery-staple`)는 원래부터 통과한다. 여기에 **끝 숫자를 붙인 암호문구(`a-b-c-d-1234`)도 약 99% 통과**한다(독립 시험 1981/2000). `release-2024`·`image-name-2024` 같은 정상 식별자와 구별할 수 없어 택한 절충이다. 중간 숫자나 단어 안에 섞인 숫자는 거부한다(수정 전 수준). 발음 가능한 합성 토큰도 구별되지 않는다.
4. **짧은 접두를 앞 글자에 붙이면 통과**한다: `sk-`, `npm_`, `hf_`, `SG.`, `ya29.`, `eyJ`, `AIza`, `AKIA` 는 경계(구성요소 시작 또는 `._-/:=@` 뒤)에서만 본다. 이는 `pnpm_store`, `risk-model`, `task-runner` 같은 정상 이름을 거부하지 않으려는 의도된 설계다. 긴 고유 접두(`github_pat_`, `whsec_`, `sk_live_`, `prefixghp_...` 의 `ghp_`+긴 꼬리 등)는 구성요소 중간에서도 거부한다.
5. **40자 base64 규칙의 한계**: 정확히 40자이고 `/` 또는 `+` 를 포함하며 엔트로피 4.0 이상인 값만 잡는다(AWS 비밀 키 모양). 이 규칙은 등록 맥락 기준이며, 저장 맥락에서도 가려지지 않는 경우가 있다(HEAD 와 같은 기존 한계, 독립 시험 6건). 다른 길이의 base64 키, 접두 없는 24자 미만 키도 원래부터 못 잡는다.
6. **좁은 범위**: HTTP body 템플릿·스크립트 본문·JSON 텍스트처럼 경로가 공백으로 구분된 독립 필드가 아니라 따옴표·`":"` 로 붙어 있으면(`{"p":"/tmp/..."}`, `--out="/x"`) 경로 예외가 적용되지 않는다. 오탐이 늘 뿐 미탐은 늘지 않는다.
7. **남은 오탐(안전한 쪽)**: 집합 밖 루트의 정상 파일 경로(`/mydata/<무작위>/...`), 공백이 든 경로(`/Library/Application Support/<24자>/...`, 독립 시험 10건)는 계속 거부된다. `git+https://...#<sha>` 는 URL 엄격 정책상 의도된 거부다(6건).

**독립 측정 요약**(Verifier 코퍼스: 가짜 비밀 340·정상 386, 고정 시드): 인자 재현율 HEAD 95.3% → 95.6%, 정밀도 62.3% → 95.3%, 정상 오탐 196 → 16, 저장 맥락 재현율 98.2%, 순수 무작위 약 5.5만 개 중 새로 통과한 것 0건. 재현율이 100% 가 아닌 것은 위 잔여 위험 때문이다.

- **사용자 안내**: 경로·식별자에 비밀을 넣지 말고 비밀은 항상 `--env KEY='${ENV_NAME}'`·`--header 'Authorization=${ENV_NAME}'` 참조로 넣는다. 인자로만 받는 서버는 환경변수를 읽어 실행하는 래퍼 스크립트를 등록한다.

### MCP 등록 후속 (TASK-0017)

**1. stdio 인자의 `${ENV}`** — `forge mcp add ... -- node s.js --root=${ROOT}` 는 **등록된다**(이전과 같다. Claude Code 가 args 의 `${VAR}` 를 확장하므로 거부는 과하다) 대신 미리보기(`--preview` 포함)에 **경고**가 나온다(`mcp.ArgRefIndex`, `internal/mcp/preview.go`). 이유: Forge(`refresh`·게이트웨이)는 `exec.Command` 로 인자를 그대로 넘기므로 서버는 글자 그대로 `${ROOT}` 를 받는다. 반면 Claude Code 는 `command, args, env, url, headers` 의 `${VAR}` 를 확장하고(`docs/clients/claude-code.md` 의 env/헤더 행, [doc]) 직접 노출하면 확장되지만, OpenCode 는 `{env:VAR}`·`{file:path}` 문법이라 직접 노출은 sync 에서 거부된다(`SubstInCommand`, `docs/clients/opencode.md` [doc]·[bin]). 같은 등록이 경로에 따라 다르게 동작한다. 경고는 인자 번호만 보여 주고 값·기본값은 싣지 않으며 `{env:}`·`{file:}` 도 같이 본다. 권장: `--env KEY='${ENV_NAME}'` 로 넘기고 서버가 환경변수에서 읽게 하거나 환경변수를 읽어 실행하는 래퍼 스크립트를 command 로 등록한다. `sh -c '<스크립트>'` 류는 셸이 직접 확장하므로 경고하지 않는다. `--strict` 는 모든 등록에 승인을 요구하는 기존 동작 그대로다(경고 여부와 무관). 이 점검은 **등록 시점에만** 있고 `Spec.Validate`(sync 렌더 재검증)에는 없어 이미 등록된 서버의 렌더링은 바뀌지 않는다. **`forge tool add` 는 거부한다**: Forge 가 도구를 셸 없이 직접 실행하고 `${...}` 를 확장하지 않으므로 프로그램이 글자 그대로 받는다(argv 번호는 다른 도구 메시지와 같은 `argv #N`, shell 도구는 제외). 이것도 등록 시점(`ValidateNew`)에만 있고, 이전 버전이 저장한 도구 행은 그대로 해석·실행된다. 테스트: `TestEnvRefInArgsRegistersWithWarning`, `TestMCPAddWarnsOnEnvRefInArgs`, `TestCommandToolRefusesEnvRefInArgv`, `TestStoredToolWithEnvRefInArgvStillRuns`.

**2. 짧은 플래그·변형** — 플래그별 결정과 근거. 독립 코퍼스(가짜 비밀 340·정상 386)는 저장소 밖이라 이번에 다시 돌리지 못했고, 저장소 코퍼스(`TestCorpusMetrics`)는 인자 재현율 174/174·정밀도 133/133 으로 변화가 없다.

| 플래그 | 결정 | 근거 |
|--------|------|------|
| `-pVALUE` (붙여 쓴 비밀번호, mysql 방식) | **거부** (포트 `-p8080`·`-p8080:80`, 버전 `-p3.12`, 경로, `${ENV}` 는 통과) | `-p VALUE`·`-p=VALUE` 는 이미 거부 중이고 붙여 쓴 형태만 빠져 있었다. 잔여 오탐: 컴파일러 플래그 `-pthread` 류(MCP 서버 인자로는 드묾) |
| `--tokens=` | **거부**, 단 값이 숫자이면 통과(`--tokens 500`) | 인증 도구에서는 토큰 목록, LLM 도구에서는 개수여서 값 모양으로 가른다. `--max-tokens`·`--token-limit` 은 기존대로 통과 |
| `--jwt=`, `--pat=` | **거부**, 이름의 **마지막 단어**일 때만(`--jwt`, `--github-pat`) | `--jwt-issuer`·`--jwt-algorithm`·`--pat-file` 같은 비밀 아닌 옵션을 거부하지 않기 위해 |
| `--secrets=`, `--passwords=` | **거부** | 이미 거부되는 `--secret`·`--password` 의 복수형이고 다른 뜻이 없다 |
| `-k` | **미적용(통과)** | curl 의 `-k` 는 `--insecure`(값 없는 불리언)이고 다음 인자는 URL 이다. 비밀로 보면 정상 인자를 거부한다. openssl `-k <암호>` 같은 드문 용법은 미탐으로 둔다 |
| `-t` | **미적용(통과)** | docker `-t`(tty)·태그·타입 등 비밀이 아닌 뜻이 훨씬 흔하다 |

한계(미탐): `-k`·`-t`·`-a`(redis-cli 비밀번호) 같은 한 글자 플래그 뒤의 값, `-p` 외 한 글자 플래그에 붙여 쓴 값, 비밀을 뜻하는 단어가 이름에 없는 플래그의 값은 이름으로 알 수 없어 값 모양 검사에만 의존한다. **남는 오탐(안전한 쪽, 거부됨)**: 붙여 쓴 `-p` 계열 컴파일러·빌드 플래그(`-pl`(maven 모듈), `-pg`, `-pipe`, `-pthread`), `--secrets-manager=`·`--secrets-backend=`(비밀 값이 아니라 백엔드 이름), `--tokens=8k`·`--tokens=1e5`(숫자가 아닌 개수 표기). 필요하면 값을 환경변수·설정 파일로 옮기거나 `--secret` 이 들지 않는 이름의 플래그를 쓴다. 테스트: `TestCheckArgsShortFlagVariants`(거부·통과 목록이 위 표와 같다).

**3. 마스킹과 인코딩 에코, 줄 상한** — (a) 검토 결과: `--env KEY=${ENV}` 로 해석된 비밀은 값 그대로만 치환했고 base64·URL 인코딩 에코는 가려지지 않았다. 이제 `mcp.SecretForms` 가 비밀과 표준·URL-safe base64(패딩 유무 4종)·URL 쿼리/경로 이스케이프를 합쳐 **비밀당 최대 7개 문자열**만 치환 목록에 넣는다(비용 한정. 8바이트 미만 비밀은 인코딩 형태가 정상 텍스트와 겹칠 수 있어 값 그대로만). 적용 범위: refresh 캐시(`maskDiscovered`)·진단 메시지·게이트웨이 응답·`forge tool` 출력(`masker`). 중복이던 치환 루프 3개는 `maskText` 하나로 모았다(순서가 비결정이던 `sanitize` 도 긴 값 먼저로 통일). **가리지 못하는 것**: 비밀이 더 긴 텍스트 안에 섞여 인코딩된 경우(정렬이 달라 전체의 인코딩이 다르다. 예 `base64("user:" + 비밀)`), hex·이중 인코딩·사용자 정의 변환, 환경변수 참조가 아닌 literal 값, 그리고 **비밀 값을 모르는 곳**: ledger 와 `sync --diff` 의 마스킹은 값이 아니라 키 이름·패턴으로 가리므로 인코딩된 에코는 패턴에 걸릴 때만 가려진다. (b) stdio 서버의 한 줄(JSON-RPC 메시지 하나, tools/list 응답 포함)이 4 MiB 를 넘으면 연결을 끊는다. 오류가 이제 서버 이름·상한(4 MiB)·원인(tools/list 응답 등)·대처(도구 설명·스키마를 줄이거나 서버를 나눠 등록)를 한 문장으로 알리고 출력 내용은 싣지 않는다. 테스트: `TestRefreshMasksEncodedEchoesOfResolvedSecret`(refresh 통합), `TestMaskCoversBase64AndURLEscapedEchoes`, `TestMaskerCoversEncodedEchoes`, `TestStdioOversizedLineErrorNamesServerAndLimit`.

**4. 버전 미고정 패키지의 고정 방법** — 경고(`unpinned`·`range`·`tag`) 바로 아래에 `고정 방법:` 한 줄을 더 보여 준다(기본 동작과 `--strict` 는 그대로). 근거가 설치본이 아니라 각 도구의 알려진 문법이므로 버전에 따라 다를 수 있는 것은 "미확인"으로 표시한다.

| 러너 | 안내하는 형태 | 비고 |
|------|---------------|------|
| `npx`·`bunx`·`pnpm dlx`·`npm exec` (`yarn dlx`·`bun x` 도 같은 문법) | `패키지@1.2.3` (예: `npx -y pkg@1.2.3`), 최신 버전 확인 `npm view 패키지 version` | `@latest`·`^`·`~`·태그는 고정이 아님 |
| `uvx`·`uv tool run` | `패키지==1.2.3` | `패키지@1.2.3` 형태는 uv 버전별 지원 **미확인** (등록 판정은 둘 다 고정으로 본다) |
| `pipx run` | `--spec '패키지==1.2.3' 실행파일` | `pipx run '패키지==1.2.3'` 형태는 pipx 버전별 **미확인** |
| `deno run` | `npm:패키지@버전`·`jsr:패키지@버전`, URL 은 경로에 버전 포함 | |
| `docker`·`podman run` | `이미지:정확한 태그` 또는 `이미지@sha256:<64자 hex>` (태그는 재지정될 수 있어 다이제스트 권장). 다이제스트는 이미지를 받은 뒤 `docker inspect --format '{{index .RepoDigests 0}}' 이미지` | |

테스트: `TestPreviewPinGuidancePerRunner`(deno 는 실제로 쓴 스킴 `npm:`/`jsr:` 만 보여 준다). 이 안내는 문구일 뿐 어떤 명령도 실행하지 않는다.

**결정 필요(사용자)** — 아래 둘은 **구현하지 않았고 기본값은 바꾸지 않았다**. 권고만 적는다.

1. `forge mcp add --strict` 기본값(현재 꺼짐, 질문 없이 등록): **권고 — 기본은 유지**하고, 경고(`warn`)가 있을 때만 승인을 요구하는 중간 단계를 다음 후보로 둔다. 이유: 등록은 서버를 실행하지 않고(`refresh` 가 실행), 기본 질문은 시연·스크립트 호환을 깨며, 경고 없는 등록에는 질문의 이득이 없다. 바꾼다면 비대화형에서 `--yes` 가 없을 때 거부할지 통과시킬지도 함께 정해야 한다.
2. `npx -y` 의 설치 확인 생략: Forge 는 사용자가 준 명령줄을 바꾸지 않는다(바꾸면 서버 동작이 달라진다). **권고 — 지금은 경고만 유지**하고, 다음 단계 후보는 "`-y` + 버전 미고정" 조합에만 승인을 요구하는 것이다. 거부는 권하지 않는다: 클라이언트가 띄우는 stdio 서버에는 터미널이 없어 `-y` 없이는 설치 질문에서 멈출 수 있다(npm 버전별 동작은 **미확인**). 고정 버전(`pkg@1.2.3`)과 함께 쓰는 `-y` 는 매번 다른 코드를 받지 않으므로 경고 수준을 낮추는 것도 검토할 수 있다.

## 5. 사용자가 해야 할 일 (체크리스트)

1. **설치 전에 미리보기를 읽는다.** `forge skill add --preview <소스>`, `forge mcp add --preview ...` 는 아무것도 설치·등록하지 않는다. 특히 `hooks`, `allowed-tools`, 실행 가능한 파일, `npx -y`·`@latest`, 평문 http, 사설 IP 경고를 본다.
2. **해시를 고정한다.** 미리보기의 `트리 sha256` 을 `--sha256` 으로, git 은 `--ref <40자 커밋>` 으로 고정한다. 정기적으로 `forge skill check`(로컬 변조 탐지)와 필요할 때 `forge skill check --fetch` · `forge skill update <이름>`(변경 요약 확인 후 승인)을 쓴다.
3. **MCP 도구 정의를 승인하고 필요하면 고정한다.** `forge mcp refresh` 후 `forge mcp show <이름>` 으로 설명을 읽고 `forge mcp approve <이름>`. 신뢰하는 서버에는 게이트웨이 노출과 `forge mcp pin <이름> --enforce` 를 고려한다. 변경 경고가 나오면 바뀐 설명을 먼저 읽는다.
4. **비밀은 `${ENV}` 참조로만** 넣는다(`--env KEY='${KEY}'`, `--header 'Authorization=${TOKEN}'`). 값을 명령줄·설정 파일에 직접 쓰지 않는다. 서버에 필요한 변수만 선언한다.
5. **`forge doctor` 를 주기적으로 실행한다.** 스킬 저장소 변조(`security.skills`), 승인된 정의와 다른 MCP 도구(`security.mcp`), 백업 권한·개수, 설정 파일 권한을 본다.
6. **신뢰할 수 없는 저장소에서는 `forge sync --apply` 전에 `--dry-run --diff` 를 확인한다.** 낯선 저장소의 `.mcp.json`·`.claude/settings.json` 은 클라이언트가 명령을 실행할 수 있는 파일이다.
7. **`confirm=ask` 가 필요한 도구는 elicitation 을 지원하는 클라이언트에서 쓴다.** 코드 실행형 바이너리를 도구로 등록할 때는 deny 목록이 샌드박스가 아님을 기억한다.
8. `~/.forge` 와 `$FORGE_HOME` 은 본인만 쓰게 둔다(권한 0700). 이 디렉터리에 쓰는 다른 프로세스는 신뢰 경계 안에 있다.

## 6. 이번 태스크에서 추가된 명령 요약

| 명령 | 요약 |
|------|------|
| `forge skill add [--preview] [--yes] [--strict-local] [--sha256 H] [--ref R] [--json] <소스>` | 미리보기(stderr) 출력. git/zip 은 승인 필수(터미널 y/N 또는 `--yes`, 아니면 거부·종료 코드 1). 로컬 디렉터리는 질문 없음(`--strict-local` 로 승인 필수) |
| `forge skill update <이름> [--yes]` | 기록된 소스를 다시 가져와 변경 요약 후 승인 시 새 불변 버전 추가 |
| `forge skill check [<이름>] [--fetch] [--json]` | 읽기 전용. 설치 파일 해시 vs 기록된 `content_hash`(변조 탐지). `--fetch` 일 때만 원본(네트워크) 비교 |
| `forge mcp add [--preview] [--strict] [--yes] ...` | 등록 전 미리보기. 기본은 질문 없음 |
| `forge mcp approve <이름> [--tool 도구]...` | 현재 캐시된 도구 정의를 승인(해시·정의·시각 저장) |
| `forge mcp pin <이름> --enforce\|--no-enforce` | 게이트웨이가 승인된 정의와 같은 도구만 노출(기본 꺼짐) |
| `forge doctor` | `security.skills`, `security.mcp`, `security.mcp.unapproved`, `security.mcp.enforce` 점검 추가 |

저장 형식: 스킬 `skill_versions.source_ref` 는 `--ref` 를 준 경우 `<요청 ref>@<커밋>`, 아니면 커밋 해시(기존 의미 유지). 승인 기록은 마이그레이션 `0002_mcp_tool_approvals.sql` 의 `mcp_tool_approvals` 테이블, enforce 는 `settings` 키 `mcp.enforce.<서버>`. `db/schema.sql` 은 이제 모든 마이그레이션을 번호 순으로 이어 붙인 스냅샷이다(`make check-schema`).
