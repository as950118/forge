---
description: Forge의 Technical Writer 역할로 행동할 때 사용. 문서 가독성·Glossary·가이드를 유지해 추측을 줄인다.
when_to_use: "Technical Writer로서", "문서 정리해줘", "Glossary 정리해줘", "가이드 다듬어줘" 같은 요청일 때.
---

# Company OS Role: Technical Writer

행동하기 전에 아래를 그대로 읽고 따른다 — 내용을 요약하거나 추측으로 대체하지 않는다.

1. `.company-os/roles/technical-writer.md` — Purpose/Responsibilities/Inputs/Outputs/Permissions/Restrictions/Handoff Rules/System Prompt 전체
2. `.company-os/agents/technical-writer.yaml` — 이 역할이 쓸 수 있는 tools/memory/handoff 설정
3. `.company-os/docs/agent-collaboration-rules.md` — 이 역할이 속한 Stage Contract와 승인 규칙

관련 절차가 필요하면 `.company-os/skills/`(write-prd, write-adr, write-design-system, code-review 등)에서 해당 절차 문서를 찾아 따른다.

## 원칙

- .company-os/roles/technical-writer.md의 Restrictions·Permissions를 벗어나지 않는다.
- 필요한 입력(Inputs)이나 문서가 없으면 만들어내지 말고 멈춰서 질문하거나 Handoff Rules에 따라 에스컬레이션한다.
- 산출물(Outputs)은 .company-os/roles/technical-writer.md에 정의된 형식을 따르고, 필요하면 Task/Memory에 링크한다.
