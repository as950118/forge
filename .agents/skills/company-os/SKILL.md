---
description: Company OS SSOT(roles/skills/workflows/memory)를 참고해 이 프로젝트의 협업 규칙을 따를 때 사용. "company-os", "역할대로 진행해줘", "문서 기준으로 해줘", "추측하지 말고 확인해줘" 같은 요청에 사용.
when_to_use: 어떤 역할(PM/Architect/Designer/Backend/Frontend/Reviewer/QA/DevOps/Technical Writer/CEO)로 행동해달라는 요청이거나, create-feature/fix-bug/release/incident/onboarding 워크플로우를 시작하라는 요청일 때. 더 구체적인 역할·워크플로우가 정해져 있다면 company-os-<role> 또는 company-os-<workflow> 스킬을 대신 사용한다.
---

# Company OS

이 프로젝트는 `.company-os/` 아래에 Company OS SSOT(Single Source of Truth)를 갖고 있다. 이 문서들의 내용을 추측하거나 요약으로 대체하지 말고, 필요한 파일을 직접 읽어라.

## 먼저 읽을 것

- `.company-os/company/vision.md`, `mission.md`, `values.md`, `tech-stack.md` — 회사/제품 컨텍스트
- `.company-os/docs/agent-collaboration-rules.md` — 전체 협업 파이프라인과 Stage Contract
- `.company-os/roles/` — 역할별 R&R (10개)
- `.company-os/skills/` — 역할 공통 재사용 절차 (write-prd, write-adr, code-review 등)
- `.company-os/workflows/` — Feature/Bug/Release 등 엔드투엔드 파이프라인
- `.company-os/memory/` — company/project/decision/task/lessons-learned 인덱스

## 더 구체적인 요청이면 이 스킬 대신 사용

- 특정 역할로 행동 → `company-os-<role>` (예: `company-os-pm`, `company-os-designer`, `company-os-architect`)
- 특정 워크플로우 진행 → `company-os-<workflow>` (예: `company-os-create-feature`, `company-os-fix-bug`)

## 원칙

- 문서에 없는 내용은 추측하지 말고 질문하거나 에스컬레이션한다.
- 승인 게이트(Reviewer → QA → DevOps)를 임의로 건너뛰지 않는다.
- 기술 결정은 `.company-os/memory/decision-memory/`에 ADR로 남긴다.
