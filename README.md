# Forge

**로컬 AI 코딩 CLI(Claude Code, OpenCode)를 위한 스킬 · 도구 · MCP 관리와 사용 이력 추적.**

> **상태: 알파.** 핵심 기능(스킬·MCP·커스텀 도구·프로필·sync·이력·doctor)은 구현되어 있고 **임시 `HOME`/`FORGE_HOME` 환경에서 검증**했습니다(자동 테스트와 [안전한 시연](docs/guide/demo.md)). **실제 Claude Code·OpenCode 클라이언트가 Forge 가 생성한 설정을 로드하는지는 아직 검증하지 못했습니다.** 프로젝트 이름과 모듈 경로는 가칭이고 라이선스는 정해지지 않았습니다. 공개 API·명령·저장 형식은 바뀔 수 있습니다.

---

## 왜 만드나

AI 코딩 CLI를 쓰다 보면 스킬, MCP 서버, 커스텀 도구가 CLI·프로젝트마다 흩어집니다.

- 어떤 스킬을 어디에 설치했는지, 어느 버전인지 알기 어렵습니다.
- MCP 서버 설정을 CLI마다 따로 복붙해서 유지해야 합니다.
- 무엇이 실제로 쓰이는지, 모델이 찾았지만 **없어서 못 쓴 스킬**이 무엇인지 알 수 없습니다.

