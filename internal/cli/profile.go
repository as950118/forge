package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/heonjinjeong/forge/internal/history"
	"github.com/heonjinjeong/forge/internal/mcp"
	"github.com/heonjinjeong/forge/internal/profile"
	"github.com/heonjinjeong/forge/internal/store"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// openProfileStore opens the read-write registry (creating/migrating like the
// other write commands).
func openProfileStore(cmd *cobra.Command) (*profile.Store, func(), error) {
	_, conn, err := openRegistry(cmd.Context())
	if err != nil {
		return nil, nil, err
	}
	return &profile.Store{DB: conn}, func() { _ = conn.Close() }, nil
}

type profileScopeFlags struct {
	projectDir string
	global     bool
}

func (f *profileScopeFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.projectDir, "project-dir", "", "프로젝트 범위 프로필: 이 디렉터리가 속한 저장소 루트의 프로필 (기본: 전역 프로필)")
	cmd.Flags().BoolVar(&f.global, "global", false, "전역 프로필 (기본값; --project-dir 와 함께 쓸 수 없음)")
}

// ref builds the profile reference of create/show/delete/add/remove: --project-dir
// selects the project scope, otherwise the profile is global. Mutations never fall
// back from one scope to the other.
//
// Only create needs the directory to exist (it may write a projects row).
// The others only LOOK UP rows, so a project that was deleted or moved still
// resolves by its normalized path string (profile.LoosePath): its profiles can
// still be shown, edited and deleted. They never create a row.
func (f *profileScopeFlags) ref(cmd *cobra.Command, name string) (profile.Ref, error) {
	return f.refFor(cmd, name, false)
}

func (f *profileScopeFlags) refFor(cmd *cobra.Command, name string, create bool) (profile.Ref, error) {
	if f.global && cmd.Flags().Changed("project-dir") {
		return profile.Ref{}, errors.New("--global 과 --project-dir 은 함께 쓸 수 없습니다")
	}
	if cmd.Flags().Changed("project-dir") {
		var root string
		var err error
		if create {
			root, err = profile.ProjectRoot(f.projectDir)
		} else {
			root, err = profile.LoosePath(f.projectDir)
		}
		if err != nil {
			return profile.Ref{}, err
		}
		return profile.Ref{Name: name, Scope: profile.ScopeProject, Project: root}, nil
	}
	return profile.Ref{Name: name, Scope: profile.ScopeGlobal}, nil
}

func profileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Manage profiles: named allow-lists of skills, tools and MCP servers per client/project",
		Long: `프로필은 "어떤 스킬·도구·MCP 서버를 어떤 방식으로 노출할지"의 묶음입니다.

  forge profile create work --from-current          # 지금 노출 중인 것을 스냅샷
  forge profile add work --mcp github --exposure gateway --client claude-code
  forge profile add work --skill review --pin 1.0.0 # 스킬을 불변 버전에 고정
  forge profile use work --client all               # 활성화 기록만(파일은 쓰지 않음)
  forge sync --target all [--apply]                 # 두 클라이언트 설정에 반영

우선순위(하나의 규칙): (클라이언트, 이 프로젝트)에 활성 프로필 -> 그 클라이언트의 전역 활성 프로필
-> 프로필 없음(기존 동작: 활성화된 항목 전체 + mcp.exposure.* 설정).
프로필이 적용되면 허용 목록입니다: 프로필에 없는 항목은 활성화되어 있어도 노출되지 않습니다.
자세한 내용은 docs/profile.md.`,
	}
	cmd.AddCommand(profileCreateCmd(), profileListCmd(), profileShowCmd(), profileDeleteCmd(),
		profileAddCmd(), profileRemoveCmd(), profileUseCmd(), profileUnuseCmd(), profileCurrentCmd(), profilePruneStaleCmd())
	return cmd
}

