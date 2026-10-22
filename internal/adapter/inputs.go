package adapter

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/heonjinjeong/forge/internal/history"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/skill"
)

// LoadInputs reads the registry for one client through a read-only database
// handle: nothing under the Forge home is created or changed. Only enabled
// skills (at their current version) and enabled MCP servers are returned.
func LoadInputs(ctx context.Context, forgeHome, client string) (Inputs, error) {
	in := Inputs{Client: client}
	conn, err := history.Open(ctx, forgeHome)
	if err != nil {
		return in, err
	}
	defer conn.Close()
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
		full, err := sreg.Get(ctx, it.Name)
		if err != nil {
			return in, err
		}
		for _, v := range full.Versions {
			if !v.Current {
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
		full, err := mreg.Get(ctx, s.Name)
		if err != nil {
			return in, err
		}
		in.Servers = append(in.Servers, full)
	}
	return in, nil
}
