# 문서 목차

Forge 의 문서 인덱스입니다. 프로젝트 소개와 설치는 저장소 루트의 [`README.md`](../README.md) 를 보세요.

## 사용자 가이드 (`docs/guide/`)

| 문서 | 내용 |
|------|------|
| [빠른 시작](guide/quickstart.md) | 공통 빠른 시작: 빌드, `forge init`, 스킬 추가(미리보기·승인), MCP 등록(`${ENV}` 참조), 커스텀 도구(`confirm` 정책), 프로필, sync 계획, `forge doctor`, 이력 조회 |
| [빠른 시작: Claude Code](guide/quickstart-claude-code.md) | `forge sync --target claude-code` 의 계획 → `--diff` → `--apply`, `.mcp.json`·`.claude/skills`·hook, user 스코프, 버전 게이팅, 백업·복원, 게이트웨이 연결 |
| [빠른 시작: OpenCode](guide/quickstart-opencode.md) | `forge sync --target opencode`, `opencode.json` 의 `mcp`, `.opencode/skills`, `opencode.jsonc` 한계, `opencode reload`, `confirm=ask` 도구와 elicitation |
| [안전한 시연](guide/demo.md) | `make demo` 의 단계별 시나리오와 기대 출력(격리된 임시 환경, 가짜 클라이언트) |
| [문제 해결](guide/troubleshooting.md) | `forge doctor` 출력 → 원인 → 해결 명령 표, 충돌·중단된 sync·버전 게이트·승인 거부·하드링크·백업 복원 |

가이드의 명령은 `make docs-check`(링크와 명령·플래그 존재 검사)와 `make quickstart-check`(빠른 시작 명령 실제 실행)로 검증합니다. 두 검사의 범위와 한계는 [문제 해결](guide/troubleshooting.md#알려진-한계)에 있습니다.

## 설계 문서

| 문서 | 내용 |
|------|------|
| [`gateway.md`](gateway.md) | 게이트웨이 `forge mcp serve`: 도구 이름 규칙, 노출 필터링, `confirm` 정책, 프록시, 도구 정의 고정, `forge_skill`, 원장 |
| [`sync.md`](sync.md) | `forge sync`: 스코프별 대상, 사용자 항목 보존, 백업·복원, 크래시 복구, 프로필·`--target all`, 버전 게이트 |
| [`profile.md`](profile.md) | 프로필: 우선순위 규칙, 허용 목록, 활성화 |
| [`export.md`](export.md) | `forge export`: 집계 스냅샷(허용 목록), JSON, Postgres(Supabase Session pooler) 전체 교체, 검증 상태 |
| [`serve-api.md`](serve-api.md) | `forge serve-api`: 읽기 전용 조회 API, Bearer 인증(fail closed), 한계·보안 헤더, Docker·Render 배포, 검증 상태 |
| [`doctor.md`](doctor.md) | `forge doctor`: 점검 항목, 심각도, `--json` 스키마, 버전 게이팅, 읽기 전용 보장 |
| [`adapter.md`](adapter.md) | 동기화 어댑터 인터페이스와 공통 엔진 |
| [`security.md`](security.md) | 위협 모델, 완화책과 테스트, 알려진 한계, 사용자 체크리스트 |
| [`clients/claude-code.md`](clients/claude-code.md) | Claude Code 설정 형식 근거(`[doc]`/`[help]`/`[local]`/`[미확인]`) |
| [`clients/opencode.md`](clients/opencode.md) | OpenCode 설정 형식 근거 |

## 계획

- [`PLAN.md`](PLAN.md): 목표, ARTEX 벤치마킹 매핑, 핵심 설계, 마일스톤(구현 상태 포함), 리스크와 열린 질문. 구현 현황은 [`README.md`](../README.md) 를 참고하세요.
