# 조회 API: `forge serve-api`

`forge export --to-postgres` 로 올린 집계(스키마 `forge`)를 **읽기 전용 HTTP API** 로 제공합니다. 대시보드(예: Vercel)가 서버 측에서 이 API 를 호출하는 구조를 전제로 합니다. 결정 배경과 위협 모델은 ADR-F0008 에 있습니다.

```
로컬 forge.db ─ forge export ─▶ Postgres(Supabase, 스키마 forge) ◀─ forge serve-api(Render) ◀─ 대시보드(서버 측)
```

## 라우트

GET/HEAD 만 받습니다. 그 밖의 메서드는 모든 경로에서 405(`Allow: GET, HEAD`)이고, 쓰기 라우트는 없습니다.

| 경로 | 인증 | 내용 |
|------|------|------|
| `GET /health` | 없음 | DB 를 쓰지 않고 `{"status":"ok","readOnly":true}` |
| `GET /health/db` | 없음 | `SELECT 1` 한 번. 실패하면 503 과 일반 메시지(`database unavailable`)만 |
| `GET /v1/summary` | Bearer | `meta` + `items` + `daily`(최근 366일) |
| `GET /v1/items?kind=skill\|tool\|mcp` | Bearer | 항목 집계(`kind` 생략 시 전체). 잘못된 값은 400 |
| `GET /v1/daily?days=N` | Bearer | 일별(UTC) 집계, `days` 는 1..366(기본 30). 범위 밖이면 400 |

응답 필드는 `forge export` 의 허용 목록과 같습니다([export.md](export.md)). 명령줄·경로·인자·세션·본문은 DB 에도 없고 응답 타입에도 없습니다. 아직 내보낸 스냅샷이 없으면 `/v1/summary` 는 404 입니다.

`/health/db` 는 토큰 없이 열려 있습니다. 한 번의 호출로 API 와 DB 를 모두 깨어 있게 하는 유휴 방지(keepalive) 용도입니다. DB 가 살아 있는지 여부만 알려 줍니다.

## 실행

```bash
export FORGE_EXPORT_URL='postgresql://<읽기 전용 역할>:<비밀번호>@<호스트>:5432/postgres'   # Session pooler, 6543 거부
export FORGE_PG_CA="$(cat prod-ca.crt)"      # 선택
export FORGE_API_TOKEN="$(openssl rand -hex 24)"
forge serve-api                              # 기본 127.0.0.1:8080
forge serve-api --addr 0.0.0.0:8080          # 컨테이너 등에서 명시적으로
```

- `FORGE_EXPORT_URL`·`FORGE_PG_CA` 는 `forge export` 와 같은 검증(`6543` 거부, 원격 TLS 필수, 허용되지 않는 URL 매개변수)을 씁니다.
- `FORGE_API_TOKEN` 이 비었거나 16자보다 짧으면 **시작하지 않습니다**(fail closed). DB 에 연결하기 전에 확인합니다.
- 포트: 환경변수 `PORT`(Render)가 있으면 `0.0.0.0:$PORT`, 없으면 `--addr`(기본 `127.0.0.1:8080`). `--addr` 를 직접 주면 `PORT` 보다 우선합니다. 기본값은 loopback 이라 0.0.0.0 바인딩은 `--addr` 나 `PORT` 를 지정했을 때만 일어납니다.
- SIGTERM/SIGINT 를 받으면 진행 중인 요청을 마친 뒤(최대 10초) 종료합니다.
- DB 연결은 요청마다 짧게 열고(동시 4개 제한), `BEGIN READ ONLY` 트랜잭션과 문 타임아웃(5초)에서 SELECT 만 실행합니다. 시작할 때는 DB 에 연결하지 않으므로 DB 가 내려가 있어도 `/health` 는 응답합니다. **전용 읽기 전용 DB 역할**(forge 스키마 SELECT 권한만)을 쓰는 것을 권장합니다. 앱이 읽기 전용 트랜잭션을 쓰더라도 역할 권한이 마지막 방어선입니다.

## 인증과 보안

