package gateway

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/profile"
	"github.com/heonjinjeong/forge/internal/skill"
	"github.com/heonjinjeong/forge/internal/tool"
)

// Exposure decides what the gateway shows to a client. client is a Forge client
// id ("claude-code", "opencode") or "" for an unidentified client.
//
// Implementations: ProfileExposure (what `forge mcp serve` uses: the active
// profile for (client, project), else SettingsExposure) and SettingsExposure
// (the pre-profile behaviour).
type Exposure interface {
	// ServerMode returns mcp.ExposureGateway|Direct|Hidden for a registered server.
	// Only servers returning ExposureGateway are proxied.
	ServerMode(info *mcp.Info, client string) string
	// ToolVisible reports whether a (enabled) custom tool is shown to the client.
	ToolVisible(def *tool.Definition, client string) bool
	// SkillVisible reports whether forge_skill may serve an (enabled) skill.
	SkillVisible(info *skill.Info, client string) bool
}

// SettingsExposure is the pre-profile implementation: servers use the
// per-client "mcp.exposure.<server>.<client>" settings (Info.Exposure, default
// direct) plus the enabled flag; custom tools and skills are exposed whenever
// they are enabled (no per-client storage exists for them yet).
type SettingsExposure struct{}

// ServerMode for an unidentified client ("") is conservative: gateway only when
// the server is set to gateway for every client; hidden if any client hides it;
// otherwise direct (= not proxied).
func (SettingsExposure) ServerMode(info *mcp.Info, client string) string {
	if client != "" {
		if m, ok := info.Exposure[client]; ok {
			return m
		}
		return mcp.ExposureDirect
	}
	if len(info.Exposure) == 0 {
		return mcp.ExposureDirect
	}
	all := true
	for _, m := range info.Exposure {
		if m == mcp.ExposureHidden {
			return mcp.ExposureHidden
		}
		if m != mcp.ExposureGateway {
			all = false
		}
	}
	if all {
		return mcp.ExposureGateway
	}
	return mcp.ExposureDirect
}

func (SettingsExposure) ToolVisible(def *tool.Definition, _ string) bool { return def.Enabled }

func (SettingsExposure) SkillVisible(info *skill.Info, _ string) bool { return info.Enabled }

// PinnedSkills is an optional Exposure extension: the immutable skill version a
// profile pins for the client ("" = serve the current version).
type PinnedSkills interface {
	PinnedVersion(info *skill.Info, client string) string
}

// Snapshotter is an optional Exposure extension: the gateway asks for one frozen
// view per tools/list or tools/call request, so every decision inside the request
// comes from one consistent read (updates between requests still apply at once).
type Snapshotter interface {
	Snapshot(ctx context.Context, client string) Exposure
}

type snapKey struct{}

// exposure returns the request's frozen view (or the live Exposure).
func (g *Gateway) exposure(ctx context.Context) Exposure {
	if e, ok := ctx.Value(snapKey{}).(Exposure); ok {
		return e
	}
	return g.o.Exposure
}

// withSnapshot freezes the exposure for the rest of the request.
func (g *Gateway) withSnapshot(ctx context.Context, client string) context.Context {
	if s, ok := g.o.Exposure.(Snapshotter); ok {
		return context.WithValue(ctx, snapKey{}, s.Snapshot(ctx, client))
	}
	return ctx
}

// profileTimeout bounds one profile lookup. A lookup that fails (database busy,
// unreadable) hides the item: the allow list fails closed.
const profileTimeout = 3 * time.Second

// ProfileExposure applies the precedence rule of package profile:
//
//	profile activated for (client, Project) -> profile activated for the client
//	for all projects -> Fallback (SettingsExposure: enabled + mcp.exposure.*).
//
// With a profile the items are an ALLOW LIST (docs/profile.md): a server, tool
// or skill that is not a member is not exposed; a hidden member is not exposed;
// a skill member with exposure gateway is served by forge_skill only (it is not
// rendered into the client's skills directory by sync). The profile is read on
// every call, so `forge profile use|add|remove` take effect on the next
// tools/list or tools/call of a running gateway without a restart.
//
// An unidentified client ("") gets the conservative intersection: an item must
// be visible under the legacy rule and under the profile of every known client
// that has one.
type ProfileExposure struct {
	DB profile.Querier
	// Project is the canonical project root of the gateway (profile.ProjectRoot of its cwd).
	Project string
	// Fallback is used when no profile applies (default SettingsExposure{}).
	Fallback Exposure
}

func (p ProfileExposure) fallback() Exposure {
	if p.Fallback != nil {
		return p.Fallback
	}
	return SettingsExposure{}
}

// view is the shared implementation of ProfileExposure (live: every question
// reads the database) and its per-request snapshot (frozen: answers from one
// consistent read, see Snapshot).
type view struct {
	fb Exposure
	// resolve returns the profile rule of a client; clients lists the clients an
	// unidentified connection must satisfy. An error hides the item (fail closed).
	resolve func(client string) (*profile.Resolved, error)
	clients func() ([]string, error)
}

