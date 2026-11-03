package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/as950118/forge/internal/textsafe"
)

// Korean wording for the flag-parsing errors cobra/pflag produce in English. The
// original text is kept for anything not recognized. Echoed user values go through
// textsafe.Display (they may carry terminal escapes).
var (
	reInvalidArg   = regexp.MustCompile(`^invalid argument "(.*)" for "(.*)" flag: (.*)$`)
	reUnknownFlag  = regexp.MustCompile(`^unknown flag: (.*)$`)
	reUnknownShort = regexp.MustCompile(`^unknown shorthand flag: '(.*)' in (.*)$`)
	reNeedsArg     = regexp.MustCompile(`^flag needs an argument: (.*)$`)
	reBadSyntax    = regexp.MustCompile(`^bad flag syntax: (.*)$`)
)

func shownArg(s string) string { return textsafe.Display(s, 80) }

// localizeFlagError translates a pflag error message.
func localizeFlagError(c *cobra.Command, err error) error {
	msg := err.Error()
	hint := ""
	if c != nil {
		hint = fmt.Sprintf(" (%s --help 로 사용법을 확인하세요)", c.CommandPath())
	}
	switch {
	case reInvalidArg.MatchString(msg):
		m := reInvalidArg.FindStringSubmatch(msg)
		why := "형식이 맞지 않습니다"
		switch {
		case strings.Contains(m[3], "out of range"):
			why = "범위를 벗어났습니다"
		case strings.Contains(m[3], "invalid syntax"):
			why = "형식이 맞지 않습니다 (예: 숫자가 필요한 곳에 숫자가 아닌 값)"
		}
		return fmt.Errorf("%s 값 %q 이(가) 올바르지 않습니다: %s%s", shownArg(m[2]), shownArg(m[1]), why, hint)
	case reUnknownFlag.MatchString(msg):
		return fmt.Errorf("알 수 없는 플래그입니다: %s%s", shownArg(reUnknownFlag.FindStringSubmatch(msg)[1]), hint)
	case reUnknownShort.MatchString(msg):
		m := reUnknownShort.FindStringSubmatch(msg)
		return fmt.Errorf("알 수 없는 짧은 플래그입니다: -%s (%s)%s", shownArg(m[1]), shownArg(m[2]), hint)
	case reNeedsArg.MatchString(msg):
		return fmt.Errorf("플래그에 값이 필요합니다: %s%s", shownArg(reNeedsArg.FindStringSubmatch(msg)[1]), hint)
	case reBadSyntax.MatchString(msg):
		return fmt.Errorf("플래그 형식이 올바르지 않습니다: %s%s", shownArg(reBadSyntax.FindStringSubmatch(msg)[1]), hint)
	}
	return err
}

// noArgs is cobra.NoArgs with a Korean message.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("%s 는 위치 인자를 받지 않습니다: %q (%s --help)", cmd.CommandPath(), shownArg(args[0]), cmd.CommandPath())
}
