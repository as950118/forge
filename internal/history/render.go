package history

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/heonjinjeong/forge/internal/ledger"
	"github.com/heonjinjeong/forge/internal/textsafe"
)

// Output formats.
const (
	FormatTable = "table"
	FormatJSON  = "json"
	FormatCSV   = "csv"
)

// maxCell bounds one table cell (after escaping).
const maxCell = 240

// Report is one result ready to be written as a table, JSON or CSV.
type Report struct {
	Headers []string   // human table headers
	Human   [][]string // table rows (local time, raw DB strings: escaped on output)
	CSVHead []string
	CSV     [][]string // csv rows (UTC time, raw DB strings: escaped on output)
	JSON    any        // already masked
	Empty   string     // printed instead of an empty table
	Notes   []string   // lines printed under the table
}

// Write renders the report in the requested format.
func (r Report) Write(w io.Writer, format string) error {
	switch format {
	case FormatJSON:
		b, err := SafeJSON(r.JSON)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	case FormatCSV:
		return writeCSV(w, r.CSVHead, r.CSV)
	}
	if len(r.Human) == 0 {
		_, err := fmt.Fprintln(w, r.Empty)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(r.Headers, "\t"))
	for _, row := range r.Human {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = textsafe.Display(c, maxCell)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, n := range r.Notes {
		fmt.Fprintln(w, n)
	}
	return nil
}

// SafeJSON marshals v (indented) so that no terminal-active character is ever
// written raw: encoding/json already escapes C0 controls and U+2028/2029; DEL,
// C1 controls (including CSI), bidi controls, zero-width and other format
// characters are additionally written as \uXXXX escapes, which decode to the
// same string.
func SafeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	in := buf.Bytes()
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); {
		r, size := utf8.DecodeRune(in[i:])
		if r >= 0x7f && size > 0 && textsafe.NeedsEscape(r) {
			if r > 0xffff {
				r -= 0x10000
				out = append(out, fmt.Sprintf(`\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))...)
			} else {
				out = append(out, fmt.Sprintf(`\u%04x`, r)...)
			}
		} else {
			out = append(out, in[i:i+size]...)
		}
		i += size
	}
	return out, nil
}

// CSVCell neutralizes one cell: terminal-active characters other than tab, CR
// and LF become visible escapes, and a cell that starts with = + - @ TAB or CR
// gets a leading apostrophe so spreadsheets do not evaluate it as a formula
// (OWASP CSV injection guidance). Quoting is left to encoding/csv.
func CSVCell(s string) string {
	s = textsafe.EscapeExcept(s, "\t\r\n")
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		s = "'" + s
	}
	return s
}

func writeCSV(w io.Writer, head []string, rows [][]string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(head); err != nil {
		return err
	}
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			cells[i] = CSVCell(c)
		}
		if err := cw.Write(cells); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// mk masks inline credential patterns (token=..., password: ..., --token X, URL
// userinfo) with the ledger masker. The ledger holds metadata only, so this is
// defense in depth for names and paths; the whole-value entropy rule of
// ledger.MaskText is not used because it hides legitimate long names and
// temp-directory paths.
func mk(s string) string { return ledger.MaskInline(s) }

func localTime(ms int64, loc *time.Location) string {
	return time.UnixMilli(ms).In(loc).Format("2006-01-02 15:04:05 -0700")
}

func boolWord(b *bool, yes, no string) string {
	if b == nil {
		return ""
	}
	if *b {
		return yes
	}
	return no
}

func i64(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func displayName(server, name string) string {
	if server == "" {
		return name
	}
	return server + "::" + name
}

// MaskCall returns c with secret-looking text masked (for JSON/CSV/table).
func maskCall(c Call) Call {
	c.Name, c.MCPServer, c.Project = mk(c.Name), mk(c.MCPServer), mk(c.Project)
	return c
}

// ListReport builds the report of a call list.
func ListReport(calls []Call, limit int, raw bool, loc *time.Location) Report {
	r := Report{
		Headers: []string{"TIME", "KIND", "NAME", "CLIENT", "SOURCE", "RESULT", "LATENCY_MS", "PROJECT", "SESSION"},
		CSVHead: []string{"time_utc", "ts_ms", "id", "kind", "mcp_server", "name", "client", "sources", "ok", "error_class", "latency_ms", "found", "invocation", "project", "session_id"},
		Empty:   "조회된 호출이 없습니다.",
	}
	if raw {
		r.Headers = append(r.Headers, "TOOL_USE_ID")
		r.CSVHead = append(r.CSVHead, "tool_use_id")
	}
	masked := make([]Call, len(calls))
	for i, c := range calls {
		c = maskCall(c)
		masked[i] = c
		res := boolWord(c.OK, "ok", "failed")
		if c.OK != nil && !*c.OK && c.ErrorClass != "" {
			res += "(" + c.ErrorClass + ")"
		}
		src := strings.Join(c.Sources, ",")
		row := []string{localTime(c.TS, loc), c.Kind, displayName(c.MCPServer, c.Name), dash(c.Client), src, dash(res), dash(i64(c.LatencyMS)), dash(c.Project), dash(c.SessionID)}
		crow := []string{c.Time, strconv.FormatInt(c.TS, 10), strconv.FormatInt(c.ID, 10), c.Kind, c.MCPServer, c.Name, c.Client, src,
			boolWord(c.OK, "1", "0"), c.ErrorClass, i64(c.LatencyMS), boolWord(c.Found, "1", "0"), c.Invocation, c.Project, c.SessionID}
		if raw {
			row = append(row, dash(c.ToolUseID))
			crow = append(crow, c.ToolUseID)
		}
		r.Human = append(r.Human, row)
		r.CSV = append(r.CSV, crow)
	}
	if !raw {
		for i := range masked {
			masked[i].ToolUseID = ""
		}
	}
	r.JSON = map[string]any{"count": len(masked), "limit": limit, "raw": raw, "calls": masked}
	if len(calls) >= limit {
		r.Notes = []string{fmt.Sprintf("최근 %d건만 표시했습니다 (--limit 으로 조정). 시각은 로컬 시간대입니다.", limit)}
	} else {
		r.Notes = []string{"시각은 로컬 시간대입니다."}
	}
	return r
}

// SummaryReport builds the report of Summary/Top.
func SummaryReport(groups []Group, groupBy string, loc *time.Location) Report {
	cols := KeyCols(groupBy)
	head := make([]string, 0, len(cols)+7)
	for _, c := range cols {
		head = append(head, strings.ToUpper(c))
	}
	if groupBy == "day" {
		head[0] = "DAY(UTC)"
	}
	chead := append([]string{}, cols...)
	head = append(head, "CALLS", "OK", "FAILED", "UNKNOWN", "AVG_MS", "MAX_MS", "LAST")
	chead = append(chead, "calls", "ok", "failed", "unknown", "avg_latency_ms", "max_latency_ms", "last_time_utc", "last_ts_ms")
	r := Report{Headers: head, CSVHead: chead, Empty: "집계할 호출이 없습니다."}
	masked := make([]Group, len(groups))
	for i, g := range groups {
		g.Key = maskKey(g.Key)
		masked[i] = g
		avg := ""
		if g.AvgMS != nil {
			avg = strconv.FormatFloat(*g.AvgMS, 'f', 1, 64)
		}
		var key, ckey []string
		for _, c := range cols {
			key = append(key, dash(g.Key[c]))
			ckey = append(ckey, g.Key[c])
		}
		row := append(key, strconv.FormatInt(g.Calls, 10), strconv.FormatInt(g.OK, 10), strconv.FormatInt(g.Failed, 10),
			strconv.FormatInt(g.Unknown, 10), dash(avg), dash(i64(g.MaxMS)), localTime(g.LastTS, loc))
		r.Human = append(r.Human, row)
		r.CSV = append(r.CSV, append(ckey, strconv.FormatInt(g.Calls, 10), strconv.FormatInt(g.OK, 10), strconv.FormatInt(g.Failed, 10),
			strconv.FormatInt(g.Unknown, 10), avg, i64(g.MaxMS), g.LastTime, strconv.FormatInt(g.LastTS, 10)))
	}
	r.JSON = map[string]any{"group_by": groupBy, "count": len(masked), "groups": masked}
	r.Notes = []string{"같은 tool_use_id 로 여러 source 가 기록한 호출은 1건으로 병합해 셉니다. UNKNOWN = 결과를 관찰하지 못한 호출."}
	return r
}

func maskKey(k map[string]string) map[string]string {
	out := make(map[string]string, len(k))
	for c, v := range k {
		switch c {
		case "name", "mcp_server", "project":
			v = mk(v)
		}
		out[c] = v
	}
	return out
}

// UnusedReport builds the report of UnusedItems.
func UnusedReport(items []Unused, loc *time.Location) Report {
	r := Report{
		Headers: []string{"KIND", "NAME", "SCOPE", "STATE", "LAST_CALL"},
		CSVHead: []string{"kind", "name", "scope", "enabled", "last_time_utc", "last_ts_ms"},
		Empty:   "기간 내 호출이 없는 등록 항목이 없습니다.",
	}
	masked := make([]Unused, len(items))
	for i, u := range items {
		u.Name = mk(u.Name)
		masked[i] = u
		state := "활성"
		en := "1"
		if !u.Enabled {
			state, en = "비활성", "0"
		}
		last := "never"
		if u.LastTS != nil {
			last = localTime(*u.LastTS, loc)
		}
		r.Human = append(r.Human, []string{u.Kind, u.Name, u.Scope, state, last})
		r.CSV = append(r.CSV, []string{u.Kind, u.Name, u.Scope, en, u.Last, i64(u.LastTS)})
	}
	r.JSON = map[string]any{"count": len(masked), "unused": masked}
	r.Notes = []string{"LAST_CALL 은 기간과 무관한 마지막 호출 시각입니다(never = 한 번도 호출되지 않음). 비활성 항목은 STATE 로 구분합니다."}
	return r
}

// MissesReport builds the report of Misses. what names the table ("skill" or "history").
func MissesReport(ms []Miss, loc *time.Location) Report {
	r := Report{
		Headers: []string{"KIND", "SERVER", "UNREGISTERED_NAME", "COUNT", "FIRST", "LAST", "CLIENTS"},
		CSVHead: []string{"kind", "mcp_server", "name", "count", "first_time_utc", "first_ts_ms", "last_time_utc", "last_ts_ms", "clients"},
		Empty:   "미등록 호출 이름이 없습니다.",
	}
	masked := make([]Miss, len(ms))
	for i, m := range ms {
		m.Name, m.MCPServer = mk(m.Name), mk(m.MCPServer)
		cl := make(map[string]int, len(m.Clients))
		for k, v := range m.Clients {
			cl[mk(k)] += v
		}
		m.Clients = cl
		masked[i] = m
		cs := clientDist(cl)
		r.Human = append(r.Human, []string{m.Kind, dash(m.MCPServer), m.Name, strconv.FormatInt(m.Count, 10),
			localTime(m.FirstTS, loc), localTime(m.LastTS, loc), cs})
		r.CSV = append(r.CSV, []string{m.Kind, m.MCPServer, m.Name, strconv.FormatInt(m.Count, 10), m.First, strconv.FormatInt(m.FirstTS, 10),
			m.Last, strconv.FormatInt(m.LastTS, 10), cs})
	}
	r.JSON = map[string]any{"count": len(masked), "misses": masked}
	r.Notes = []string{
		"미등록 호출 이름: Forge 레지스트리에 없는 이름으로 호출된 기록(found=0)입니다. 도구 이름은 기록 시 정화·120바이트로 잘린 값이며 실제 도구명이 아닐 수 있습니다.",
		"사용자가 클라이언트에 직접 붙인 MCP 서버·내장 도구의 호출도 포함될 수 있습니다(거짓 양성).",
	}
	return r
}

func clientDist(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ":" + strconv.Itoa(m[k])
	}
	return strings.Join(parts, ",")
}
