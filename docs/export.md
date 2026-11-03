# 내보내기: `forge export`

로컬 SQLite 원장에서 **공개해도 되는 집계**만 뽑아 JSON 파일이나 Postgres(Supabase)로 내보냅니다. 읽기 전용 대시보드를 배포하기 위한 첫 단계이며, 로컬 CLI 와 `forge.db` 는 그대로입니다. 결정 배경과 위협 모델은 ADR-F0008(허용 목록, 6543 거부, 전체 교체, 위협 모델)에 정리되어 있습니다.

```
로컬 forge.db ──(읽기 전용)──▶ 집계 스냅샷 ──▶ JSON 파일 / Postgres 스키마 forge
```

## 무엇이 나가는가 (허용 목록)

| 포함 | 설명 |
|------|------|
| 항목 | 종류(`skill`/`tool`/`mcp`), MCP 서버 이름, 이름, 호출 수, 성공·실패·미관찰 수, 성공률(성공/(성공+실패), 관찰된 결과가 없으면 null), 최근 호출 시각(UTC) |
| 일별 | 종류별 UTC 날짜(`YYYY-MM-DD`)의 호출 수·성공·실패·미관찰. **일 단위는 UTC 기준**입니다(로컬 시간대가 아닙니다) |
| 메타 | 생성 시각(UTC), Forge 버전, 스냅샷 스키마 버전, 일 기준(`UTC`) |

**나가지 않는 것**: 명령줄, 경로, 인자, 환경변수, 헤더, 세션 ID, 프로젝트 경로, 클라이언트 이름·버전, 입출력 본문(payload), 오류 분류·메시지, 레지스트리의 명령·URL. 스냅샷은 필드를 하나씩 골라 담는 허용 목록 방식이라 원장에 새 열이 생겨도 저절로 나가지 않습니다.

이름은 그 자체로 공개됩니다. 제어·비가시 문자(줄바꿈, ESC, 양방향 제어, 영폭 문자 등)는 `\uXXXX`(BMP 밖은 `\UXXXXXXXX`, 잘못된 UTF-8 은 `\xNN`)로 바뀌고, 200자를 넘는 이름은 잘려 `...` 가 붙습니다. 바뀐 결과가 같아지는 이름은 한 항목으로 합칩니다. 비밀이 들어간 이름의 스킬·도구는 쓰지 마세요(Forge 는 이름 안의 비밀을 알아보지 못합니다).

호출 집계는 `forge history summary` 와 같은 병합 규칙(같은 호출을 hook·gateway 가 기록하면 1건, 결과는 실패 우선)을 씁니다.

## JSON

```bash
forge export --format json                       # 표준 출력
forge export --format json --output snap.json    # 새 파일만, 권한 0600 (덮어쓰기는 --force)
```

## Postgres (Supabase)

1. Supabase 프로젝트에서 **Connect → Session pooler** 연결 문자열을 복사합니다
   (`postgresql://postgres.<ref>:<비밀번호>@aws-0-<region>.pooler.supabase.com:5432/postgres`).
   **Transaction pooler(포트 6543)는 거부합니다.** 이 내보내기는 한 연결에서 여러 문장을 한 트랜잭션으로 실행하며 스키마를 만들기 때문입니다.
2. 환경변수를 지정합니다. URL 은 셸 기록에 남지 않게 파일·비밀 관리자에서 읽어 오세요.
   ```bash
   export FORGE_EXPORT_URL='postgresql://postgres.<ref>:<비밀번호>@aws-0-<region>.pooler.supabase.com:5432/postgres'
   export FORGE_PG_CA="$(cat prod-ca.crt)"     # 선택
   ```
   - `FORGE_PG_CA`: Supabase Database Settings → SSL Configuration 의 CA 인증서(PEM). 줄바꿈을 `\n` 으로 바꾼 한 줄도 됩니다. 있으면 서버 인증서와 호스트 이름을 검증합니다. 없으면 TLS 는 쓰지만 서버 인증서는 검증하지 않습니다(`sslmode=require` 와 같음). 원격 호스트에서는 TLS 가 필수이며 `sslmode=disable|allow|prefer` 는 거부합니다(loopback 은 예외).
   - URL 의 `host`, `hostaddr`, `service`, `passfile`, `sslrootcert`, `sslcert`, `sslkey`, `options` 매개변수는 지원하지 않습니다.
3. 먼저 dry-run 으로 대상과 행 수를 확인합니다. 연결하지 않으며 아무것도 쓰지 않습니다.
   ```bash
   forge export --to-postgres
   ```
4. 기록합니다.
   ```bash
   forge export --to-postgres --yes
   ```

스키마 `forge` 의 `meta`(1행), `items`, `daily` 테이블을 `IF NOT EXISTS` 로 만들고(행 수준 보안 활성화), **한 트랜잭션에서 기존 행을 모두 지운 뒤 새 스냅샷을 넣습니다**. 중간에 실패하면 전부 롤백되어 이전 내용이 그대로 남고, 같은 스냅샷을 다시 실행해도 결과가 같습니다(멱등). 스키마·테이블 이름은 상수이고 값은 모두 바인딩 인자로만 전달합니다. Supabase 의 `public` 스키마와 섞이지 않으며, `forge` 스키마는 API(PostgREST)에 노출하지 마세요.

URL 의 비밀번호는 출력과 오류 메시지에서 `xxxxx`/`***` 로 가려집니다.

## 검증 상태

- 자동 시험(`go test ./internal/export ./internal/cli`)은 스냅샷 내용, 가짜 비밀·명령줄이 JSON 과 INSERT 인자에 없음, UTC 일 경계, 이름 이스케이프, 6543 거부, 비밀번호 마스킹, dry-run 을 확인합니다.
- **실제 Postgres/Supabase 에 연결한 시험은 하지 못했습니다(미확인).** `FORGE_PG_TEST_URL`(일회용 데이터베이스) 이 있을 때만 `TestPostgresReplace` 가 실행되고(전체 교체·멱등·롤백), 없으면 건너뜁니다. Supabase 의 Session pooler·CA 동작은 문서 기준이며 실제로 확인하지 않았습니다.
