-- Forge registry schema v3: miss 집계 보정 (TASK-0019).
-- 1) v_misses 가 kind='mcp' 를 서버 구분 없이 (kind, name) 으로 묶던 것을 (kind, mcp_server, name) 으로 묶는다
--    (kind != 'mcp' 는 mcp_server 가 NULL 이라 기존과 같다). 컬럼 mcp_server 를 추가한다.
-- 2) 레지스트리에 없는 MCP 서버를 사용자가 직접 붙인 호출(hook)은 miss 가 아니므로 found 를 NULL(미상)로 되돌린다.
--    gateway 가 명시한 found 값과 레지스트리에 연결된 행(mcp_server_id 있음)은 건드리지 않는다.
-- (0001/0002 는 수정하지 않는다. 뷰는 DROP 후 재생성.)
UPDATE ledger SET found = NULL WHERE kind = 'mcp' AND found = 0 AND source = 'hook' AND mcp_server_id IS NULL;
DROP VIEW v_misses;
CREATE VIEW v_misses AS
SELECT kind, name, mcp_server, COUNT(*) AS miss_count, MIN(ts) AS first_ts, MAX(ts) AS last_ts
FROM ledger WHERE found = 0 GROUP BY kind, mcp_server, name;
