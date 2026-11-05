# 배포: Supabase + Render + Vercel

로컬의 Forge 이력 집계를 읽기 전용 웹 대시보드로 볼 수 있게 배포하는 절차입니다. 로컬 CLI 와 SQLite(`forge.db`)는 그대로이며, 클라우드에는 **집계와 이름만** 올라갑니다.

```
 로컬 forge ── forge export --to-postgres ──▶ Supabase (Postgres, 스키마 forge)
                                                    ▲
 Vercel (web/, 대시보드) ──서버 측, Bearer──▶ Render (forge serve-api, 읽기 전용) ┘
        ▲
   브라우저 (HTTP Basic: DASHBOARD_PASSWORD)
```

- **Render** 는 조회 API 만 제공합니다([docs/serve-api.md](docs/serve-api.md)). 쓰기 라우트가 없고 `FORGE_API_TOKEN` 없이는 시작하지 않습니다.
- **Vercel** 의 함수가 Render API 를 서버에서 호출합니다. API 토큰은 브라우저로 가지 않습니다.
- 대시보드 자체도 `DASHBOARD_PASSWORD` 가 없으면 모든 요청을 503 으로 거부합니다.

## 1. Supabase

1. 프로젝트를 만든 뒤 **Connect → Session pooler** 연결 문자열을 복사합니다.
   - 형식: `postgresql://postgres.<ref>:<비밀번호>@aws-0-<region>.pooler.supabase.com:5432/postgres`
   - **Transaction pooler(포트 6543)는 쓰지 않습니다.** 내보내기는 한 연결에서 여러 문장을 한 트랜잭션으로 실행하고 스키마를 만들기 때문에 transaction pooler 와 맞지 않습니다. Forge 는 6543 을 시작할 때 거부합니다.
   - Direct 연결은 IPv6 전용이라 Render 나 일부 로컬 네트워크에서 접속되지 않을 수 있습니다.
2. 테이블은 `forge` 스키마(`meta`, `items`, `daily`)에 생기고 행 수준 보안(RLS)이 켜져 있습니다. Supabase 의 `public` 스키마와 섞이지 않습니다. `forge` 스키마를 Data API(PostgREST)에 노출하지 마세요.
3. **SSL CA:** Supabase 서버 인증서는 Supabase 자체 CA 로 서명됩니다. Database Settings → SSL Configuration 에서 CA 인증서(PEM)를 받아 환경변수 `FORGE_PG_CA` 에 넣으면 서버 인증서와 호스트 이름을 검증합니다. 줄바꿈을 `\n` 으로 바꾼 한 줄도 됩니다. 없으면 TLS 는 쓰지만 서버 인증서는 검증하지 않습니다.
4. **용량과 일시 중지:** Free 프로젝트는 용량 제한이 있고 활동이 없으면 일시 중지됩니다. Forge 집계는 작지만(항목·일별 행만), 일시 중지를 피하려면 아래 keepalive 를 켜세요.
5. 가능하면 `forge` 스키마 SELECT 권한만 가진 **읽기 전용 DB 역할**을 따로 만들어 Render 에 쓰세요. 쓰기용 URL 은 로컬 내보내기에만 씁니다.

## 2. 로컬에서 데이터 넣기

```bash
export DB_INDEX='postgresql://postgres.<ref>:<비밀번호>@aws-0-<region>.pooler.supabase.com:5432/postgres'
export FORGE_PG_CA="$(cat prod-ca.crt)"      # 선택

forge export --to-postgres                   # dry-run: 대상(비밀번호 마스킹)과 행 수만 표시, 연결하지 않음
forge export --to-postgres --yes             # 한 트랜잭션으로 전체 교체(멱등, 실패하면 롤백)
```

이력이 바뀔 때마다 `--yes` 를 다시 실행하면 최신 스냅샷으로 교체됩니다. 자세한 내용은 [docs/export.md](docs/export.md) 입니다.

## 3. Render (API)

저장소 루트의 `render.yaml`(Blueprint)과 `Dockerfile` 을 씁니다. Docker 빌드, free 플랜, Singapore 리전, health check `/health`, push 시 자동 배포입니다.

1. **New → Blueprint** 로 이 저장소를 연결합니다.
2. 입력란에 `DB_INDEX`(1단계 Session pooler URL, 가능하면 읽기 전용 역할)과 선택 `FORGE_PG_CA` 를 넣습니다.
3. `FORGE_API_TOKEN` 은 Blueprint 가 자동 생성합니다. 서비스 **Environment** 탭에서 값을 복사해 4단계의 Vercel 환경변수로 넣습니다.

**무료 플랜 제약:** 15분 동안 요청이 없으면 서비스가 잠들고 첫 요청이 느립니다. Supabase free 프로젝트도 일시 중지됩니다.

