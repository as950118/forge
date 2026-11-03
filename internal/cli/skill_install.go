package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/as950118/forge/internal/adapter"
	"github.com/as950118/forge/internal/skill"
	"github.com/as950118/forge/internal/store"
	"github.com/as950118/forge/internal/textsafe"
)

// skillGit is the git implementation used by skill add/update/check. nil means
// the real git binary; tests inject a fake (never the network).
var skillGit skill.Cloner

// stdinIsTerminal reports whether r is an interactive terminal. A variable so
// tests can simulate a terminal; anything that is not an *os.File (tests,
// pipes of the test harness) is never a terminal.
var stdinIsTerminal = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0 && isTerminalFd(f.Fd())
}

// confirmInstall asks for explicit approval after the preview. yes (--yes) is
// the non-interactive approval. Without a terminal and without --yes it
// refuses: nothing may be installed because nobody could have read the preview.
// Only "y"/"yes" approves; EOF, empty and anything else decline.
// afterApprovalHook is a test-only seam: it runs after the user approved an install or
// update and before the staging is moved (Restage), so a test can swap the staged tree
// at the moment an attacker would. It is nil in production and set by no production code.
var afterApprovalHook func(p *skill.Prepared)

func confirmInstall(cmd *cobra.Command, yes bool, question, previewCmd string) error {
	if yes {
		return nil
	}
	if !stdinIsTerminal(cmd.InOrStdin()) {
		return fmt.Errorf("승인이 필요하지만 대화형 터미널이 아니고 --yes 도 지정되지 않았습니다. 아무것도 설치하지 않았습니다.\n  1) 미리보기 확인: %s\n  2) 내용을 확인했다면 같은 명령에 --yes 를 붙여 다시 실행하세요", previewCmd)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N] ", question)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(cmd.ErrOrStderr())
		return errors.New("취소되었습니다 (입력 없음). 아무것도 설치하지 않았습니다")
	}
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("취소되었습니다. 아무것도 설치하지 않았습니다")
	}
	return nil
}

