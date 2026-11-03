package serveapi

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/as950118/forge/internal/export"
)

// SQL is constant text; values travel only as bound arguments. Every statement is a
// SELECT and runs inside BEGIN READ ONLY.
const (
	sqlTimeout = `SET LOCAL statement_timeout = '5s'`
	sqlPing    = `SELECT 1`
	sqlMeta    = `SELECT schema_version, generated_at, forge_version, day_basis FROM forge.meta WHERE id = 1`
	sqlItems   = `SELECT kind, server, name, calls, ok, failed, unknown, success_rate, last_call FROM forge.items WHERE ($1::text = '' OR kind = $1::text) ORDER BY calls DESC, kind, server, name LIMIT 10000`
	sqlDaily   = `SELECT day, kind, calls, ok, failed, unknown FROM forge.daily WHERE day > (now() AT TIME ZONE 'utc')::date - $1::int ORDER BY day, kind LIMIT 5000`
)

const maxConns = 4

// PGStore reads schema forge. It opens a short-lived connection per call (the
// service is idle most of the time and poolers drop idle sessions) and bounds the
// concurrent connections.
type PGStore struct {
	cfg *pgx.ConnConfig
	sem chan struct{}
}

// NewPGStore builds a store from a validated target; it does not connect.
func NewPGStore(t *export.Target) *PGStore {
	return &PGStore{cfg: t.Config, sem: make(chan struct{}, maxConns)}
}

func (p *PGStore) readTx(ctx context.Context, fn func(pgx.Tx) error) error {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	conn, err := pgx.ConnectConfig(ctx, p.cfg.Copy())
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Close(c)
	}()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if _, err := tx.Exec(ctx, sqlTimeout); err != nil {
		return err
	}
	return fn(tx)
}

// Ping runs SELECT 1 once.
func (p *PGStore) Ping(ctx context.Context) error {
	return p.readTx(ctx, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, sqlPing).Scan(&n)
	})
}

// Summary returns meta, all items and the last days of daily rows from one snapshot
// of the database (one read-only transaction).
func (p *PGStore) Summary(ctx context.Context, days int) (Summary, error) {
	var s Summary
	err := p.readTx(ctx, func(tx pgx.Tx) error {
		m, err := queryMeta(ctx, tx)
		if err != nil {
			return err
		}
		s.Meta = m
		if s.Items, err = queryItems(ctx, tx, ""); err != nil {
			return err
		}
		s.Daily, err = queryDaily(ctx, tx, days)
		return err
	})
	return s, err
}

// Items returns the items of one kind ("" = all).
func (p *PGStore) Items(ctx context.Context, kind string) (out []Item, err error) {
	err = p.readTx(ctx, func(tx pgx.Tx) error {
		out, err = queryItems(ctx, tx, kind)
		return err
	})
	return out, err
}

// Daily returns the daily rows of the last days UTC days.
func (p *PGStore) Daily(ctx context.Context, days int) (out []Day, err error) {
	err = p.readTx(ctx, func(tx pgx.Tx) error {
		out, err = queryDaily(ctx, tx, days)
		return err
	})
	return out, err
}

func queryMeta(ctx context.Context, tx pgx.Tx) (Meta, error) {
	var m Meta
	var gen time.Time
	var ver int32
	err := tx.QueryRow(ctx, sqlMeta).Scan(&ver, &gen, &m.ForgeVersion, &m.DayBasis)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNoSnapshot
	}
	m.SchemaVersion = int(ver)
	m.GeneratedAt = gen.UTC().Format(time.RFC3339)
	return m, err
}

func queryItems(ctx context.Context, tx pgx.Tx, kind string) ([]Item, error) {
	rows, err := tx.Query(ctx, sqlItems, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Item{}
	for rows.Next() {
		var it Item
		var last time.Time
		if err := rows.Scan(&it.Kind, &it.Server, &it.Name, &it.Calls, &it.OK, &it.Failed, &it.Unknown, &it.SuccessRate, &last); err != nil {
			return nil, err
		}
		it.LastCall = last.UTC().Format(time.RFC3339)
		out = append(out, it)
	}
	return out, rows.Err()
}

func queryDaily(ctx context.Context, tx pgx.Tx, days int) ([]Day, error) {
	rows, err := tx.Query(ctx, sqlDaily, int32(days))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Day{}
	for rows.Next() {
		var d Day
		var day time.Time
		if err := rows.Scan(&day, &d.Kind, &d.Calls, &d.OK, &d.Failed, &d.Unknown); err != nil {
			return nil, err
		}
		d.Day = day.Format("2006-01-02")
		out = append(out, d)
	}
	return out, rows.Err()
}
