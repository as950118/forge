package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/as950118/forge/internal/textsafe"
)

// Report is the stable JSON document of `forge sync --json`.
type Report struct {
	Schema    int            `json:"schema"`
	Target    string         `json:"target"`
	Scope     Scope          `json:"scope"`
	Root      string         `json:"root"`
	Mode      string         `json:"mode"` // "plan" | "apply"
	Applied   bool           `json:"applied"`
	Conflicts int            `json:"conflicts"`
	Summary   map[string]int `json:"summary"`
	Items     []Item         `json:"items"`
	Files     []FileChange   `json:"files"`
	Commands  []string       `json:"commands"`
	Notes     []string       `json:"notes"`
	Diff      []FileDiff     `json:"diff,omitempty"`
	Backup    *BackupRef     `json:"backup,omitempty"`
	// Interrupted: earlier syncs that were killed before finishing (the plan
	// accounts for them; --apply finishes the work).
	Interrupted []InterruptedInfo `json:"interrupted,omitempty"`
	// LostBackups: interrupted syncs whose backup directory was deleted (their
	// journal copy next to the state file is all that is left; see Notes).
	LostBackups []string `json:"lost_backups,omitempty"`
	// StaleTempFiles: leftovers of killed syncs; --apply removes them.
	StaleTempFiles []string `json:"stale_temp_files,omitempty"`
	// CleanedTempFiles / Recovered: what an apply did about the above.
	CleanedTempFiles []string `json:"cleaned_temp_files,omitempty"`
	Recovered        []string `json:"recovered,omitempty"`
}

// BackupRef points at the backup made by an apply.
type BackupRef struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

func safe(s string) string { return textsafe.Display(s, 300) }

// Limits for the free-text fields (bytes after escaping). A Korean sentence takes
// about three bytes per character, so the 300 bytes of safe() cut a note or a
// reason in the middle of the advice it exists to give (TASK-0027 AC-19). The
// table still shortens a reason for its cell and prints the rest below it.
const (
	maxReasonBytes = 700
	maxNoteBytes   = 2400
)

func safeReason(s string) string { return textsafe.Display(s, maxReasonBytes) }
func safeNote(s string) string   { return textsafe.Display(s, maxNoteBytes) }

// Table cells are display-only: they are cut to a fixed number of characters so
// that one very long name cannot stretch the table across hundreds of columns.
// --json keeps the full (sanitized) strings.
const (
	maxNameCell   = 80
	maxReasonCell = 120
	wrapWidth     = 140 // width of the continuation lines below the table
)

