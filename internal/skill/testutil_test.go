package skill

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/as950118/forge/internal/store"
)

const goodSkill = "---\nname: demo-skill\ndescription: A demo skill.\nx-custom: keep-me\n---\n# Body\n"

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(func() { makeWritable(home) }) // snapshots are read-only
	if err := store.EnsureLayout(home); err != nil {
		t.Fatal(err)
	}
	conn, err := store.Open(context.Background(), store.DBPath(home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &Registry{DB: conn, Home: home}
}

func dirSkill(t *testing.T, name, skillMD string, extra map[string]string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), name)
	writeFile(t, filepath.Join(d, "SKILL.md"), skillMD)
	for k, v := range extra {
		writeFile(t, filepath.Join(d, k), v)
	}
	return d
}

type zentry struct {
	name   string
	body   string
	mode   os.FileMode
	method uint16
}

func makeZip(t *testing.T, entries []zentry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.method != 0 {
			h.Method = e.method
		}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}
