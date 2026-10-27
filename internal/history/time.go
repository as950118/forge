// Package history implements the read-only queries behind `forge history` and
// `forge skill misses` (TASK-0009): filtered call lists, aggregates, unused
// registry items, miss counts and safe table/JSON/CSV rendering.
package history

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Now is replaced by tests.
var Now = time.Now

// maxRelative bounds a relative period (about 100 years) so the arithmetic
// cannot overflow.
const maxRelative = 100 * 366 * 24 * time.Hour

// ParseTime parses a --since/--until value into UTC milliseconds.
//
//	7d 24h 90m 30s 2w   relative: that long before now (units s m h d w, integer)
//	2026-10-01          a date: 00:00 at the start of that day in loc
//	RFC3339             2026-10-01T09:00:00+09:00, ...Z (fractions allowed)
//
// --since is inclusive and --until is exclusive.
func ParseTime(flag, v string, loc *time.Location) (int64, error) {
	raw := v
	v = strings.TrimSpace(v)
	bad := func(why string) error {
		return fmt.Errorf("--%s 값 %q 을(를) 해석할 수 없습니다: %s (예: 7d, 24h, 90m, 2026-10-01, 2026-10-01T09:00:00+09:00)", flag, trimShow(raw), why)
	}
	if v == "" {
		return 0, bad("빈 값")
	}
	if loc == nil {
		loc = time.Local
	}
	if last := v[len(v)-1]; (last >= 'a' && last <= 'z' || last >= 'A' && last <= 'Z') && relativeShape(v) {
		num, unit := v[:len(v)-1], v[len(v)-1]
		var d time.Duration
		switch unit {
		case 's':
			d = time.Second
		case 'm':
			d = time.Minute
		case 'h':
			d = time.Hour
		case 'd':
			d = 24 * time.Hour
		case 'w':
			d = 7 * 24 * time.Hour
		default:
			return 0, bad("알 수 없는 단위 " + strconv.Quote(string(unit)) + " (s, m, h, d, w 만 가능)")
		}
		if num == "" || len(num) > 9 {
			return 0, bad("숫자가 올바르지 않습니다")
		}
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil || n < 0 {
			return 0, bad("0 이상의 정수여야 합니다")
		}
		if time.Duration(n) > maxRelative/d {
			return 0, bad("기간이 너무 큽니다")
		}
		return Now().Add(-time.Duration(n) * d).UnixMilli(), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, loc); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UnixMilli(), nil
	}
	return 0, bad("기간(숫자+s|m|h|d|w), 날짜(YYYY-MM-DD), RFC3339 중 하나여야 합니다")
}

// relativeShape reports a value that looks like a relative period: an optional sign
// and digits followed by exactly one letter ("7d", "90m", "5x"). Anything else that
// ends in a letter ("foo", "2026-10-01T09:00:00Z") is not a relative period and is
// reported as a format error (or parsed as an absolute time).
func relativeShape(v string) bool {
	body := strings.TrimPrefix(v[:len(v)-1], "-")
	for _, c := range body {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func trimShow(s string) string {
	if len(s) > 40 {
		s = s[:40] + "..."
	}
	// The value is echoed in an error message: never let it carry controls.
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return '?'
		}
		return r
	}, s)
}

func msToTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}