- `Authorization: Bearer <토큰>` 헤더 하나만 인정합니다. 헤더가 없거나 둘 이상이거나, 스킴이 `Bearer`(대소문자 무시)가 아니거나, 토큰이 비었거나 512자를 넘으면 401(`WWW-Authenticate: Bearer`)입니다. 쿼리스트링·쿠키·다른 헤더의 토큰은 읽지 않습니다. 인증이 필요한 경로 아래의 알 수 없는 경로도 먼저 401 입니다.
- 토큰 비교는 양쪽을 SHA-256 으로 해시한 뒤 상수 시간(`subtle.ConstantTimeCompare`)으로 합니다. 길이도 드러나지 않습니다.
- 한계: 요청 URI 2048바이트, 헤더 8KB(`MaxHeaderBytes`), 요청 본문 불허(있으면 400), 응답 4MB, 항목 10000행·일별 5000행. 서버 타임아웃은 ReadHeader 5초, Read 10초, Write 15초, Idle 60초, 요청 처리 10초입니다.
- 모든 응답에 `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`, `Cache-Control: no-store` 를 붙입니다. **CORS 헤더는 없습니다**(`OPTIONS` 는 405). 토큰이 브라우저로 가지 않도록 대시보드의 서버 측에서만 호출하세요.
- 오류 응답은 일반 메시지(`unauthorized`, `database unavailable` 등)뿐입니다. 내부 오류 원문은 로그에만 남고, 로그에는 URL·비밀번호(`Target.Redact`)와 토큰을 `***` 로 바꿔 씁니다. 로그에는 메서드·경로·상태·소요 시간만 있고 쿼리스트링은 쓰지 않으며 경로는 이스케이프합니다.
- HTTPS 종료는 Render 같은 플랫폼 앞단이 합니다. 이 프로세스는 평문 HTTP 입니다.

## 배포(Render, Docker)

저장소 루트의 `Dockerfile` 과 `render.yaml`(Blueprint)을 씁니다.

- `Dockerfile`: 멀티스테이지(`golang` 빌드 → `distroless/static:nonroot`), `CGO_ENABLED=0`, 비루트 사용자(65532), 진입점 `forge serve-api`. 이미지에는 DB URL·토큰을 넣지 않고(실행 시 환경변수), `ENV PORT=8080` 만 기본값으로 둡니다(Render 가 자기 값으로 덮어씁니다).
- `render.yaml`: web 서비스, `runtime: docker`, `plan: free`, `region: singapore`, `healthCheckPath: /health`, `autoDeploy: true`. `FORGE_API_TOKEN` 은 자동 생성(`generateValue`), `FORGE_EXPORT_URL`·`FORGE_PG_CA` 는 `sync: false` 라 대시보드에서 직접 넣습니다.

1. Supabase Session pooler URL(가능하면 읽기 전용 역할)을 준비합니다([export.md](export.md)).
2. Render: **New → Blueprint** 로 저장소를 연결하고 `FORGE_EXPORT_URL`(선택 `FORGE_PG_CA`)을 입력합니다.
3. 서비스 Environment 의 `FORGE_API_TOKEN` 을 대시보드의 서버 측 환경변수로 복사합니다.
4. 확인: `curl https://<서비스>.onrender.com/health`, 토큰으로 `curl -H "Authorization: Bearer $TOKEN" .../v1/summary`.

Free 플랜은 15분 동안 요청이 없으면 잠들고 첫 요청이 느립니다. Supabase free 프로젝트도 활동이 없으면 일시 중지됩니다. `/health/db` 를 주기적으로 호출하면 둘 다 깨어 있게 할 수 있습니다(이 저장소는 그 호출기를 제공하지 않습니다).

## 검증 상태

- 자동 시험(`go test ./internal/serveapi ./internal/cli`): httptest 로 인증 우회 시도(헤더 없음·빈 토큰·스킴·긴 토큰·여러 Authorization 헤더·쿼리스트링·쿠키·경로 변형), 쓰기 메서드 405, 큰 URI·헤더·본문, 오류 일반화와 비밀 미노출(응답·로그), 짧은 토큰의 시작 거부, 응답 필드 허용 목록, `/health/db` 실패, 정상 종료, 쿼리가 SELECT 상수이고 읽기 전용 트랜잭션을 여는지, Dockerfile·render.yaml 정적 점검(멀티스테이지, 비루트, COPY 대상, ENTRYPOINT, 비밀 미포함).
- **실제 Docker 이미지 빌드는 하지 못했습니다(미확인).** `golang:1.27-alpine`·`gcr.io/distroless/static-debian12:nonroot` 태그의 존재와 빌드·실행도 확인하지 않았습니다. Render Blueprint 키는 공식 문서 기준이며 실제 배포로 확인하지 않았습니다.
- **실제 Postgres 에 연결한 시험은 하지 못했습니다(미확인).** `FORGE_PG_TEST_URL`(일회용 DB) 이 있을 때만 `TestPGStoreAgainstPostgres` 가 실행됩니다. 문 타임아웃과 읽기 전용 트랜잭션이 Supabase Session pooler 에서 동작하는지는 확인하지 않았습니다.
