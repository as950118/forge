# Forge — 작업 그라운드 룰

프로젝트 개요는 `README.md`, 계획은 `docs/PLAN.md`, 태스크는 `.company-os/tasks/` (company-os-cli)에서 관리한다.

## 에이전트 작업 방식 (필수)

1. **한 번에 태스크 하나.** `.company-os/tasks/README.md`의 순서·의존 관계를 따른다. 앞 태스크가 Done이 아니면 다음으로 넘어가지 않는다.
2. **작업은 서브 에이전트(Worker)가 수행한다.** 메인 에이전트는 구현·조사를 직접 하지 않고, 태스크 파일 경로와 범위를 담은 브리프만 넘긴다.
3. **검증은 별도의 서브 에이전트(Verifier)가 수행한다.** Worker와 다른 에이전트여야 하며, Worker의 결론이 아니라 산출물(파일, 명령 실행 결과)을 직접 확인한다. 각 AC를 항목별로 PASS/FAIL로 판정하고 근거를 남긴다.
4. **메인 에이전트는 결과만 받아 보고한다.** 사용자에게는 태스크 ID, 변경 파일, AC 판정, 남은 이슈를 간결히 전한다. Worker·Verifier의 로그를 그대로 붙이지 않는다.
5. **FAIL이면** Verifier의 지적을 Worker에게 되돌려 수정시키고 다시 검증한다(최대 2회). 그래도 실패하면 상태를 바꾸지 않고 사용자에게 보고한다.
6. **상태 갱신은 Verifier가 전원 PASS일 때만** 한다: 태스크 파일의 `Status`와 `tasks/README.md` 인덱스를 함께 `Done`으로 바꾼다.
7. **추측 금지.** 외부 CLI 형식·버전 등 사실은 실제 설치본/공식 문서로 확인하고, 확인하지 못한 것은 "미확인"으로 적는다.
8. **되돌리기 어렵거나 외부에 영향을 주는 동작**(push, 배포, 사용자 홈 설정 파일 수정, 전역 설치)은 사용자 확인 후에만 한다. 원격 push는 항상 사용자 확인 후에만 한다.
8-1. **브랜치·커밋·머지(2026-10-21 사용자 승인)**: 태스크마다 `main`에서 `task/TASK-NNNN-<slug>` 브랜치를 만들어 작업한다. Worker·Verifier는 커밋하지 않고, **Verifier 전원 PASS 후 메인이 커밋**한다(수정 라운드는 `fix:` 별도 커밋). 커밋 메시지는 `feat|fix|chore|docs(<scope>): <요약> (TASK-NNNN)` 형식이다. 커밋 후 곧바로 `main`에 머지(`--no-ff`)하고 태스크 브랜치는 남긴다. 한 커밋에는 해당 태스크 산출물만 담는다.
8-2. **태스크 이력은 로컬 전용**: `.company-os/`와 `.claude/`는 `.gitignore` 대상이며 커밋하지 않는다(태스크 상태 변경은 파일에만 반영). ADR 등 `.company-os/` 안의 산출물도 로컬에만 남는다.
8-3. **체크포인트(사용자 요청, 세션 한도 대비)**: `.company-os/memory/CHECKPOINT.md`(로컬 전용)에 현재 브랜치·진행 중 태스크와 단계·서브 에이전트 ID·다음 행동·사용자 결정과 미결정·반복된 함정을 적는다. 태스크 완료/머지, Worker·Verifier 결과 수신, 사용자 결정, 큰 서브 에이전트 실행 직전에 갱신한다(맨 위 `갱신` 시각 포함). 한도로 끊긴 서브 에이전트는 새로 만들지 말고 `SendMessage` 로 재개하며, 재개 전에 작업 트리 무결성(변이 테스트 잔존 여부)을 점검시킨다. 머지 직전에는 `python3 -I .company-os/memory/scan_invisible.py` 로 비가시 문자를 확인한다.
9. **ARTEX 참고 범위**: 관리 평면(스킬·도구·MCP·이력)의 개념과 데이터 모델만. 공격형 기능은 만들지 않고 코드도 복사하지 않는다(AGPL-3.0).

## 서브 에이전트 브리프 템플릿

- Worker: 태스크 ID, 읽을 파일(태스크·PLAN.md), 산출물 경로, 하지 말 것(범위 밖 수정·커밋·홈 설정 변경), 완료 시 변경 파일 목록과 자체 점검 결과를 요약해 반환.
- Verifier: 태스크 ID, AC 목록, "Worker 보고를 신뢰하지 말고 산출물을 직접 확인", 반환 형식은 AC별 PASS/FAIL + 근거 + 재작업 지시.

## graphify

This project has a knowledge graph at graphify-out/ with god nodes, community structure, and cross-file relationships.

Rules:
- When graphify-out/graph.json exists, run `graphify query "<question>"` before your first grep, search or source-file read for a codebase question, and put this rule in every subagent prompt that explores code. Read raw files only for lines the graph or an earlier read already located. Use `graphify path "<A>" "<B>"` for relationships and `graphify explain "<concept>"` for focused concepts. These return a scoped subgraph, usually much smaller than GRAPH_REPORT.md or raw grep output.
- If graphify-out/wiki/index.md exists, use it for broad navigation instead of raw source browsing.
- Read graphify-out/GRAPH_REPORT.md only for broad architecture review or when query/path/explain do not surface enough context.
- After modifying code, run `graphify update .` to keep the graph current (AST-only, no API cost).
- A committed graphify-out/graph.json is rewritten whole on every build, so a large diff to it in a pull request is expected. Do not flag it as churn or suggest skipping the graph rebuild or its hooks.