func profileCreateCmd() *cobra.Command {
	var sc profileScopeFlags
	var fromCurrent, asJSON bool
	var desc string
	cmd := &cobra.Command{
		Use:   "create <name> [--project-dir <dir> | --global] [--from-current]",
		Short: "Create a profile (empty, or a snapshot of what is exposed now)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := sc.refFor(cmd, args[0], true)
			if err != nil {
				return err
			}
			ps, closeFn, err := openProfileStore(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			d, err := ps.Create(cmd.Context(), ref, desc, fromCurrent)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{"schema": 1, "profile": d})
			}
			fmt.Fprintf(out, "프로필 생성: %s (%s, 멤버 %d)\n", textsafe.Display(d.Name, 70), scopeLabel(d.Meta), d.Members)
			if fromCurrent {
				fmt.Fprintln(out, "현재 활성화된 스킬·도구·MCP 서버와 mcp.exposure.* 설정을 복사했습니다 (기존 설정은 그대로 둡니다).")
			}
			fmt.Fprintf(out, "다음: forge profile use %s --client all%s  (그다음 forge sync --target all)\n", d.Name, projectFlagHint(d.Meta))
			return nil
		},
	}
	sc.add(cmd)
	cmd.Flags().BoolVar(&fromCurrent, "from-current", false, "현재 활성화된 항목과 기존 mcp.exposure.* 설정을 프로필로 스냅샷 (레거시 이관)")
	cmd.Flags().StringVar(&desc, "description", "", "설명 (500자 이하)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func scopeLabel(m profile.Meta) string {
	if m.Scope == profile.ScopeProject {
		return "프로젝트 " + textsafe.Display(m.Project, 100)
	}
	return "전역"
}

func projectFlagHint(m profile.Meta) string {
	if m.Scope == profile.ScopeProject {
		return " --project-dir " + shellQuote(m.Project)
	}
	return ""
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+=:@%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(textsafe.Display(s, 400), "'", `'\''`) + "'"
}

func openReadOnlyRegistry(cmd *cobra.Command) (*profileReader, error) {
	home, err := store.Home()
	if err != nil {
		return nil, err
	}
	conn, err := history.Open(cmd.Context(), home)
	if err != nil {
		return nil, err
	}
	return &profileReader{conn}, nil
}

type profileReader struct{ profile.Querier }

func (r *profileReader) close() {
	if c, ok := r.Querier.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

func profileListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List profiles and where they are active",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rd, err := openReadOnlyRegistry(cmd)
			if err != nil {
				return err
			}
			defer rd.close()
			metas, err := profile.ListMeta(cmd.Context(), rd)
			if err != nil {
				return err
			}
			acts, err := profile.Activations(cmd.Context(), rd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if metas == nil {
					metas = []profile.Meta{}
				}
				if acts == nil {
					acts = []profile.Activation{}
				}
				return writeJSON(out, map[string]any{"schema": 1, "profiles": metas, "activations": acts})
			}
			if len(metas) == 0 {
				fmt.Fprintln(out, "프로필이 없습니다 (프로필 없음 = 기존 동작). 만들기: forge profile create <name> [--from-current]")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "이름\t범위\t멤버\t활성")
			for _, m := range metas {
				var on []string
				for _, a := range acts {
					if a.Profile == m.Name && a.Scope == m.Scope {
						where := "전역"
						if a.Project != "" {
							where = textsafe.Display(a.Project, 60)
						}
						on = append(on, a.Client+"@"+where)
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", textsafe.Display(m.Name, 70), scopeLabel(m), m.Members, strings.Join(on, ", "))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func profileShowCmd() *cobra.Command {
	var sc profileScopeFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <name> [--project-dir <dir> | --global]",
		Short: "Show a profile with its members and per-client exposure",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := sc.ref(cmd, args[0])
			if err != nil {
				return err
			}
			rd, err := openReadOnlyRegistry(cmd)
			if err != nil {
				return err
			}
			defer rd.close()
			d, err := profile.Get(cmd.Context(), rd, ref)
			if err != nil {
				return err
			}
			clients, err := profile.Clients(cmd.Context(), rd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{"schema": 1, "profile": d, "clients": clients})
			}
			fmt.Fprintf(out, "이름:  %s\n범위:  %s\n", textsafe.Display(d.Name, 70), scopeLabel(d.Meta))
			if d.Description != "" {
				fmt.Fprintf(out, "설명:  %s\n", textsafe.Display(d.Description, 300))
			}
			fmt.Fprintf(out, "멤버:  %d (허용 목록: 목록에 없는 항목은 이 프로필이 적용되는 클라이언트에 노출되지 않습니다)\n", d.Members)
			if len(d.MemberList) == 0 {
				return nil
			}
			profile.SortMembers(d.MemberList)
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "\n종류\t이름\t활성\t고정 버전\t%s\n", strings.Join(clients, "\t"))
			for _, m := range d.MemberList {
				pin := "-"
				if m.PinnedVersion != "" {
					pin = textsafe.Display(m.PinnedVersion, 40)
				}
				row := []string{m.Kind, textsafe.Display(m.Name, 70), yesNo(m.Enabled), pin}
				for _, c := range clients {
					row = append(row, m.ExposureFor(c))
				}
				if m.Broken != "" {
					row[1] += fmt.Sprintf(" [손상: %s; 멤버 번호 %d]", textsafe.Display(m.Broken, 60), m.ID)
				}
				fmt.Fprintln(tw, strings.Join(row, "\t"))
			}
			return tw.Flush()
		},
	}
	sc.add(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func profileDeleteCmd() *cobra.Command {
	var sc profileScopeFlags
	cmd := &cobra.Command{
		Use:   "delete <name> [--project-dir <dir> | --global]",
		Short: "Delete a profile (refused while it is active somewhere)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := sc.ref(cmd, args[0])
			if err != nil {
				return err
			}
			ps, closeFn, err := openProfileStore(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := ps.Delete(cmd.Context(), ref); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "프로필 삭제: %s\n", textsafe.Display(ref.Name, 70))
			return nil
		},
	}
	sc.add(cmd)
	return cmd
}

