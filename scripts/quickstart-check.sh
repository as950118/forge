#!/usr/bin/env bash
# Runs the commands of docs/guide/quickstart.md (then the two client quickstarts)
# for real and prints how long they took.
#
#   make quickstart-check                        (builds dist/forge first)
#   FORGE_BIN=dist/forge bash scripts/quickstart-check.sh
#
# What it does: extracts every ```bash block of the guides (except blocks
# preceded by a "<!-- qc:skip -->" comment line) and runs them, unchanged and in
# order, in ONE bash session inside an isolated temp directory:
#   - HOME, FORGE_HOME, TMPDIR and the project all live under one mktemp root
#   - claude/opencode are fake scripts that record argv and answer only --version;
#     real install locations (~/.local/bin, ~/.opencode/bin) are removed from PATH
#   - after every block the script re-checks that HOME/FORGE_HOME are still inside
#     the temp root and aborts otherwise
#   - at the end it verifies: real ~/.forge unchanged (stat only), decoy
#     ~/.claude.json byte-identical, fake clients only ever got `--version`,
#     go.mod/go.sum unchanged
# It never runs the real claude/opencode, calls a model or uses the network
# (the build uses the local Go module cache).
#
# LIMIT: the printed time is the SUM OF COMMAND RUN TIMES only (reading time and
# typing are not counted; build time depends on the Go cache). A human reading and
# pasting the guide is estimated at about 10 minutes. Blocks preceded by
# "<!-- qc:skip -->" are NOT executed: they change user settings, block on stdin
# (forge mcp serve), delete the practice directories (the cleanup block, which
# was run by hand once), or are placeholders such as <repo URL>. This does not show that real Claude Code/OpenCode load the generated
# configuration (not verified).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "$HERE/.." && pwd -P)"
REAL_HOME="${HOME:-}"
GUIDES=("docs/guide/quickstart.md" "docs/guide/quickstart-claude-code.md" "docs/guide/quickstart-opencode.md")

for need in python3 git go make; do
  command -v "$need" >/dev/null 2>&1 || { echo "중단: $need 가 필요합니다" >&2; exit 2; }
done

# Pin the Go caches before HOME changes (the guide's `make build` needs them).
GOMODCACHE_PIN="$(go env GOMODCACHE)"
GOCACHE_PIN="$(go env GOCACHE)"

