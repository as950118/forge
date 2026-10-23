package adapter

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/heonjinjeong/forge/internal/history"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/profile"
	"github.com/heonjinjeong/forge/internal/skill"
)

// LoadInputs is LoadInputsFor without a project: only a profile activated for
// all projects (the client's global default) can apply.
func LoadInputs(ctx context.Context, forgeHome, client string) (Inputs, error) {
	return LoadInputsFor(ctx, forgeHome, client, "")
}

// LoadInputsFor reads the registry for one client through a read-only database
// handle: nothing under the Forge home is created or changed. Only enabled
// skills and enabled MCP servers are returned, narrowed by the profile that
// applies to (client, project) (docs/profile.md, precedence in package profile):
//
//   - no profile: every enabled skill at its current version, every enabled MCP
//     server with its "mcp.exposure.*" setting (the pre-profile behaviour);
//   - a profile: only its members (allow list). A skill member is rendered when
//     its exposure is direct, at the pinned version when one is set; gateway and
//     hidden skills are not rendered. An MCP member gets the profile's exposure
//     for the client in place of the legacy setting.
//
// project is a canonical project root ("" = no project, e.g. user scope).
func LoadInputsFor(ctx context.Context, forgeHome, client, project string) (Inputs, error) {
	conn, err := history.Open(ctx, forgeHome)
	if err != nil {
		return Inputs{Client: client}, err
	}
	defer conn.Close()
	return LoadInputsDB(ctx, conn, forgeHome, client, project)
}

// LoadInputsDB is LoadInputsFor on an already opened (read-only) handle.
func LoadInputsDB(ctx context.Context, conn *sql.DB, forgeHome, client, project string) (Inputs, error) {
	in := Inputs{Client: client}
	res, err := profile.Resolve(ctx, conn, client, project)
	if err != nil {
		return in, fmt.Errorf("프로필을 읽을 수 없습니다: %w", err)
	}
	in.Profile = res
	sreg := &skill.Registry{DB: conn, Home: forgeHome}
	items, err := sreg.List(ctx)
	if err != nil {
		return in, err
	}
	skillsRoot := filepath.Join(forgeHome, "skills")
	for _, it := range items {
		if !it.Enabled || it.CurrentVersion == "" {
			continue
		}
		want := "" // pinned version ("" = current)
		if res.Active() {
			if mode, ok := res.Skills[it.Name]; !ok || mode != profile.ExposureDirect {
				continue
			}
			want = res.Pins[it.Name]
		}
		full, err := sreg.Get(ctx, it.Name)
		if err != nil {
			return in, err
		}
		for _, v := range full.Versions {
			if (want == "" && !v.Current) || (want != "" && v.Version != want) {
				continue
			}
			dir := filepath.Join(forgeHome, filepath.FromSlash(v.StorePath))
			rel, err := filepath.Rel(skillsRoot, dir)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return in, fmt.Errorf("스킬 %q 의 저장 경로가 저장소 밖입니다", it.Name)
			}
			in.Skills = append(in.Skills, SkillInput{Name: it.Name, Version: v.Version, Description: it.Description, Hash: v.Hash, StoreDir: dir})
		}
	}
	mreg := &mcp.Registry{DB: conn}
	servers, err := mreg.List(ctx)
	if err != nil {
		return in, err
	}
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		if res.Active() {
			if _, ok := res.MCP[s.Name]; !ok {
				continue
			}
		}
		full, err := mreg.Get(ctx, s.Name)
		if err != nil {
			return in, err
		}
		if res.Active() {
			full.Exposure[client] = res.MCP[s.Name]
		}
		in.Servers = append(in.Servers, full)
	}
	return in, nil
}
