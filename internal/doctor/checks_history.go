package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/heonjinjeong/forge/internal/adapter"
	"github.com/heonjinjeong/forge/internal/adapter/claudecode"
	"github.com/heonjinjeong/forge/internal/store"
)

const (
	bigLedgerRows  = 5_000_000
	bigDBBytes     = 1 << 30
	unknownRowsCap = 100 // gateway: unregistered-name rows per session (internal/gateway/call.go maxUnknownRows)
)

// checkHistory: hook activity, ledger size, unregistered-name cap.
func checkHistory(e *env) []Check {
	conn, err := e.database()
	if err != nil {
		return []Check{e.dbSkipped("history.skipped", "history", "이력", "이력")}
	}
	var out []Check
	out = append(out, e.hookActivity(conn)...)

	rows, approx := int64(0), false
	cctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	err = conn.QueryRowContext(cctx, `SELECT count(*) FROM ledger`).Scan(&rows)
	cancel()
	if err != nil {
		approx = true
		err = conn.QueryRowContext(e.ctx, `SELECT COALESCE(max(id),0) FROM ledger`).Scan(&rows)
	}
	var size int64
	if fi, serr := os.Stat(store.DBPath(e.o.Home)); serr == nil {
		size = fi.Size()
	}
	switch {
	case err != nil:
		out = append(out, unavailable("history.size", "history", "이력 크기", err))
	case rows > bigLedgerRows || size > bigDBBytes:
		out = append(out, mk("history.size", "history", "이력 크기", Warn, fmt.Sprintf("원장 %s%d행, DB %s", map[bool]string{true: "약 ", false: ""}[approx], rows, humanBytes(size)),
			"이력이 매우 커서 기간 없는 집계가 느립니다. 오래된 행을 정리하는 forge 명령은 아직 없습니다",
			"forge history --format csv --output ledger-backup.csv --since 365d   # 먼저 내보내기", "forge history summary --since 30d   # 기간을 좁혀 조회"))
	default:
		out = append(out, mk("history.size", "history", "이력 크기", OK, fmt.Sprintf("원장 %s%d행, DB %s", map[bool]string{true: "약 ", false: ""}[approx], rows, humanBytes(size)), ""))
	}

	var maxUnknown int64
	uctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	err = conn.QueryRowContext(uctx, `SELECT COALESCE(max(c),0) FROM (SELECT count(*) c FROM ledger WHERE source='gateway' AND found=0 GROUP BY session_id)`).Scan(&maxUnknown)
	cancel()
	switch {
	case err != nil && isCtx(err):
		out = append(out, mk("history.unknown", "history", "미등록 이름", Info, "미등록 이름 상한 점검이 시간 안에 끝나지 않아 건너뜁니다", "원장이 매우 큽니다"))
	case err != nil:
		out = append(out, unavailable("history.unknown", "history", "미등록 이름", err))
	case maxUnknown >= unknownRowsCap:
		out = append(out, mk("history.unknown", "history", "미등록 이름", Warn, fmt.Sprintf("게이트웨이 세션 하나가 미등록 이름 %d행을 기록해 세션당 상한(%d)에 도달했습니다", maxUnknown, unknownRowsCap),
			"클라이언트가 노출 목록에 없는 도구를 반복 호출했습니다 (상한 이후의 미등록 호출은 기록되지 않음). 프로필이 항목을 숨겼거나 설정이 오래되었을 수 있습니다",
			"forge history misses --since 7d", "forge sync --target all"+e.projFlag()+"   # 설정이 최신인지 확인"))
	default:
		out = append(out, mk("history.unknown", "history", "미등록 이름", OK, "세션당 미등록 이름 기록 상한에 도달한 세션이 없습니다", ""))
	}
	return out
}

// hookActivity: a claude-code hook is installed but recorded nothing recently
// (the hook may not be called: wrong binary path, client settings overridden,
// client never used since the install).
func (e *env) hookActivity(conn *sql.DB) []Check {
	var newest time.Time
	installed := false
	for _, p := range e.orderedPlans() {
		if p.target != claudecode.Name || p.state == nil {
			continue
		}
		for _, it := range p.state.Items {
			if it.Kind != adapter.KindHook {
				continue
			}
			installed = true
			if t, err := time.Parse(time.RFC3339, it.AppliedAt); err == nil && t.After(newest) {
				newest = t
			}
		}
	}
	if !installed {
		return nil
	}
	since := e.now.Add(-time.Duration(e.o.HookDays) * 24 * time.Hour)
	var n int64
	err := conn.QueryRowContext(e.ctx, `SELECT count(*) FROM ledger WHERE source='hook' AND ts >= ?`, since.UnixMilli()).Scan(&n)
	if err != nil {
		return []Check{unavailable("history.hook", "history", "hook 이벤트", err)}
	}
	if n > 0 {
		return []Check{mk("history.hook", "history", "hook 이벤트", OK, fmt.Sprintf("최근 %d일 hook 이벤트 %d건", e.o.HookDays, n), "")}
	}
	if !newest.IsZero() && newest.After(since) {
		return []Check{mk("history.hook", "history", "hook 이벤트", Info, fmt.Sprintf("hook 을 설치한 지 %d일이 안 되어 이벤트가 아직 없습니다", e.o.HookDays), "클라이언트에서 도구를 한 번 사용한 뒤 다시 확인하세요",
			"forge history --source hook --since "+fmt.Sprintf("%dd", e.o.HookDays))}
	}
	return []Check{mk("history.hook", "history", "hook 이벤트", Warn, fmt.Sprintf("claude-code hook 이 설치되어 있는데 최근 %d일 hook 이벤트가 0건입니다", e.o.HookDays),
		"hook 이 호출되지 않을 수 있습니다 (사용하지 않았거나, forge 경로가 바뀌었거나, 사용자 설정이 hook 을 덮어씀). 설치된 hook 의 forge 경로를 확인하세요",
		"forge history --source hook --since "+fmt.Sprintf("%dd", e.o.HookDays), "forge sync --target claude-code"+e.projFlag()+" --diff   # hook 항목과 경로 확인", "forge sync --target claude-code"+e.projFlag()+" --apply   # hook 갱신")}
}
