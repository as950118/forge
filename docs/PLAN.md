# Forge 프로젝트 계획

> 상태: Draft v0.1 · 2026-10-21 · 태스크는 `.company-os/tasks/` 에서 관리한다.

## 1. 목표

로컬 AI 코딩 CLI(Claude Code, OpenCode 등)에서 쓰는 **스킬 · 도구 · MCP 서버**를 한곳에서 등록·관리하고,
**사용 이력**을 남겨 무엇이 실제로 쓰이는지(그리고 무엇이 없어서 못 썼는지) 볼 수 있게 한다.
관리 모델은 ARTEX 웹 콘솔의 시스템 메뉴(스킬·도구·MCP)와 기록 메뉴(도구 실행·LLM 기록)에서 벤치마킹한다.

한 줄 요약: **"ARTEX의 관리 평면(control plane)만 떼어, 로컬 CLI 에이전트용으로 다시 만든다."**

### 비목표 (Non-goals)

- ARTEX의 공격형 기능(침투 테스트 에이전트, 녹화 프록시, 자산 트리, 취약점 집계, 알림 발송)은 **만들지 않는다.**
- 자체 에이전트 루프·LLM 호출기를 만들지 않는다. 실행 주체는 항상 사용자의 CLI다.
- v1에서 클라우드/멀티테넌트 서버는 다루지 않는다. 로컬 우선(local-first).

## 2. ARTEX 벤치마킹 매핑

| ARTEX | 근거 (artex-ko) | Forge에서의 형태 | 단계 |
|-------|-----------------|------------------|------|
| 시스템 › 스킬 (`/system/skills/`) | `skills/*/SKILL.md`, `server/skill_zip.go`, `skill_usage` 테이블 | `forge skill` — 디렉터리/zip/git 가져오기, 검증, 버전, 사용 통계 | M1 |
| 스킬 miss 기록 (`skill_usage.found=false`) | `server/skill_usage.go` | "있었으면 했던 스킬" 목록 (`forge skill misses`) | M4 |
| 시스템 › MCP (`/system/mcp/`) | `mcp_servers` 테이블, `server/mcpdiscover.go`, `mcphttp/` | `forge mcp` — stdio/http/sse 등록, `tools/list` 발견·캐시, 클라이언트별 노출 범위 | M2 |
| 시스템 › 도구 (`/system/tools/`) | `tools` 테이블, `server/customtool.go` (command/script/http) | `forge tool` — 커스텀 도구 정의·스키마·바인딩, Forge MCP 게이트웨이로 노출 | M3 |
| 기능 › 도구 실행 (`/function/commands/`) | `tool_usage` 테이블 (인자·결과는 저장하지 않고 귀속 정보만) | 호출 원장(ledger) — 기본은 메타데이터만, payload는 opt-in | M4 |
| 기능 › LLM 기록 (`/function/llm-records/`) | `llmrec/`, `llm_usage.go` | 세션 이력(시작/종료, 모델, 토큰) — CLI가 노출하는 범위에서 수집 | M4 |
| 시스템 › 가로채기 규칙 (`/system/intercept/`) | `intercept/` | (선택) 위험 명령 차단 규칙을 hook으로 배포 | M7 |
| 시스템 › 에이전트 (`/system/agents/`) | 에이전트별 프롬프트·도구 구성 | **프로필**: 클라이언트별/프로젝트별로 어떤 스킬·도구·MCP를 켤지 묶음 | M5 |

> 라이선스 주의: ARTEX는 AGPL-3.0 이다. 이 프로젝트는 **개념과 데이터 모델만 참고**하고 코드를 복사하지 않는다.
> 또한 상류 ARTEX는 오남용 이슈가 보고된 도구이므로, Forge는 공격형 기능을 의도적으로 제외한 별개 프로젝트로 유지한다.

## 3. 핵심 설계

### 3.1 구성요소

