#!/usr/bin/env python3
"""JSON-RPC client that drives `forge mcp serve` the way an MCP client would.

Usage: gw_call.py <forge-binary> <project-dir>

It pretends to be Claude Code (clientInfo.name = "claude-code", no elicitation
capability), lists tools, calls a few of them, then runs `forge profile use`
as an EXTERNAL process in the middle of the same session to show that the
gateway follows profile changes without a restart. Standard library only.
Exit code 0 when every check passed, 1 otherwise.
"""
import json
import signal
import subprocess
import sys

forge, proj = sys.argv[1], sys.argv[2]
# Hard stop so a hung gateway can never hang the demo (host load can be high).
signal.alarm(240)
failures = 0
_next_id = 0


def check(label, cond, detail=""):
    global failures
    if cond:
        print("  [OK]   " + label)
    else:
        failures += 1
        print("  [FAIL] " + label + (" -> " + detail if detail else ""))


proc = subprocess.Popen(
    [forge, "mcp", "serve", "--client", "claude-code"],
    cwd=proj, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
    stderr=subprocess.DEVNULL, text=True, bufsize=1,
)


def send(msg):
    proc.stdin.write(json.dumps(msg) + "\n")
    proc.stdin.flush()


def rpc(method, params=None):
    global _next_id
    _next_id += 1
    send({"jsonrpc": "2.0", "id": _next_id, "method": method, "params": params or {}})
    while True:
        line = proc.stdout.readline()
        if not line:
            raise RuntimeError("gateway closed stdout")
        msg = json.loads(line)
        if msg.get("id") == _next_id and "method" not in msg:
            return msg


def text_of(resp):
    res = resp.get("result", {})
    return "".join(c.get("text", "") for c in res.get("content", []))


def tool_names():
    return sorted(t["name"] for t in rpc("tools/list")["result"]["tools"])


def run_forge(*args):
    out = subprocess.run([forge, *args], capture_output=True, text=True)
    return out.returncode, out.stdout.strip()


try:
    print("-- initialize (clientInfo=claude-code, no elicitation capability)")
    init = rpc("initialize", {
        "protocolVersion": "2025-06-18",
        "capabilities": {},
        "clientInfo": {"name": "claude-code", "version": "2.1.305"},
    })
    print("  server:", init["result"]["serverInfo"])
    send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    print("-- tools/list with profile 'work'")
    names = tool_names()
    for n in names:
        print("    " + n)
    # forge_asker is a confirm=ask tool, forge_greet is confirm=none
    for want in ("forge_greet", "forge_asker", "forge_skill", "forge_list"):
        check("노출됨: " + want, want in names)
    # gw-mcp is a gateway-exposed MCP server: '-' becomes '_' -> gw_mcp__<tool>
    check("게이트웨이 프록시 도구: gw_mcp__echo, gw_mcp__add", "gw_mcp__echo" in names and "gw_mcp__add" in names)
    check("direct 노출 서버 demo-mcp 는 게이트웨이에 프록시되지 않음 (demo_mcp__echo 없음)",
          "demo_mcp__echo" not in names)

    # pin-mcp: `forge mcp pin --enforce` + a definition changed after approval.
    check("도구 정의 고정: 승인된 pin_mcp__add 는 노출", "pin_mcp__add" in names)
    check("도구 정의 고정: 승인 후 바뀐 pin_mcp__echo 는 숨김", "pin_mcp__echo" not in names)
    print("-- tools/call forge_list (숨긴 이유가 경고로 표시되는지)")
    r = rpc("tools/call", {"name": "forge_list", "arguments": {}})
    listing = text_of(r)
    for line in listing.splitlines():
        if "enforce" in line:
            print("  ->", line.strip()[:200])
    check("forge_list 에 도구 정의 고정으로 숨긴 도구 안내", "도구 정의 고정(enforce)" in listing)
    r = rpc("tools/call", {"name": "pin_mcp__echo", "arguments": {"text": "x"}})
    check("숨겨진 pin_mcp__echo 를 직접 호출해도 거부",
          r.get("error") is not None or r.get("result", {}).get("isError") is True)

    print("-- tools/call gw_mcp__add {a: 2, b: 3} (게이트웨이가 mock MCP 서버로 프록시)")
    r = rpc("tools/call", {"name": "gw_mcp__add", "arguments": {"a": 2, "b": 3}})
    print("  ->", text_of(r).strip())
    check("프록시 호출 결과 5", text_of(r).strip() == "5")

    print("-- tools/call forge_greet {name: world}")
    r = rpc("tools/call", {"name": "forge_greet", "arguments": {"name": "world"}})
    print("  ->", text_of(r).strip())
    check("forge_greet 이 'hello world' 를 반환", "hello world" in text_of(r))

    print("-- tools/call forge_greet with injection attempt")
    r = rpc("tools/call", {"name": "forge_greet", "arguments": {"name": "; touch PWNED"}})
    print("  ->", text_of(r).strip())
    check("인젝션 문자열이 데이터로만 출력됨", "hello ; touch PWNED" in text_of(r))

    print("-- tools/call forge_skill {name: demo-skill}")
    r = rpc("tools/call", {"name": "forge_skill", "arguments": {"name": "demo-skill"}})
    body = text_of(r)
    print("  ->", body.strip().replace("\n", " | ")[:200])
    check("스킬 본문 조회 성공", "hello body" in body)

    print("-- tools/call forge_skill {name: no-such-skill} (miss)")
    r = rpc("tools/call", {"name": "forge_skill", "arguments": {"name": "no-such-skill"}})
    print("  ->", text_of(r).strip().replace("\n", " | ")[:200], "| isError=", r["result"].get("isError"))
    check("없는 스킬은 오류 결과", r["result"].get("isError") is True)

    print("-- tools/call forge_asker (confirm=ask, 클라이언트가 elicitation 미지원)")
    r = rpc("tools/call", {"name": "forge_asker", "arguments": {"name": "x"}})
    print("  ->", text_of(r).strip().replace("\n", " | ")[:200], "| isError=", r["result"].get("isError"))
    check("확인 불가라 실행 거부", r["result"].get("isError") is True and "hi x" not in text_of(r))

    print("-- 외부 프로세스로 프로필 전환: forge profile use minimal (같은 게이트웨이 세션 유지)")
    rc, out = run_forge("profile", "use", "minimal", "--client", "all", "--project-dir", proj)
    print("  rc=%d" % rc)
    check("profile use minimal 성공", rc == 0)
    names2 = tool_names()
    print("-- tools/list with profile 'minimal'")
    for n in names2:
        print("    " + n)
    check("forge_greet 사라짐", "forge_greet" not in names2)
    check("gw_mcp__echo 사라짐", "gw_mcp__echo" not in names2)
    check("메타 도구 forge_skill 은 유지", "forge_skill" in names2)
    r = rpc("tools/call", {"name": "forge_greet", "arguments": {"name": "again"}})
    check("사라진 도구를 직접 호출해도 거부", r.get("error") is not None or r.get("result", {}).get("isError") is True)

    print("-- 다시 work 로 복귀")
    rc, _ = run_forge("profile", "use", "work", "--client", "all", "--project-dir", proj)
    names3 = tool_names()
    check("forge_greet 다시 나타남", rc == 0 and "forge_greet" in names3)
finally:
    try:
        proc.stdin.close()
        proc.wait(timeout=5)
    except Exception:
        proc.kill()

print("gateway checks failed: %d" % failures)
sys.exit(1 if failures else 0)
