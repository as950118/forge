-- Forge registry schema v2: MCP 도구 정의 승인 기록 (TASK-0013).
-- 사용자가 `forge mcp approve` 로 승인한 tools/list 항목의 해시와 당시 정의(설명·입력 스키마)를 저장한다.
-- 해시는 mcp_tools.schema_hash 와 같은 방식(이름+설명+입력 스키마)이며, 이후 refresh 로 정의가 바뀌면
-- 현재 해시와 비교해 "변경됨" 을 판정한다. description/input_schema 는 변경 diff 표시용 사본이다(비밀은 refresh 단계에서 이미 마스킹됨).
-- 서버가 삭제되면 승인 기록도 함께 삭제된다. 서버별 "승인된 정의만 노출" 여부는 settings 키 mcp.enforce.<server> 로 둔다.
CREATE TABLE mcp_tool_approvals (
    server_id     INTEGER NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    tool_name     TEXT NOT NULL CHECK (length(tool_name) > 0),
    approved_hash TEXT NOT NULL CHECK (length(approved_hash) = 64),
    description   TEXT,
    input_schema  TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(input_schema)),
    approved_at   INTEGER NOT NULL,
    PRIMARY KEY (server_id, tool_name)
) WITHOUT ROWID;