func cell(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// wrapRunes cuts s into lines of at most width characters (display only).
func wrapRunes(s string, width int) []string {
	r := []rune(s)
	var out []string
	for len(r) > width {
		out = append(out, string(r[:width]))
		r = r[width:]
	}
	return append(out, string(r))
}

func truncatedCells(rows []Item) bool {
	for _, it := range rows {
		if len([]rune(it.Name)) > maxNameCell || len([]rune(it.Reason)) > maxReasonCell {
			return true
		}
	}
	return false
}

var actionLabel = map[Action]string{
	ActionAdd: "추가", ActionUpdate: "변경", ActionRemove: "삭제", ActionConflict: "충돌",
	ActionSkip: "건너뜀", ActionUnchanged: "변경 없음", ActionInfo: "안내",
}

var kindLabel = map[string]string{KindSkill: "스킬", KindMCP: "MCP", KindHook: "hook", KindCommand: "명령"}

var changeLabel = map[string]string{"create": "생성", "modify": "수정", "delete": "삭제", "replace": "교체"}

// BuildReport assembles the JSON/text report data with every dynamic string sanitized.
func BuildReport(p *Plan, res *ApplyResult, withDiff bool) *Report {
	r := &Report{Schema: 1, Target: p.Target, Scope: p.Scope, Root: safe(p.Root), Mode: "plan",
		Conflicts: p.Conflicts(), Summary: map[string]int{}, Items: []Item{}, Files: []FileChange{}, Commands: []string{}, Notes: []string{}}
	for _, a := range []Action{ActionAdd, ActionUpdate, ActionRemove, ActionConflict, ActionSkip, ActionUnchanged} {
		r.Summary[string(a)] = 0
	}
	for _, it := range p.Items {
		r.Summary[string(it.Action)]++
		it.Name, it.Reason, it.Path = safe(it.Name), safeReason(it.Reason), safe(it.Path)
		r.Items = append(r.Items, it)
	}
	for _, f := range p.Ops {
		r.Files = append(r.Files, FileChange{Path: safe(f.Path), Change: changeKind(f), BeforeHash: f.Before.Hash, AfterHash: expectedAfter(f).Hash})
	}
	for _, c := range p.Commands {
		r.Commands = append(r.Commands, textsafe.EscapeExcept(c, ""))
	}
	for _, in := range p.Interrupted {
		in.ID, in.Kind, in.CreatedAt, in.RestoreOf = safe(in.ID), safe(in.Kind), safe(in.CreatedAt), safe(in.RestoreOf)
		r.Interrupted = append(r.Interrupted, in)
	}
	for _, id := range p.LostBackups {
		r.LostBackups = append(r.LostBackups, safe(id))
	}
	for _, t := range p.StaleTemps {
		r.StaleTempFiles = append(r.StaleTempFiles, safe(t))
	}
	for _, n := range p.Notes {
		r.Notes = append(r.Notes, safeNote(n))
	}
	if withDiff {
		r.Diff = append([]FileDiff{}, p.Diffs...)
	}
	if res != nil {
		r.Mode, r.Applied = "apply", true
		for _, t := range res.CleanedTemps {
			r.CleanedTempFiles = append(r.CleanedTempFiles, safe(t))
		}
		for _, id := range res.Recovered {
			r.Recovered = append(r.Recovered, safe(id))
		}
		if res.BackupID != "" {
			r.Backup = &BackupRef{ID: res.BackupID, Path: safe(res.BackupPath)}
		}
		r.Files = r.Files[:0]
		for _, c := range res.Changed {
			c.Path = safe(c.Path)
			r.Files = append(r.Files, c)
		}
	}
	return r
}

// WriteJSON writes the report.
func WriteJSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// Mode lines of WriteTextAs for an --apply run whose plan was NOT applied
// (res == nil): the plain "plan only" line would be wrong there.
const (
	ModeApplyFailed     = "모드: 적용을 시도했으나 실패했습니다 (롤백 결과는 아래 오류 메시지를 보세요). 이 계획은 적용되지 않았습니다."
	ModeApplyNotApplied = "모드: --apply 였지만 이 대상은 적용하지 않았습니다 (파일을 쓰지 않음). 이유는 아래 오류 메시지·요약을 보세요."
)

// WriteText prints the plan as a human-readable table.
func WriteText(w io.Writer, p *Plan, res *ApplyResult, withDiff bool) {
	WriteTextAs(w, p, res, withDiff, "")
}

// WriteTextAs is WriteText with an explicit mode line for a plan that was not
// applied during --apply (ModeApplyFailed, ModeApplyNotApplied); "" = the default.
func WriteTextAs(w io.Writer, p *Plan, res *ApplyResult, withDiff bool, mode string) {
	r := BuildReport(p, res, withDiff)
	fmt.Fprintf(w, "forge sync: %s  scope=%s  root=%s\n", r.Target, r.Scope, r.Root)
	switch {
	case res != nil:
		fmt.Fprintln(w, "모드: 적용")
	case mode != "":
		fmt.Fprintln(w, mode)
	default:
		fmt.Fprintln(w, "모드: 계획만 출력합니다 (파일을 쓰지 않음). 적용하려면 --apply 를 지정하세요.")
	}
	for _, in := range r.Interrupted {
		what := "sync"
		if in.Kind == "restore" {
			what = "restore"
		}
		fmt.Fprintf(w, "경고: 이전 %s 가 중단되었습니다 (백업 %s, %s 시작). 파일 %d개 중 완료 %d · 대기 %d · 수정/중간 상태 %d.\n",
			what, in.ID, in.CreatedAt, in.Ops, in.Done, in.Todo, in.Changed)
		if in.Kind == "restore" {
			if in.RestoreOf != "" {
				fmt.Fprintf(w, "      복원은 자동으로 이어 하지 않습니다. 이어서 완료하려면 forge sync --restore %s --force 를 다시 실행하세요 (%s 는 복원 직전 상태의 안전 백업이라 latest 에서 제외됩니다).\n", in.RestoreOf, in.ID)
			} else {
				fmt.Fprintf(w, "      복원은 자동으로 이어 하지 않습니다. 원래 복원 명령(forge sync --restore <복원하던 백업 id> --force)을 다시 실행하세요. %s 는 복원 직전 상태의 안전 백업이라 이것을 복원하면 복원이 되돌려집니다.\n", in.ID)
			}
		}
	}
	for _, id := range r.LostBackups {
		fmt.Fprintf(w, "경고: 이전 sync 가 중단되었고 그 백업(%s)이 삭제되어 이어서 완료하거나 되돌릴 수 없습니다 (저널 사본만 남음).\n", id)
		fmt.Fprintln(w, "      그 sync 가 만든 항목은 충돌로 보입니다. 안전한 해소: --skip-conflicts (충돌 항목만 제외). --force-managed 는 이 충돌에 효과가 없습니다. 자세한 내용은 아래 참고.")
	}
	// Only an interrupted SYNC is finished by --apply; a restore is never resumed
	// automatically (its own line above says what to run), so the two statements
	// must not appear for the same interruption (TASK-0027 AC-19).
	syncs, restores := 0, 0
	for _, in := range r.Interrupted {
		if in.Kind == "restore" {
			restores++
		} else {
			syncs++
		}
	}
	if syncs > 0 {
		if res == nil {
			fmt.Fprintln(w, "      --apply 는 중단된 sync 를 현재 파일을 다시 계획해 이어서 완료합니다 (그 사이 수정한 파일은 건드리지 않고 충돌로 보고, 바꾸는 파일은 먼저 새 백업에 보존).")
		} else {
			fmt.Fprintln(w, "      이번 --apply 가 중단된 sync 를 이어서 완료했습니다.")
		}
	}
	if restores > 0 && res == nil {
		fmt.Fprintln(w, "      --apply 는 중단된 restore 를 이어 하지 않습니다 (레지스트리 기준으로 새로 계획한 sync 만 적용하고 이 중단 기록은 닫습니다). 복원을 끝내려면 위 forge sync --restore 명령을 먼저 실행하세요.")
	}
	if res == nil && len(r.StaleTempFiles) > 0 {
		fmt.Fprintf(w, "정리 예정: 중단된 sync 가 남긴 임시 파일 %d개 (--apply 시 삭제)\n", len(r.StaleTempFiles))
		for i, t := range r.StaleTempFiles {
			if i == 10 {
				fmt.Fprintf(w, "  … 외 %d개\n", len(r.StaleTempFiles)-10)
				break
			}
			fmt.Fprintf(w, "  %s\n", t)
		}
	}
	if res != nil && len(r.CleanedTempFiles) > 0 {
		fmt.Fprintf(w, "정리함: 중단된 sync 가 남긴 임시 파일 %d개\n", len(r.CleanedTempFiles))
	}
	if len(r.Interrupted) > 0 || len(r.StaleTempFiles) > 0 || len(r.LostBackups) > 0 {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	var rows []Item
	for _, it := range r.Items {
		if it.Action != ActionUnchanged {
			rows = append(rows, it)
		}
	}
	if len(rows) > 0 {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "종류\t이름\t작업\t이유")
		for _, it := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", kindLabel[it.Kind], cell(it.Name, maxNameCell), actionLabel[it.Action], cell(it.Reason, maxReasonCell))
		}
		_ = tw.Flush()
		if truncatedCells(rows) {
			// The cut text is often the way out (the advice ends the reason), so the
			// full text follows the table (display-limited), not only --json.
			fmt.Fprintln(w, "(긴 이름/이유는 표에서 줄였습니다. 줄인 항목의 전체 내용은 아래에 이어서 보이며 --json 으로도 볼 수 있습니다)")
			for _, it := range rows {
				if len([]rune(it.Name)) > maxNameCell || len([]rune(it.Reason)) > maxReasonCell {
					head := fmt.Sprintf("  - %s %s: ", kindLabel[it.Kind], cell(it.Name, maxNameCell))
					for i, ln := range wrapRunes(it.Reason, wrapWidth) {
						if i == 0 {
							fmt.Fprintln(w, head+ln)
						} else {
							fmt.Fprintln(w, "      "+ln)
						}
					}
				}
			}
		}
	}
	if len(p.Ops) == 0 && r.Conflicts == 0 {
		if len(rows) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "변경 없음: 쓸 파일이 없습니다 (레지스트리와 클라이언트 설정이 이미 일치합니다).")
	}
	if len(r.Files) > 0 {
		fmt.Fprintln(w)
		if res == nil {
			fmt.Fprintln(w, "쓸 파일:")
		} else {
			fmt.Fprintln(w, "변경한 파일:")
		}
		for _, f := range r.Files {
			fmt.Fprintf(w, "  %s  %s\n", changeLabel[f.Change], f.Path)
		}
	}
	if len(r.Commands) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "사용자가 직접 실행할 명령 (forge 는 실행하지 않으며 사용자 파일을 쓰지 않습니다):")
		for _, c := range r.Commands {
			fmt.Fprintf(w, "  %s\n", c)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "참고: %s\n", n)
	}
	if withDiff && len(r.Diff) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "diff (비밀 값은 *** 로 가림):")
		for _, d := range r.Diff {
			fmt.Fprint(w, d.Diff)
		}
	}
	fmt.Fprintln(w)
	keys := []Action{ActionAdd, ActionUpdate, ActionRemove, ActionConflict, ActionSkip, ActionUnchanged}
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", actionLabel[k], r.Summary[string(k)]))
	}
	fmt.Fprintf(w, "요약: %s\n", strings.Join(parts, " · "))
	if r.Conflicts > 0 {
		tampered := 0
		for _, it := range r.Items {
			if it.Action == ActionConflict && it.Tampered {
				tampered++
			}
		}
		if tampered < r.Conflicts {
			fmt.Fprintln(w, "충돌 항목은 사용자 소유이거나 손으로 수정된 항목이라 건드리지 않습니다.")
		}
		if tampered > 0 {
			fmt.Fprintln(w, "변조 의심 스킬 "+fmt.Sprint(tampered)+"건: "+TamperedSnapshotHelp)
		}
		if res == nil {
			fmt.Fprintln(w, "--apply 는 충돌이 하나라도 있으면 아무것도 쓰지 않습니다. 충돌을 해소하거나 --skip-conflicts 로 충돌 항목만 제외하고 적용하세요.")
		}
	}
	if res != nil && res.BackupID != "" {
		fmt.Fprintf(w, "백업: %s (되돌리기: forge sync --restore %s)\n", res.BackupID, res.BackupID)
	}
}