```text
                 ┌──────────────────────────────────────────┐
                 │               forge (Go 단일 바이너리)     │
 forge CLI ─────▶│  registry(SQLite) · importer · adapters   │
                 │  gateway(MCP server) · ledger · web UI(선택)│
                 └───────┬───────────────┬───────────────┬───┘
                         │ sync          │ MCP(stdio/http)│ hook events
                         ▼               ▼               ▲
                 Claude Code      OpenCode …        (PostToolUse 등)
                 ~/.claude/…      opencode.json
```

- **Registry**: `~/.forge/forge.db`(SQLite) + `~/.forge/skills/<name>@<version>/` 파일. 프로젝트 범위는 `.forge/`.
- **Adapters**: 레지스트리 내용을 각 CLI가 읽는 형태로 *렌더링*한다. 원본은 항상 Forge 쪽이 소스 오브 트루스.
  - Claude Code: `.claude/skills/`, `.mcp.json`, `settings.json` hooks
  - OpenCode: `opencode.json`(mcp), 스킬 디렉터리, 플러그인 이벤트
  - 형식 세부는 **M0 스파이크에서 실제 버전으로 검증**한 뒤 확정한다(추정으로 구현하지 않는다).
- **Gateway** (`forge mcp serve`): Forge가 MCP 서버가 되어
  1. 등록된 커스텀 도구,
  2. 등록된 MCP 서버의 프록시(집계·필터),
  3. `skill` 메타 도구(이름으로 스킬 본문 조회 — ARTEX의 Skill meta-tool과 동일 아이디어)
  를 하나의 엔드포인트로 제공한다. CLI에는 서버 하나만 등록하면 되고, **호출 이력은 게이트웨이에서 자연스럽게 기록**된다.
- **Ledger**: 호출 1건 = 1행. 기본 필드는 `ts, kind(skill|tool|mcp), name, client, project, session_id, ok, latency_ms, found`.
  인자·결과 본문은 기본 미저장(ARTEX `tool_usage` 방침), `--record-payload`로만 켠다. 비밀값 마스킹 필수.

### 3.2 기술 선택 (제안)

| 항목 | 선택 | 이유 |
|------|------|------|
| 언어 | Go | 단일 바이너리 배포, ARTEX와 같은 스택이라 구조 비교가 쉬움, MCP 서버/클라이언트 구현 용이 |
| 저장소 | SQLite (순수 Go 드라이버) | 로컬 우선, 설치 의존성 없음. ARTEX의 PostgreSQL 은 과함 |
| CLI | cobra | 하위 명령 구조(`skill/tool/mcp/history/sync`)에 적합 |
| 스킬 규격 | agentskills.io `SKILL.md` | ARTEX·Claude Code가 공통으로 따르는 규격 |
| UI | v1은 CLI/TUI, 웹 UI는 M6(선택) | 먼저 데이터 모델과 동기화 신뢰성 확보 |

### 3.3 CLI 초안

```text
forge init [--project]
forge skill   add|list|show|remove|enable|disable|validate|misses
forge mcp     add|list|show|remove|refresh|enable|disable|serve
forge tool    add|list|show|remove|enable|disable|test
forge profile create|use|list                # 클라이언트/프로젝트별 노출 묶음
forge sync    [--target claude-code|opencode] [--dry-run] [--diff]
forge history [--kind] [--since] [--client] [--json]   # 조회/집계/내보내기
forge doctor                                  # 클라이언트 설정·드리프트 점검
```

### 3.4 보안 원칙

- 비밀값(MCP `env`, HTTP 헤더)은 평문 DB 저장 금지 → OS 키체인 또는 `${ENV}` 참조만 저장.
- `command`/`script` 도구는 실행 전 확인 정책(allowlist·타임아웃·작업 디렉터리 제한)을 가진다.
- 외부에서 가져온 스킬/MCP는 **설치 전 미리보기**(실행 명령·요청 URL·스크립트 목록 표시)와 해시 고정.
- `forge sync`는 항상 `--dry-run`/diff 가능하고, 덮어쓰기 전 백업한다. Forge가 관리하는 항목은 마커로 구분하여 사용자가 직접 쓴 설정을 건드리지 않는다.
- 이력에는 기본적으로 프롬프트/코드 본문을 저장하지 않는다.

