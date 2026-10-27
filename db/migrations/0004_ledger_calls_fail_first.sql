-- Forge registry schema v4: 병합 호출의 ok 는 실패 우선 (TASK-0021 AC-2, ADR-F0003).
-- v_ledger_calls 가 MAX(ok) 로 병합해, 같은 호출을 hook=1 / gateway=0 으로 기록하면 실패가 가려졌다.
-- MIN(ok): 한 source 라도 실패(0)면 실패, 실패가 없고 하나라도 성공(1)이면 성공, 모두 미관찰(NULL)이면 NULL.
-- (0001~0003 은 수정하지 않는다. 뷰는 DROP 후 재생성.)
DROP VIEW v_ledger_calls;
CREATE VIEW v_ledger_calls AS
SELECT MIN(id) AS id, MIN(ts) AS ts, kind, name, mcp_server, client, project, session_id,
       MIN(ok) AS ok, MAX(latency_ms) AS latency_ms, MIN(found) AS found,
       group_concat(DISTINCT source) AS sources
FROM ledger
GROUP BY kind, name, COALESCE(mcp_server,''), COALESCE(client,''), COALESCE(session_id,''),
         COALESCE(tool_use_id, 'row:' || id);
