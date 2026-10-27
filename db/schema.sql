-- Forge registry schema v1  (ADR-F0001)
-- 대상: SQLite >= 3.35 (순수 Go 드라이버 포함). 연결마다 PRAGMA foreign_keys=ON 필요(연결 단위 설정이라 DDL 에 영구 반영 불가).
-- 이 파일은 마이그레이션 0001_init 의 스냅샷이다. 이후 변경은 번호 순 SQL 파일(ADR-F0001 §마이그레이션)로만 추가한다. 이 파일만 단독 재적용할 수 없다(재실행은 schema_migrations 가 막는다).
-- 시간은 모두 UTC epoch milliseconds(INTEGER). 비밀값 평문은 어떤 컬럼에도 저장하지 않는다(*_secrets 의 ref 컬럼 참조).
-- 개념 참고: ARTEX 관리 평면(스킬/MCP/도구/사용 기록). DDL 은 Forge 독자 설계이며 ARTEX 코드를 복사하지 않았다.

PRAGMA foreign_keys = ON;

------------------------------------------------------------------
-- 0. 메타
------------------------------------------------------------------
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY CHECK (version > 0),
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,                       -- 적용한 SQL 파일의 sha256 (변조/드리프트 감지)
    applied_at INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
);

-- 전역 설정(키-값). 예: record_payload=0|1, payload_retention_days
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
);

-- 지원 클라이언트(어댑터). 신규 클라이언트는 INSERT 로 추가(마이그레이션 불필요).
CREATE TABLE clients (
    id   TEXT PRIMARY KEY CHECK (id GLOB '[a-z]*' AND id NOT GLOB '*[^a-z0-9-]*'),
    name TEXT NOT NULL
);

-- 프로젝트(= .forge/ 를 가진 작업 디렉터리). 전역 DB 가 모든 프로젝트를 안다.
CREATE TABLE projects (
    id         INTEGER PRIMARY KEY,
    path       TEXT NOT NULL UNIQUE,                   -- 정규화된 절대 경로(realpath)
    name       TEXT NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
);

------------------------------------------------------------------
-- 1. 스킬 (+ 버전)
------------------------------------------------------------------
CREATE TABLE skills (
    id                 INTEGER PRIMARY KEY,
    scope              TEXT NOT NULL CHECK (scope IN ('global','project')),
    project_id         INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    name               TEXT NOT NULL CHECK (name GLOB '[a-z0-9]*' AND name NOT GLOB '*[^a-z0-9-]*' AND length(name) BETWEEN 1 AND 64),
    description        TEXT NOT NULL CHECK (length(description) BETWEEN 1 AND 1024),
    enabled            INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    current_version_id INTEGER REFERENCES skill_versions(id) ON DELETE SET NULL,
    created_at         INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000),
    updated_at         INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000),
    CHECK ((scope = 'global' AND project_id IS NULL) OR (scope = 'project' AND project_id IS NOT NULL))
);
CREATE UNIQUE INDEX ux_skills_scope_name ON skills(scope, COALESCE(project_id, 0), name);

CREATE TABLE skill_versions (
    id            INTEGER PRIMARY KEY,
    skill_id      INTEGER NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    version       TEXT NOT NULL CHECK (length(version) > 0),   -- semver 권장. 없으면 'h-' || 해시 앞 12자
    content_hash  TEXT NOT NULL CHECK (length(content_hash) = 64), -- 디렉터리 트리 sha256 (해시 고정)
    source_type   TEXT NOT NULL CHECK (source_type IN ('dir','zip','git','url','builtin')),
    source_uri    TEXT,                                        -- 가져온 위치. 자격증명 포함 URL 금지
    source_ref    TEXT,                                        -- git commit/tag 등
    store_path    TEXT NOT NULL,                               -- ~/.forge 또는 .forge 기준 상대 경로
    frontmatter   TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(frontmatter)),  -- name/description/license 등 원문
    created_at    INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000),
    UNIQUE (skill_id, version),
    UNIQUE (skill_id, content_hash)                            -- 같은 내용 재등록 방지
);
CREATE INDEX ix_skill_versions_skill ON skill_versions(skill_id);

