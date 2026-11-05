package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportJSON(t *testing.T) {
	initHome(t)
	out := mustRun(t, "export", "--format", "json")
	var snap map[string]any
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if snap["day_basis"] != "UTC" || snap["forge_version"] != "v" {
		t.Fatalf("meta: %v", snap)
	}
	for _, f := range []string{"/work/proj", "sess-1", "claude-code", "opencode", "t1"} {
		if strings.Contains(out, f) {
			t.Errorf("export contains %q", f)
		}
	}
	if len(snap["items"].([]any)) == 0 || len(snap["daily"].([]any)) == 0 {
		t.Fatalf("empty snapshot: %s", out)
	}

	dst := filepath.Join(t.TempDir(), "snap.json")
	mustRun(t, "export", "--format", "json", "--output", dst)
	if st, err := os.Stat(dst); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("output file: %v %v", st, err)
	}
	if _, err := runErr(t, "", "export", "--format", "json", "--output", dst); err == nil {
		t.Fatal("existing file must not be overwritten without --force")
	}
	mustRun(t, "export", "--format", "json", "--output", dst, "--force")
}

func TestExportFlagRules(t *testing.T) {
	initHome(t)
	for _, args := range [][]string{
		{"export"},
		{"export", "--format", "json", "--to-postgres"},
		{"export", "--format", "csv"},
		{"export", "--format", "json", "--yes"},
		{"export", "--to-postgres", "--output", "x"},
	} {
		if _, err := runErr(t, "", args...); err == nil {
			t.Errorf("forge %v should fail", args)
		}
	}
}

func TestExportToPostgresDryRunAndGuards(t *testing.T) {
	initHome(t)
	const pw = "FAKE-PW-s3cr3t"
	t.Setenv("FORGE_PG_CA", "")

	// dry-run: never connects (the port is closed), masks the password.
	t.Setenv("DB_INDEX", "postgresql://u:"+pw+"@127.0.0.1:1/postgres")
	out := mustRun(t, "export", "--to-postgres")
	if strings.Contains(out, pw) || !strings.Contains(out, "dry-run") || !strings.Contains(out, "xxxxx") {
		t.Fatalf("dry-run output: %s", out)
	}

	// --yes tries to connect and fails without leaking the password.
	out, err := runErr(t, "", "export", "--to-postgres", "--yes")
	if err == nil || strings.Contains(out+err.Error(), pw) {
		t.Fatalf("--yes: err=%v out=%s", err, out)
	}

	// 6543 is refused even in dry-run.
	t.Setenv("DB_INDEX", "postgresql://u:"+pw+"@db.example.com:6543/postgres")
	out, err = runErr(t, "", "export", "--to-postgres")
	if err == nil || !strings.Contains(err.Error(), "6543") || strings.Contains(out+err.Error(), pw) {
		t.Fatalf("6543: err=%v out=%s", err, out)
	}

	// Missing URL.
	t.Setenv("DB_INDEX", "")
	if _, err = runErr(t, "", "export", "--to-postgres"); err == nil || !strings.Contains(err.Error(), "DB_INDEX") {
		t.Fatalf("missing url: %v", err)
	}
}
