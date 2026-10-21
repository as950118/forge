# Forge

**로컬 AI 코딩 CLI(Claude Code, OpenCode 등)를 위한 스킬 · 도구 · MCP 관리와 사용 이력 추적.**

> 🚧 **상태: 기획 단계 (Pre-alpha).** 아래의 명령과 구조는 목표 설계이며 아직 구현되지 않았습니다.
> 진행 상황은 [`docs/PLAN.md`](docs/PLAN.md)와 [`.company-os/tasks/`](.company-os/tasks/README.md)에서 확인하세요.

---

## 왜 만드나

AI 코딩 CLI를 쓰다 보면 스킬, MCP 서버, 커스텀 도구가 CLI·프로젝트마다 흩어집니다.

- 어떤 스킬을 어디에 설치했는지, 어느 버전인지 알기 어렵습니다.
- MCP 서버 설정을 CLI마다 따로 복붙해서 유지해야 합니다.
- 무엇이 실제로 쓰이는지, 모델이 찾았지만 **없어서 못 쓴 스킬**이 무엇인지 알 수 없습니다.

Forge는 이를 한곳에서 등록·관리하고, 각 CLI가 읽는 형태로 내려주며, 호출 이력을 남깁니다.
관리 모델은 오픈소스 [ARTEX 한국어판](https://github.com/jiwoochris/artex-ko)의 웹 콘솔(스킬·도구·MCP·기록 메뉴)에서 벤치마킹했습니다.

## 핵심 기능 (목표)

| 영역 | 하는 일 |
|------|---------|
| **스킬 관리** | [agentskills.io](https://agentskills.io) 규격 `SKILL.md` 가져오기(디렉터리·zip·git), 검증, 버전 보관, 활성화/비활성화 |
| **MCP 관리** | stdio / http / sse 서버 등록, `tools/list` 발견·캐시, 클라이언트·프로젝트별 노출 범위 |
| **도구 관리** | command / script / http 커스텀 도구 정의(JSON 스키마), 실행 정책(타임아웃·allowlist) |
| **이력 관리** | 스킬·도구·MCP 호출 원장, 기간·종류·클라이언트별 집계, "없어서 못 쓴 스킬(miss)" 목록 |
| **CLI 연동** | `forge sync` 로 Claude Code·OpenCode 설정에 반영, `forge mcp serve` 게이트웨이로 하나의 MCP 엔드포인트 제공 |

## 동작 구조

```text
 forge CLI ──▶ Registry (SQLite + ~/.forge/skills)
                 │
     ┌───────────┼───────────────────────┐
     ▼           ▼                       ▼
  sync 어댑터   게이트웨이(MCP server)     Ledger(이력)
  (설정 렌더링)  도구·MCP 프록시·skill 조회   ▲ hook / 게이트웨이 / transcript
     │           │                         │
     ▼           ▼                         │
  Claude Code   OpenCode  ─────────────────┘
```

- **Registry가 원본**입니다. 각 CLI의 설정 파일은 Forge가 렌더링한 결과물이며, Forge가 관리하는 항목은 마커로 구분해 직접 작성한 설정은 건드리지 않습니다.
- **게이트웨이**를 쓰면 CLI에 Forge MCP 서버 하나만 등록해도 커스텀 도구, 등록한 MCP(프록시), 스킬 조회를 모두 쓸 수 있고, 호출 이력이 자동으로 기록됩니다.

## 사용 예 (목표 UX)

```bash
forge init                                  # ~/.forge 초기화 (프로젝트 범위는 --project)

# 스킬
forge skill add ./my-skill                  # 디렉터리/zip/git URL
forge skill list
forge skill misses                          # 모델이 찾았지만 없던 스킬

# MCP
forge mcp add github --transport http --url https://example.com/mcp
forge mcp refresh github                    # tools/list 발견·캐시

# 도구
forge tool add lint --kind command --exec 'make lint'
forge tool test lint

# CLI에 반영
forge sync --target claude-code --dry-run --diff
forge sync --target claude-code
forge sync --target opencode

# 이력
forge history --since 7d --kind skill
forge doctor                                # 설정 드리프트 점검
```

## 보안과 프라이버시 원칙

- 이력은 **기본적으로 메타데이터만** 저장합니다(시각, 종류, 이름, 클라이언트, 성공 여부, 지연). 프롬프트·코드·도구 인자/결과는 `--record-payload` 로 직접 켰을 때만 저장하고, 비밀값은 마스킹합니다.
- MCP `env`·HTTP 헤더의 비밀값은 DB에 평문으로 저장하지 않습니다(키체인 또는 `${ENV}` 참조).
- 외부 스킬/MCP는 설치 전에 실행 명령·URL·스크립트를 미리 보여주고 해시를 고정합니다.
- `forge sync` 는 항상 `--dry-run`/diff 가 가능하며 덮어쓰기 전에 백업합니다.

## ARTEX와의 관계

| 가져오는 것 | 가져오지 않는 것 |
|-------------|------------------|
| 스킬·도구·MCP 관리 모델, 사용 원장, miss 기록, 에이전트별 노출 범위 개념 | 침투 테스트 에이전트, 녹화 프록시, 자산·취약점 관리, 알림 발송 등 **공격형 기능 전부** |

ARTEX는 AGPL-3.0 입니다. Forge는 **개념과 데이터 모델만 참고**하고 코드를 복사하지 않습니다.

## 로드맵

| 마일스톤 | 내용 |
|----------|------|
| M0 | 레포 구조, 데이터 모델 ADR, CLI 설정 형식 검증 |
| M1 | 스킬 레지스트리 |
| M2 | MCP 레지스트리 |
| M3 | 커스텀 도구 + 게이트웨이 |
| M4 | 이력(원장·조회·miss) |
| M5 | 동기화 어댑터·프로필·doctor |
| M6 / M7 | (선택) 웹 UI / 팀 공유 |

상세 계획: [`docs/PLAN.md`](docs/PLAN.md)

## 개발 및 태스크 관리

태스크는 [company-os-cli](.company-os/README.md) 로 관리합니다.

```bash
# 태스크 목록
cat .company-os/tasks/README.md

# 새 태스크: 템플릿을 복사해 TASK-NNNN.md 작성 후 인덱스 표에 추가
cp .company-os/docs/task-template.md .company-os/tasks/TASK-0017.md

# 템플릿/스킬을 최신 company-os 버전에 맞추기 (memory/projects/tasks 는 건드리지 않음)
~/.local/company-os-venv/bin/company-os upgrade
```

워크플로: `PM → Architect → Backend → Reviewer → QA → Release`, 상태: `Backlog → Ready → In Progress → In Review → QA → Done`

## 라이선스

미정 (Q1 에서 프로젝트명과 함께 확정).