type memberFlags struct{ skill, tool, mcp string }

func (m *memberFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&m.skill, "skill", "", "스킬 이름")
	cmd.Flags().StringVar(&m.tool, "tool", "", "커스텀 도구 이름")
	cmd.Flags().StringVar(&m.mcp, "mcp", "", "MCP 서버 이름")
}

func (m *memberFlags) one() (kind, name string, err error) {
	n := 0
	for k, v := range map[string]string{profile.KindSkill: m.skill, profile.KindTool: m.tool, profile.KindMCP: m.mcp} {
		if v != "" {
			n++
			kind, name = k, v
		}
	}
	if n != 1 {
		return "", "", errors.New("--skill, --tool, --mcp 중 정확히 하나가 필요합니다")
	}
	return kind, name, nil
}

func profileAddCmd() *cobra.Command {
	var sc profileScopeFlags
	var mf memberFlags
	var exposure, pin string
	var clients []string
	cmd := &cobra.Command{
		Use:   "add <name> (--skill|--tool|--mcp) <item> [--exposure direct|gateway|hidden --client <id>] [--pin <skill-version>]",
		Short: "Add a skill, tool or MCP server to a profile (or change its exposure/pin)",
		Long: `프로필에 멤버를 추가하거나(이미 있으면) 노출/고정 버전을 바꿉니다.

노출(--exposure, --client 와 함께; --client all 가능):
  mcp   direct  클라이언트 설정에 직접 렌더링 (기본)   gateway  forge 게이트웨이가 프록시   hidden  노출 안 함
  skill direct  sync 가 클라이언트 스킬 디렉터리에 렌더링 + forge_skill 로 조회 (기본)
        gateway forge_skill 로만 조회(디렉터리에 쓰지 않음)   hidden  노출 안 함
  tool  gateway 게이트웨이(forge_<이름>)로 노출 (기본; 커스텀 도구는 게이트웨이로만 노출됨)   hidden  노출 안 함
--pin 은 스킬의 등록된 버전(불변)을 고정합니다. 레지스트리에 없는 버전은 거부됩니다.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, item, err := mf.one()
			if err != nil {
				return err
			}
			ref, err := sc.ref(cmd, args[0])
			if err != nil {
				return err
			}
			ps, closeFn, err := openProfileStore(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := ps.Add(cmd.Context(), ref, profile.AddSpec{Kind: kind, Name: item, Exposure: exposure, Clients: clients, Pin: pin}); err != nil {
				return err
			}
			msg := fmt.Sprintf("프로필 %s: %s %s 추가/갱신", textsafe.Display(ref.Name, 70), kind, textsafe.Display(item, 70))
			if exposure != "" {
				msg += fmt.Sprintf(" (노출=%s, 클라이언트 %s)", exposure, strings.Join(clients, ","))
			}
			if pin != "" {
				msg += fmt.Sprintf(" (고정 버전 %s)", textsafe.Display(pin, 40))
			}
			fmt.Fprintln(cmd.OutOrStdout(), msg)
			return nil
		},
	}
	sc.add(cmd)
	mf.add(cmd)
	cmd.Flags().StringVar(&exposure, "exposure", "", "direct|gateway|hidden")
	cmd.Flags().StringSliceVar(&clients, "client", nil, "노출을 적용할 클라이언트 (claude-code|opencode|all; 여러 번 가능)")
	cmd.Flags().StringVar(&pin, "pin", "", "스킬 버전 고정 (등록된 버전 문자열)")
	return cmd
}

func profileRemoveCmd() *cobra.Command {
	var sc profileScopeFlags
	var mf memberFlags
	var memberID int64
	cmd := &cobra.Command{
		Use:   "remove <name> (--skill|--tool|--mcp) <item> | --member-id <id>",
		Short: "Remove a member from a profile (--member-id: by row id, for damaged members)",
		Long: `프로필에서 멤버를 제거합니다. 이름으로(--skill|--tool|--mcp) 또는 멤버 번호로(--member-id) 지정합니다.
손상된 멤버(레지스트리에서 사라진 항목을 가리키는 행)는 이름이 없으므로 --member-id 로만 지울 수 있습니다.
번호는 forge profile show <이름> 과 forge doctor 의 해결 명령에 나옵니다.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("member-id") {
				if mf.skill != "" || mf.tool != "" || mf.mcp != "" {
					return errors.New("--member-id 는 --skill/--tool/--mcp 와 함께 쓸 수 없습니다")
				}
				if memberID <= 0 {
					return errors.New("--member-id 는 양의 정수여야 합니다")
				}
			}
			var kind, item string
			if !cmd.Flags().Changed("member-id") {
				var err error
				if kind, item, err = mf.one(); err != nil {
					return err
				}
			}
			ref, err := sc.ref(cmd, args[0])
			if err != nil {
				return err
			}
			ps, closeFn, err := openProfileStore(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			if cmd.Flags().Changed("member-id") {
				if err := ps.RemoveMemberID(cmd.Context(), ref, memberID); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "프로필 %s: 멤버 번호 %d 제거\n", textsafe.Display(ref.Name, 70), memberID)
				return nil
			}
			if err := ps.Remove(cmd.Context(), ref, kind, item); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "프로필 %s: %s %s 제거\n", textsafe.Display(ref.Name, 70), kind, textsafe.Display(item, 70))
			return nil
		},
	}
	sc.add(cmd)
	mf.add(cmd)
	cmd.Flags().Int64Var(&memberID, "member-id", 0, "멤버 번호(forge profile show 의 번호)로 제거 — 손상된 멤버 정리용")
	return cmd
}