------------------------------------------------------------------
-- 2. MCP 서버 (+ 비밀 참조, 발견 캐시)
------------------------------------------------------------------
CREATE TABLE mcp_servers (
    id          INTEGER PRIMARY KEY,
    scope       TEXT NOT NULL CHECK (scope IN ('global','project')),
    project_id  INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL CHECK (name GLOB '[A-Za-z0-9]*' AND name NOT GLOB '*[^A-Za-z0-9_-]*' AND length(name) <= 64),
    -- Forge 모델은 http/sse 를 구분한다. OpenCode 어댑터는 렌더링 시 remote 로 접는다.
    transport   TEXT NOT NULL CHECK (transport IN ('stdio','http','sse')),
    command     TEXT,                                          -- stdio: 실행 파일 (args 와 분리 저장, 어댑터가 클라이언트 형식으로 합침)
    args        TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(args)),
    cwd         TEXT,
    url         TEXT,                                          -- http/sse. 쿼리스트링에 비밀 금지(검증 단계에서 거부)
    timeout_ms  INTEGER CHECK (timeout_ms IS NULL OR timeout_ms > 0),
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    created_at  INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000),
    updated_at  INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000),
    CHECK ((scope = 'global' AND project_id IS NULL) OR (scope = 'project' AND project_id IS NOT NULL)),
    CHECK ((transport = 'stdio' AND command IS NOT NULL AND url IS NULL)
        OR (transport IN ('http','sse') AND url IS NOT NULL AND command IS NULL))
);
CREATE UNIQUE INDEX ux_mcp_servers_scope_name ON mcp_servers(scope, COALESCE(project_id, 0), name);

-- 비밀/환경 값 참조. 평문 비밀 저장 금지:
--   env      : ref = 환경변수 이름 (렌더링 시 클라이언트별 치환 문법 ${NAME} / {env:NAME})
--   keychain : ref = 'service/account' (OS 키체인 항목 지정자. 값 자체는 키체인에만 존재)
--   literal  : 비밀이 아닌 공개 값만 허용 (is_secret=0 일 때만). value 컬럼에 저장
CREATE TABLE mcp_server_values (
    id         INTEGER PRIMARY KEY,
    server_id  INTEGER NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    slot       TEXT NOT NULL CHECK (slot IN ('env','header')),
    key        TEXT NOT NULL CHECK (length(key) > 0),
    is_secret  INTEGER NOT NULL DEFAULT 1 CHECK (is_secret IN (0,1)),
    ref_type   TEXT NOT NULL CHECK (ref_type IN ('env','keychain','literal')),
    ref        TEXT,                                           -- env 변수명 또는 keychain 지정자
    value      TEXT,                                           -- literal 전용
    UNIQUE (server_id, slot, key),
    CHECK ((ref_type = 'literal' AND is_secret = 0 AND value IS NOT NULL AND ref IS NULL)
        OR (ref_type IN ('env','keychain') AND ref IS NOT NULL AND value IS NULL))
);

-- 발견 시도 결과(서버당 1행, 최신)
CREATE TABLE mcp_discovery (
    server_id        INTEGER PRIMARY KEY REFERENCES mcp_servers(id) ON DELETE CASCADE,
    last_attempt_at  INTEGER NOT NULL,
    ok               INTEGER NOT NULL CHECK (ok IN (0,1)),
    error            TEXT,                                     -- 요약만. 비밀·원문 응답 저장 금지
    protocol_version TEXT,
    server_name      TEXT,
    server_version   TEXT
);

-- tools/list 캐시
CREATE TABLE mcp_tools (
    server_id     INTEGER NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    description   TEXT,
    input_schema  TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(input_schema)),
    schema_hash   TEXT NOT NULL,                               -- 변경 감지
    stale         INTEGER NOT NULL DEFAULT 0 CHECK (stale IN (0,1)),  -- 마지막 발견에서 사라짐
    discovered_at INTEGER NOT NULL,
    PRIMARY KEY (server_id, name)
) WITHOUT ROWID;

