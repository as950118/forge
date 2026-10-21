package tool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// denyExec lists executables that must never be a command tool's argv[0], even
// when the registrant allowlists them. The allowlist only pins the *first*
// program; these binaries run another program or interpret code taken from
// their own arguments, which turns a "safe" allowlisted name into arbitrary
// execution (`env awk 'BEGIN{system("...")}' x`).
//
// Rationale per group (kept conservative: ls, cat, grep, git, jq, curl ... are
// NOT listed so ordinary tools keep working):
//
//	wrappers that exec their argument : env xargs find fd nice nohup timeout time ionice taskset
//	                                    setsid stdbuf chroot flock watch unshare nsenter parallel
//	                                    script at batch busybox toybox coreutils (multicall) expect
//	privilege / remote                : sudo doas su ssh scp sftp rsync (rsync -e), launchctl, systemd-run
//	interpreters / code evaluators    : awk gawk mawk nawk sed ed ex vi vim view nvim emacs nano
//	                                    php lua tclsh deno bun osascript pwsh powershell cmd
//	                                    (shells and python/node/ruby/perl are in shellSet/interpreterSet)
//	pagers with shell escapes         : less more man pager
//	build / archive runners           : make gmake cmake ninja tar gtar zip cpio (--to-command, -T, --checkpoint-action)
//	container runners                 : docker podman nerdctl
//	debuggers / tracers               : strace ltrace gdb lldb
//
// Names are matched after normalisation (see normExecNames) so versioned
// (python3.11, gawk-5), GNU-prefixed (gsed) and ".exe" forms are covered.
var denyExec = setOf(
	"env", "xargs", "find", "fd", "fdfind", "nice", "nohup", "timeout", "time", "ionice", "taskset", "setsid", "stdbuf", "chroot",
	"flock", "watch", "unshare", "nsenter", "parallel", "script", "at", "batch", "expect",
	"gtimeout", "gnice", "gnohup", "gstdbuf", "gchroot", "genv", "gxargs", "gfind", "gtime",
	"sudo", "doas", "su", "ssh", "scp", "sftp", "rsync", "launchctl", "systemd-run",
	"awk", "gawk", "mawk", "nawk", "sed", "gsed", "ed", "ex", "vi", "vim", "view", "nvim", "emacs", "nano",
	"php", "lua", "luajit", "tclsh", "wish", "deno", "bun", "osascript", "pwsh", "powershell", "cmd",
	"less", "more", "man", "pager",
	"make", "gmake", "cmake", "ninja", "tar", "gtar", "zip", "cpio",
	"docker", "podman", "nerdctl",
	"strace", "ltrace", "gdb", "lldb",
)

// multicallExec dispatch on their first argument (or on the name they were
// invoked as). Invoking them directly is denied; reaching one through a
// differently named symlink is judged by the link's own name instead (an
// applet link called `env`/`sh`/`awk` is denied, one called `ls` is not).
var multicallExec = setOf("busybox", "toybox", "coreutils")

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

var verSuffixRe = regexp.MustCompile(`[0-9][0-9._-]*$`)

// normExecNames returns the lookup candidates for an executable file name.
func normExecNames(base string) []string {
	b := strings.ToLower(base)
	b = strings.TrimSuffix(b, ".exe")
	out := []string{b}
	if t := strings.TrimRight(verSuffixRe.ReplaceAllString(b, ""), "-_."); t != "" && t != b {
		out = append(out, t) // python3.11 -> python, gawk-5.1 -> gawk
	}
	if i := strings.IndexByte(b, '.'); i > 0 {
		out = append(out, b[:i]) // vim.basic -> vim
	}
	return out
}

// deniedExecName reports the denied/shell/interpreter name matched by base.
func deniedExecName(base string, allowInterpreters bool) (string, bool) {
	for _, n := range normExecNames(base) {
		if denyExec[n] || shellSet[n] || (!allowInterpreters && interpreterSet[n]) {
			return n, true
		}
	}
	return "", false
}

// execChain follows symlinks hop by hop and returns the file name of every hop
// (first = the path as given, last = the real file) so that an innocent link
// name cannot hide env/busybox/awk behind it.
func execChain(path string) ([]string, error) {
	var names []string
	cur := path
	for i := 0; i < 40; i++ {
		names = append(names, filepath.Base(cur))
		fi, err := os.Lstat(cur)
		if err != nil {
			return names, err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return names, nil
		}
		next, err := os.Readlink(cur)
		if err != nil {
			return names, err
		}
		if !filepath.IsAbs(next) {
			next = filepath.Join(filepath.Dir(cur), next)
		}
		cur = next
	}
	return names, errors.New("심볼릭 링크가 너무 깊습니다")
}

// checkExecutable applies the deny list to the command name and to every hop of
// the resolved path (and its final EvalSymlinks target). It is called at
// registration, on load (Validate) and again right before exec.
func checkExecutable(command, resolved string) error {
	if n, bad := deniedExecName(filepath.Base(command), false); bad {
		return fmt.Errorf("%s 은(는) 다른 프로그램을 실행하거나 코드를 해석하는 범용 바이너리라 command 도구로 쓸 수 없습니다 (allowlist 를 우회하는 래퍼). 필요한 동작을 직접 실행하는 전용 프로그램을 쓰거나 script 도구를 사용하세요", n)
	}
	if multicallExec[strings.ToLower(filepath.Base(command))] {
		return fmt.Errorf("%s 은(는) 첫 인자로 임의 프로그램을 고르는 멀티콜 바이너리라 command 도구로 쓸 수 없습니다", filepath.Base(command))
	}
	if resolved == "" {
		return nil
	}
	hops, err := execChain(resolved)
	if err != nil && len(hops) == 0 {
		return nil
	}
	for i, h := range hops {
		if n, bad := deniedExecName(h, false); bad {
			return fmt.Errorf("%q 는 심볼릭 링크 체인에서 %s 로 이어집니다. 이름이 아니라 실제 바이너리 기준으로 금지된 실행 파일입니다 (래퍼/인터프리터)", command, n)
		}
		if multicallExec[strings.ToLower(h)] && i == 0 {
			return fmt.Errorf("%s 은(는) 멀티콜 바이너리라 직접 실행할 수 없습니다", h)
		}
	}
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		if n, bad := deniedExecName(filepath.Base(real), false); bad {
			return fmt.Errorf("%q 의 실제 바이너리(%s)는 금지된 실행 파일입니다 (래퍼/인터프리터)", command, n)
		}
	}
	return nil
}

// gitUnsafeOpt: git options that run programs or change what runs. Subcommand
// position and user-supplied option positions are covered by the generic argv
// template rules; this rejects the literal forms in a template.
var gitUnsafeOpt = []string{"-c", "--config-env", "--exec-path", "--upload-pack", "--receive-pack", "--ssh-command", "--ext-cmd"}

func checkGitArgs(args []string) error {
	for _, a := range args {
		for _, o := range gitUnsafeOpt {
			if a == o || strings.HasPrefix(a, o+"=") {
				return fmt.Errorf("git 옵션 %s 는 임의 명령 실행(core.sshCommand, alias, upload-pack 등)에 쓰일 수 있어 허용되지 않습니다", o)
			}
		}
	}
	return nil
}
