package claudecode

import (
	"os"
	"testing"

	"github.com/heonjinjeong/forge/internal/testenv"
)

// TestMain keeps t.TempDir() paths short whatever TMPDIR is (see testenv).
func TestMain(m *testing.M) { os.Exit(testenv.Run(m)) }