func clientList(c string) []string {
	if c == "" {
		return nil
	}
	return []string{c}
}

func profileUseCmd() *cobra.Command {
	var client, projectDir string
	cmd := &cobra.Command{
		Use:   "use <name> --client claude-code|opencode|all [--project-dir <dir>]",
		Short: "Activate a profile for a client (records the activation only; run forge sync to apply)",
		Long: `프로필을 클라이언트에 활성화합니다. 활성화 기록(profile_activations)만 바꾸며 클라이언트 설정 파일은 쓰지 않습니다.
설정에 반영하려면 forge sync --target all [--apply] 를 실행하세요. 실행 중인 게이트웨이(forge mcp serve)는 재시작 없이 다음 요청부터 따릅니다.

  --project-dir 없음   그 클라이언트의 전역(모든 프로젝트) 활성 프로필
  --project-dir <dir>  그 프로젝트에서만 적용 (전역 활성 프로필보다 우선). 이름은 프로젝트 프로필 -> 전역 프로필 순으로 찾습니다.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project := ""
			if cmd.Flags().Changed("project-dir") {
				root, err := profile.ProjectRoot(projectDir)
				if err != nil {
					return err
				}
				project = root
			}
			ps, closeFn, err := openProfileStore(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			m, cl, err := ps.Use(cmd.Context(), args[0], project, clientList(client))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			where := "전역(모든 프로젝트)"
			if project != "" {
				where = "프로젝트 " + textsafe.Display(project, 100)
			}
			fmt.Fprintf(out, "프로필 활성화: %s (%s) -> %s, %s\n", textsafe.Display(m.Name, 70), scopeLabel(*m), strings.Join(cl, ", "), where)
			fmt.Fprintln(out, "설정 파일은 아직 바뀌지 않았습니다. 반영: forge sync --target all (계획 확인) -> forge sync --target all --apply")
			return nil
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "claude-code | opencode | all (필수)")
	cmd.Flags().StringVar(&projectDir, "project-dir", "", "이 프로젝트에서만 활성화")
	return cmd
}

func profileUnuseCmd() *cobra.Command {
	var client, projectDir string
	cmd := &cobra.Command{
		Use:   "unuse --client claude-code|opencode|all [--project-dir <dir>]",
		Short: "Deactivate the profile of a client (the next sync goes back to the rule below it)",
		Long: `클라이언트의 활성 프로필을 해제합니다(--project-dir 없음: 전역 활성화, 있음: 그 프로젝트의 활성화).
프로젝트 디렉터리가 삭제·이동되어 없어도 경로 문자열로 활성화를 찾아 해제합니다(새 행은 만들지 않습니다).
없어진 프로젝트의 활성화를 한꺼번에 정리하려면 forge profile prune-stale 을 쓰세요.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A deleted or moved project is still found by its path string (no row is created).
			project := ""
			if cmd.Flags().Changed("project-dir") {
				root, err := profile.LoosePath(projectDir)
				if err != nil {
					return err
				}
				project = root
			}
			ps, closeFn, err := openProfileStore(cmd)
			if err != nil {
				return err
			}
			defer closeFn()
			removed, err := ps.Unuse(cmd.Context(), project, clientList(client))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(removed) == 0 {
				fmt.Fprintln(out, "해제할 활성화가 없습니다.")
				return nil
			}
			fmt.Fprintf(out, "프로필 해제: %s\n설정 파일은 아직 바뀌지 않았습니다. 반영: forge sync --target all --apply\n", strings.Join(removed, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "claude-code | opencode | all (필수)")
	cmd.Flags().StringVar(&projectDir, "project-dir", "", "이 프로젝트의 활성화를 해제 (기본: 전역 활성화)")
	return cmd
}

func profileCurrentCmd() *cobra.Command {
	var client, projectDir string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "current [--client <id>|all] [--project-dir <dir>]",
		Short: "Show which rule applies per client: project profile, global profile or legacy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := profile.LoosePath(projectDir)
			if err != nil {
				return err
			}
			rd, err := openReadOnlyRegistry(cmd)
			if err != nil {
				return err
			}
			defer rd.close()
			clients, err := profile.Clients(cmd.Context(), rd)
			if err != nil {
				return err
			}
			if client != "" && client != "all" {
				ok := false
				for _, c := range clients {
					ok = ok || c == client
				}
				if !ok {
					return fmt.Errorf("알 수 없는 클라이언트: %q (지원: %s)", textsafe.Display(client, 40), strings.Join(clients, ", "))
				}
				clients = []string{client}
			}
			type row struct {
				Client  string `json:"client"`
				Rule    string `json:"rule"`
				Profile string `json:"profile,omitempty"`
				Scope   string `json:"profile_scope,omitempty"`
				Members int    `json:"members,omitempty"`
			}
			var rows []row
			sort.Strings(clients)
			for _, c := range clients {
				r, err := profile.Resolve(cmd.Context(), rd, c, root)
				if err != nil {
					return err
				}
				x := row{Client: c, Rule: r.Rule}
				if r.Active() {
					x.Profile, x.Scope, x.Members = r.Profile.Name, r.Profile.Scope, len(r.Skills)+len(r.Tools)+len(r.MCP)
				}
				rows = append(rows, x)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, map[string]any{"schema": 1, "project": root, "clients": rows})
			}
			fmt.Fprintf(out, "프로젝트: %s\n", textsafe.Display(root, 200))
			if _, serr := os.Stat(root); serr != nil {
				if errors.Is(serr, fs.ErrNotExist) || errors.Is(serr, syscall.ENOTDIR) {
					fmt.Fprintln(out, "  (이 경로는 존재하지 않습니다: 저장된 활성화만 보여 줍니다. 정리: forge profile prune-stale)")
				} else {
					fmt.Fprintf(out, "  (이 경로를 확인할 수 없습니다: %s. 저장된 활성화만 보여 줍니다)\n", textsafe.Display(serr.Error(), 120))
				}
			}
			for _, x := range rows {
				switch x.Rule {
				case profile.RuleLegacy:
					fmt.Fprintf(out, "  %-12s 프로필 없음 -> 레거시 동작 (활성화된 항목 전체 + mcp.exposure.* 설정)\n", x.Client)
				default:
					label := map[string]string{profile.RuleProject: "이 프로젝트에 활성화", profile.RuleGlobal: "전역 활성화"}[x.Rule]
					fmt.Fprintf(out, "  %-12s 프로필 %s (%s; 멤버 %d, 허용 목록)\n", x.Client, textsafe.Display(x.Profile, 70), label, x.Members)
				}
			}
			fmt.Fprintln(out, "우선순위: 이 프로젝트의 활성 프로필 -> 클라이언트의 전역 활성 프로필 -> 레거시 동작")
			return nil
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "클라이언트 하나만 (기본·all: 전체)")
	cmd.Flags().StringVar(&projectDir, "project-dir", "", "프로젝트 디렉터리 (기본: 현재 디렉터리)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

// refuseWhileProfileActive is the policy of the legacy `forge mcp expose|enable|
// disable --client`: while a profile applies to the client here (this project or
// the client's global profile) the mcp.exposure.* setting is ignored by sync and
// the gateway, so changing it would silently do nothing. The command refuses and
// prints the exact profile command instead (safe: nothing changes, nothing is
// guessed). Without an applicable profile the legacy behaviour is unchanged.
func refuseWhileProfileActive(cmd *cobra.Command, reg *mcp.Registry, client, server, mode string) error {
	root, err := profile.ProjectRoot("")
	if err != nil {
		return nil // cannot tell which project: only the global rule is checked below
	}
	r, err := profile.Resolve(cmd.Context(), reg.DB, client, root)
	if err != nil || !r.Active() {
		return nil // an unreadable profile table is reported by forge doctor, not here
	}
	flag := ""
	if r.Profile.Scope == profile.ScopeProject {
		flag = " --project-dir " + shellQuote(r.Profile.Project)
	}
	return fmt.Errorf("클라이언트 %s 에는 프로필 %q 가 적용 중이라 mcp.exposure.* 설정은 무시됩니다 (프로필이 노출의 정식 저장소입니다). "+
		"프로필을 수정하세요: forge profile add %s --mcp %s --exposure %s --client %s%s",
		client, textsafe.Display(r.Profile.Name, 70), shellQuote(r.Profile.Name), shellQuote(server), mode, client, flag)
}

// noteProfilesActive tells the user that enabling/disabling a server interacts
// with profiles (the enabled flag wins over membership).
func noteProfilesActive(cmd *cobra.Command, reg *mcp.Registry) {
	acts, err := profile.Activations(cmd.Context(), reg.DB)
	if err != nil || len(acts) == 0 {
		return
	}
	fmt.Fprintln(cmd.OutOrStdout(), "참고: 프로필이 활성화된 클라이언트가 있습니다. 비활성화된 서버는 프로필 멤버여도 노출되지 않습니다 (forge profile current)")
}

func profilePruneStaleCmd() *cobra.Command {
	var dryRun, yes, asJSON bool
	cmd := &cobra.Command{
		Use:   "prune-stale [--dry-run] [--yes] [--json]",
		Short: "List (default) or remove activations of projects whose directory no longer exists",
		Long: `삭제되었거나 이동되어 경로가 더 이상 없는 프로젝트의 프로필 활성화를 정리합니다(ADR-F0001 O-4).
기본은 목록만 보여 주는 dry-run 이며, --yes 를 줄 때만 지웁니다(한 트랜잭션).

  forge profile prune-stale             # 목록(경로, 활성화, 마지막 활성화 시각)만 표시
  forge profile prune-stale --yes       # 실제로 정리

지우는 것: 경로가 없는 프로젝트의 활성화, 그리고 그 뒤 아무것도 참조하지 않는 projects 행.
지우지 않는 것: 프로필 자체(프로젝트 프로필 포함), 경로가 있는 프로젝트, 권한 오류 등으로 "없음"을 확인하지 못한 경로.
프로젝트를 옮겼다면 지우기 전에 새 위치에서 forge profile use <프로필> --client <c> --project-dir <새 경로> 로 다시 활성화하세요.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRun && yes {
				return errors.New("--dry-run 과 --yes 는 함께 쓸 수 없습니다")
			}
			var list []profile.StaleProject
			if yes {
				ps, closeFn, err := openProfileStore(cmd)
				if err != nil {
					return err
				}
				defer closeFn()
				if list, err = ps.PruneStale(cmd.Context()); err != nil {
					return err
				}
			} else {
				rd, err := openReadOnlyRegistry(cmd)
				if err != nil {
					return err
				}
				defer rd.close()
				if list, err = profile.Stale(cmd.Context(), rd); err != nil {
					return err
				}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if list == nil {
					list = []profile.StaleProject{}
				}
				return writeJSON(out, map[string]any{"schema": 1, "dry_run": !yes, "projects": list})
			}
			if len(list) == 0 {
				fmt.Fprintln(out, "정리할 항목이 없습니다 (경로가 없어진 프로젝트의 활성화 없음).")
				return nil
			}
			if yes {
				fmt.Fprintf(out, "정리함: 프로젝트 %d개\n", len(list))
			} else {
				fmt.Fprintf(out, "경로가 없는 프로젝트 %d개 (dry-run: 아무것도 지우지 않았습니다):\n", len(list))
			}
			nact := 0
			for _, p := range list {
				fmt.Fprintf(out, "\n  %s\n", textsafe.Display(p.Path, 300))
				for _, a := range p.Activations {
					nact++
					fmt.Fprintf(out, "    활성화: %s -> 프로필 %s (%s), 마지막 활성화 %s\n", a.Client, textsafe.Display(a.Profile, 70), a.ProfileScope,
						time.UnixMilli(a.UpdatedAt).Local().Format("2006-01-02 15:04"))
				}
				if len(p.KeptProfiles) > 0 {
					fmt.Fprintf(out, "    유지: 이 프로젝트의 프로젝트 프로필 %d개(%s)는 지우지 않습니다\n", len(p.KeptProfiles), textsafe.Display(strings.Join(p.KeptProfiles, ", "), 150))
				}
				if p.RemoveRow {
					fmt.Fprintln(out, "    프로젝트 기록(projects 행)도 지웁니다: 더 이상 아무것도 참조하지 않음")
				}
			}
			if !yes {
				fmt.Fprintf(out, "\n활성화 %d건을 지우려면: forge profile prune-stale --yes\n", nact)
				fmt.Fprintln(out, "프로젝트를 옮겼다면 먼저 새 위치에서: forge profile use <프로필> --client <클라이언트> --project-dir <새 경로>")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "목록만 표시 (기본 동작과 같음)")
	cmd.Flags().BoolVar(&yes, "yes", false, "실제로 정리")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

// warnProfileMembers tells the user which profiles lose the item when it is
// removed (profile_members rows go with it, ON DELETE CASCADE). It is printed
// before the confirmation and also with --yes; it never blocks. A profile that is
// active somewhere is marked, because it then exposes one item less.
func warnProfileMembers(cmd *cobra.Command, db profile.Querier, kind, name string) {
	ms, err := profile.MembershipsOf(cmd.Context(), db, kind, name)
	if err != nil || len(ms) == 0 {
		return
	}
	parts := make([]string, 0, len(ms))
	for i, m := range ms {
		if i == 8 {
			parts = append(parts, fmt.Sprintf("외 %d건", len(ms)-8))
			break
		}
		s := textsafe.Display(m.Profile, 70)
		if m.Scope == profile.ScopeProject {
			s += " (프로젝트 " + textsafe.Display(m.Project, 80) + ")"
		}
		if len(m.Active) > 0 {
			s += " [활성 프로필: " + textsafe.Display(strings.Join(m.Active, ", "), 120) + "]"
		}
		parts = append(parts, s)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "다음 프로필에서 제외됩니다: %s\n", strings.Join(parts, ", "))
}
