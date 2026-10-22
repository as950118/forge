package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/heonjinjeong/forge/internal/textsafe"
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

// Table cells are display-only: they are cut to a fixed number of characters so
// that one very long name cannot stretch the table across hundreds of columns.
// --json keeps the full (sanitized) strings.
const (
	maxNameCell   = 80
	maxReasonCell = 120
)

func cell(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
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
		it.Name, it.Reason, it.Path = safe(it.Name), safe(it.Reason), safe(it.Path)
		r.Items = append(r.Items, it)
	}
	for _, f := range p.Ops {
		r.Files = append(r.Files, FileChange{Path: safe(f.Path), Change: changeKind(f), BeforeHash: f.Before.Hash, AfterHash: expectedAfter(f).Hash})
	}
	for _, c := range p.Commands {
		r.Commands = append(r.Commands, textsafe.EscapeExcept(c, ""))
	}
	for _, in := range p.Interrupted {
		in.ID, in.Kind, in.CreatedAt = safe(in.ID), safe(in.Kind), safe(in.CreatedAt)
		r.Interrupted = append(r.Interrupted, in)
	}
	for _, t := range p.StaleTemps {
		r.StaleTempFiles = append(r.StaleTempFiles, safe(t))
	}
	for _, n := range p.Notes {
		r.Notes = append(r.Notes, safe(n))
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

// WriteText prints the plan as a human-readable table.
func WriteText(w io.Writer, p *Plan, res *ApplyResult, withDiff bool) {
	r := BuildReport(p, res, withDiff)
	fmt.Fprintf(w, "forge sync: %s  scope=%s  root=%s\n", r.Target, r.Scope, r.Root)
	if res == nil {
		fmt.Fprintln(w, "모드: 계획만 출력합니다 (파일을 쓰지 않음). 적용하려면 --apply 를 지정하세요.")
	} else {
		fmt.Fprintln(w, "모드: 적용")
	}
	for _, in := range r.Interrupted {
		what := "sync"
		if in.Kind == "restore" {
			what = "restore"
		}
		fmt.Fprintf(w, "경고: 이전 %s 가 중단되었습니다 (백업 %s, %s 시작). 파일 %d개 중 완료 %d · 대기 %d · 수정/중간 상태 %d.\n",
			what, in.ID, in.CreatedAt, in.Ops, in.Done, in.Todo, in.Changed)
		if in.Kind == "restore" {
			fmt.Fprintln(w, "      복원은 자동으로 이어 하지 않습니다. 같은 복원 명령(forge sync --restore <id> --force)을 다시 실행하세요.")
		}
	}
	if len(r.Interrupted) > 0 {
		if res == nil {
			fmt.Fprintln(w, "      --apply 는 현재 파일을 다시 계획해 이어서 완료합니다 (그 사이 수정한 파일은 건드리지 않고 충돌로 보고, 바꾸는 파일은 먼저 새 백업에 보존).")
		} else {
			fmt.Fprintln(w, "      이번 --apply 가 중단된 sync 를 이어서 완료했습니다.")
		}
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
	if len(r.Interrupted) > 0 || len(r.StaleTempFiles) > 0 {
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
			fmt.Fprintln(w, "(긴 이름/이유는 표에서 줄였습니다. 전체는 --json 으로 볼 수 있습니다)")
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
		fmt.Fprintln(w, "충돌 항목은 사용자 소유이거나 손으로 수정된 항목이라 건드리지 않습니다.")
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
