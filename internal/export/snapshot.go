// Package export builds the public, aggregate-only snapshot of the local ledger
// (TASK-0036, ADR-F0008) and writes it to JSON or to a Postgres database.
//
// The snapshot is an allow list: it is assembled field by field from names, kinds,
// counts, success rates and timestamps. Nothing else of the ledger (command lines,
// paths, arguments, env, headers, session ids, project paths, payloads, error
// messages) is read into it, so a new ledger column can never leak by accident.
package export

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/as950118/forge/internal/history"
	"github.com/as950118/forge/internal/textsafe"
)

// SchemaVersion is the version of the snapshot layout (JSON and Postgres).
const SchemaVersion = 1

// DayBasis documents that daily buckets are UTC calendar days.
const DayBasis = "UTC"

// maxNameRunes bounds a published name (longer names are cut, marked with "...").
const maxNameRunes = 200

// Snapshot is everything that leaves the machine.
type Snapshot struct {
	SchemaVersion int      `json:"schema_version"`
	GeneratedAt   string   `json:"generated_at"` // UTC RFC3339
	ForgeVersion  string   `json:"forge_version"`
	DayBasis      string   `json:"day_basis"` // always "UTC"
	Items         []Item   `json:"items"`
	Daily         []DayRow `json:"daily"`
}

// Item is the aggregate of one skill, tool or MCP tool (MCP: with its server name).
type Item struct {
	Kind        string   `json:"kind"`   // skill|tool|mcp
	Server      string   `json:"server"` // MCP server name ('-' folded to '_'); "" otherwise
	Name        string   `json:"name"`
	Calls       int64    `json:"calls"`
	OK          int64    `json:"ok"`
	Failed      int64    `json:"failed"`
	Unknown     int64    `json:"unknown"`      // result not observed
	SuccessRate *float64 `json:"success_rate"` // ok/(ok+failed), 0..1, null when no result was observed
	LastCall    string   `json:"last_call"`    // UTC RFC3339
}

// DayRow is the aggregate of one kind on one UTC day.
type DayRow struct {
	Day     string `json:"day"` // YYYY-MM-DD, UTC
	Kind    string `json:"kind"`
	Calls   int64  `json:"calls"`
	OK      int64  `json:"ok"`
	Failed  int64  `json:"failed"`
	Unknown int64  `json:"unknown"`
}

// Build reads the ledger through the existing read-only history queries and
// returns the snapshot. It has no side effects; now and version are inputs so
// that the result is deterministic in tests.
func Build(ctx context.Context, db *sql.DB, now time.Time, version string) (Snapshot, error) {
	s := Snapshot{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		ForgeVersion:  SafeName(version),
		DayBasis:      DayBasis,
		Items:         []Item{},
		Daily:         []DayRow{},
	}
	groups, err := history.Summary(ctx, db, history.Filter{}, "name", math.MaxInt32)
	if err != nil {
		return Snapshot{}, fmt.Errorf("이름별 집계를 읽을 수 없습니다: %w", err)
	}
	for _, g := range groups {
		it := Item{
			Kind:     g.Key["kind"],
			Server:   SafeName(g.Key["mcp_server"]),
			Name:     SafeName(g.Key["name"]),
			Calls:    g.Calls,
			OK:       g.OK,
			Failed:   g.Failed,
			Unknown:  g.Unknown,
			LastCall: msUTC(g.LastTS),
		}
		if g.OK+g.Failed > 0 {
			r := math.Round(float64(g.OK)/float64(g.OK+g.Failed)*10000) / 10000
			it.SuccessRate = &r
		}
		s.Items = append(s.Items, it)
	}
	s.Items = mergeItems(s.Items)
	for _, kind := range history.Kinds {
		days, err := history.Summary(ctx, db, history.Filter{Kind: kind}, "day", math.MaxInt32)
		if err != nil {
			return Snapshot{}, fmt.Errorf("일별 집계를 읽을 수 없습니다: %w", err)
		}
		for _, g := range days {
			s.Daily = append(s.Daily, DayRow{Day: g.Key["day"], Kind: kind, Calls: g.Calls, OK: g.OK, Failed: g.Failed, Unknown: g.Unknown})
		}
	}
	return s, nil
}

func msUTC(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

// SafeName makes a name publishable: control, bidirectional, zero-width and other
// invisible characters become \uXXXX (\UXXXXXXXX above the BMP), invalid UTF-8
// bytes become \xNN, and names longer than 200 characters are cut. The result
// contains only visible characters.
func SafeName(s string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); {
		if n >= maxNameRunes {
			b.WriteString("...")
			break
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02X`, s[i])
		case textsafe.NeedsEscape(r):
			if r > 0xFFFF {
				fmt.Fprintf(&b, `\U%08X`, r)
			} else {
				fmt.Fprintf(&b, `\u%04X`, r)
			}
		default:
			b.WriteRune(r)
		}
		i += size
		n++
	}
	return b.String()
}

// mergeItems folds items whose published (kind, server, name) became equal (two
// raw names that differ only in characters SafeName rewrites, or that were cut at
// the same length) so the key is unique in every sink. Order is kept (first seen).
func mergeItems(in []Item) []Item {
	type key struct{ kind, server, name string }
	idx := map[key]int{}
	out := make([]Item, 0, len(in))
	for _, it := range in {
		k := key{it.Kind, it.Server, it.Name}
		i, dup := idx[k]
		if !dup {
			idx[k] = len(out)
			out = append(out, it)
			continue
		}
		m := &out[i]
		m.Calls += it.Calls
		m.OK += it.OK
		m.Failed += it.Failed
		m.Unknown += it.Unknown
		if it.LastCall > m.LastCall {
			m.LastCall = it.LastCall
		}
		m.SuccessRate = nil
		if m.OK+m.Failed > 0 {
			r := math.Round(float64(m.OK)/float64(m.OK+m.Failed)*10000) / 10000
			m.SuccessRate = &r
		}
	}
	return out
}
