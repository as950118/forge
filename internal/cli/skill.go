package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/heonjinjeong/forge/internal/profile"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/heonjinjeong/forge/internal/skill"
)

func skillCmd() *cobra.Command {
	var project bool
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage skills (add|list|show|remove|enable|disable|validate|misses)",
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if project {
				// TODO(project-scope): see skill.ScopeGlobal.
				return fmt.Errorf("--project 는 아직 지원하지 않습니다 (TODO: 프로젝트 범위 스킬). 현재는 전역(global) 범위만 가능합니다")
			}
			return nil
		},
	}
	cmd.PersistentFlags().BoolVar(&project, "project", false, "프로젝트 범위 (미지원)")
	cmd.AddCommand(
		skillAddCmd(), skillListCmd(), skillShowCmd(), skillRemoveCmd(),
		skillToggleCmd("enable", true), skillToggleCmd("disable", false),
		skillValidateCmd(), skillMissesCmd(),
	)
	return cmd
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func skillAddCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "add <dir|file.zip|https-or-ssh-git-url>",
		Short: "Add a skill from a directory, zip file or git URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, conn, err := openRegistry(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			p, err := skill.Prepare(cmd.Context(), args[0], skill.PrepareOptions{TmpDir: homeTmp(reg.Home)})
			if err != nil {
				return err
			}
			defer p.Cleanup()
			res, err := reg.Add(cmd.Context(), p)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{"result": res, "warnings": p.Warnings})
			}
			for _, w := range p.Warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "경고: %s\n", w)
			}
			switch res.Status {
			case "unchanged":
				fmt.Fprintf(out, "변경 없음: %s@%s 는 이미 등록되어 있습니다 (내용 해시 %.12s)\n", res.Name, res.Version, res.Hash)
			case "new_version":
				fmt.Fprintf(out, "새 버전 추가: %s@%s (해시 %.12s)\n", res.Name, res.Version, res.Hash)
				fmt.Fprintf(out, "새 버전이 현재 버전이 되었습니다(이전: %s)\n", res.PreviousVersion)
			default:
				fmt.Fprintf(out, "추가됨: %s@%s (해시 %.12s)\n", res.Name, res.Version, res.Hash)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func homeTmp(home string) string { return filepath.Join(home, "tmp") }

func skillListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered skills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, conn, err := openRegistry(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			items, err := reg.List(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, items)
			}
			if len(items) == 0 {
				fmt.Fprintln(out, "등록된 스킬이 없습니다.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tVERSION\tENABLED\tSCOPE\tDESCRIPTION")
			for _, i := range items {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", i.Name, i.CurrentVersion, yesNo(i.Enabled), i.Scope, truncate(i.Description, 60))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func skillShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a skill and its versions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, conn, err := openRegistry(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			i, err := reg.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, i)
			}
			fmt.Fprintf(out, "이름:     %s\n범위:     %s\n활성:     %s\n설명:     %s\n현재 버전: %s\n수정:     %s\n\n",
				i.Name, i.Scope, yesNo(i.Enabled), i.Description, i.CurrentVersion,
				time.UnixMilli(i.UpdatedAt).Local().Format("2006-01-02 15:04:05"))
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tCURRENT\tHASH\tSOURCE\tURI\tPATH")
			for _, v := range i.Versions {
				cur := ""
				if v.Current {
					cur = "*"
				}
				fmt.Fprintf(tw, "%s\t%s\t%.12s\t%s\t%s\t%s\n", v.Version, cur, v.Hash, v.SourceType, v.SourceURI, v.StorePath)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func skillRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a skill and all its versions (requires --yes or confirmation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, conn, err := openRegistry(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			info, err := reg.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			warnProfileMembers(cmd, conn, profile.KindSkill, args[0])
			if !yes {
				fmt.Fprintf(cmd.ErrOrStderr(), "스킬 %q (버전 %d개) 와 저장된 파일을 모두 삭제합니다. 계속할까요? [y/N] ", info.Name, info.VersionCount)
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return errors.New("취소되었습니다 (삭제하려면 --yes 를 지정하세요)")
				}
			}
			if err := reg.Remove(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "삭제됨: %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "확인 없이 삭제")
	return cmd
}

func skillToggleCmd(use string, enable bool) *cobra.Command {
	word := map[bool]string{true: "활성화", false: "비활성화"}[enable]
	return &cobra.Command{
		Use:   use + " <name>",
		Short: use + " a skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, conn, err := openRegistry(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			if err := reg.SetEnabled(cmd.Context(), args[0], enable); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", word, args[0])
			return nil
		},
	}
}

func skillValidateCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "validate <dir|file.zip|git-url>",
		Short: "Validate SKILL.md and the source without registering",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// validate does not need (or create) FORGE_HOME.
			p, err := skill.Prepare(cmd.Context(), args[0], skill.PrepareOptions{})
			if err != nil {
				if asJSON {
					_ = writeJSON(cmd.OutOrStdout(), map[string]any{"valid": false, "error": err.Error()})
				}
				return fmt.Errorf("검증 실패: %w", err)
			}
			defer p.Cleanup()
			hash, err := skill.HashTree(p.Root)
			if err != nil {
				return err
			}
			ver := p.Meta.Version
			if ver == "" {
				ver = "h-" + hash[:12]
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{"valid": true, "name": p.Meta.Name, "version": ver,
					"content_hash": hash, "warnings": p.Warnings})
			}
			fmt.Fprintf(out, "OK: %s@%s (해시 %.12s)\n", p.Meta.Name, ver, hash)
			for _, w := range p.Warnings {
				fmt.Fprintf(out, "경고: %s\n", w)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
