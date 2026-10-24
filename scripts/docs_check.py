#!/usr/bin/env python3
"""Static checks for README.md and docs/ (run by scripts/docs-check.sh, `make docs-check`).

Checks (every failure prints file:line and the process exits 1):

  (a) links     every relative markdown link, image, reference-style definition
                ("[r]: x.md") and HTML href/src points at an existing file; a
                "#anchor" must match a heading of the target markdown file
                (GitHub-style slug).
  (b) commands  in fenced code blocks tagged bash/sh/shell/zsh/console/
                shell-session/powershell or untagged (= bash), in "text" blocks
                (only lines starting with "forge ") and in 4-space-indented code
                lines starting with "forge ", every
                `forge ...` command line must name subcommands that exist
                (`forge <cmd> [<sub>] --help` exits 0) and every `--flag` must be
                listed in that command's `--help` (its own and global flags).
                Placeholders such as <name> and [optional] are allowed.
                For README.md and docs/guide/*.md inline `forge ...` code spans
                get the same subcommand check (flags are not checked inline).
  (c) invisible no raw invisible / bidirectional-control characters in the docs,
                scripts/ and Makefile (same rule as the repository's local scan:
                categories Cf, Zl, Zp, Cc except TAB/CR, plus U+00A0 and U+0085).
  (d) local     no ".company-os" path in the docs (that directory is a local,
                git-ignored history other readers cannot open).

OUT OF SCOPE (not checked): commands inside $(...), after sudo or inside
`bash -c '...'`; whether `make` targets or scripts/* paths named in the docs
exist; commands in inline code of docs other than README.md and docs/guide/.

LIMITS (stated honestly): this only proves that the documented commands and
flags EXIST. It does not run them and does not check their output or
behaviour. scripts/quickstart-check.sh runs the quickstart commands for real.
A flag is accepted when it is declared by the command (or its group's
persistent flags); whether the value after it is valid is not checked.

Usage: python3 -I scripts/docs_check.py [--root DIR]   (FORGE_BIN=path/to/forge)
"""
import os
import re
import shlex
import subprocess
import sys
import unicodedata

# Fences checked as shell. A fence WITHOUT a language counts as bash. "text" fences
# are only scanned for lines that START with "forge " (output examples should use a
# "$ ", "> " or "# " prefix or be excluded explicitly, see SKIP_MARK).
SHELL_LANGS = {"bash", "sh", "shell", "zsh", "console", "shell-session", "powershell", "ps1", ""}
TEXT_LANGS = {"text", "txt", "plain"}
# "<!-- docs-check:skip -->" on the line right before a fence (or an indented code
# line) excludes that block from the command check. Say why in a nearby comment.
SKIP_MARK = "<!-- docs-check:skip -->"
INLINE_CHECKED = ("README.md", os.path.join("docs", "guide") + os.sep)
LOCAL_DIR = ".company-os"

errors = []


def err(path, lineno, msg):
    errors.append("%s:%d: %s" % (path, lineno, msg))


def find_root(argv):
    if "--root" in argv:
        return os.path.abspath(argv[argv.index("--root") + 1])
    return os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))


def doc_files(root):
    out = []
    readme = os.path.join(root, "README.md")
    if os.path.isfile(readme):
        out.append(readme)
    for base, dirs, files in os.walk(os.path.join(root, "docs")):
        dirs.sort()
        for n in sorted(files):
            if n.endswith(".md"):
                out.append(os.path.join(base, n))
    return out


def rel(root, p):
    return os.path.relpath(p, root)


# ------------------------------------------------------------ markdown parse

FENCE = re.compile(r"^(\s*)(`{3,}|~{3,})\s*([A-Za-z0-9_+-]*)")


def parse_md(text):
    """Return (lines, fences, prose) where fences = [(lang, start_lineno, [lines])]
    and prose = [(lineno, line)] for lines outside code fences."""
    lines = text.split("\n")
    fences, prose = [], []
    in_fence = False
    marker = ""
    cur = None
    prev = ""
    for i, line in enumerate(lines, 1):
        m = FENCE.match(line)
        if not in_fence:
            if m:
                in_fence = True
                marker = m.group(2)
                cur = (m.group(3).lower(), i, [], prev.strip() == SKIP_MARK)
            else:
                prose.append((i, line))
                if line.strip():
                    prev = line
        else:
            if m and m.group(2).startswith(marker[0]) and len(m.group(2)) >= len(marker) and not m.group(3):
                in_fence = False
                fences.append(cur)
                cur = None
                prev = ""
            else:
                cur[2].append(line)
    if in_fence and cur:
        fences.append(cur)
    return lines, fences, prose