------------------------------------------------------------------
-- 3. 커스텀 도구 (command / script / http)
------------------------------------------------------------------
CREATE TABLE tools (
    id            INTEGER PRIMARY KEY,
    scope         TEXT NOT NULL CHECK (scope IN ('global','project')),
    project_id    INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    name          TEXT NOT NULL CHECK (name GLOB '[A-Za-z0-9]*' AND name NOT GLOB '*[^A-Za-z0-9_-]*' AND length(name) <= 64),
    kind          TEXT NOT NULL CHECK (kind IN ('command','script','http')),
    description   TEXT NOT NULL,
    input_schema  TEXT NOT NULL DEFAULT '{"type":"object"}' CHECK (json_valid(input_schema)),
    -- command
    command       TEXT,
    args          TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(args)),
    -- script
    script_path   TEXT,                                        -- 저장소 상대 경로
    script_hash   TEXT CHECK (script_hash IS NULL OR length(script_hash) = 64),
    interpreter   TEXT,
    -- http
    http_method   TEXT CHECK (http_method IS NULL OR http_method IN ('GET','POST','PUT','PATCH','DELETE')),
    http_url      TEXT,
    -- 실행 정책 (PLAN §3.4)
    working_dir   TEXT,
    timeout_ms    INTEGER NOT NULL DEFAULT 30000 CHECK (timeout_ms > 0),
    confirm       TEXT NOT NULL DEFAULT 'ask' CHECK (confirm IN ('none','ask','deny')),
    revision      INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),  -- 수정 시 +1. 이력 보존은 v1 비목표
    enabled       INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
    created_at    INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000),
    updated_at    INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000),
    CHECK ((scope = 'global' AND project_id IS NULL) OR (scope = 'project' AND project_id IS NOT NULL)),
    CHECK ((kind = 'command' AND command IS NOT NULL AND script_path IS NULL AND http_url IS NULL)
        OR (kind = 'script'  AND script_path IS NOT NULL AND script_hash IS NOT NULL AND command IS NULL AND http_url IS NULL)
        OR (kind = 'http'    AND http_url IS NOT NULL AND http_method IS NOT NULL AND command IS NULL AND script_path IS NULL))
);
CREATE UNIQUE INDEX ux_tools_scope_name ON tools(scope, COALESCE(project_id, 0), name);

-- 도구의 env/header 값. mcp_server_values 와 동일 규칙.
CREATE TABLE tool_values (
    id         INTEGER PRIMARY KEY,
    tool_id    INTEGER NOT NULL REFERENCES tools(id) ON DELETE CASCADE,
    slot       TEXT NOT NULL CHECK (slot IN ('env','header')),
    key        TEXT NOT NULL CHECK (length(key) > 0),
    is_secret  INTEGER NOT NULL DEFAULT 1 CHECK (is_secret IN (0,1)),
    ref_type   TEXT NOT NULL CHECK (ref_type IN ('env','keychain','literal')),
    ref        TEXT,
    value      TEXT,
    UNIQUE (tool_id, slot, key),
    CHECK ((ref_type = 'literal' AND is_secret = 0 AND value IS NOT NULL AND ref IS NULL)
        OR (ref_type IN ('env','keychain') AND ref IS NOT NULL AND value IS NULL))
);

------------------------------------------------------------------
-- 4. 프로필 (+ 멤버십, 클라이언트별 노출, 활성 지정)
------------------------------------------------------------------
INSERT INTO clients(id, name) VALUES ('claude-code','Claude Code'), ('opencode','OpenCode');

CREATE TABLE profiles (
    id          INTEGER PRIMARY KEY,
    scope       TEXT NOT NULL CHECK (scope IN ('global','project')),
    project_id  INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    description TEXT,
    created_at  INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000),
    CHECK ((scope = 'global' AND project_id IS NULL) OR (scope = 'project' AND project_id IS NOT NULL))
);
CREATE UNIQUE INDEX ux_profiles_scope_name ON profiles(scope, COALESCE(project_id, 0), name);

-- 멤버: 스킬/도구/MCP 중 정확히 하나를 가리킨다. pinned_version_id 는 스킬 전용(NULL=current 추종).
CREATE TABLE profile_members (
    id                INTEGER PRIMARY KEY,
    profile_id        INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    kind              TEXT NOT NULL CHECK (kind IN ('skill','tool','mcp')),
    skill_id          INTEGER REFERENCES skills(id) ON DELETE CASCADE,
    tool_id           INTEGER REFERENCES tools(id) ON DELETE CASCADE,
    mcp_server_id     INTEGER REFERENCES mcp_servers(id) ON DELETE CASCADE,
    pinned_version_id INTEGER REFERENCES skill_versions(id) ON DELETE RESTRICT,
    CHECK ((kind = 'skill' AND skill_id IS NOT NULL AND tool_id IS NULL AND mcp_server_id IS NULL)
        OR (kind = 'tool'  AND tool_id  IS NOT NULL AND skill_id IS NULL AND mcp_server_id IS NULL)
        OR (kind = 'mcp'   AND mcp_server_id IS NOT NULL AND skill_id IS NULL AND tool_id IS NULL)),
    CHECK (pinned_version_id IS NULL OR kind = 'skill')
);
CREATE UNIQUE INDEX ux_profile_member_skill ON profile_members(profile_id, skill_id)      WHERE skill_id IS NOT NULL;
CREATE UNIQUE INDEX ux_profile_member_tool  ON profile_members(profile_id, tool_id)       WHERE tool_id IS NOT NULL;
CREATE UNIQUE INDEX ux_profile_member_mcp   ON profile_members(profile_id, mcp_server_id) WHERE mcp_server_id IS NOT NULL;