// openRegistryRO opens the registry for reading only (nothing is created or
// migrated). A missing database returns (nil, nil, nil).
func openRegistryRO() (*skill.Registry, func(), error) {
	home, err := store.Home()
	if err != nil {
		return nil, nil, err
	}
	conn, err := store.OpenReadOnly(store.DBPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, func() {}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &skill.Registry{DB: conn, Home: home}, func() { _ = conn.Close() }, nil
}

func installedOf(ctx context.Context, reg *skill.Registry, name string) *skill.Installed {
	if reg == nil {
		return nil
	}
	info, err := reg.Get(ctx, name)
	if err != nil {
		return nil
	}
	for _, v := range info.Versions {
		if v.Current {
			return &skill.Installed{Version: v.Version, Hash: v.Hash, Root: filepath.Join(reg.Home, filepath.FromSlash(v.StorePath))}
		}
	}
	return nil
}

func shq(s string) string {
	for _, r := range s {
		if textsafe.NeedsEscape(r) {
			return "'" + strings.ReplaceAll(textsafe.Display(s, 300), "'", `'\''`) + "'"
		}
	}
	return adapter.ShellQuote(s)
}

func renderSkillPreview(w io.Writer, pv *skill.Preview, title string) {
	p := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }
	p("== %s ==\n", title)
	p("소스:        %s  %s\n", pv.SourceType, pv.SourceURI)
	if pv.SourceType == skill.SourceGit {
		p("커밋:        %s\n", pv.Commit)
		switch pv.RefStatus {
		case "pinned":
			p("ref:         %s  (고정됨: 커밋 해시)\n", pv.RequestedRef)
		case "moving":
			p("ref:         %s  (고정되지 않음: 브랜치/태그는 이동할 수 있음)\n", pv.RequestedRef)
		default:
			p("ref:         (지정 안 함)  (고정되지 않음: 기본 브랜치 HEAD)\n")
		}
	}
	p("이름:        %s%s\n", pv.Name, map[bool]string{true: "  버전 " + pv.Version, false: ""}[pv.Version != ""])
	p("설명:        %s\n", pv.Description)
	if pv.License != "" {
		p("라이선스:    %s\n", pv.License)
	}
	p("파일:        %d개, %d 바이트\n", pv.FileCount, pv.TotalBytes)
	p("트리 sha256: %s   (고정하려면: --sha256 %s)\n", pv.TreeHash, pv.TreeHash)
	p("\n[frontmatter]\n")
	for _, f := range pv.Frontmatter {
		mark := "  "
		if f.Risky {
			mark = "! "
		}
		p("%s%s: %s\n", mark, f.Key, f.Value)
		if f.Risky {
			p("    ↳ 주의: %s\n", f.Why)
		}
	}
	if len(pv.Executables) > 0 {
		p("\n[실행 가능한 파일] (위험도 순: 위험 패턴·실행 비트·셔뱅 우선, 같으면 경로 순 — 패턴과 첫 줄은 휴리스틱이며 파일 일부일 뿐)\n")
		for _, e := range pv.Executables {
			p("  %s  %d바이트  sha256=%s  인터프리터=%s  (%s)\n", e.Path, e.Size, e.SHA256, orDash(e.Interpreter), strings.Join(e.Reasons, ", "))
			if len(e.RiskPatterns) > 0 {
				p("    위험 패턴: %s\n", strings.Join(e.RiskPatterns, ", "))
			}
			for i, ln := range e.Head {
				p("    %2d| %s\n", i+1, ln)
			}
			if e.HeadMore {
				p("    … (이하 생략)\n")
			}
		}
		if pv.ExecMore > 0 {
			p("  … 외 %d개 (위험도가 낮은 순서; 목록에서 생략됨)\n", pv.ExecMore)
		}
		if pv.ExecRisky > 0 {
			p("  위험 패턴이 있는 실행 가능한 파일: 전체 %d개\n", pv.ExecRisky)
		}
	} else {
		p("\n[실행 가능한 파일] 없음\n")
	}
	listBlock(w, "숨김 파일", pv.Hidden, pv.HiddenMore)
	listBlock(w, "텍스트가 아닌 파일", pv.Binary, pv.BinaryMore)
	p("\n[SKILL.md 위험 패턴 — 휴리스틱]\n")
	if len(pv.Patterns)+len(pv.URLs)+len(pv.IPs) == 0 {
		p("  발견 없음 (안전하다는 뜻은 아닙니다)\n")
	}
	for _, h := range pv.Patterns {
		p("  %s: %d줄 (처음 %d행) 예: %s\n", h.Kind, h.Count, h.FirstLine, h.Example)
	}
	if len(pv.URLs) > 0 {
		p("  URL: %s\n", strings.Join(pv.URLs, "  "))
	}
	if len(pv.IPs) > 0 {
		p("  IP: %s\n", strings.Join(pv.IPs, "  "))
	}
	if c := pv.Existing; c != nil {
		p("\n[기존 설치본과의 변경] 현재 버전 %s (sha256 %.12s)\n", c.CurrentVersion, c.CurrentHash)
		switch {
		case c.Note != "":
			p("  %s\n", c.Note)
		case c.Identical:
			p("  내용이 같습니다 (변경 없음)\n")
		default:
			listBlock(w, "추가된 파일", c.Added, 0)
			listBlock(w, "삭제된 파일", c.Removed, 0)
			listBlock(w, "변경된 파일", c.Changed, c.More)
			listBlock(w, "새로 생긴 실행 파일", c.NewExecutables, 0)
			listBlock(w, "새로 생긴 위험 frontmatter", c.NewRisky, 0)
			listBlock(w, "값이 바뀐 위험 frontmatter", c.ChangedRisky, 0)
		}
	}
	if len(pv.Warnings) > 0 {
		p("\n[경고]\n")
		for _, x := range pv.Warnings {
			p("  ! %s\n", x)
		}
	}
	p("\n")
	for _, n := range pv.Notes {
		p("참고: %s\n", n)
	}
	p("\n")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func listBlock(w io.Writer, title string, items []string, more int) {
	if len(items) == 0 && more == 0 {
		return
	}
	fmt.Fprintf(w, "\n[%s]\n", title)
	for _, it := range items {
		fmt.Fprintf(w, "  %s\n", it)
	}
	if more > 0 {
		fmt.Fprintf(w, "  … 외 %d개\n", more)
	}
}

func skillAddCmd() *cobra.Command {
	var (
		asJSON, yes, previewOnly, strictLocal bool
		sha, ref                              string
	)
	cmd := &cobra.Command{
		Use:   "add <dir|file.zip|https-or-ssh-git-url>",
		Short: "Add a skill (preview first; git/zip need approval)",
		Long: `스킬을 가져와 레지스트리에 추가합니다. 설치 전에 항상 미리보기(stderr)를 출력합니다:
소스·커밋, 파일 수·크기, 트리 sha256, frontmatter(hooks·allowed-tools 등 위험 필드 강조),
실행 가능한 파일(크기·해시·인터프리터), SKILL.md 의 위험 패턴(휴리스틱), 기존 설치본과의 변경 요약.

승인 정책
  git URL / .zip : 승인 필수. 터미널에서는 y/N 질문, 비대화형에서는 --yes 가 없으면 거부(종료 코드 1, 아무것도 설치하지 않음).
  로컬 디렉터리  : 디스크에서 직접 볼 수 있으므로 미리보기만 출력하고 질문 없이 진행. --strict-local 이면 승인 필수.
  --preview      : 미리보기만 출력하고 아무것도 설치·기록하지 않습니다(git 은 임시 디렉터리에 받아 즉시 삭제).
고정
  --sha256 <해시>: 가져온 트리의 sha256 이 다르면 설치하지 않습니다.
  --ref <ref>    : git 소스를 브랜치/태그/커밋으로 지정(40자 커밋 해시만 "고정됨"; 브랜치·태그는 이동 가능).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			wantHash := ""
			if sha != "" {
				h, err := skill.ParseSHA256(sha)
				if err != nil {
					return err
				}
				wantHash = h
			}
			if previewOnly && yes {
				return errors.New("--preview 와 --yes 는 함께 쓸 수 없습니다 (--preview 는 아무것도 설치하지 않습니다)")
			}
			// Everything before the approval is side-effect free: the registry is
			// opened read-only (a missing FORGE_HOME or database is not created)
			// and the staging goes to the system temp directory. FORGE_HOME is
			// created and written only after the install is approved.
			opts := skill.PrepareOptions{Git: skillGit, Ref: ref}
			reg, closeFn, err := openRegistryRO()
			if err != nil {
				if previewOnly {
					return err
				}
				// Not readable read-only (e.g. an unflushed WAL): the read-write
				// open of the previous behavior is the fallback for installs.
				r, conn, oerr := openRegistry(ctx)
				if oerr != nil {
					return oerr
				}
				reg, closeFn = r, func() { _ = conn.Close() }
				opts.TmpDir = homeTmp(reg.Home)
			}
			defer func() { closeFn() }()
			p, err := skill.Prepare(ctx, args[0], opts)
			if err != nil {
				return err
			}
			defer p.Cleanup()
			hash, err := skill.HashTree(p.Root)
			if err != nil {
				return fmt.Errorf("해시 계산 실패: %w", err)
			}
			pv, err := skill.BuildPreview(p, hash, installedOf(ctx, reg, p.Meta.Name))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !asJSON {
				renderSkillPreview(cmd.ErrOrStderr(), pv, "스킬 설치 미리보기")
				for _, w := range p.Warnings {
					fmt.Fprintf(cmd.ErrOrStderr(), "경고: %s\n", textsafe.Display(w, 300))
				}
			}
			if previewOnly {
				if asJSON {
					return writeJSON(out, map[string]any{"preview": pv, "installed": false, "preview_only": true, "warnings": p.Warnings})
				}
				fmt.Fprintln(out, "미리보기만 출력했습니다. 아무것도 설치하지 않았습니다.")
				return nil
			}
			refuse := func(err error) error {
				if asJSON {
					_ = writeJSON(out, map[string]any{"preview": pv, "installed": false, "error": err.Error()})
				}
				return err
			}
			if wantHash != "" && wantHash != hash {
				return refuse(fmt.Errorf("%w: 기대 %s, 실제 %s. 아무것도 설치하지 않았습니다 (원본이 바뀌었거나 다른 소스일 수 있습니다 — 미리보기를 다시 확인하세요)", skill.ErrHashMismatch, wantHash, hash))
			}
			if p.SourceType != skill.SourceDir || strictLocal {
				prev := "forge skill add --preview " + shq(args[0])
				if err := confirmInstall(cmd, yes, "위 미리보기를 확인했습니다. 이 스킬을 설치할까요?", prev); err != nil {
					return refuse(err)
				}
			}
			if opts.TmpDir == "" {
				// Approved: only now create FORGE_HOME and the database.
				closeFn()
				r, conn, err := openRegistry(ctx)
				if err != nil {
					return err
				}
				closeFn = func() { _ = conn.Close() }
				reg = r
			}
			// Both paths: the read-write fallback (opts.TmpDir set: the staging already lives
			// under FORGE_HOME/tmp) also moves the staging into a fresh directory with the
			// approved hash, so a staging replaced after the preview is refused (TASK-0032 AC-6).
			if afterApprovalHook != nil {
				afterApprovalHook(p)
			}
			if err := p.Restage(homeTmp(reg.Home), hash); err != nil {
				return err
			}
			res, err := reg.Add(ctx, p)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(out, map[string]any{"result": res, "installed": res.Status != "unchanged", "warnings": p.Warnings, "preview": pv})
			}
			printAddResult(cmd, res)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&asJSON, "json", false, "JSON 출력 (미리보기 포함)")
	f.BoolVarP(&yes, "yes", "y", false, "미리보기를 확인했음을 선언하고 비대화형으로 승인")
	f.BoolVar(&previewOnly, "preview", false, "미리보기만 출력하고 아무것도 설치하지 않음")
	f.BoolVar(&strictLocal, "strict-local", false, "로컬 디렉터리도 승인 필수")
	f.StringVar(&sha, "sha256", "", "가져온 트리의 기대 sha256 (다르면 설치하지 않음)")
	f.StringVar(&ref, "ref", "", "git 소스의 브랜치/태그/커밋 (40자 커밋 해시만 고정됨)")
	return cmd
}

func printAddResult(cmd *cobra.Command, res *skill.AddResult) {
	out := cmd.OutOrStdout()
	switch res.Status {
	case "unchanged":
		fmt.Fprintf(out, "변경 없음: %s@%s 는 이미 등록되어 있습니다 (내용 해시 %.12s)\n", res.Name, res.Version, res.Hash)
	case "new_version":
		fmt.Fprintf(out, "새 버전 추가: %s@%s (해시 %.12s)\n", res.Name, res.Version, res.Hash)
		fmt.Fprintf(out, "새 버전이 현재 버전이 되었습니다(이전: %s)\n", res.PreviousVersion)
	default:
		fmt.Fprintf(out, "추가됨: %s@%s (해시 %.12s)\n", res.Name, res.Version, res.Hash)
	}
}

func skillUpdateCmd() *cobra.Command {
	var yes, asJSON bool
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Re-fetch the recorded source and install it as a new immutable version after approval",
		Long: `기록된 소스(source_uri/source_ref)를 다시 가져와 현재 설치 버전과의 변경 요약을 미리보기로 보여 주고,
승인(터미널 y/N 또는 --yes)할 때만 새 불변 버전으로 추가합니다. 옛 버전은 보존되며 새 버전이 현재 버전이 됩니다
(활성/비활성 상태는 그대로). git 소스는 --ref 로 지정했던 ref 를 다시 가져옵니다(커밋 고정이면 항상 같은 내용).
로컬 디렉터리/zip 소스도 기록된 경로를 다시 읽습니다. 네트워크가 필요한 것은 git 소스뿐입니다.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			reg, conn, err := openRegistry(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			info, err := reg.Get(ctx, args[0])
			if err != nil {
				return err
			}
			var cur *skill.Version
			for i := range info.Versions {
				if info.Versions[i].Current {
					cur = &info.Versions[i]
				}
			}
			if cur == nil {
				return errors.New("현재 버전이 없는 스킬은 업데이트할 수 없습니다")
			}
			if cur.SourceURI == "" {
				return fmt.Errorf("소스가 기록되지 않은 스킬입니다 (source_type=%s). forge skill add <소스> 로 다시 추가하세요", cur.SourceType)
			}
			opts := skill.PrepareOptions{TmpDir: homeTmp(reg.Home), Git: skillGit}
			if cur.SourceType == skill.SourceGit {
				opts.Ref, _ = skill.SplitSourceRef(cur.SourceRef)
			}
			p, err := skill.Prepare(ctx, cur.SourceURI, opts)
			if err != nil {
				return fmt.Errorf("기록된 소스를 다시 가져오지 못했습니다: %w", err)
			}
			defer p.Cleanup()
			if p.Meta.Name != info.Name {
				return fmt.Errorf("소스의 스킬 이름이 %q 로 바뀌었습니다 (등록된 이름 %q). 다른 스킬일 수 있어 업데이트하지 않습니다 — 확인 후 forge skill add 를 쓰세요", textsafe.Display(p.Meta.Name, 80), info.Name)
			}
			hash, err := skill.HashTree(p.Root)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if hash == cur.Hash {
				if asJSON {
					return writeJSON(out, map[string]any{"name": info.Name, "status": "unchanged", "current_hash": cur.Hash})
				}
				fmt.Fprintf(out, "변경 없음: %s@%s (원본이 설치된 내용과 같습니다, 해시 %.12s)\n", info.Name, cur.Version, cur.Hash)
				return nil
			}
			pv, err := skill.BuildPreview(p, hash, &skill.Installed{Version: cur.Version, Hash: cur.Hash, Root: filepath.Join(reg.Home, filepath.FromSlash(cur.StorePath))})
			if err != nil {
				return err
			}
			if !asJSON {
				renderSkillPreview(cmd.ErrOrStderr(), pv, "스킬 업데이트 미리보기")
			}
			refuse := func(err error) error {
				if asJSON {
					_ = writeJSON(out, map[string]any{"preview": pv, "installed": false, "error": err.Error()})
				}
				return err
			}
			if err := confirmInstall(cmd, yes, "위 변경을 확인했습니다. 새 버전으로 업데이트할까요?", "forge skill update "+shq(info.Name)+"   # 터미널에서 실행하면 미리보기 후 질문합니다"); err != nil {
				return refuse(err)
			}
			// the staging may have been replaced while the question was open: install only the approved tree
			if afterApprovalHook != nil {
				afterApprovalHook(p)
			}
			if err := p.Restage(homeTmp(reg.Home), hash); err != nil {
				return err
			}
			res, err := reg.Add(ctx, p)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(out, map[string]any{"result": res, "installed": true, "preview": pv})
			}
			printAddResult(cmd, res)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "변경 요약을 확인했음을 선언하고 비대화형으로 승인")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

type skillCheckRow struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Current    bool   `json:"current"`
	Store      string `json:"store_state"` // ok | modified | missing | unreadable
	StoredHash string `json:"content_hash"`
	ActualHash string `json:"actual_hash,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Upstream   string `json:"upstream"` // not_checked | same | changed | error
	UpDetail   string `json:"upstream_detail,omitempty"`
}

func skillCheckCmd() *cobra.Command {
	var fetch, asJSON bool
	cmd := &cobra.Command{
		Use:   "check [name]",
		Short: "Read-only: detect tampering of installed skill files (and, with --fetch, upstream changes)",
		Long: `기본 동작은 로컬 정보만 사용하는 읽기 전용 점검입니다: ~/.forge/skills/<이름>@<버전>/ 의 파일을 다시 해시해
등록된 content_hash 와 비교합니다(설치 후 파일이 바뀌었는지 = 저장소 변조 탐지). 네트워크에는 접근하지 않습니다.
--fetch 를 주면 현재 버전의 기록된 소스(git URL 포함)를 다시 가져와 설치본과 해시를 비교합니다(원본이 바뀌었는지).
변조·누락이 있으면 종료 코드 1 입니다.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			reg, closeFn, err := openRegistryRO()
			if err != nil {
				return err
			}
			defer closeFn()
			if reg == nil {
				return errors.New("Forge 저장소가 없습니다 (forge init)")
			}
			var names []string
			if len(args) == 1 {
				names = args
			} else {
				items, err := reg.List(ctx)
				if err != nil {
					return err
				}
				for _, i := range items {
					names = append(names, i.Name)
				}
			}
			var rows []skillCheckRow
			bad := 0
			for _, n := range names {
				info, err := reg.Get(ctx, n)
				if err != nil {
					return err
				}
				for _, v := range info.Versions {
					r := skillCheckRow{Name: info.Name, Version: v.Version, Current: v.Current, StoredHash: v.Hash, Upstream: "not_checked"}
					root := filepath.Join(reg.Home, filepath.FromSlash(v.StorePath))
					if rel, e := filepath.Rel(filepath.Join(reg.Home, "skills"), root); e != nil || strings.HasPrefix(rel, "..") {
						r.Store, r.Detail = skill.StoreModified, "저장 경로가 저장소 밖을 가리킵니다"
					} else {
						st, got, e := skill.VerifyStore(root, v.Hash)
						r.Store, r.ActualHash = st, got
						if e != nil {
							r.Detail = textsafe.Display(e.Error(), 200)
						}
					}
					if r.Store != skill.StoreOK {
						bad++
					}
					if fetch && v.Current {
						r.Upstream, r.UpDetail = checkUpstream(ctx, v)
					}
					rows = append(rows, r)
				}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if err := writeJSON(out, map[string]any{"schema": 1, "fetch": fetch, "tampered": bad, "skills": rows}); err != nil {
					return err
				}
			} else {
				if len(rows) == 0 {
					fmt.Fprintln(out, "등록된 스킬이 없습니다.")
				}
				for _, r := range rows {
					cur := ""
					if r.Current {
						cur = " (현재)"
					}
					switch r.Store {
					case skill.StoreOK:
						fmt.Fprintf(out, "OK       %s@%s%s: 설치 파일이 등록된 해시와 같습니다\n", r.Name, r.Version, cur)
					case skill.StoreMissing:
						fmt.Fprintf(out, "누락     %s@%s%s: 저장된 파일이 없습니다\n", r.Name, r.Version, cur)
					case skill.StoreModified:
						fmt.Fprintf(out, "변조 의심 %s@%s%s: 설치된 파일이 등록 후 바뀌었습니다 (등록 %.12s, 현재 %.12s) %s\n", r.Name, r.Version, cur, r.StoredHash, r.ActualHash, r.Detail)
					default:
						fmt.Fprintf(out, "점검 불가 %s@%s%s: %s\n", r.Name, r.Version, cur, r.Detail)
					}
					switch r.Upstream {
					case "same":
						fmt.Fprintf(out, "         원본: 설치본과 같습니다\n")
					case "changed":
						fmt.Fprintf(out, "         원본: 설치본과 다릅니다 → 변경 확인: forge skill update %s\n", shq(r.Name))
					case "error":
						fmt.Fprintf(out, "         원본: 확인 실패: %s\n", r.UpDetail)
					}
				}
				if !fetch && len(rows) > 0 {
					fmt.Fprintln(out, "(원본과의 비교는 네트워크가 필요할 수 있어 --fetch 를 줄 때만 합니다)")
				}
			}
			if bad > 0 {
				return fmt.Errorf("스킬 저장소 이상 %d건: 변조 또는 손상일 수 있습니다. 확인: forge skill show <이름> / 복구: forge skill remove <이름> 후 신뢰하는 소스에서 다시 추가", bad)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fetch, "fetch", false, "기록된 소스(네트워크 포함)를 다시 가져와 원본 변경도 확인")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON 출력")
	return cmd
}

func checkUpstream(ctx context.Context, v skill.Version) (state, detail string) {
	if v.SourceURI == "" {
		return "error", "소스가 기록되지 않았습니다"
	}
	opts := skill.PrepareOptions{Git: skillGit}
	if v.SourceType == skill.SourceGit {
		opts.Ref, _ = skill.SplitSourceRef(v.SourceRef)
	}
	p, err := skill.Prepare(ctx, v.SourceURI, opts)
	if err != nil {
		return "error", textsafe.Display(err.Error(), 200)
	}
	defer p.Cleanup()
	h, err := skill.HashTree(p.Root)
	if err != nil {
		return "error", textsafe.Display(err.Error(), 200)
	}
	if h == v.Hash {
		return "same", ""
	}
	return "changed", ""
}
