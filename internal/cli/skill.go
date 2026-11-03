package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/as950118/forge/internal/profile"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/as950118/forge/internal/skill"
	"github.com/as950118/forge/internal/textsafe"
)

func skillCmd() *cobra.Command {
	var project bool
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage skills (add|update|check|list|show|remove|enable|disable|validate|misses)",
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
		skillAddCmd(), skillUpdateCmd(), skillCheckCmd(), skillListCmd(), skillShowCmd(), skillRemoveCmd(),
		skillToggleCmd("enable", true), skillToggleCmd("disable", false),
		skillValidateCmd(), skillMissesCmd(),
	)
	return cmd
}

// writeJSON prints v as indented JSON. The structure and every value are kept; only
// characters a terminal would act on (ESC, bidi overrides such as U+202E, zero-width
// and other format characters; textsafe.NeedsEscape) are written as \uXXXX escapes,
// which JSON parsers read back as the same characters. encoding/json already escapes
// C0 controls and U+2028/2029 but passes U+202E and the C1 controls through, so
// `forge ... --json` piped to a terminal could otherwise reorder or hide text
// (TASK-0020 AC-8).
func writeJSON(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := w.Write(escapeJSONControls(buf.Bytes()))
	return err
}

// escapeJSONControls rewrites terminal-active runes of already valid JSON text as
// \uXXXX (a surrogate pair above U+FFFF). Such runes can only occur inside strings.
func escapeJSONControls(b []byte) []byte {
	clean := true
	for _, r := range string(b) {
		if r == utf8.RuneError || textsafe.NeedsEscape(r) && r != '\n' && r != '\t' && r != '\r' {
			clean = false
			break
		}
	}
	if clean {
		return b
	}
	var out bytes.Buffer
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			out.WriteString(`\ufffd`)
		case textsafe.NeedsEscape(r) && r != '\n' && r != '\t' && r != '\r':
			if r > 0xffff {
				r -= 0x10000
				fmt.Fprintf(&out, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			} else {
				fmt.Fprintf(&out, `\u%04x`, r)
			}
		default:
			out.Write(b[i : i+size])
		}
		i += size
	}
	return out.Bytes()
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