func (p ProfileExposure) live() view {
	return view{
		fb: p.fallback(),
		resolve: func(client string) (*profile.Resolved, error) {
			ctx, cancel := context.WithTimeout(context.Background(), profileTimeout)
			defer cancel()
			return profile.Resolve(ctx, p.DB, client, p.Project)
		},
		clients: func() ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), profileTimeout)
			defer cancel()
			return profile.Clients(ctx, p.DB)
		},
	}
}

// Snapshot implements Snapshotter: the profile rules of every client this
// request can ask about are read in ONE read transaction, so a `forge profile
// use` running at the same moment can never produce a list that mixes two
// states. The transaction ends before Snapshot returns (the gateway's registry
// reads share the connection); the frozen answers live as long as the request.
func (p ProfileExposure) Snapshot(ctx context.Context, client string) Exposure {
	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()
	var q profile.Querier = p.DB
	if b, ok := p.DB.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	}); ok {
		// ReadOnly makes a deferred BEGIN (no write lock).
		if tx, err := b.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err == nil {
			defer func() { _ = tx.Rollback() }()
			q = tx
		}
	}
	frozen := map[string]*profile.Resolved{}
	var ferr error
	ids := []string{client}
	if client == "" {
		all, err := profile.Clients(ctx, q)
		if err != nil {
			ferr = err
		}
		ids = all
	}
	known := ids
	for _, c := range ids {
		r, err := profile.Resolve(ctx, q, c, p.Project)
		if err != nil {
			ferr = err
			break
		}
		frozen[c] = r
	}
	return view{
		fb: p.fallback(),
		resolve: func(c string) (*profile.Resolved, error) {
			if ferr != nil {
				return nil, ferr
			}
			if r, ok := frozen[c]; ok {
				return r, nil
			}
			return nil, errors.New("스냅샷에 없는 클라이언트: " + c)
		},
		clients: func() ([]string, error) { return known, ferr },
	}
}

// active returns the profiles of every client the item must satisfy: the one
// client, or all known clients that have a profile when client is "". err means
// the lookup failed (callers hide the item).
func (v view) active(client string) (map[string]*profile.Resolved, error) {
	out := map[string]*profile.Resolved{}
	clients := []string{client}
	if client == "" {
		ids, err := v.clients()
		if err != nil {
			return nil, err
		}
		clients = ids
	}
	for _, c := range clients {
		r, err := v.resolve(c)
		if err != nil {
			return nil, err
		}
		if r.Active() {
			out[c] = r
		}
	}
	return out, nil
}

// ServerMode implements Exposure.
func (p ProfileExposure) ServerMode(info *mcp.Info, client string) string {
	return p.live().ServerMode(info, client)
}

// ToolVisible implements Exposure.
func (p ProfileExposure) ToolVisible(def *tool.Definition, client string) bool {
	return p.live().ToolVisible(def, client)
}

// SkillVisible implements Exposure.
func (p ProfileExposure) SkillVisible(info *skill.Info, client string) bool {
	return p.live().SkillVisible(info, client)
}

// PinnedVersion implements PinnedSkills (identified clients only).
func (p ProfileExposure) PinnedVersion(info *skill.Info, client string) string {
	return p.live().PinnedVersion(info, client)
}

func (v view) ServerMode(info *mcp.Info, client string) string {
	act, err := v.active(client)
	if err != nil {
		return mcp.ExposureHidden
	}
	mode := v.fb.ServerMode(info, client)
	if len(act) == 0 {
		return mode
	}
	if client != "" { // a profile replaces the legacy setting for this client
		m, ok := act[client].MCP[info.Name]
		if !ok {
			return mcp.ExposureHidden
		}
		return m
	}
	for _, r := range act {
		m, ok := r.MCP[info.Name]
		switch {
		case !ok || m == mcp.ExposureHidden:
			return mcp.ExposureHidden
		case m != mcp.ExposureGateway && mode == mcp.ExposureGateway:
			mode = mcp.ExposureDirect
		}
	}
	return mode
}

func (v view) ToolVisible(def *tool.Definition, client string) bool {
	act, err := v.active(client)
	if err != nil {
		return false
	}
	if !v.fb.ToolVisible(def, client) {
		return false
	}
	for _, r := range act {
		if m, ok := r.Tools[def.Name]; !ok || m == profile.ExposureHidden {
			return false
		}
	}
	return true
}

func (v view) SkillVisible(info *skill.Info, client string) bool {
	act, err := v.active(client)
	if err != nil {
		return false
	}
	if !v.fb.SkillVisible(info, client) {
		return false
	}
	for _, r := range act {
		if m, ok := r.Skills[info.Name]; !ok || m == profile.ExposureHidden {
			return false
		}
	}
	return true
}

func (v view) PinnedVersion(info *skill.Info, client string) string {
	if client == "" {
		return ""
	}
	r, err := v.resolve(client)
	if err != nil || !r.Active() {
		return ""
	}
	return r.Pins[info.Name]
}
