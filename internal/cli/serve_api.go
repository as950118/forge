package cli

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/as950118/forge/internal/export"
	"github.com/as950118/forge/internal/serveapi"
)

// EnvAPIToken is the environment variable with the Bearer token of serve-api.
const EnvAPIToken = "FORGE_API_TOKEN"

const defaultAPIAddr = "127.0.0.1:8080"

func serveAPICmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve-api [--addr host:port]",
		Short: "Serve the exported snapshot as a read-only HTTP API (Postgres)",
		Long: `forge export --to-postgres 로 올린 집계(스키마 forge)를 읽기 전용 HTTP API 로 제공합니다. GET/HEAD 만 받고 쓰지 않습니다.

  GET /health            DB 없이 {"status":"ok","readOnly":true}
  GET /health/db         SELECT 1 한 번(토큰 없이 열림). 실패하면 503 과 일반 메시지
  GET /v1/summary        meta + items + daily        (Bearer 토큰 필요)
  GET /v1/items?kind=    skill|tool|mcp              (Bearer 토큰 필요)
  GET /v1/daily?days=N   최근 N일(1..366)            (Bearer 토큰 필요)

환경변수: FORGE_EXPORT_URL(필수, Session pooler URL, 6543 거부), FORGE_PG_CA(선택), FORGE_API_TOKEN(필수, 16자 이상).
토큰이 없거나 짧으면 시작하지 않습니다. 포트는 $PORT 가 있으면 0.0.0.0:$PORT, 없으면 --addr(기본 127.0.0.1:8080).
--addr 를 직접 주면 $PORT 보다 우선합니다. CORS 헤더는 없으며 서버 측 호출을 전제로 합니다.`,
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token := os.Getenv(EnvAPIToken)
			if len(token) < serveapi.MinTokenLen {
				return fmt.Errorf("환경변수 %s 가 비었거나 %d자보다 짧습니다. 토큰 없이는 시작하지 않습니다", EnvAPIToken, serveapi.MinTokenLen)
			}
			tgt, err := export.ParseTarget(os.Getenv(export.EnvURL), os.Getenv(export.EnvCA))
			if err != nil {
				return err
			}
			listen := addr
			if !cmd.Flags().Changed("addr") {
				if p := os.Getenv("PORT"); p != "" {
					n, err := strconv.Atoi(p)
					if err != nil || n < 1 || n > 65535 {
						return fmt.Errorf("환경변수 PORT 가 올바른 포트 번호가 아닙니다")
					}
					listen = net.JoinHostPort("0.0.0.0", p)
				}
			}
			errw := cmd.ErrOrStderr()
			logf := func(format string, a ...any) { fmt.Fprintf(errw, format+"\n", a...) }
			srv, err := serveapi.New(serveapi.Options{
				Store: serveapi.NewPGStore(tgt), Token: token, Redact: tgt.Redact, Logf: logf,
			})
			if err != nil {
				return err
			}
			ln, err := net.Listen("tcp", listen)
			if err != nil {
				return fmt.Errorf("%s 에서 대기할 수 없습니다: %v", listen, err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			logf("forge serve-api: %s 에서 대기 (읽기 전용, DB %s)", ln.Addr(), tgt.Masked)
			return serveapi.Run(ctx, ln, srv, logf)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultAPIAddr, "대기 주소 (기본 loopback; 컨테이너에서는 0.0.0.0:PORT 또는 $PORT)")
	return cmd
}
