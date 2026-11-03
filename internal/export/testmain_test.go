package export

import (
	"os"
	"testing"

	"github.com/as950118/forge/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Run(m)) }
