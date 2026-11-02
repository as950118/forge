package tool

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// RiskCodeExec marks a command tool whose program interprets data as code
// (TASK-0018, docs/security.md 3.3 T1 and 4.1). The mark is DERIVED from the
// definition at read time (Definition.Risk is never stored, so no schema change
// and every already registered tool is re-classified the moment forge is
// upgraded). The gateway turns confirm=none into confirm=ask for such a tool
// unless the registrant explicitly acknowledged it (Policy.AckCodeExec).
const RiskCodeExec = "code-exec"

// codeExecExec lists programs that take code, a script, a shell command or a
// package to run from their ARGUMENTS but are not in the deny list because
// ordinary tools legitimately wrap them (`sqlite3 db.sqlite {{query}}`,
// `npm run {{script}}`). This is a name heuristic: a renamed copy, a wrapper
// script around one of them, or a program that is not listed escapes it. Names
// are matched after normExecNames (versions, .exe, symlink hops).
//
//	database shells with shell escapes : sqlite3 sqlite psql mysql mariadb mongosh redis-cli(EVAL)
//	JS / Python / Ruby package runners : npm npx pnpm pnpx yarn bunx uv uvx pipx pip poetry conda
//	                                     bundle bundler gem rake composer
//	JVM / .NET / Swift / others        : swift swiftc java jshell kotlin kotlinc scala sbt gradle mvn lein dotnet
//	                                     irb ghc runghc ghci julia elixir mix erl dart flutter Rscript
//	desktop launchers                  : open xdg-open
//	subcommand-dependent (see below)   : go cargo
var codeExecExec = setOf(
	"sqlite3", "sqlite", "psql", "mysql", "mariadb", "mongosh", "redis-cli",
	"npm", "npx", "pnpm", "pnpx", "yarn", "bunx", "uv", "uvx", "pipx", "pip", "poetry", "conda", "bundle", "bundler", "gem", "rake", "composer",
	"swift", "swiftc", "java", "jshell", "kotlin", "kotlinc", "scala", "sbt", "gradle", "mvn", "lein", "dotnet",
	"irb", "ghc", "runghc", "ghci", "julia", "elixir", "mix", "erl", "dart", "flutter", "rscript",
	"open", "xdg-open",
)

// codeExecSubcmd: programs that are only code-exec for some subcommands
// (`go list` and `cargo metadata` are not judged; `go run`, `cargo run` are).
var codeExecSubcmd = map[string]map[string]bool{
	"go":    setOf("run", "generate", "test", "install", "tool", "get", "build", "vet"),
	"cargo": setOf("run", "test", "install", "build", "bench", "check", "script", "clippy", "doc", "fix"),
}

var shellSegSplit = regexp.MustCompile(`[;&|\n(){}` + "`" + `]+`)
var shellAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// codeExecName judges one executable name plus its literal arguments.
func codeExecName(base string, args []string) string {
	for _, n := range normExecNames(base) {
		if codeExecExec[n] {
			return n
		}
		if subs, ok := codeExecSubcmd[n]; ok {
			first := ""
			for i, a := range args {
				if strings.HasPrefix(a, "-") {
					continue
				}
				if first == "" {
					first = a
				}
				// the subcommand may follow a flag with a value (`go -C dir test`)
				if i < 4 && subs[a] {
					return n + " " + a
				}
			}
			if strings.Contains(first, "{{") { // subcommand chosen by a variable
				return n + " " + strings.TrimSpace(strings.SplitN(first, "{{", 2)[0])
			}
		}
	}
	return ""
}

// CodeExecBinary returns the code-executing program a command tool runs ("" when
// it does not run one, or for non-command kinds). For --shell tools the first
// word of each command segment of the script is judged (heuristic: quoting,
// `eval`, variables holding a program name and `$(...)` are not followed;
// shell scripts are user-authored, the agent only supplies FORGE_ARG_* data).
func (d *Definition) CodeExecBinary() string {
	if d.Kind != KindCommand {
		return ""
	}
	if d.Shell {
		if len(d.Args) == 0 {
			return ""
		}
		for _, seg := range shellSegSplit.Split(d.Args[len(d.Args)-1], -1) {
			f := strings.Fields(seg)
			for len(f) > 0 && shellAssignRe.MatchString(f[0]) {
				f = f[1:]
			}
			if len(f) == 0 {
				continue
			}
			if n := codeExecName(filepath.Base(strings.Trim(f[0], `"'`)), f[1:]); n != "" {
				return n
			}
		}
		return ""
	}
	if n := codeExecName(filepath.Base(d.Command), d.Args); n != "" {
		return n
	}
	// a link named innocently that ends in one of these (ln -s .../npm mytool)
	if p := lookupForCheck(d.Command); p != "" {
		if hops, _ := execChain(p); len(hops) > 1 {
			for _, h := range hops[1:] {
				if n := codeExecName(h, d.Args); n != "" {
					return n
				}
			}
		}
	}
	return ""
}

// refreshRisk recomputes the derived Risk field.
func (d *Definition) refreshRisk() {
	d.Risk = ""
	if d.CodeExecBinary() != "" {
		d.Risk = RiskCodeExec
	}
}

// ForcedAsk reports whether the gateway must ask a human before running d
// whatever the stored confirm says: a code-exec tool with confirm=none that the
// registrant never acknowledged (legacy rows included).
func (d *Definition) ForcedAsk() bool {
	c := d.Policy.Confirm
	return (c == "" || c == ConfirmNone) && !d.Policy.AckCodeExec && d.CodeExecBinary() != ""
}

// CodeExecWarning is the text `forge tool add` prints for a code-exec tool.
func CodeExecWarning(d *Definition) string {
	bin := d.CodeExecBinary()
	if bin == "" {
		return ""
	}
	s := fmt.Sprintf("경고: %q 는 코드 실행형 프로그램입니다 (risk=code-exec): 인자나 입력이 코드·명령으로 해석될 수 있어(예: sqlite3 의 .shell, npm exec, uv run) 모델이 준 값으로 임의 실행이 가능할 수 있습니다. 이름 기반 휴리스틱이며 샌드박스가 아닙니다 (docs/security.md).", bin)
	if d.Policy.Confirm == ConfirmNone {
		return s + " --confirm none 을 명시 승인(--yes-code-exec)했으므로 게이트웨이는 실행 전 확인을 요구하지 않습니다."
	}
	return s + " 게이트웨이는 confirm=ask 를 강제합니다 (confirm=none 이어도 --yes-code-exec 없이는 확인을 요구)."
}

func (d *Definition) validateCodeExecAck() error {
	if d.Kind != KindCommand {
		return nil
	}
	if d.Policy.AckCodeExec && d.Policy.Confirm != ConfirmNone {
		return fmt.Errorf("--yes-code-exec 는 --confirm none 과 함께만 의미가 있습니다 (현재 confirm=%s)", d.Policy.Confirm)
	}
	if bin := d.CodeExecBinary(); bin != "" && d.Policy.Confirm == ConfirmNone && !d.Policy.AckCodeExec {
		return fmt.Errorf("%q 는 코드 실행형 프로그램(risk=code-exec)이라 --confirm none 으로 등록하려면 위험을 이해했다는 명시 확인 --yes-code-exec 가 필요합니다. 기본(confirm=ask)으로 두면 호출마다 사용자 확인을 거칩니다", bin)
	}
	return nil
}