-- 클라이언트별 노출 방식. 행이 없으면 'direct' 로 간주(어댑터 기본).
--   direct  : 클라이언트 설정에 직접 렌더링
--   gateway : Forge 게이트웨이(forge mcp serve) 경유로만 노출 (호출 이력 보장 경로)
--   hidden  : 해당 클라이언트에는 노출하지 않음 (예: OpenCode 에서 숨길 MCP)
CREATE TABLE profile_member_exposure (
    member_id INTEGER NOT NULL REFERENCES profile_members(id) ON DELETE CASCADE,
    client    TEXT NOT NULL REFERENCES clients(id),
    exposure  TEXT NOT NULL CHECK (exposure IN ('direct','gateway','hidden')),
    PRIMARY KEY (member_id, client)
) WITHOUT ROWID;

-- 어떤 프로필이 (클라이언트, 프로젝트)에 적용 중인가. project_id NULL = 전역 기본.
CREATE TABLE profile_activations (
    id         INTEGER PRIMARY KEY,
    client     TEXT NOT NULL REFERENCES clients(id),
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
);
CREATE UNIQUE INDEX ux_profile_activation ON profile_activations(client, COALESCE(project_id, 0));

------------------------------------------------------------------
-- 5. 원장 (ledger): 호출 1건 = 1행. 기본은 메타데이터만.
------------------------------------------------------------------
CREATE TABLE ledger (
    id             INTEGER PRIMARY KEY,
    ts             INTEGER NOT NULL,                           -- 호출 시각 (UTC ms)
    kind           TEXT NOT NULL CHECK (kind IN ('skill','tool','mcp')),
    name           TEXT NOT NULL,                              -- skill: 스킬명 / tool: 도구명 / mcp: MCP 도구명
    mcp_server     TEXT,                                       -- kind='mcp' 일 때 서버명
    client         TEXT REFERENCES clients(id),                -- 미식별 호출은 NULL
    client_version TEXT,                                       -- 예: 2.1.305 (R1 버전 드리프트 분석용)
    project        TEXT,                                       -- 정규화된 프로젝트 경로. projects 삭제 후에도 이력 보존 위해 FK 아님
    session_id     TEXT,                                       -- 클라이언트 세션 ID
    agent_id       TEXT,                                       -- Claude Code 서브에이전트 등
    tool_use_id    TEXT,                                       -- 클라이언트의 호출 ID (hook/transcript/gateway 교차 검증·중복제거 키)
    source         TEXT NOT NULL CHECK (source IN ('hook','gateway','transcript')),
    invocation     TEXT CHECK (invocation IS NULL OR invocation IN ('model','user')),  -- 스킬: 모델 호출(Skill 도구) vs 사용자 /name
    ok             INTEGER CHECK (ok IS NULL OR ok IN (0,1)),  -- NULL = 결과 미관찰 (PreToolUse 만 본 경우 등)
    error_class    TEXT,                                       -- timeout|denied|not_found|exec|... 분류만. 메시지 원문 금지
    latency_ms     INTEGER CHECK (latency_ms IS NULL OR latency_ms >= 0),
    found          INTEGER CHECK (found IS NULL OR found IN (0,1)), -- 요청한 스킬/도구가 레지스트리에 있었나. 0 = miss
    skill_version  TEXT,                                       -- 호출 시점의 스킬 버전(알 수 있을 때)
    skill_id       INTEGER REFERENCES skills(id)      ON DELETE SET NULL,
    tool_id        INTEGER REFERENCES tools(id)       ON DELETE SET NULL,
    mcp_server_id  INTEGER REFERENCES mcp_servers(id) ON DELETE SET NULL,
    recorded_at    INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000),
    CHECK (kind = 'mcp' OR mcp_server IS NULL),
    CHECK (kind = 'skill' OR invocation IS NULL)
);
-- 같은 소스가 같은 호출을 두 번 적재하지 않도록(hook 재전송 등). 서로 다른 source 의 중복은 허용하고 조회 시 병합한다.
CREATE UNIQUE INDEX ux_ledger_dedup ON ledger(client, session_id, tool_use_id, source) WHERE tool_use_id IS NOT NULL AND client IS NOT NULL AND session_id IS NOT NULL;
CREATE INDEX ix_ledger_ts        ON ledger(ts);
CREATE INDEX ix_ledger_kind_name ON ledger(kind, name, ts);
CREATE INDEX ix_ledger_session   ON ledger(client, session_id);
CREATE INDEX ix_ledger_project   ON ledger(project, ts);
CREATE INDEX ix_ledger_miss      ON ledger(kind, name) WHERE found = 0;

