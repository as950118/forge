---
description: Forge에서 "onboarding" 워크플로우(Trigger: New human/agent/project)를 시작하거나 진행할 때 사용.
when_to_use: "새 프로젝트 온보딩해줘", "새 인원/에이전트 합류 절차 진행해줘" 같은 요청일 때.
---

# Company OS Workflow: onboarding

진행하기 전에 `.company-os/workflows/onboarding.md`를 전체 읽고 그대로 따른다 — Trigger/Participants/Inputs/Outputs/Approval Rules/Escalation Rules/Completion Conditions를 요약하거나 추측으로 대체하지 않는다.

전체 파이프라인 순서와 게이트는 `.company-os/docs/agent-collaboration-rules.md`(Stage Contracts, 승인 규칙)를 따른다. 각 단계에서 맡은 역할로 행동할 때는 해당 `company-os-<role>` 스킬을 사용한다 (예: PM 단계는 `company-os-pm`, Architect 단계는 `company-os-architect`).

## 원칙

- Approval Rules에 정의된 게이트(예: Reviewer → QA → DevOps)를 건너뛰지 않는다.
- 산출물(Artifact) 없는 Handoff는 무효로 취급한다.
- Escalation Rules에 해당하면 즉시 멈추고 에스컬레이션한다.
- 결정/사고는 `.company-os/memory/`에 기록한다.