**유휴 방지:** `.github/workflows/keepalive.yml` 이 10분마다 `GET /health/db` 를 호출합니다. 이 라우트는 토큰 없이 열려 있고 `SELECT 1` 한 번만 실행하므로 한 번의 호출로 Render 와 Supabase 를 함께 깨웁니다. 저장소 **Settings → Secrets and variables → Actions → Variables** 에 `FORGE_API_URL`(예: `https://forge-api.onrender.com`)을 넣어야 동작하고, 없으면 건너뜁니다. 비밀은 쓰지 않고 권한은 `permissions: {}` 입니다.

## 4. Vercel (대시보드)

**Add New Project** → 이 저장소 → **Root Directory: `web`**. 프레임워크는 "Other", 빌드 명령 없음, npm 의존성 없음입니다. `web/vercel.json` 이 함수 리전을 Singapore(`sin1`)로 고정합니다.

| 환경변수 | 값 |
|---|---|
| `FORGE_API_URL` | Render 서비스 URL, 예: `https://forge-api.onrender.com` (https 만 허용, 끝에 경로 없음) |
| `FORGE_API_TOKEN` | Render 의 `FORGE_API_TOKEN` 과 같은 값 |
| `DASHBOARD_PASSWORD` | **필수, 12자 이상.** 없거나 짧으면 모든 요청이 503 입니다 |
| `DASHBOARD_USER` | 선택. HTTP Basic 사용자명, 기본값 `forge` |

`FORGE_API_TOKEN` 과 `DASHBOARD_PASSWORD` 는 **Sensitive** 로 저장하세요. 환경변수를 바꾼 뒤에는 재배포해야 반영됩니다.

### 구조와 선택 근거

- 모든 경로(정적 파일 포함)를 `vercel.json` rewrites 로 한 함수(`web/api/index.js`)에 보내고, 함수가 인증을 통과한 요청에만 파일(`web/api/_static/`)과 데이터를 줍니다. 정적 자산을 `public/` 에 두면 Vercel 이 인증 없이 직접 서빙하므로 두지 않았습니다.
- Edge Middleware 방식은 쓰지 않았습니다. 프레임워크 없는 프로젝트에서 의존성 없이 동작하는지 이 환경에서 확인하지 못했기 때문입니다(미확인).
- 데이터 경로는 `/data/summary`, `/data/items?kind=`, `/data/daily?days=` 이며 Render 의 `/v1/*` JSON 을 그대로 전달합니다. 매개변수는 검증한 뒤에만 넘기고, 업스트림 타임아웃은 8초, 응답 상한은 4MB 이며 오류는 `upstream_error`·`upstream_timeout`·`no_data`·`not_configured` 같은 일반 코드만 돌려줍니다.
- 모든 응답에 `nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Cache-Control: private, no-store`, CSP(`default-src 'none'`, 스크립트·스타일은 같은 출처의 파일만)가 붙습니다. 화면의 동적 텍스트는 `textContent` 로만 넣습니다.

## 5. 배포 후 확인

```bash
API=https://forge-api.onrender.com
WEB=https://<프로젝트>.vercel.app

curl -s  "$API/health"                                  # {"status":"ok","readOnly":true}
curl -s  "$API/health/db"
curl -s  -H "Authorization: Bearer $FORGE_API_TOKEN" "$API/v1/summary" | head -c 300

curl -s -o /dev/null -w '%{http_code}\n' "$WEB/"                                  # 401
curl -s -o /dev/null -w '%{http_code}\n' "$WEB/package.json"                      # 401 (정적 파일도 인증 앞)
curl -s -o /dev/null -w '%{http_code}\n' -u "forge:$DASHBOARD_PASSWORD" "$WEB/"   # 200
curl -s -u "forge:$DASHBOARD_PASSWORD" "$WEB/data/summary" | head -c 300
```

`DASHBOARD_PASSWORD` 를 설정하지 않은 상태에서는 `$WEB/` 가 503 이어야 합니다. 응답 어디에도 `FORGE_API_TOKEN` 값이 보이면 안 됩니다.

## 6. 보안 주의

