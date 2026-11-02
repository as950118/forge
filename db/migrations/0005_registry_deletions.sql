-- Forge registry schema v5: 레지스트리 삭제 감사 기록 (TASK-0032 AC-1).
-- 스킬·도구·MCP 서버를 지우면 profile_members 가 ON DELETE CASCADE 로 조용히 사라진다. 삭제 직전(BEFORE DELETE)
-- 트리거가 이름·종류·범위·시각과 "사라질 프로필 멤버" 목록을 기록해 forge doctor 가 알릴 수 있게 한다.
-- 트리거라서 CLI 외의 삭제 경로(프로젝트 삭제의 CASCADE 포함)도 같은 규칙으로 기록된다.
-- 비밀값은 저장하지 않는다: 이름(레지스트리 CHECK 로 문자 제한)·범위·프로필 이름·프로젝트 경로뿐이다.
-- members: JSON 배열, 항목은 "<프로필 scope>:<프로필 이름>" (정렬됨). 프로필 멤버가 없으면 '[]'.
-- 프로젝트를 지우면 그 프로젝트의 스킬·도구·MCP 서버와 프로젝트 프로필이 CASCADE 로 지워지는데, 자식 트리거가 돌 때는
-- 이미 프로젝트 행과 프로젝트 프로필 멤버가 사라져 경로와 멤버를 알 수 없다. 그래서 projects 의 BEFORE DELETE 트리거가
-- (아무것도 지워지기 전에) 그 프로젝트의 항목을 경로·멤버와 함께 기록하고, 자식 트리거는 프로젝트가 이미 없으면 기록하지 않는다(중복 방지).
-- (0001~0004 는 수정하지 않는다.)
CREATE TABLE registry_deletions (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('skill','tool','mcp')),
    name       TEXT NOT NULL,
    scope      TEXT NOT NULL CHECK (scope IN ('global','project')),
    project    TEXT,                                           -- scope='project' 일 때 프로젝트 경로(알 수 있을 때). FK 아님
    members    TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(members)),
    deleted_at INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
);
CREATE INDEX ix_registry_deletions_at ON registry_deletions(deleted_at);

CREATE TRIGGER trg_skills_audit_delete BEFORE DELETE ON skills
WHEN OLD.project_id IS NULL OR EXISTS (SELECT 1 FROM projects WHERE id = OLD.project_id)
BEGIN
    INSERT INTO registry_deletions(kind, name, scope, project, members)
    VALUES ('skill', OLD.name, OLD.scope, (SELECT path FROM projects WHERE id = OLD.project_id),
        (SELECT json_group_array(x) FROM (SELECT p.scope || ':' || p.name AS x FROM profile_members m JOIN profiles p ON p.id = m.profile_id WHERE m.skill_id = OLD.id ORDER BY 1)));
END;

CREATE TRIGGER trg_tools_audit_delete BEFORE DELETE ON tools
WHEN OLD.project_id IS NULL OR EXISTS (SELECT 1 FROM projects WHERE id = OLD.project_id)
BEGIN
    INSERT INTO registry_deletions(kind, name, scope, project, members)
    VALUES ('tool', OLD.name, OLD.scope, (SELECT path FROM projects WHERE id = OLD.project_id),
        (SELECT json_group_array(x) FROM (SELECT p.scope || ':' || p.name AS x FROM profile_members m JOIN profiles p ON p.id = m.profile_id WHERE m.tool_id = OLD.id ORDER BY 1)));
END;

CREATE TRIGGER trg_mcp_servers_audit_delete BEFORE DELETE ON mcp_servers
WHEN OLD.project_id IS NULL OR EXISTS (SELECT 1 FROM projects WHERE id = OLD.project_id)
BEGIN
    INSERT INTO registry_deletions(kind, name, scope, project, members)
    VALUES ('mcp', OLD.name, OLD.scope, (SELECT path FROM projects WHERE id = OLD.project_id),
        (SELECT json_group_array(x) FROM (SELECT p.scope || ':' || p.name AS x FROM profile_members m JOIN profiles p ON p.id = m.profile_id WHERE m.mcp_server_id = OLD.id ORDER BY 1)));
END;

CREATE TRIGGER trg_projects_audit_delete_skill BEFORE DELETE ON projects
BEGIN
    INSERT INTO registry_deletions(kind, name, scope, project, members)
    SELECT 'skill', t.name, t.scope, OLD.path,
        (SELECT json_group_array(x) FROM (SELECT p.scope || ':' || p.name AS x FROM profile_members m JOIN profiles p ON p.id = m.profile_id WHERE m.skill_id = t.id ORDER BY 1))
    FROM skills t WHERE t.project_id = OLD.id;
END;

CREATE TRIGGER trg_projects_audit_delete_tool BEFORE DELETE ON projects
BEGIN
    INSERT INTO registry_deletions(kind, name, scope, project, members)
    SELECT 'tool', t.name, t.scope, OLD.path,
        (SELECT json_group_array(x) FROM (SELECT p.scope || ':' || p.name AS x FROM profile_members m JOIN profiles p ON p.id = m.profile_id WHERE m.tool_id = t.id ORDER BY 1))
    FROM tools t WHERE t.project_id = OLD.id;
END;

CREATE TRIGGER trg_projects_audit_delete_mcp BEFORE DELETE ON projects
BEGIN
    INSERT INTO registry_deletions(kind, name, scope, project, members)
    SELECT 'mcp', t.name, t.scope, OLD.path,
        (SELECT json_group_array(x) FROM (SELECT p.scope || ':' || p.name AS x FROM profile_members m JOIN profiles p ON p.id = m.profile_id WHERE m.mcp_server_id = t.id ORDER BY 1))
    FROM mcp_servers t WHERE t.project_id = OLD.id;
END;