// WriteBackups prints --list-backups.
func WriteBackups(w io.Writer, list []BackupInfo) {
	if len(list) == 0 {
		fmt.Fprintln(w, "백업이 없습니다.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\t생성(UTC)\t종류\t대상\t범위\t상태\t파일\t루트")
	for _, b := range list {
		if b.Error != "" {
			fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t손상\t-\t%s\n", safe(b.ID), safe(b.Error))
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", safe(b.ID), safe(b.CreatedAt), safe(b.Kind), safe(b.Target), b.Scope, safe(b.Status), b.Files, safe(b.Root))
	}
	_ = tw.Flush()
}

// WriteRestore prints a restore (dry run) result.
func WriteRestore(w io.Writer, r *RestoreResult) {
	if r.DryRun {
		fmt.Fprintf(w, "복원 계획 (파일을 쓰지 않음): 백업 %s  대상=%s scope=%s\n", r.ID, safe(r.Target), r.Scope)
	} else {
		fmt.Fprintf(w, "복원: 백업 %s  대상=%s scope=%s\n", r.ID, safe(r.Target), r.Scope)
	}
	es := append([]RestoreEntry{}, r.Entries...)
	sort.Slice(es, func(i, j int) bool { return es[i].Path < es[j].Path })
	label := map[string]string{"restore": "원복", "delete": "삭제(백업 시점에 없던 파일)", "unchanged": "변경 없음"}
	for _, e := range es {
		drift := ""
		if e.Drift {
			drift = "  [경고: 백업 이후 수정됨]"
		}
		fmt.Fprintf(w, "  %s  %s%s\n", label[e.Action], safe(e.Path), drift)
	}
	if r.Drift > 0 {
		if r.NewBackupID != "" {
			fmt.Fprintf(w, "경고: 백업 이후 수정된 파일 %d개를 --force 로 되돌렸습니다 (수정본은 새 백업에 보존됨).\n", r.Drift)
		} else {
			fmt.Fprintf(w, "경고: 백업 이후 수정된 파일이 %d개 있습니다. 복원하려면 --force 가 필요합니다 (현재 상태는 새 백업으로 보존).\n", r.Drift)
		}
	}
	if r.NewBackupID != "" {
		fmt.Fprintf(w, "복원 직전 상태를 백업했습니다: %s (되돌리려면 forge sync --restore %s)\n", r.NewBackupID, r.NewBackupID)
	}
}