def slug(heading):
    h = heading.strip().lower()
    h = re.sub(r"`", "", h)
    h = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", h)
    out = []
    for ch in h:
        if ch.isalnum() or ch in "_-":
            out.append(ch)
        elif ch == " ":
            out.append("-")
    return "".join(out)


def anchors_of(path, cache={}):
    if path in cache:
        return cache[path]
    res = set()
    try:
        with open(path, encoding="utf-8") as f:
            _l, _f, prose = parse_md(f.read())
    except OSError:
        cache[path] = res
        return res
    seen = {}
    for _i, line in prose:
        m = re.match(r"^#{1,6}\s+(.*?)\s*#*\s*$", line)
        if m:
            s = slug(m.group(1))
            n = seen.get(s, 0)
            seen[s] = n + 1
            res.add(s if n == 0 else "%s-%d" % (s, n))
    cache[path] = res
    return res


# ------------------------------------------------------------------ (a) links

LINK = re.compile(r"!?\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
REFDEF = re.compile(r"^\s{0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?")
HTMLREF = re.compile(r"""(?:href|src)\s*=\s*["']([^"']+)["']""", re.I)
INLINE_CODE = re.compile(r"`[^`]*`")


def check_links(root, path, prose):
    for lineno, line in prose:
        stripped = INLINE_CODE.sub("", line)
        targets = [m.group(1) for m in LINK.finditer(stripped)]
        targets += [m.group(1) for m in REFDEF.finditer(stripped)]
        targets += [m.group(1) for m in HTMLREF.finditer(stripped)]
        for target in targets:
            if re.match(r"^[a-zA-Z][a-zA-Z0-9+.-]*:", target):
                continue  # http:, https:, mailto: ...
            file_part, _, anchor = target.partition("#")
            if file_part == "":
                dest = path
            else:
                dest = os.path.normpath(os.path.join(os.path.dirname(path), file_part))
                if not os.path.exists(dest):
                    err(rel(root, path), lineno, "깨진 링크: %s (파일 없음)" % target)
                    continue
            if anchor and dest.endswith(".md") and os.path.isfile(dest):
                if anchor.lower() not in anchors_of(dest):
                    err(rel(root, path), lineno, "깨진 앵커: %s (제목 없음)" % target)


# --------------------------------------------------------------- (b) commands


class Forge:
    def __init__(self, binary):
        self.binary = binary
        self.cache = {}

    def help(self, path):
        key = tuple(path)
        if key not in self.cache:
            try:
                # --help never needs a home; point HOME/FORGE_HOME at a path that does not exist.
                env = dict(os.environ, HOME="/nonexistent-docs-check", FORGE_HOME="/nonexistent-docs-check/forge")
                p = subprocess.run([self.binary] + list(path) + ["--help"], capture_output=True,
                                   text=True, timeout=30, stdin=subprocess.DEVNULL, env=env)
                self.cache[key] = (p.returncode, p.stdout + p.stderr)
            except Exception as e:  # noqa: BLE001
                self.cache[key] = (127, str(e))
        return self.cache[key]

    def subcommands(self, path):
        rc, out = self.help(path)
        if rc != 0:
            return None
        subs, on = set(), False
        for line in out.split("\n"):
            if line.strip() == "Available Commands:":
                on = True
                continue
            if on:
                if not line.strip():
                    on = False
                    continue
                m = re.match(r"^\s{2}(\S+)\s", line + " ")
                if m:
                    subs.add(m.group(1))
        return subs

    def flags(self, path):
        rc, out = self.help(path)
        if rc != 0:
            return None
        fl, on = {"--help"}, False
        if not path:
            fl.add("--version")
        for line in out.split("\n"):
            if line.strip() in ("Flags:", "Global Flags:"):
                on = True
                continue
            if on:
                if not line.strip():
                    on = False
                    continue
                m = re.match(r"^\s+(?:-\w,\s+)?(--[A-Za-z][\w-]*)", line)
                if m:
                    fl.add(m.group(1))
        return fl


OPERATORS = {"|", "||", "&&", ";", "&", ">", ">>", "<", "2>&1", "2>", "<<"}
ASSIGN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")


def split_segments(tokens):
    seg, segs = [], []
    for t in tokens:
        if t in OPERATORS or t.startswith(("2>", ">&", ">/", ">>")):
            if seg:
                segs.append(seg)
            seg = []
            if t in ("<<",):
                break
        else:
            seg.append(t)
    if seg:
        segs.append(seg)
    return segs


def is_placeholder(tok):
    return "<" in tok or tok.startswith("[") or tok.startswith("$") or "{" in tok or tok in ("...", "…")


def check_forge_args(forge, root, rpath, lineno, args, check_flags, shown):
    """args: tokens after the `forge` word."""
    path = []
    i = 0
    # walk the subcommand path: tokens directly after forge / the previous subcommand
    while i < len(args):
        tok = args[i]
        if tok.startswith("-") or is_placeholder(tok):
            break
        subs = forge.subcommands(path)
        if subs is None:
            err(rpath, lineno, "`forge %s --help` 가 실패했습니다: %s" % (" ".join(path), shown))
            return
        if not subs:
            break  # leaf command: the rest are arguments
        if tok in subs:
            path.append(tok)
            i += 1
            continue
        err(rpath, lineno, "존재하지 않는 하위 명령: forge %s (줄: %s)" % (" ".join(path + [tok]), shown))
        return
    rc, _out = forge.help(path)
    if rc != 0:
        err(rpath, lineno, "`forge %s --help` 종료 코드 %d (줄: %s)" % (" ".join(path), rc, shown))
        return
    if not check_flags:
        return
    known = forge.flags(path)
    rest = args[i:]
    for tok in rest:
        if tok == "--":
            break
        t = tok.strip("[]")
        if not t.startswith("--") or is_placeholder(t):
            continue
        name = t.split("=", 1)[0].rstrip("],|")
        name = name.split("|", 1)[0]
        if re.match(r"^--[A-Za-z][\w-]*$", name) and name not in known:
            err(rpath, lineno, "존재하지 않는 플래그: forge %s %s (줄: %s)" % (" ".join(path), name, shown))


def forge_commands_in(line):
    """Yield token lists following a `forge` command word in one shell line."""
    s = line.strip()
    if s.startswith("$ "):
        s = s[2:]
    if not s or s.startswith("#"):
        return
    try:
        lex = shlex.shlex(s, posix=True, punctuation_chars=True)
        lex.whitespace_split = True
        lex.commenters = "#"
        tokens = list(lex)
    except ValueError:
        yield None
        return
    for seg in split_segments(tokens):
        j = 0
        while j < len(seg) and ASSIGN.match(seg[j]):
            j += 1
        if j < len(seg) and os.path.basename(seg[j]) == "forge":
            yield seg[j + 1:]


def check_command_lines(forge, rpath, joined, text_mode):
    """joined: [(lineno, line)] after joining backslash continuations."""
    in_heredoc = None
    for no, line in joined:
        if in_heredoc is not None:
            if line.strip() == in_heredoc:
                in_heredoc = None
            continue
        if text_mode:
            # "text" blocks: only raw lines that start with "forge " are commands.
            if not line.startswith("forge "):
                continue
        m = re.search(r"<<-?\s*['\"]?([A-Za-z_][A-Za-z0-9_]*)['\"]?\s*$", line)
        for args in forge_commands_in(line):
            if args is None:
                err(rpath, no, "명령 줄을 해석할 수 없습니다(따옴표 불일치?): %s" % line.strip())
                break
            check_forge_args(forge, None, rpath, no, args, True, line.strip())
        if m:
            in_heredoc = m.group(1)


def join_continuations(start, body):
    joined, buf, buf_no = [], "", 0
    for off, line in enumerate(body):
        no = start + off
        if not buf:
            buf_no = no
        if line.rstrip().endswith("\\"):
            buf += line.rstrip()[:-1] + " "
            continue
        joined.append((buf_no, buf + line))
        buf = ""
    if buf:
        joined.append((buf_no, buf))
    return joined


def check_fenced_commands(forge, root, path, fences):
    rpath = rel(root, path)
    for lang, start, body, skip in fences:
        if skip:
            continue
        if lang in SHELL_LANGS:
            check_command_lines(forge, rpath, join_continuations(start + 1, body), False)
        elif lang in TEXT_LANGS:
            check_command_lines(forge, rpath, join_continuations(start + 1, body), True)


def check_indented_commands(forge, root, path, prose):
    """4-space / tab indented code lines that start with `forge `."""
    rpath = rel(root, path)
    prev = ""
    for lineno, line in prose:
        if re.match(r"^( {4}|\t)\s*forge\s", line) and prev.strip() != SKIP_MARK:
            check_command_lines(forge, rpath, [(lineno, line.strip())], False)
        if line.strip():
            prev = line


def check_inline_commands(forge, root, path, prose):
    rpath = rel(root, path)
    for lineno, line in prose:
        for m in INLINE_CODE.finditer(line):
            code = m.group(0).strip("`").strip()
            if not re.match(r"^(\S*/)?forge(\s|$)", code):
                continue
            for args in forge_commands_in(code):
                if args is None:
                    continue  # partial snippet inside backticks
                check_forge_args(forge, root, rpath, lineno, args, False, code)


# ----------------------------------------------------------- (c) invisible (d)


def check_invisible(root, path):
    try:
        with open(path, encoding="utf-8") as f:
            text = f.read()
    except (OSError, UnicodeDecodeError):
        return
    for lineno, line in enumerate(text.split("\n"), 1):
        for ch in line:
            cat = unicodedata.category(ch)
            if cat in ("Cf", "Zl", "Zp") or (cat == "Cc" and ch not in "\t\r") or ch in (chr(0xA0), chr(0x85)):
                err(rel(root, path), lineno, "비가시/방향 제어 문자 U+%04X (%s)" % (ord(ch), cat))


def text_files_for_invisible(root, docs):
    files = list(docs)
    mk = os.path.join(root, "Makefile")
    if os.path.isfile(mk):
        files.append(mk)
    for base, dirs, names in os.walk(os.path.join(root, "scripts")):
        dirs.sort()
        for n in sorted(names):
            if n.endswith((".sh", ".py", ".md", ".txt")):
                files.append(os.path.join(base, n))
    return files


def check_local_refs(root, path, lines):
    for lineno, line in enumerate(lines, 1):
        if LOCAL_DIR in line:
            err(rel(root, path), lineno, "로컬 전용 경로 '%s' 는 문서에 쓸 수 없습니다" % LOCAL_DIR)


# ----------------------------------------------------------------------- main


def main(argv):
    root = find_root(argv)
    binary = os.environ.get("FORGE_BIN", "")
    if not binary or not os.access(binary, os.X_OK):
        print("docs-check: FORGE_BIN 이 실행 가능한 forge 바이너리를 가리켜야 합니다 (make docs-check 가 빌드합니다)", file=sys.stderr)
        return 2
    forge = Forge(binary)
    docs = doc_files(root)
    if not docs:
        print("docs-check: 문서를 찾지 못했습니다 (root=%s)" % root, file=sys.stderr)
        return 2
    n_links = n_cmds = 0
    for path in docs:
        with open(path, encoding="utf-8") as f:
            text = f.read()
        lines, fences, prose = parse_md(text)
        check_links(root, path, prose)
        check_local_refs(root, path, lines)
        check_fenced_commands(forge, root, path, fences)
        check_indented_commands(forge, root, path, prose)
        r = rel(root, path)
        if r == "README.md" or r.startswith(os.path.join("docs", "guide") + os.sep):
            check_inline_commands(forge, root, path, prose)
        n_links += sum(len(LINK.findall(INLINE_CODE.sub("", ln))) for _i, ln in prose)
        n_cmds += sum(len(b) for lang, _s, b, _k in fences if lang in SHELL_LANGS or lang in TEXT_LANGS)
    for path in text_files_for_invisible(root, docs):
        check_invisible(root, path)
    print("docs-check: 문서 %d개, 링크 %d개, 셸 코드 블록 %d줄 점검" % (len(docs), n_links, n_cmds))
    if errors:
        for e in errors:
            print(e)
        print("docs-check: 실패 %d건" % len(errors))
        return 1
    print("docs-check: 통과")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