Forge는 이를 한곳에서 등록·관리하고, 각 CLI가 읽는 형태로 내려주며, 호출 이력을 남깁니다.
관리 모델은 오픈소스 [ARTEX 한국어판](https://github.com/jiwoochris/artex-ko)의 웹 콘솔(스킬·도구·MCP·기록 메뉴)에서 벤치마킹했습니다.

## 핵심 기능과 구현 현황

아래 명령은 모두 현재 구현되어 있습니다(`forge <명령> --help` 로 확인). 웹 UI와 팀 공유는 아직 없습니다.

| 영역 | 구현된 하위 명령 | 하는 일 |
|------|------------------|---------|
| 초기화 | `forge init` | 레지스트리(SQLite)와 `~/.forge`(또는 `$FORGE_HOME`) 생성 |
| **스킬** `forge skill` | `add` `update` `check` `list` `show` `remove` `enable` `disable` `validate` `misses` | [agentskills.io](https://agentskills.io) 규격 `SKILL.md` 가져오기(디렉터리·zip·git), 설치 전 미리보기(`--preview`), git/zip 승인 정책, `--sha256`·`--ref` 고정, 변조 탐지(`check`), 불변 버전, 활성화/비활성화 |
| **MCP** `forge mcp` | `add` `list` `show` `remove` `refresh` `approve` `pin` `enable` `disable` `expose` `serve` | stdio/http/sse 서버 등록(비밀은 `${ENV}` 참조만), `tools/list` 발견·캐시, 도구 정의 승인과 고정(`approve`, `pin --enforce`), 클라이언트별 노출(`expose`), **게이트웨이**(`serve`) |
| **도구** `forge tool` | `add` `list` `show` `remove` `enable` `disable` `test` | command/script/http 커스텀 도구(JSON 스키마, 셸 없는 argv 실행, 실행 정책·`confirm`), 게이트웨이로 노출 |
| **프로필** `forge profile` | `create` `add` `remove` `show` `list` `use` `unuse` `current` `delete` `prune-stale` | 클라이언트·프로젝트별 노출 허용 목록 묶음, 스킬 버전 고정 |
| **동기화** `forge sync` | (플래그) `--target claude-code\|opencode\|all` `--dry-run` `--diff` `--apply` `--restore` `--list-backups` `--prune-backups` 등 | 레지스트리를 Claude Code(`.claude/skills`, `.mcp.json`, hook)와 OpenCode(`.opencode/skills`, `opencode.json` 의 `mcp`) 설정으로 렌더링. 기본은 계획만, 쓰기 전 백업, 사용자 항목 보존, 충돌 시 중단, 버전 게이팅 |
| **이력** `forge history` | (목록) `summary` `top` `unused` `misses` (+ `forge skill misses`) | 호출 원장 조회·집계, 기간·종류·클라이언트 필터, table/json/csv, 없어서 못 쓴 스킬(miss) |
| **점검** `forge doctor` | (플래그) `--scope` `--project-dir` `--json` `--strict` | 읽기 전용 상태 점검, 원인과 해결 명령 안내, 보안 점검(스킬 변조·MCP 도구 정의 변경) |
| **hook** `forge hook` | `ingest` `snippet` | Claude Code hook 이벤트 기록(항상 종료 코드 0), `settings.json` 스니펫 출력 |

## 동작 구조

```text
 forge CLI ──▶ Registry ($FORGE_HOME/forge.db + skills/)
                 │   스킬 · MCP 서버 · 커스텀 도구 · 프로필(허용 목록) · 이력 원장
     ┌───────────┼───────────────────────┐
     ▼           ▼                       ▼
  forge sync   게이트웨이 forge mcp serve   Ledger(이력)
  (설정 렌더링) 커스텀 도구 · MCP 프록시       ▲ claude-code hook / 게이트웨이
     │          · forge_skill / forge_list    │
     ▼           ▼                            │
  Claude Code   Claude Code · OpenCode ───────┘
  OpenCode      (게이트웨이는 MCP 서버 하나로 등록)
```

- **Registry가 원본**입니다. 각 CLI의 설정 파일은 Forge가 렌더링한 결과물이며, Forge가 쓴 항목은 사이드카 상태 파일(해시)로 구분해 직접 작성한 설정은 건드리지 않습니다.
- **프로필**이 활성화되면 허용 목록입니다: 프로필에 없는 항목은 활성이어도 노출되지 않습니다. 활성 프로필이 없으면 활성화된 항목 전체를 노출합니다.
- **게이트웨이**를 쓰면 CLI에 Forge MCP 서버 하나만 등록해도 커스텀 도구, 게이트웨이로 노출한 MCP 서버(프록시), 스킬 조회를 쓸 수 있고 호출 이력이 기록됩니다. 이력은 Claude Code hook 과 게이트웨이로 수집하며, OpenCode 는 hook 수집이 없어 게이트웨이 경로만 있습니다.
- 클라이언트 설정 형식 근거는 [`docs/clients/`](docs/clients/claude-code.md)에 있고, 근거가 약한 부분은 "미확인"으로 표시했습니다.

## 빠른 시작

- 공통 빠른 시작: [`docs/guide/quickstart.md`](docs/guide/quickstart.md)
- Claude Code: [`docs/guide/quickstart-claude-code.md`](docs/guide/quickstart-claude-code.md)
- OpenCode: [`docs/guide/quickstart-opencode.md`](docs/guide/quickstart-opencode.md)
- 문제 해결: [`docs/guide/troubleshooting.md`](docs/guide/troubleshooting.md)

요약(연습용 임시 디렉터리만 사용하며 실제 `~/.forge` 와 클라이언트 설정은 건드리지 않습니다. 저장소 루트에서 같은 셸로 실행):

```bash
make build
export PATH="$PWD/dist:$PATH"
export FORGE_HOME="${FORGE_QS_TMP:-/tmp}/forge-quickstart-home"
forge init
forge doctor
```

이어서 스킬 추가(미리보기·승인), MCP 등록, 커스텀 도구, 프로필, `forge sync`(계획→`--diff`→`--apply`), 이력 조회까지의 전체 명령은 [빠른 시작](docs/guide/quickstart.md)에 있습니다. 그 문서의 명령은 `make quickstart-check` 로 격리된 임시 환경에서 실제 실행해 검증합니다. **소요 시간은 두 가지로 봐야 합니다**: 명령 실행 자체는 수 초(자동 검증으로 측정, 첫 빌드 제외), 문서를 처음 읽으며 따라 하면 보통 10분 안팎입니다.

## 설치

아직 배포 패키지가 없고 **공개 저장소 URL 도 없습니다**(확정 전). 이 저장소의 소스 디렉터리를 이미 가지고 있다면 그 디렉터리에서 바로 빌드합니다(Go 필요, `go.mod` 의 `go` 지시자 버전 이상).

```bash
make build
```

저장소가 공개되면 `git clone <저장소 URL>` 로 받은 뒤 같은 방법으로 빌드하세요(`<저장소 URL>` 자리표시자를 그대로 붙여넣으면 셸 오류가 납니다). 처음 빌드할 때 Go 모듈 캐시가 비어 있으면 **모듈 다운로드에 네트워크가 필요**하고, 한 번 받은 뒤에는 오프라인으로 빌드할 수 있습니다.

바이너리는 `dist/forge` 에 만들어집니다. PATH 에 두려면 `export PATH="$PWD/dist:$PATH"` 를 쓰세요. 모듈 경로(`github.com/heonjinjeong/forge`)는 가칭이며 실제 원격과 다르므로 **`go install` 은 지원하지 않습니다.** 크로스 컴파일 스냅샷은 `make release-snapshot`(`dist/forge-<os>-<arch>`)입니다. macOS 에서만 검증했고 Linux·Windows 는 검증하지 않았습니다.

## 사용 예

모두 현재 구현된 명령입니다(`forge <명령> --help` 로 플래그를 확인할 수 있고, `make docs-check` 가 이 블록의 명령과 플래그가 존재하는지 검사합니다). 아래는 형태를 보여 주는 예이며 `<...>` 는 자리표시자입니다.

```bash
forge init                                      # ~/.forge 초기화 (FORGE_HOME 으로 위치 변경)

# 스킬: 설치 전에 항상 미리보기를 봅니다
forge skill add --preview ./my-skill            # 미리보기만, 아무것도 설치하지 않음
forge skill add ./my-skill                      # 로컬 디렉터리: 미리보기 후 진행
forge skill add ./my-skill.zip --yes --sha256 <미리보기의 트리 sha256>   # zip/git 은 승인 필요
forge skill list
forge skill misses                              # 모델이 찾았지만 등록돼 있지 않던 스킬

# MCP: 비밀은 ${ENV} 참조로만 (예제 서버는 저장소의 scripts/demo/mock_mcp.py)
forge mcp add demo-mcp --env DEMO_KEY='${DEMO_KEY}' -- python3 scripts/demo/mock_mcp.py
export DEMO_KEY=example-not-a-real-key
forge mcp refresh demo-mcp                      # tools/list 발견·캐시
forge mcp show demo-mcp                         # 도구 설명을 읽고
forge mcp approve demo-mcp                      # 신뢰하면 승인(이후 변경을 경고)

# 도구
forge tool add git-log --description '최근 커밋' --schema '{"type":"object","properties":{"count":{"type":"integer","minimum":1}},"required":["count"]}' -- git log -n '{{count}}'
forge tool test git-log --args '{"count":2}' --dry-run

# 프로필 → 클라이언트 설정으로
forge profile create work
forge profile add work --skill my-skill
forge profile use work --client all
forge sync --target claude-code --dry-run --diff    # 계획과 diff (아무것도 쓰지 않음)
forge sync --target claude-code --apply             # 백업 후 기록
forge sync --target opencode --apply

# 이력과 점검
forge history --since 7d --kind skill
forge doctor
```

## 보안과 프라이버시

Forge 는 **샌드박스가 아닙니다.** 등록·승인한 명령은 사용자 권한으로 실행됩니다. Forge 가 하는 일은 실행 전에 무엇이 실행될지 보여 주고, 바뀌면 알리고, 위험한 경로를 기본으로 막고, 비밀을 저장·출력하지 않는 것입니다. 위협 모델과 구현 위치·테스트·남은 위험은 [`docs/security.md`](docs/security.md) 에 있습니다.

- 이력은 **기본적으로 메타데이터만** 저장합니다(시각, 종류, 이름, 클라이언트, 성공 여부, 지연). 입력·출력 본문은 설정 테이블의 `record_payload` 를 켰을 때만 마스킹 후 저장하며 켜는 전용 CLI 명령은 아직 없습니다.
- MCP `env`·HTTP 헤더·도구의 비밀값은 DB에 평문으로 저장하지 않고 `${ENV}` 참조만 둡니다. 비밀로 보이는 literal 은 저장을 거부합니다(키체인 참조는 아직 미지원). 마스킹은 휴리스틱이라 놓칠 수 있습니다.
- 외부 스킬은 설치 전에 미리보기(트리 sha256, 위험 frontmatter, 실행 파일, 위험 패턴 휴리스틱)를 보여 주고, git/zip 은 승인을 요구하며, `--sha256`·`--ref` 로 고정합니다. MCP 서버는 등록 전 실행 명령줄을 보여 주고, 도구 정의를 승인해 두면 변경을 경고하며 `pin --enforce` 로 승인된 정의만 게이트웨이로 노출할 수 있습니다(기본 꺼짐, 게이트웨이 경유 노출에만 적용).
- `forge sync` 는 기본이 계획 출력이고 `--apply` 에서만 쓰며, 쓰기 전에 백업하고 사용자 항목은 수정하지 않으며 충돌이 있으면 아무것도 쓰지 않습니다. `~/.claude.json` 과 OpenCode 의 비밀 파일은 읽지도 쓰지도 않습니다. Forge 가 클라이언트 바이너리에 실행하는 것은 `--version` 하나뿐입니다.

## 안전한 시연

내 환경을 건드리지 않고 전체 흐름을 한 번에 확인합니다(임시 HOME·`FORGE_HOME`·프로젝트와 가짜 `claude`/`opencode`, 모델 호출·외부 네트워크 없음). 저장소 루트에서:

```bash
make demo
```

시나리오와 단계별 기대 출력은 [`docs/guide/demo.md`](docs/guide/demo.md) 입니다. 시연은 실제 클라이언트를 실행하지 않으므로 클라이언트 로드는 증명하지 못합니다.

## 알려진 한계

- **실제 Claude Code·OpenCode 가 생성된 설정을 로드하는지 미검증**입니다. 게이트웨이의 실제 `clientInfo.name`, elicitation 지원 여부, 도구 이름 64자 한도, 스킬 frontmatter `hooks` 를 클라이언트가 어떻게 처리하는지도 문서·가짜 클라이언트 기준입니다. Linux·Windows 도 검증하지 않았습니다.
- `forge tool add` 의 기본 `confirm` 은 `ask` 이고, 클라이언트가 elicitation 을 지원하지 않으면 그 도구는 안전하게 **거부**됩니다. OpenCode 의 지원은 확인하지 못했으므로 `--confirm none` 이 필요할 수 있습니다.
- `opencode.jsonc` 만 있는 프로젝트는 주석을 보존할 수 없어 sync 가 중단됩니다. OpenCode 는 hook 수집이 없습니다.
- 프로젝트 스코프 레지스트리(`--project`)는 미지원이고 전역 레지스트리만 내보냅니다. user 스코프 MCP 는 파일을 쓰지 않고 명령만 출력합니다.
- 커스텀 도구의 deny 목록은 심층 방어일 뿐 샌드박스가 아니며, 코드 실행형 바이너리(`sqlite3`, `npm exec` 등)를 자동 분류하지 않습니다. 스킬 미리보기의 위험 패턴은 단순 휴리스틱입니다.
- 스킬 트리 **안의** 하드링크 검사는 아직 없습니다. 승인과 해시 고정은 "내가 본 내용과 같다" 는 것만 보장합니다.
- 비밀 탐지는 휴리스틱입니다. 파일 경로 인자와 `--ref`·`commit=` 같은 이름 뒤의 40·64자 hex 는 비밀로 보지 않으므로, 접두 없는 비밀을 거기에 넣으면 평문으로 저장될 수 있습니다. 비밀은 항상 `${ENV}` 참조로 넣으세요([`docs/security.md`](docs/security.md)).
- 웹 UI(M6)와 팀 공유(M7)는 구현되지 않았습니다. 프로젝트 이름은 가칭입니다.
- 자세한 한계는 [`docs/security.md`](docs/security.md) 4절과 [`docs/sync.md`](docs/sync.md) "한계" 절.

## ARTEX와의 관계

| 가져오는 것 | 가져오지 않는 것 |
|-------------|------------------|
| 스킬·도구·MCP 관리 모델, 사용 원장, miss 기록, 에이전트별 노출 범위 개념 | 침투 테스트 에이전트, 녹화 프록시, 자산·취약점 관리, 알림 발송 등 **공격형 기능 전부** |

ARTEX는 AGPL-3.0 입니다. Forge는 **개념과 데이터 모델만 참고**하고 코드를 복사하지 않습니다.

## 로드맵

| 마일스톤 | 내용 | 상태 |
|----------|------|------|
| M0 | 레포 구조, 데이터 모델 ADR, CLI 설정 형식 검증 | 구현 완료 |
| M1 | 스킬 레지스트리 | 구현 완료 |
| M2 | MCP 레지스트리 | 구현 완료 |
| M3 | 커스텀 도구 + 게이트웨이 | 구현 완료 |
| M4 | 이력(원장·조회·miss) | 구현 완료 |
| M5 | 동기화 어댑터·프로필·doctor | 구현 완료 |
| 보안 | 설치 미리보기·승인·해시 고정·MCP 도구 정의 고정·위협 모델 | 구현 완료 |
| 문서 | README, 빠른 시작 가이드, 안전한 시연 | 이 문서 |
| 예정 | 실제 클라이언트 로드 검증, 도구 위험 분류, 이력 수집 보강 등 후속 | 미정 |
| M6 / M7 | (선택) 웹 UI / 팀 공유 | 미구현 |

상세 계획: [`docs/PLAN.md`](docs/PLAN.md)

## 개발

Makefile 타깃:

| 타깃 | 하는 일 |
|------|---------|
| `make build` | `dist/forge` 빌드 |
| `make test` | 스키마 스냅샷 검사 후 `go test ./...` |
| `make lint` | `golangci-lint`(없으면 `go vet` + `gofmt` 검사) |
| `make fmt` | `gofmt -w .` |
| `make check-schema` | `db/schema.sql` 이 마이그레이션 이어 붙임과 같은지 검사 |
| `make release-snapshot` | 4개 OS/아키텍처 크로스 빌드 |
| `make demo` | 안전한 시연(`scripts/demo/demo.sh`) |
| `make docs-check` | README·`docs/` 의 링크, 명령·플래그 존재, 비가시 문자, 로컬 전용 경로 검사(명령을 실행하지는 않음) |
| `make quickstart-check` | 빠른 시작 문서의 명령을 격리된 임시 환경에서 실제 실행하고 소요 시간 출력 |
| `make clean` | `dist/` 삭제 |

테스트 실행:

```bash
go test ./... -short
```

`-short` 는 오래 걸리는 시험(예: 원장 100만 행 성능 시험)을 건너뜁니다. 전체 시험(`make test`)에는 그 100만 행 성능 시험이 포함되며 **호스트 부하에 민감**해 부하가 높으면 느려지거나 시간 제한에 걸릴 수 있습니다. 그럴 때는 `go test ./... -short -count=1 -p 1` 처럼 패키지를 순차 실행하세요. 테스트는 임시 `HOME`/`FORGE_HOME` 을 쓰며 실제 `~/.forge` 와 클라이언트 설정을 건드리지 않습니다.

## 라이선스

**미정.** 저장소에 `LICENSE` 파일이 아직 없으므로 별도 라이선스를 부여하지 않은 상태입니다(프로젝트 이름과 함께 확정 예정, 이름도 가칭입니다). 확정 전에는 사용·배포 조건이 정해지지 않았습니다.

## 문서

전체 목차는 [`docs/README.md`](docs/README.md) 입니다.

- 가이드: [빠른 시작](docs/guide/quickstart.md) · [Claude Code](docs/guide/quickstart-claude-code.md) · [OpenCode](docs/guide/quickstart-opencode.md) · [시연](docs/guide/demo.md) · [문제 해결](docs/guide/troubleshooting.md)
- 설계: [게이트웨이](docs/gateway.md) · [sync](docs/sync.md) · [프로필](docs/profile.md) · [doctor](docs/doctor.md) · [어댑터](docs/adapter.md) · [보안과 위협 모델](docs/security.md) · [클라이언트 형식(Claude Code)](docs/clients/claude-code.md) · [클라이언트 형식(OpenCode)](docs/clients/opencode.md)
- 계획: [`docs/PLAN.md`](docs/PLAN.md)
