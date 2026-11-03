package ledger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/as950118/forge/internal/mcp/secretcorpus"
)

// textMasked reports whether the display/masking path changes the text (a masked
// secret is replaced by "***", a normal value must stay readable).
func textMasked(e secretcorpus.Entry) bool { return MaskText(e.Value) != e.Value }

// TestCorpusMetricsText prints recall/precision of the display context (-v).
func TestCorpusMetricsText(t *testing.T) {
	var sec, secHit, nor, norOK, res, resHit int
	for _, e := range secretcorpus.All() {
		if e.Ctx&secretcorpus.CtxText == 0 {
			continue
		}
		m := textMasked(e)
		switch {
		case e.Residual:
			res++
			if m {
				resHit++
			}
		case e.Secret:
			sec++
			if m {
				secHit++
			}
		default:
			nor++
			if !m {
				norOK++
			}
		}
	}
	t.Logf("METRIC text   recall %d/%d  precision %d/%d  residual-masked %d/%d", secHit, sec, norOK, nor, resHit, res)
}

// TestCorpusText: in the display context every fake secret is masked and every
// normal value (paths, digests, identifiers) stays readable.
func TestCorpusText(t *testing.T) {
	for _, e := range secretcorpus.All() {
		if e.Ctx&secretcorpus.CtxText == 0 {
			continue
		}
		m := textMasked(e)
		label := e.Kind
		switch {
		case e.Residual:
			if m {
				t.Errorf("residual entry became masked (update docs/corpus): %s", label)
			}
		case e.Secret:
			if !m {
				t.Errorf("MISS (fake secret shown) %s: %.12s...", label, e.Value)
			}
		default:
			if m {
				t.Errorf("FALSE POSITIVE (normal value masked) %s: %.12s...", label, e.Value)
			}
		}
	}
}

// Display/masking call sites: hook payloads (maskPayload) keep paths and labelled
// identifiers readable and mask disguised secrets.
func TestMaskPayloadPathsAndIdentifiers(t *testing.T) {
	const mac = "/var/folders/gr/q4n6pgs90gx_jny4zh7p58dw0000gn/T/forge-demo.abc123/src/variant"
	const h40 = "0123456789abcdef0123456789abcdef01234567"
	const disguised = "/tmp/sk-Zq8Lm2Vb7Nc4Rt6Yh1Jd3Kf5Sg0Wa9Xe2Pu4Io6Qw"
	raw := `{"tool_input":{"file_path":"` + mac + `","command":"python3 s.py ` + mac + ` --ref ` + h40 + `","commit":"` + h40 + `","image":"img@sha256:` +
		strings.Repeat("ab", 32) + `","note":"` + disguised + `","opaque":"q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa"}}`
	got, err := MaskJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{mac, "--ref " + h40, `"commit":"` + h40 + `"`, "img@sha256:" + strings.Repeat("ab", 32)} {
		if !strings.Contains(got, keep) {
			t.Errorf("readable value masked: %s\n%s", keep, got)
		}
	}
	for _, leak := range []string{"sk-Zq8Lm2", "q9Zx7Lm2Vb8"} {
		if strings.Contains(got, leak) {
			t.Errorf("secret leaked: %s\n%s", leak, got)
		}
	}
}

func TestMaskTextKeepsPathsButMasksPrefixesInsideThem(t *testing.T) {
	const mac = "/var/folders/gr/q4n6pgs90gx_jny4zh7p58dw0000gn/T/x/y"
	if got := MaskText("cwd=" + mac + " done"); !strings.Contains(got, mac) {
		t.Errorf("path masked: %q", got)
	}
	if got := MaskText("/a/b/ghp_Zq8Lm2Vb7Nc4Rt6Yh1Jd3Kf5Sg0Wa9Xe2P/c"); got != "***" {
		t.Errorf("disguised secret shown: %q", got)
	}
}

func persistMasked(e secretcorpus.Entry) bool {
	b, _ := json.Marshal(map[string]string{"v": e.Value})
	got, err := maskPayload(b)
	if err != nil {
		return true
	}
	return !strings.Contains(got, e.Value)
}

// Stored payloads (ledger_payloads) use the strict rule: nothing a secret-looking
// path component or label could hide survives, residual risks of registration
// included. Only the readability of normal values suffers (reported, not asserted).
func TestCorpusPersistMasksEverySecret(t *testing.T) {
	var sec, hit, nor, ok int
	for _, e := range secretcorpus.All() {
		if e.Ctx&secretcorpus.CtxText == 0 {
			continue
		}
		m := persistMasked(e)
		if e.Secret {
			sec++
			if m {
				hit++
			} else {
				t.Errorf("MISS in stored payload (%s): %.12s...", e.Kind, e.Value)
			}
		} else {
			nor++
			if !m {
				ok++
			}
		}
	}
	t.Logf("METRIC persist recall %d/%d (incl. residual)  readable-normals %d/%d", hit, sec, ok, nor)
}

// Regression (TASK-0026 review): with record_payload=1 a random key below a known
// root directory ("/tmp/<random>") was stored in plaintext because the display
// path exception had leaked into the storage rule.
func TestStoredPayloadMasksRandomPathComponent(t *testing.T) {
	const key = "q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa9Xe2Pu4Io6Qw"
	for _, v := range []string{"/tmp/" + key, "/home/u/.config/" + key, "/var/folders/gr/" + key + "/T/x"} {
		raw := `{"tool_input":{"file_path":"` + v + `","note":"cwd ` + v + `"}}`
		got, err := maskPayload([]byte(raw))
		if err != nil || strings.Contains(got, "q9Zx7Lm2") {
			t.Errorf("random path component stored in plaintext: %s %v", got, err)
		}
		// the display rule keeps it readable (transient output only)
		if disp, _ := MaskJSON([]byte(raw)); !strings.Contains(disp, v) {
			t.Errorf("display masked a path: %s", disp)
		}
	}
}

// End to end (hook payload -> ledger_payloads): with record_payload=1 a random
// key inside a path never reaches the database in plaintext.
func TestRecordedPayloadHasNoPlaintextPathKey(t *testing.T) {
	rec, _ := newRec(t)
	ctx := context.Background()
	exec(t, rec.DB, `INSERT INTO settings(key,value) VALUES ('record_payload','1')`)
	const key = "q9Zx7Lm2Vb8Nc4Rt6Yh1Jd3Kf5Sg0Wa9Xe2Pu4Io6Qw"
	e := baseEv()
	e.Input = json.RawMessage(`{"tool_input":{"file_path":"/tmp/` + key + `","command":"cat /home/u/.config/` + key + `"}}`)
	e.Output = json.RawMessage(`{"stdout":"ok /var/folders/gr/` + key + `/T/x"}`)
	id, err := rec.Record(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	pi, po, found, _ := rec.Payload(ctx, id)
	if !found || strings.Contains(pi, "q9Zx7Lm2") || strings.Contains(po, "q9Zx7Lm2") {
		t.Errorf("plaintext path key stored: %s | %s", pi, po)
	}
}
