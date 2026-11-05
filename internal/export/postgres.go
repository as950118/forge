package export

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Env variable names of the Postgres export.
const (
	EnvURL = "DB_INDEX"    // postgresql://user:password@host:5432/db (Session pooler)
	EnvCA  = "FORGE_PG_CA" // optional PEM of the server CA ("\n" may stand for newlines)
)

// SchemaName is the Postgres schema that holds the tables (a constant, never input).
const SchemaName = "forge"

// Stmt is one SQL statement with its bound arguments. Values only ever travel as
// arguments; the SQL text consists of constants.
type Stmt struct {
	SQL  string
	Args []any
}

var ddl = []string{
	`CREATE SCHEMA IF NOT EXISTS forge`,
	`CREATE TABLE IF NOT EXISTS forge.meta (
  id             smallint PRIMARY KEY CHECK (id = 1),
  schema_version integer NOT NULL,
  generated_at   timestamptz NOT NULL,
  forge_version  text NOT NULL,
  day_basis      text NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS forge.items (
  kind         text NOT NULL CHECK (kind IN ('skill','tool','mcp')),
  server       text NOT NULL,
  name         text NOT NULL,
  calls        bigint NOT NULL,
  ok           bigint NOT NULL,
  failed       bigint NOT NULL,
  unknown      bigint NOT NULL,
  success_rate double precision,
  last_call    timestamptz NOT NULL,
  PRIMARY KEY (kind, server, name)
)`,
	`CREATE TABLE IF NOT EXISTS forge.daily (
  day     date NOT NULL,
  kind    text NOT NULL CHECK (kind IN ('skill','tool','mcp')),
  calls   bigint NOT NULL,
  ok      bigint NOT NULL,
  failed  bigint NOT NULL,
  unknown bigint NOT NULL,
  PRIMARY KEY (day, kind)
)`,
	`ALTER TABLE forge.meta ENABLE ROW LEVEL SECURITY`,
	`ALTER TABLE forge.items ENABLE ROW LEVEL SECURITY`,
	`ALTER TABLE forge.daily ENABLE ROW LEVEL SECURITY`,
	`DELETE FROM forge.meta`,
	`DELETE FROM forge.items`,
	`DELETE FROM forge.daily`,
}