hash_cmd() { if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$@"; else sha256sum "$@"; fi; }
real_forge_stat() {
  if [ -e "$REAL_HOME/.forge" ]; then
    if stat -f '%m %N' "$REAL_HOME/.forge" "$REAL_HOME/.forge/forge.db" 2>/dev/null; then :; else stat -c '%Y %n' "$REAL_HOME/.forge" "$REAL_HOME/.forge/forge.db" 2>/dev/null || true; fi
  else
    echo "(실제 ~/.forge 없음)"
  fi
}
REAL_STAT_BEFORE="$(real_forge_stat)"
GOMOD_BEFORE="$(cd "$REPO_ROOT" && git diff --stat -- go.mod go.sum | wc -l | tr -d ' ')"


# The temp root stays under /tmp (not $TMPDIR) so the run is short-path and
# reproducible; long random paths such as the macOS default TMPDIR work too (the
# secret heuristic no longer mistakes file-path arguments for secrets, TASK-0026).
# Short, TMPDIR-independent root (override with FORGE_QC_TMPDIR).
QC_BASE="${FORGE_QC_TMPDIR:-/tmp}"
[ -d "$QC_BASE" ] || { echo "중단: 임시 디렉터리 기준 경로가 없습니다: $QC_BASE" >&2; exit 2; }
ROOT="$(mktemp -d "$QC_BASE/forge-qc.XXXXXX")"
ROOT="$(cd "$ROOT" && pwd -P)"
chmod 700 "$ROOT"

cleanup() {
  local rc=$?
  if [ -n "${ROOT:-}" ] && [ -d "$ROOT" ] && [ "$(basename "$ROOT" | cut -c1-9)" = "forge-qc." ]; then
    if [ "${KEEP:-}" = 1 ]; then
      echo "KEEP=1: 임시 디렉터리를 보존했습니다: $ROOT"
    else
      chmod -R u+w "$ROOT" 2>/dev/null || true
      rm -rf "$ROOT"
    fi
  fi
  exit $rc
}
trap cleanup EXIT

mkdir -p "$ROOT/home" "$ROOT/fh" "$ROOT/tmp" "$ROOT/fakebin"
chmod 700 "$ROOT/fh" "$ROOT/tmp" "$ROOT/fakebin"
HOME_DIR="$ROOT/home"

# Fake clients: record argv, answer only a lone --version.
make_fake() {
  local n="$1"
  printf '%s\n' "$2" > "$ROOT/fakebin/.$n.version"
  cat > "$ROOT/fakebin/$n" <<EOF
#!/bin/sh
printf 'argc=%s argv=%s\n' "\$#" "\$*" >> "$ROOT/fake-$n.log"
if [ "\$#" -eq 1 ] && [ "\$1" = "--version" ]; then cat "$ROOT/fakebin/.$n.version"; exit 0; fi
echo "fake $n: unsupported arguments" >&2
exit 2
EOF
  chmod 755 "$ROOT/fakebin/$n"
  : > "$ROOT/fake-$n.log"
}
make_fake claude '2.1.305 (Claude Code)'
make_fake opencode '2.0.24'

# PATH without any entry that contains a real claude/opencode, fakes first.
CLEAN_PATH=""
OLD_IFS="$IFS"; IFS=':'
for d in $PATH; do
  case "$d" in /*) ;; *) continue ;; esac
  if [ -x "$d/claude" ] || [ -x "$d/opencode" ]; then continue; fi
  CLEAN_PATH="${CLEAN_PATH:+$CLEAN_PATH:}$d"
done
IFS="$OLD_IFS"

export HOME="$HOME_DIR"
export FORGE_HOME="$ROOT/fh"
export TMPDIR="$ROOT/tmp"
export FORGE_QS_TMP="$ROOT/tmp"
export GOMODCACHE="$GOMODCACHE_PIN" GOCACHE="$GOCACHE_PIN"
export PATH="$ROOT/fakebin:$CLEAN_PATH"
unset FORGE_BIN || true

case "$HOME/" in "$ROOT"/*) ;; *) echo "중단: HOME 이 임시 디렉터리 밖입니다" >&2; exit 2 ;; esac
case "$FORGE_HOME/" in "$ROOT"/*) ;; *) echo "중단: FORGE_HOME 이 임시 디렉터리 밖입니다" >&2; exit 2 ;; esac
if [ "$HOME" = "$REAL_HOME" ] || [ "$FORGE_HOME" = "$REAL_HOME/.forge" ]; then echo "중단: 실제 홈을 가리킵니다" >&2; exit 2; fi
case "$(command -v claude)" in "$ROOT"/fakebin/claude) ;; *) echo "중단: claude 가 가짜가 아닙니다" >&2; exit 2 ;; esac
case "$(command -v opencode)" in "$ROOT"/fakebin/opencode) ;; *) echo "중단: opencode 가 가짜가 아닙니다" >&2; exit 2 ;; esac

# Decoy ~/.claude.json inside the temp HOME (must stay byte-identical).
printf '{"oauthAccount":{"token":"FAKE-DECOY-SECRET-0000"}}\n' > "$HOME/.claude.json"
chmod 600 "$HOME/.claude.json"
DECOY_BEFORE="$(hash_cmd "$HOME/.claude.json" | awk '{print $1}')"

# Build the combined script from the guides' runnable ```bash blocks.
PLAN="$ROOT/blocks.tsv"
RUNNER="$ROOT/runner.sh"
python3 -I - "$REPO_ROOT" "$ROOT" "${GUIDES[@]}" > "$PLAN" <<'PYEOF'
import os, re, sys
repo, root, guides = sys.argv[1], sys.argv[2], sys.argv[3:]
runner = open(os.path.join(root, "runner.sh"), "w", encoding="utf-8")
runner.write('''#!/usr/bin/env bash
set -eo pipefail
__qc_now() { python3 -I -c 'import time; print("%.3f" % time.time())'; }
__qc_guard() {
  case "$HOME/" in "$QC_ROOT"/*) ;; *) echo "중단: HOME 이 임시 디렉터리 밖입니다: $HOME" >&2; exit 97 ;; esac
  case "$FORGE_HOME/" in "$QC_ROOT"/*) ;; *) echo "중단: FORGE_HOME 이 임시 디렉터리 밖입니다: $FORGE_HOME" >&2; exit 97 ;; esac
}
cd "$QC_REPO"
''')
fence = re.compile(r"^(\s*)(`{3,})\s*([A-Za-z0-9_+-]*)\s*$")
n = 0
for g in guides:
    path = os.path.join(repo, g)
    if not os.path.isfile(path):
        print("MISSING\t%s\t0" % g)
        continue
    lines = open(path, encoding="utf-8").read().split("\n")
    in_f, lang, body, start, prev = False, "", [], 0, ""
    skip = False
    for i, line in enumerate(lines, 1):
        m = fence.match(line)
        if not in_f:
            if m and m.group(3):
                in_f, lang, body, start = True, m.group(3).lower(), [], i
                skip = prev.strip() == "<!-- qc:skip -->"
            elif m:
                in_f, lang, body, start = True, "", [], i
                skip = True
            if not m and line.strip():
                prev = line
        else:
            if m and not m.group(3):
                in_f = False
                if lang == "bash" and not skip:
                    n += 1
                    runner.write('echo; echo "---- [%s] 블록 %d (%s:%d) ----"\n' % (g, n, g, start))
                    runner.write("cat <<'__QC_SHOW__'\n" + "\n".join(body) + "\n__QC_SHOW__\n")
                    runner.write("__t0=$(__qc_now)\n")
                    runner.write("\n".join(body) + "\n")
                    runner.write("__t1=$(__qc_now)\n__qc_guard\n")
                    runner.write('printf "BLOCK\\t%s\\t%d\\t%s\\n" "' + g + '" ' + str(n) + ' "$(python3 -I -c \'import sys; print("%.3f" % (float(sys.argv[2]) - float(sys.argv[1])))\' "$__t0" "$__t1")" >> "$QC_TIMES"\n')
                    print("BLOCK\t%s\t%d" % (g, n))
                prev = ""
            else:
                body.append(line)
runner.close()
PYEOF

if grep -q '^MISSING' "$PLAN"; then
  echo "중단: 가이드 파일이 없습니다:" >&2
  grep '^MISSING' "$PLAN" >&2
  exit 2
fi
NBLOCKS="$(grep -c '^BLOCK' "$PLAN" || true)"
echo "quickstart-check: 격리 환경 $ROOT (종료 시 삭제)"
echo "quickstart-check: 실행할 bash 블록 ${NBLOCKS}개 (가이드: ${GUIDES[*]})"

export QC_ROOT="$ROOT" QC_REPO="$REPO_ROOT" QC_TIMES="$ROOT/times.tsv"
: > "$QC_TIMES"
START=$(python3 -I -c 'import time; print("%.3f" % time.time())')
RC=0
bash "$RUNNER" || RC=$?
END=$(python3 -I -c 'import time; print("%.3f" % time.time())')

fail=0
bad() { echo "[FAIL] $*"; fail=1; }
ok() { echo "[OK]   $*"; }

echo
echo "================ 검사 ================"
if [ "$RC" = 0 ]; then ok "모든 문서 명령이 종료 코드 0 으로 끝남"; else bad "문서 명령 실행 중 실패 (종료 코드 $RC)"; fi

# Timing summary: quickstart.md only is the "5-minute path".
TIMING_RC=0
python3 -I - "$QC_TIMES" <<'PYEOF' || TIMING_RC=$?
import sys, collections
tot = collections.OrderedDict()
cnt = collections.Counter()
for line in open(sys.argv[1], encoding="utf-8"):
    p = line.rstrip("\n").split("\t")
    if len(p) == 4:
        tot[p[1]] = tot.get(p[1], 0.0) + float(p[3])
        cnt[p[1]] += 1
for g, t in tot.items():
    print("  %-40s 블록 %2d개  명령 실행 시간 합계 %.1f초" % (g, cnt[g], t))
q = tot.get("docs/guide/quickstart.md", 0.0)
print("  quickstart.md 명령 실행 시간 합계: %.1f초 (읽고 입력하는 시간 제외, 빌드 포함; 사람이 따라 하면 보통 10분 안팎으로 추정)" % q)
sys.exit(0 if 0 < q < 300 else 1)
PYEOF
if [ "$TIMING_RC" = 0 ]; then ok "quickstart.md 명령 실행 시간 합계가 300초 미만"; else bad "quickstart.md 명령 실행 시간이 300초 이상이거나 측정되지 않음"; fi

TOTAL_S=$(python3 -I -c 'import sys; print("%.1f" % (float(sys.argv[2]) - float(sys.argv[1])))' "$START" "$END")
echo "  전체 실행 시간(가이드 3개, 검사 제외): ${TOTAL_S}초"

REAL_STAT_AFTER="$(real_forge_stat)"
if [ "$REAL_STAT_AFTER" = "$REAL_STAT_BEFORE" ]; then ok "실제 ~/.forge 수정 시각 불변 (stat 만 사용)"; else bad "실제 ~/.forge 가 바뀌었습니다"; fi
DECOY_AFTER="$(hash_cmd "$HOME/.claude.json" | awk '{print $1}')"
if [ "$DECOY_AFTER" = "$DECOY_BEFORE" ]; then ok "미끼 ~/.claude.json 불변"; else bad "미끼 ~/.claude.json 이 바뀌었습니다"; fi
BAD_ARGV="$(cat "$ROOT/fake-claude.log" "$ROOT/fake-opencode.log" | grep -vc '^argc=1 argv=--version$' || true)"
if [ "$BAD_ARGV" = 0 ]; then
  ok "가짜 클라이언트는 --version 외 인자를 받지 않음 (claude $(wc -l < "$ROOT/fake-claude.log" | tr -d ' ')회, opencode $(wc -l < "$ROOT/fake-opencode.log" | tr -d ' ')회)"
else
  bad "가짜 클라이언트가 --version 외 인자를 받음"
fi
GOMOD_AFTER="$(cd "$REPO_ROOT" && git diff --stat -- go.mod go.sum | wc -l | tr -d ' ')"
if [ "$GOMOD_AFTER" = "$GOMOD_BEFORE" ]; then ok "go.mod/go.sum 변경 없음"; else bad "go.mod/go.sum 이 바뀌었습니다"; fi

if [ "$fail" != 0 ]; then echo "quickstart-check: 실패"; exit 1; fi
echo "quickstart-check: 통과"
