package mcp

import (
	"strings"
	"testing"

	"github.com/heonjinjeong/forge/internal/mcp/secretcorpus"
)

// corpusRejects reports whether the registration-time rules refuse the entry in
// the given context (the registration contexts of TASK-0026).
func corpusRejects(e secretcorpus.Entry, ctx int) bool {
	switch ctx {
	case secretcorpus.CtxArg:
		return CheckArgs(e.ArgVec()) != nil
	case secretcorpus.CtxEnv:
		_, err := ParseValue(SlotEnv, e.Key+"="+e.Value)
		return err != nil
	case secretcorpus.CtxHeader:
		_, err := ParseValue(SlotHeader, e.Key+"="+e.Value)
		return err != nil
	case secretcorpus.CtxURL:
		s := Spec{Name: "a", Transport: TransportHTTP, URL: e.Value}
		return s.Validate() != nil
	}
	panic("unsupported context")
}

var registrationCtxs = []struct {
	name string
	bit  int
}{
	{"arg", secretcorpus.CtxArg}, {"env", secretcorpus.CtxEnv}, {"header", secretcorpus.CtxHeader}, {"url", secretcorpus.CtxURL},
}

// TestCorpusMetrics prints recall (fake secrets refused) and precision (normal
// values accepted) per registration context; run with -v to read the numbers.
func TestCorpusMetrics(t *testing.T) {
	for _, c := range registrationCtxs {
		var sec, secHit, nor, norOK, res, resHit int
		for _, e := range secretcorpus.All() {
			if e.Ctx&c.bit == 0 {
				continue
			}
			rej := corpusRejects(e, c.bit)
			switch {
			case e.Residual:
				res++
				if rej {
					resHit++
				}
			case e.Secret:
				sec++
				if rej {
					secHit++
				}
			default:
				nor++
				if !rej {
					norOK++
				}
			}
		}
		t.Logf("METRIC %-6s recall %d/%d  precision %d/%d  residual-refused %d/%d", c.name, secHit, sec, norOK, nor, resHit, res)
	}
}

// TestCorpusSizes guards the corpus requirement (>=120 each).
func TestCorpusSizes(t *testing.T) {
	var sec, nor int
	for _, e := range secretcorpus.All() {
		if e.Secret && !e.Residual {
			sec++
		}
		if !e.Secret {
			nor++
		}
	}
	if sec < 120 || nor < 120 {
		t.Fatalf("corpus too small: secrets=%d normals=%d", sec, nor)
	}
}

// TestCorpusRegistration: every fake secret stays refused, every normal value
// registers, in each registration context (the corpus is the regression net of
// the path/identifier exceptions). Residual secrets pass on purpose.
func TestCorpusRegistration(t *testing.T) {
	for _, c := range registrationCtxs {
		for _, e := range secretcorpus.All() {
			if e.Ctx&c.bit == 0 {
				continue
			}
			rej := corpusRejects(e, c.bit)
			label := e.Kind + " " + c.name + " " + strings.Join(e.ArgVec(), " ")
			if len(label) > 40 {
				label = label[:40] // never print the whole (fake) value
			}
			switch {
			case e.Residual:
				if rej {
					t.Errorf("residual entry became refused (update docs/corpus): %s...", label)
				}
			case e.Secret:
				if !rej {
					t.Errorf("MISS (fake secret accepted) %s...", label)
				}
			default:
				if rej {
					t.Errorf("FALSE POSITIVE %s...", label)
				}
			}
		}
	}
}