// Plan returns the statements of a full replacement, in order. It is pure.
func Plan(s Snapshot) ([]Stmt, error) {
	gen, err := time.Parse(time.RFC3339, s.GeneratedAt)
	if err != nil {
		return nil, fmt.Errorf("생성 시각 형식 오류: %w", err)
	}
	out := make([]Stmt, 0, len(ddl)+1+len(s.Items)+len(s.Daily))
	for _, q := range ddl {
		out = append(out, Stmt{SQL: q})
	}
	out = append(out, Stmt{
		SQL:  `INSERT INTO forge.meta (id, schema_version, generated_at, forge_version, day_basis) VALUES (1, $1, $2, $3, $4)`,
		Args: []any{int32(s.SchemaVersion), gen, s.ForgeVersion, s.DayBasis},
	})
	for _, it := range s.Items {
		last, err := time.Parse(time.RFC3339, it.LastCall)
		if err != nil {
			return nil, fmt.Errorf("최근 호출 시각 형식 오류: %w", err)
		}
		var rate any // NULL when no result was observed
		if it.SuccessRate != nil {
			rate = *it.SuccessRate
		}
		out = append(out, Stmt{
			SQL:  `INSERT INTO forge.items (kind, server, name, calls, ok, failed, unknown, success_rate, last_call) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			Args: []any{it.Kind, it.Server, it.Name, it.Calls, it.OK, it.Failed, it.Unknown, rate, last},
		})
	}
	for _, d := range s.Daily {
		day, err := time.Parse("2006-01-02", d.Day)
		if err != nil {
			return nil, fmt.Errorf("일자 형식 오류: %w", err)
		}
		out = append(out, Stmt{
			SQL:  `INSERT INTO forge.daily (day, kind, calls, ok, failed, unknown) VALUES ($1, $2, $3, $4, $5, $6)`,
			Args: []any{day, d.Kind, d.Calls, d.OK, d.Failed, d.Unknown},
		})
	}
	return out, nil
}

// Target is a validated connection target.
type Target struct {
	Config *pgx.ConnConfig
	Masked string // URL with the password replaced; the only form that is printed
	secret []string
}

// Redact removes the password (raw and URL-escaped) and the whole URL from text.
func (t *Target) Redact(text string) string { return redactAll(text, t.secret) }

func redactAll(text string, secrets []string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}

// MaskURL returns raw with the password replaced by "xxxxx". An unparsable value
// is not echoed at all.
func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(해석할 수 없는 URL)"
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	q := u.Query()
	for _, k := range []string{"password", "sslpassword"} {
		if q.Has(k) {
			q.Set(k, "xxxxx")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ErrTransactionPooler is returned for port 6543 (Supabase transaction pooler).
var ErrTransactionPooler = errors.New("포트 6543(transaction pooler)은 사용할 수 없습니다. Supabase 의 Session pooler(포트 5432) 연결 문자열을 쓰세요")

// ParseTarget validates the URL and builds the pgx configuration without
// connecting. Only URL form is accepted; port 6543 is refused (in the authority and
// in a port= query parameter); TLS is required except for loopback hosts. With a
// CA the server certificate is verified against it (host name included).
func ParseTarget(rawURL, caPEM string) (*Target, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("환경변수 %s 가 비어 있습니다 (postgresql://사용자:비밀번호@호스트:5432/postgres, Supabase Session pooler)", EnvURL)
	}
	t := &Target{secret: secretsOf(rawURL)}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return nil, fmt.Errorf("%s 는 postgresql://... 형식의 URL 이어야 합니다", EnvURL)
	}
	t.Masked = MaskURL(rawURL)
	if strings.Contains(u.Host, ",") {
		return nil, errors.New("호스트를 둘 이상 지정할 수 없습니다")
	}
	q := u.Query()
	if u.Port() == "6543" || q.Get("port") == "6543" {
		return nil, ErrTransactionPooler
	}
	for _, k := range []string{"host", "hostaddr", "service", "passfile", "sslrootcert", "sslcert", "sslkey", "options"} {
		if q.Has(k) {
			return nil, fmt.Errorf("URL 의 %s 매개변수는 지원하지 않습니다", k)
		}
	}
	loopback := isLoopback(u.Hostname())
	switch mode := q.Get("sslmode"); mode {
	case "":
		if !loopback {
			q.Set("sslmode", "require")
		}
	case "disable", "allow", "prefer":
		if !loopback {
			return nil, fmt.Errorf("sslmode=%s 는 원격 호스트에서 쓸 수 없습니다 (TLS 필수)", mode)
		}
	}
	u.RawQuery = q.Encode()
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("연결 URL 을 해석할 수 없습니다: %s", t.Redact(err.Error()))
	}
	if int(cfg.Port) == 6543 {
		return nil, ErrTransactionPooler
	}
	if strings.TrimSpace(caPEM) != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(strings.ReplaceAll(caPEM, `\n`, "\n"))) {
			return nil, fmt.Errorf("%s 에서 PEM 인증서를 읽을 수 없습니다", EnvCA)
		}
		cfg.TLSConfig = &tls.Config{RootCAs: pool, ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
		cfg.Fallbacks = nil
	}
	cfg.ConnectTimeout = 15 * time.Second
	t.Config = cfg
	return t, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func secretsOf(rawURL string) []string {
	secrets := []string{rawURL}
	if u, err := url.Parse(rawURL); err == nil {
		if u.User != nil {
			if p, ok := u.User.Password(); ok && p != "" {
				secrets = append(secrets, p, url.QueryEscape(p), url.PathEscape(p))
			}
		}
		q := u.Query()
		for _, k := range []string{"password", "sslpassword"} {
			if v := q.Get(k); v != "" {
				secrets = append(secrets, v, url.QueryEscape(v))
			}
		}
	}
	return secrets
}

// Write replaces the content of schema forge with the snapshot in ONE transaction:
// on any error nothing changes; running it twice leaves the same rows (idempotent).
func Write(ctx context.Context, t *Target, s Snapshot) (err error) {
	stmts, err := Plan(s)
	if err != nil {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, t.Config)
	if err != nil {
		return fmt.Errorf("연결할 수 없습니다: %s", t.Redact(err.Error()))
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("트랜잭션을 시작할 수 없습니다: %s", t.Redact(err.Error()))
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.Background())
		}
	}()
	for _, st := range stmts {
		if _, err = tx.Exec(ctx, st.SQL, st.Args...); err != nil {
			return fmt.Errorf("기록 실패(전체 롤백): %s", t.Redact(err.Error()))
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("커밋 실패: %s", t.Redact(err.Error()))
	}
	return nil
}
