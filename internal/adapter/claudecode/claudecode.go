// Package claudecode renders the Forge registry into Claude Code configuration:
// skills (.claude/skills/<name>/), MCP servers (.mcp.json) and ledger hooks
// (settings.json). It only builds adapter.Plan values; all writing, backup and
// rollback is done by the adapter engine.
//
// Safety rules implemented here (docs/sync.md):
//   - Managed items are recognized only through the sidecar state file plus an
//     exact content-hash match; user-written items are never modified or deleted.
//   - Forge-specific fields are never added to .mcp.json or settings.json.
//   - The Claude Code user file in the home directory is never opened (see
//     adapter.GuardPath); user-scope MCP servers are printed as `claude mcp add`
//     commands only.
//
// Verified format versions: see SupportedVersions and testdata/.
package claudecode

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/heonjinjeong/forge/internal/adapter"
)

// Name is the --target value.
const Name = "claude-code"

// SupportedVersions lists the Claude Code versions the formats were documented
// against (docs/clients/claude-code.md). Fixtures live in testdata/<version>/.
// The adapter never runs the claude binary. The version gate of `forge sync
// --apply` and `forge doctor` (package clientver) runs "claude --version" and
// nothing else; its verified-version table must match this list.
var SupportedVersions = []string{"2.1.305"}

// GatewayServerName is the .mcp.json entry that points at `forge mcp serve`.
const GatewayServerName = "forge"

// Adapter implements adapter.Adapter for Claude Code.
type Adapter struct {
	// ForgeHome holds the user-scope state file (<ForgeHome>/sync-state/).
	ForgeHome string
}

// New returns the adapter for the given Forge home directory.
func New(forgeHome string) *Adapter { return &Adapter{ForgeHome: forgeHome} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return Name }

type paths struct {
	adapter.Paths
	settings string
	mcpJSON  string // project scope only
}

func (a *Adapter) paths(scope adapter.Scope, root string) paths {
	p := paths{Paths: adapter.Paths{Root: root, SkillsDir: filepath.Join(root, ".claude", "skills")}, settings: filepath.Join(root, ".claude", "settings.json")}
	if scope == adapter.ScopeProject {
		p.mcpJSON = filepath.Join(root, ".mcp.json")
		p.State = filepath.Join(root, ".forge", "sync-state.json")
		p.Journal = filepath.Join(root, ".forge", "sync-journal.json")
		p.StateBase = root
	} else {
		p.State = filepath.Join(a.ForgeHome, "sync-state", "claude-code-user.json")
		p.Journal = filepath.Join(a.ForgeHome, "sync-state", "claude-code-user.journal.json")
		p.StateBase = a.ForgeHome
	}
	return p
}

// StatePath returns the sidecar state file of a scope (root is the project root
// or the home directory). Other adapters (OpenCode) read it, read-only, to see
// which skills this target manages; they never write it.
func (a *Adapter) StatePath(scope adapter.Scope, root string) string {
	return a.paths(scope, root).State
}

// AllowedPath implements adapter.Adapter: only the files and skill directories
// that sync manages are writable, and never the Claude Code user file.
func (a *Adapter) AllowedPath(scope adapter.Scope, root, path string) (string, error) {
	p := a.paths(scope, root)
	return adapter.AllowedPath(Name, scope, root, path, p.Paths, map[string]string{p.settings: root, p.mcpJSON: root})
}

// ResolveRoot resolves the sync root (the CLI needs it to name the sync lock
// before planning: plan and apply run under one lock). The home directory, the
// Forge home and ~/.claude are never a project root.
func (a *Adapter) ResolveRoot(req adapter.Request) (string, []string, error) {
	if req.Scope == "" {
		req.Scope = adapter.ScopeProject
	}
	return adapter.ResolveRoot(req, a.ForgeHome, func(home string) []adapter.ForbiddenDir {
		if home == "" {
			return nil
		}
		return []adapter.ForbiddenDir{{Path: filepath.Join(home, ".claude"), Why: "프로젝트 루트가 ~/.claude 입니다. 사용자 설정은 --scope user 로 동기화하세요"}}
	})
}

// planner adds the claude-code paths to the common planner.
type planner struct {
	*adapter.Planner
	p paths
}

// Plan implements adapter.Adapter. It reads files but writes nothing.
func (a *Adapter) Plan(ctx context.Context, in adapter.Inputs, req adapter.Request) (*adapter.Plan, error) {
	if req.Scope == "" {
		req.Scope = adapter.ScopeProject
	}
	root, notes, err := a.ResolveRoot(req)
	if err != nil {
		return nil, err
	}
	if err := adapter.ValidateBin(req.ForgeBin, nil); err != nil {
		return nil, err
	}
	if req.NoHooks && req.RemoveHooks {
		return nil, errors.New("--no-hooks 와 --remove-hooks 는 함께 쓸 수 없습니다")
	}
	p := a.paths(req.Scope, root)
	pl := &planner{Planner: adapter.NewPlanner(a, in, req, root, p.Paths, notes), p: p}
	dirs := []string{filepath.Dir(p.settings), p.SkillsDir, filepath.Dir(p.State)}
	if req.Scope == adapter.ScopeProject {
		dirs = append([]string{root}, dirs...)
	}
	pl.Preflight(dirs)
	if err := pl.LoadState(); err != nil {
		return nil, err
	}
	if err := pl.PlanSkills(ctx, adapter.SkillPolicy{LinkHint: " (.claude 또는 skills 가 링크인지 확인)"}); err != nil {
		return nil, err
	}
	if req.Scope == adapter.ScopeProject {
		if err := pl.planMCPFile(); err != nil {
			return nil, err
		}
	} else {
		pl.planMCPCommands()
	}
	if err := pl.planHooks(); err != nil {
		return nil, err
	}
	pl.FinishState()
	return pl.Plan, nil
}