## 4. 마일스톤

| 마일스톤 | 내용 | 완료 기준 |
|----------|------|-----------|
| **M0 기반** | 레포 구조, 데이터 모델 ADR, CLI 형식 검증 스파이크 | 클라이언트별 설정 형식이 문서로 확정, 스키마 v1 합의 |
| **M1 스킬** | 스킬 가져오기·검증·버전·활성화 | `forge skill add ./dir` 후 Claude Code에서 스킬이 보임 |
| **M2 MCP** | MCP 서버 등록·발견·캐시·클라이언트별 노출 | 등록한 MCP의 `tools/list` 가 캐시되고 sync로 설정에 반영 |
| **M3 도구 + 게이트웨이** | command/script/http 도구, `forge mcp serve` | CLI에 Forge 하나만 등록해도 도구·MCP·스킬 조회가 동작 |
| **M4 이력** | 원장, hook/게이트웨이 수집, 조회·집계, miss 목록 | `forge history --since 7d` 가 스킬/도구/MCP별 호출 수를 보여줌 |
| **M5 동기화·프로필** | 어댑터 완성, 프로필, doctor, 드리프트 감지 | 프로필 전환 한 번으로 두 CLI 설정이 일관되게 바뀜 |
| **M6 웹 UI (선택)** | 로컬 대시보드(스킬/도구/MCP/이력) | ARTEX 시스템·기록 화면에 대응하는 읽기/편집 화면 |
| **M7 팀 공유 (선택)** | git 기반 레지스트리 공유, 위험 명령 차단 규칙 | 팀 레포에서 `forge sync` 로 동일 구성 재현 |

M0 → M1 → M2 → M3 → M4 순으로 진행하고, M5는 M1~M2 와 병행 가능하다. M6·M7은 v1 이후.

## 5. 리스크와 열린 질문

| # | 항목 | 대응 |
|---|------|------|
| R1 | CLI별 설정 형식·훅 이벤트가 버전마다 바뀜 | 어댑터 격리 + `forge doctor` + 버전별 픽스처 테스트 |
| R2 | CLI 내부 LLM 호출/토큰은 외부에서 보이지 않을 수 있음 | v1은 "도구/스킬/MCP 호출 이력" 중심. 토큰은 CLI가 transcript 로 제공하는 범위만 수집 |
| R3 | 스킬 호출이 "모델이 SKILL.md를 읽는 행위"라 hook 으로 정확히 잡기 어려움 | 게이트웨이 `skill` 메타 도구 경로 + transcript 파싱 중 어느 쪽이 가능한지 M0에서 검증 |
| R4 | 민감정보가 이력/설정에 섞임 | 마스킹, payload opt-in, 키체인 |
| Q1 | 프로젝트명 `forge` 확정 여부 | 확정 전까지 가칭 |
| Q2 | 1차 지원 클라이언트 범위 | Claude Code + OpenCode 로 시작, Codex·Gemini CLI 는 어댑터 인터페이스 확인 후 |
| Q3 | 웹 UI 필요 시점 | M4 이후 사용 패턴을 보고 결정 |

## 6. 태스크 관리

- 도구: `company-os-cli` (`company-os init` 으로 스캐폴드, `.company-os/tasks/TASK-NNNN.md`)
- 인덱스: `.company-os/tasks/README.md` 표
- 워크플로: `create-feature` (PM → Architect → Backend → Reviewer → QA → Release), 상태: Backlog → Ready → In Progress → In Review → QA → Done
- 초기 백로그는 마일스톤별로 TASK-0001 ~ 에 등록되어 있다.
