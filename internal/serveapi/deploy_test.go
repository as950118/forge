package serveapi

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Static checks of the deployment files. They do NOT build the image: Docker was
// not available when this was written, so a real image build is unverified.

func readRoot(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDockerfileStatic(t *testing.T) {
	df := readRoot(t, "Dockerfile")
	var lines []string
	for _, l := range strings.Split(df, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	joined := strings.Join(lines, "\n")
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "FROM ") {
			n++
		}
	}
	if n < 2 {
		t.Errorf("multi-stage build expected, FROM count %d", n)
	}
	for _, want := range []string{"CGO_ENABLED=0", "go build", "USER 65532", `ENTRYPOINT ["/usr/local/bin/forge", "serve-api"]`, "distroless/static", "COPY --from=build /out/forge"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	if strings.Contains(joined, "USER root") || strings.Contains(joined, "USER 0") {
		t.Error("runs as root")
	}
	// the last USER must be a non-root numeric id
	us := regexp.MustCompile(`(?m)^USER (\S+)`).FindAllStringSubmatch(joined, -1)
	if len(us) == 0 || us[len(us)-1][1] == "0" || strings.HasPrefix(us[len(us)-1][1], "0:") {
		t.Errorf("USER: %v", us)
	}
	// no secrets baked in: only PORT may be set with ENV, no ARG/ENV for URL or token
	for _, l := range lines {
		u := strings.ToUpper(l)
		if (strings.HasPrefix(l, "ENV ") || strings.HasPrefix(l, "ARG ")) && (strings.Contains(u, "FORGE_EXPORT_URL") || strings.Contains(u, "TOKEN") || strings.Contains(u, "FORGE_PG_CA") || strings.Contains(u, "PASSWORD") || strings.Contains(u, "SECRET")) {
			t.Errorf("secret-looking ENV/ARG: %s", l)
		}
	}
	// every COPY source (build context) exists and is not excluded by .dockerignore
	ign := readRoot(t, ".dockerignore")
	for _, l := range lines {
		if !strings.HasPrefix(l, "COPY ") || strings.Contains(l, "--from=") {
			continue
		}
		f := strings.Fields(l)[1:]
		for _, src := range f[:len(f)-1] {
			if _, err := os.Stat("../../" + src); err != nil {
				t.Errorf("COPY source %s: %v", src, err)
			}
			for _, il := range strings.Split(ign, "\n") {
				if strings.TrimSpace(il) == strings.TrimPrefix(src, "./") {
					t.Errorf("COPY source %s is in .dockerignore", src)
				}
			}
		}
	}
	for _, d := range []string{".git", ".company-os", ".env", ".env.*", "dist", "graphify-out", ".agents", ".codex"} {
		found := false
		for _, il := range strings.Split(ign, "\n") {
			if strings.TrimSpace(il) == d {
				found = true
			}
		}
		if !found {
			t.Errorf(".dockerignore lacks %s", d)
		}
	}
}

func TestRenderBlueprintStatic(t *testing.T) {
	y := readRoot(t, "render.yaml")
	for _, want := range []string{"type: web", "runtime: docker", "plan: free", "region: singapore", "healthCheckPath: /health\n", "autoDeploy: true"} {
		if !strings.Contains(y, want) {
			t.Errorf("render.yaml lacks %q", want)
		}
	}
	for key, attr := range map[string]string{"FORGE_API_TOKEN": "generateValue: true", "FORGE_EXPORT_URL": "sync: false", "FORGE_PG_CA": "sync: false"} {
		re := regexp.MustCompile(`- key: ` + key + `\n\s+` + regexp.QuoteMeta(attr))
		if !re.MatchString(y) {
			t.Errorf("render.yaml: %s without %s", key, attr)
		}
	}
	if strings.Contains(y, "postgresql://") || regexp.MustCompile(`value:`).MatchString(y) {
		t.Error("render.yaml must not hold values")
	}
}
