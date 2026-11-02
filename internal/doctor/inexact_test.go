package doctor

import (
	"strings"
	"testing"
)

// TASK-0032 AC-5: the paste warning is added once even when the check inserted it itself.
func TestNoteInexactIsIdempotent(t *testing.T) {
	e := &env{}
	q := e.q("/tmp/bad\x1b[31mname")
	if !e.inexact[q] {
		t.Fatal("q must record the inexact path")
	}
	c := Check{Fix: []string{"ls -ld " + q, inexactNote, "which -a claude"}}
	e.noteInexact(&c)
	n := 0
	for _, f := range c.Fix {
		if strings.Contains(f, "붙여 넣으면") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the note appears %d times: %v", n, c.Fix)
	}
	c = Check{Fix: []string{"ls -ld " + q}}
	e.noteInexact(&c)
	if len(c.Fix) != 2 || c.Fix[1] != inexactNote {
		t.Fatalf("a check without the note still gets it: %v", c.Fix)
	}
}