- **공개 범위는 집계와 이름뿐입니다.** 종류, 이름(MCP 는 서버 이름 포함), 호출·성공·실패 수, 성공률, 최근 호출 시각, 일별 합계, 생성 시각, Forge 버전. 명령줄·경로·인자·세션·본문은 DB 에도 API 에도 없습니다.
- **이름에 비밀이 들어가면 그대로 나갑니다.** Forge 는 이름 안의 비밀을 알아보지 못합니다. 비밀이 들어간 이름의 스킬·도구·MCP 서버는 쓰지 마세요.
- 대시보드 비밀번호는 필수이며 기본값이 없습니다. 추측하기 어려운 12자 이상 값을 쓰세요. 인증은 HTTP Basic 이므로 로그인 시도 제한이 없습니다(긴 무작위 비밀번호로 보완).
- **토큰 회전:** Render 서비스 Environment 에서 `FORGE_API_TOKEN` 을 새 값(`openssl rand -hex 24`)으로 바꾸고 저장·재배포한 뒤, 같은 값을 Vercel 에 넣고 재배포합니다. 그 사이 대시보드는 잠깐 오류 상태가 됩니다. `DASHBOARD_PASSWORD` 도 Vercel 에서 바꾸고 재배포합니다. DB 비밀번호가 새어 나갔다면 Supabase 에서 재설정하고 `DB_INDEX` 을 갱신합니다.
- 비밀은 저장소에 커밋하지 마세요. 이 저장소의 `render.yaml`·`vercel.json`·워크플로우에는 비밀 값이 없습니다.

## 7. 로컬 시연 (클라우드 없이 loopback 에서)

Supabase·Render·Vercel 없이 대시보드 화면과 인증을 확인합니다. 아래는 모두 `127.0.0.1` 에서만 열고, 가짜 스냅샷을 씁니다. Node 22 이상이 필요하며 `npm install` 은 필요 없습니다.

```bash
cd web && node --test        # 또는 저장소 루트에서 make test-web

mkdir -p /tmp/forge-web-demo && cd /tmp/forge-web-demo
cat > fake-api.mjs <<'JS'
import http from 'node:http';
const snap = {
  meta: { schema_version: 1, generated_at: new Date().toISOString(), forge_version: 'demo', day_basis: 'UTC' },
  items: [
    { kind: 'skill', server: '', name: 'demo-skill', calls: 5, ok: 4, failed: 1, unknown: 0, success_rate: 0.8, last_call: new Date().toISOString() },
    { kind: 'mcp', server: 'demo', name: 'echo', calls: 2, ok: 2, failed: 0, unknown: 0, success_rate: 1, last_call: new Date().toISOString() },
  ],
  daily: [{ day: new Date().toISOString().slice(0, 10), kind: 'skill', calls: 5, ok: 4, failed: 1, unknown: 0 }],
};
http.createServer((req, res) => {
  if (req.headers.authorization !== 'Bearer demo-token-not-a-secret') { res.statusCode = 401; return res.end('{}'); }
  res.setHeader('content-type', 'application/json');
  res.end(JSON.stringify(req.url.startsWith('/v1/summary') ? snap : []));
}).listen(8099, '127.0.0.1');
JS
cat > web-demo.mjs <<'JS'
import http from 'node:http';
const { default: handler } = await import(process.env.FORGE_WEB_DIR + '/api/index.js');
http.createServer(handler).listen(3000, '127.0.0.1');
JS

node fake-api.mjs &
FORGE_WEB_DIR=<저장소 경로>/web NODE_ENV=development FORGE_API_URL=http://127.0.0.1:8099 \
  FORGE_API_TOKEN=demo-token-not-a-secret DASHBOARD_PASSWORD=demo-password-123 node web-demo.mjs &

curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:3000/                              # 401
curl -s -u forge:demo-password-123 http://127.0.0.1:3000/data/summary | head -c 200
# 브라우저에서 http://127.0.0.1:3000/ 을 열고 forge / demo-password-123 로 로그인
```

끝나면 백그라운드로 띄운 두 프로세스를 종료하세요(`jobs` 와 `kill %1 %2`). `http://` 업스트림은 `NODE_ENV=development` 와 loopback 주소일 때만 허용됩니다. 실제 Postgres 가 있으면 `forge serve-api` 를 같은 방식으로 loopback 에 띄워 `FORGE_API_URL` 을 그쪽으로 지정할 수도 있습니다.

## 8. 확인하지 못한 것 (미확인)

이 환경에서는 네트워크·`vercel` CLI·Docker 를 쓰지 않았습니다. 아래는 문서와 로컬 자동 시험 기준이며 실제로 확인하지 않았습니다.

- 실제 Supabase 프로젝트 연결(Session pooler, CA 검증, 일시 중지 동작)과 실제 Postgres 에 대한 내보내기·조회
- Docker 이미지 빌드와 Render Blueprint 배포, 무료 플랜의 잠듦·깨움 동작
- Vercel 배포: `vercel.json` rewrites·`includeFiles`·`sin1` 리전 적용, rewrite 뒤 `req.url` 의 형태(함수는 `?p=` 와 원래 경로를 모두 처리하도록 작성), 루트 정적 파일이 함수 인증 앞에 서빙되는지 여부(5단계의 `package.json` 401 확인으로 점검), 환경변수 반영
- keepalive 워크플로우의 실제 실행(GitHub Actions)
- 실제 브라우저에서의 화면 표시(레이아웃·다크모드·모바일)
