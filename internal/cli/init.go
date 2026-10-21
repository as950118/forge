package cli

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/heonjinjeong/forge/internal/skill"
	"github.com/heonjinjeong/forge/internal/store"
)

// openRegistry resolves FORGE_HOME (default ~/.forge), ensures the directory
// layout, opens the DB and applies migrations.
func openRegistry(ctx context.Context) (*skill.Registry, *sql.DB, error) {
	home, err := store.Home()
	if err != nil {
		return nil, nil, err
	}
	if err := store.EnsureLayout(home); err != nil {
		return nil, nil, err
	}
	conn, err := store.Open(ctx, store.DBPath(home))
	if err != nil {
		return nil, nil, err
	}
	return &skill.Registry{DB: conn, Home: home}, conn, nil
}

func initCmd() *cobra.Command {
	var project bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize the Forge registry (~/.forge or $FORGE_HOME)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if project {
				// TODO(project-scope): .forge/forge.yaml manifest (ADR-F0001 O-2) is not defined yet.
				return fmt.Errorf("--project 는 아직 지원하지 않습니다 (TODO: 프로젝트 범위, ADR-F0001 열린 이슈 O-2)")
			}
			_, conn, err := openRegistry(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			home, _ := store.Home()
			fmt.Fprintf(cmd.OutOrStdout(), "Forge 레지스트리를 초기화했습니다: %s\n", filepath.Join(home, "forge.db"))
			return nil
		},
	}
	cmd.Flags().BoolVar(&project, "project", false, "프로젝트 범위 초기화 (미지원)")
	return cmd
}