-- payload 는 기본 미저장. settings.record_payload=1 일 때만 별도 테이블에 마스킹 후 저장.
CREATE TABLE ledger_payloads (
    ledger_id         INTEGER PRIMARY KEY REFERENCES ledger(id) ON DELETE CASCADE,
    input_json        TEXT CHECK (input_json  IS NULL OR json_valid(input_json)),
    output_json       TEXT CHECK (output_json IS NULL OR json_valid(output_json)),
    redaction_version INTEGER NOT NULL CHECK (redaction_version >= 1),  -- 마스킹 규칙 버전. 마스킹 없이 저장 불가
    recorded_at       INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
);

------------------------------------------------------------------
-- 6. 세션 / LLM 사용량 (R2): 클라이언트별 해상도 차이를 granularity 로 표현
------------------------------------------------------------------
CREATE TABLE sessions (
    client         TEXT NOT NULL REFERENCES clients(id),
    session_id     TEXT NOT NULL,
    project        TEXT,
    client_version TEXT,
    started_at     INTEGER,
    ended_at       INTEGER,
    title          TEXT,                                       -- 기본 미수집(NULL). 프롬프트 파생 문자열이라 opt-in 대상
    PRIMARY KEY (client, session_id)
) WITHOUT ROWID;

CREATE TABLE llm_usage (
    id            INTEGER PRIMARY KEY,
    client        TEXT NOT NULL REFERENCES clients(id),
    session_id    TEXT NOT NULL,
    granularity   TEXT NOT NULL CHECK (granularity IN ('message','session')),
        -- claude-code transcript: 'message' (assistant 응답 단위 message.usage)
        -- opencode session export/DB : 'session' (세션 누적. 메시지 단위는 미확인)
    message_id    TEXT NOT NULL DEFAULT '',                    -- granularity='message' 일 때 message.id/requestId. 중복 집계 방지 키
    ts            INTEGER,
    model         TEXT,
    input_tokens       INTEGER CHECK (input_tokens       IS NULL OR input_tokens       >= 0),
    output_tokens      INTEGER CHECK (output_tokens      IS NULL OR output_tokens      >= 0),
    reasoning_tokens   INTEGER CHECK (reasoning_tokens   IS NULL OR reasoning_tokens   >= 0),
    cache_read_tokens  INTEGER CHECK (cache_read_tokens  IS NULL OR cache_read_tokens  >= 0),
    cache_write_tokens INTEGER CHECK (cache_write_tokens IS NULL OR cache_write_tokens >= 0),
    cost_usd      REAL CHECK (cost_usd IS NULL OR cost_usd >= 0),
    source        TEXT NOT NULL CHECK (source IN ('hook','gateway','transcript')),
    FOREIGN KEY (client, session_id) REFERENCES sessions(client, session_id) ON DELETE CASCADE,
    CHECK ((granularity = 'message' AND message_id <> '') OR (granularity = 'session' AND message_id = ''))
);
-- 세션 누적(granularity='session')은 재수집 시 UPSERT 로 덮어쓴다.
CREATE UNIQUE INDEX ux_llm_usage_key ON llm_usage(client, session_id, granularity, message_id);
CREATE INDEX ix_llm_usage_ts ON llm_usage(ts);

------------------------------------------------------------------
-- 7. 조회용 뷰
------------------------------------------------------------------
-- 서로 다른 source 가 기록한 같은 호출을 병합해 호출 수를 센다 (tool_use_id 있으면 그 단위, 없으면 행 단위).
CREATE VIEW v_ledger_calls AS
SELECT MIN(id) AS id, MIN(ts) AS ts, kind, name, mcp_server, client, project, session_id,
       MAX(ok) AS ok, MAX(latency_ms) AS latency_ms, MIN(found) AS found,
       group_concat(DISTINCT source) AS sources
FROM ledger
GROUP BY kind, name, COALESCE(mcp_server,''), COALESCE(client,''), COALESCE(session_id,''),
         COALESCE(tool_use_id, 'row:' || id);

-- "있었으면 했던 스킬/도구" 목록 (miss 집계)
CREATE VIEW v_misses AS
SELECT kind, name, COUNT(*) AS miss_count, MIN(ts) AS first_ts, MAX(ts) AS last_ts
FROM ledger WHERE found = 0 GROUP BY kind, name;

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
