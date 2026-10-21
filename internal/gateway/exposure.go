package gateway

import (
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/skill"
	"github.com/heonjinjeong/forge/internal/tool"
)

// Exposure decides what the gateway shows to a client. client is a Forge client
// id ("claude-code", "opencode") or "" for an unidentified client.
//
// TODO(profile-exposure): TASK-0012 replaces SettingsExposure by an
// implementation backed by profile_members / profile_member_exposure and the
// active profile for (client, project). Only this interface changes.
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
