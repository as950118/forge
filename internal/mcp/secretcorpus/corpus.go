// Package secretcorpus is the regression corpus of the secret heuristics
// (TASK-0026). Every "secret" is FAKE: it is generated at call time from a fixed
// seed with provider-like shapes, so no real credential is stored in the
// repository and the corpus is identical on every run. Only tests import it.
package secretcorpus

import (
	"fmt"
	"math/rand"
	"strings"
)

// Contexts an entry applies to.
const (
	CtxArg    = 1 << iota // mcp add / tool add argv element(s)
	CtxEnv                // mcp add --env KEY=VALUE (Key, Value)
	CtxHeader             // mcp add --header KEY=VALUE (Key, Value)
	CtxURL                // mcp add --url Value
	CtxText               // display / masking of free text (ledger.MaskText)
)

// Entry is one corpus value.
type Entry struct {
	Kind   string
	Key    string   // env/header key (neutral name unless the entry is about the key)
	Value  string   // the value (arg, env/header value, URL or text)
	Args   []string // argv when the entry is a multi-element argument list (CtxArg)
	Secret bool     // fake credential: must be refused / masked
	Ctx    int
	// Residual marks a fake secret that is NOT refused by design (documented in
	// docs/security.md): it is measured separately and never counted as recall.
	Residual bool
}

// ArgVec returns the argv of an entry.
func (e Entry) ArgVec() []string {
	if e.Args != nil {
		return e.Args
	}
	return []string{e.Value}
}

type gen struct{ r *rand.Rand }

const (
	lower  = "abcdefghijklmnopqrstuvwxyz"
	upper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digits = "0123456789"
	alnum  = lower + upper + digits
	b64u   = alnum + "-_"
	lowdig = lower + digits
	hexs   = "0123456789abcdef"
	upnum  = upper + digits
)

func (g *gen) from(set string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = set[g.r.Intn(len(set))]
	}
	return string(b)
}

// mixed returns a random string that contains letters of both cases and digits.
func (g *gen) mixed(set string, n int) string {
	for {
		s := g.from(set, n)
		if strings.ContainsAny(s, digits) && strings.ContainsAny(s, lower) && strings.ContainsAny(s, upper) {
			return s
		}
	}
}

func (g *gen) uuid() string {
	h := g.from(hexs, 32)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// All returns the whole corpus (fake secrets, residual/baseline-miss fake
// secrets and normal values).
func All() []Entry {
	g := &gen{r: rand.New(rand.NewSource(26))}
	var out []Entry
	out = append(out, g.secrets()...)
	out = append(out, g.residual()...)
	out = append(out, g.normals()...)
	for i := range out {
		if out[i].Key == "" {
			out[i].Key = "SERVICE_ID"
		}
	}
	return out
}

const anyCtx = CtxArg | CtxEnv | CtxHeader | CtxText

func (g *gen) secrets() []Entry {
	var out []Entry
	add := func(kind, v string, ctx int) {
		out = append(out, Entry{Kind: kind, Key: "SERVICE_ID", Value: v, Secret: true, Ctx: ctx})
	}
	rep := func(n int, kind string, ctx int, f func() string) {
		for i := 0; i < n; i++ {
			add(kind, f(), ctx)
		}
	}
	rep(4, "openai", anyCtx, func() string { return "sk-" + g.from(alnum, 48) })
	rep(3, "openai-proj", anyCtx, func() string { return "sk-proj-" + g.from(b64u, 74) })
	rep(3, "anthropic", anyCtx, func() string { return "sk-ant-api03-" + g.from(b64u, 93) })
	for _, p := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
		rep(2, "github-"+p, anyCtx, func() string { return p + g.from(alnum, 36) })
	}
	rep(3, "github-pat", anyCtx, func() string { return "github_pat_" + g.from(alnum, 22) + "_" + g.from(alnum, 59) })
	rep(3, "aws-akia", anyCtx, func() string { return "AKIA" + g.from(upnum, 16) })
	rep(2, "aws-asia", anyCtx, func() string { return "ASIA" + g.from(upnum, 16) })
	for i := 0; i < 3; i++ {
		out = append(out, Entry{Kind: "aws-secret-40", Key: "SERVICE_ID", Value: g.from(alnum+"/+", 40), Secret: true, Ctx: anyCtx})
	}
	rep(3, "google", anyCtx, func() string { return "AIza" + g.from(b64u, 35) })
	for _, p := range []string{"xoxb-", "xoxa-", "xoxp-", "xoxr-", "xoxs-"} {
		rep(2, "slack-"+p, anyCtx, func() string {
			return p + g.from(digits, 12) + "-" + g.from(digits, 12) + "-" + g.from(alnum, 24)
		})
	}
	for _, p := range []string{"sk_live_", "rk_live_", "sk_test_"} {
		rep(3, "stripe-"+p, anyCtx, func() string { return p + g.from(alnum, 24) })
	}
	rep(3, "gitlab", anyCtx, func() string { return "glpat-" + g.from(b64u, 20) })
	rep(3, "npm", anyCtx, func() string { return "npm_" + g.from(alnum, 36) })
	rep(4, "jwt", anyCtx, func() string {
		return "eyJ" + g.from(b64u, 34) + ".eyJ" + g.from(b64u, 60) + "." + g.from(b64u, 43)
	})
	rep(5, "bearer", anyCtx, func() string { return "Bearer " + g.mixed(alnum, 40) })
	rep(2, "basic", anyCtx, func() string { return "Basic " + g.from(alnum, 28) })
	rep(4, "hex32-apikey", anyCtx, func() string { return g.from(hexs, 32) })
	rep(3, "hex40-nocontext", anyCtx, func() string { return g.from(hexs, 40) })
	rep(3, "base64-40", anyCtx, func() string { return g.mixed(alnum, 40) })
	rep(3, "opaque-28", anyCtx, func() string { return g.mixed(alnum, 28) })

	// URL query / path / fragment tokens (a URL is never a file path)
	for i := 0; i < 4; i++ {
		add("url-path-token", "https://h.example/v1/"+g.mixed(alnum, 32)+"/mcp", CtxURL|CtxArg|CtxText)
	}
	for i := 0; i < 3; i++ {
		add("url-query-token", "https://h.example/mcp?token="+g.mixed(alnum, 32), CtxURL|CtxArg|CtxText)
	}
	for i := 0; i < 3; i++ {
		add("url-query-value", "https://h.example/mcp?k="+g.mixed(alnum, 32), CtxURL|CtxArg|CtxText)
	}
	add("url-fragment", "https://h.example/mcp#"+g.mixed(alnum, 32), CtxURL|CtxArg)
	add("url-prefix-path", "https://h.example/"+"sk-"+g.from(alnum, 40)+"/mcp", CtxURL|CtxArg|CtxText)
	add("url-userinfo", "https://user:"+g.from(alnum, 12)+"@h.example/mcp", CtxURL|CtxArg|CtxText)
	add("url-userinfo", "postgres://app:"+g.from(alnum, 14)+"@db.example/app", CtxArg|CtxText)
	add("file-url-token", "file:///tmp/x/"+g.mixed(alnum, 32)+"/a", CtxArg|CtxText)
	add("url-file-like-path", "https://h.example/api/"+g.mixed(alnum, 30)+"/v1/tools/list", CtxURL|CtxArg)

	// key=value
	for _, k := range []string{"token", "password", "API_KEY", "client_secret", "access_token"} {
		add("keyvalue-"+k, k+"="+g.from(alnum, 20), CtxArg|CtxText)
	}
	add("flag-token", "--token="+g.from(alnum, 20), CtxArg|CtxText)
	add("flag-apikey", "--api-key="+g.from(alnum, 20), CtxArg|CtxText)
	out = append(out, Entry{Kind: "flag-token-next", Args: []string{"--token", g.from(alnum, 20)}, Secret: true, Ctx: CtxArg})
	out = append(out, Entry{Kind: "flag-password-next", Args: []string{"--password", g.from(alnum, 12)}, Secret: true, Ctx: CtxArg})
	out = append(out, Entry{Kind: "keyvalue-hexname", Value: "commit=" + g.from(hexs, 32), Secret: true, Ctx: CtxArg | CtxText}) // 32 hex is not a commit
	out = append(out, Entry{Kind: "digest-wrong-len", Value: "sha256:" + g.from(hexs, 40), Secret: true, Ctx: CtxArg | CtxText})
	out = append(out, Entry{Kind: "digest-not-hex", Value: "sha256:" + g.mixed(alnum, 64), Secret: true, Ctx: CtxArg | CtxText})
	out = append(out, Entry{Kind: "flag-ref-wrong-len", Args: []string{"--ref", g.from(hexs, 32)}, Secret: true, Ctx: CtxArg})
	out = append(out, Entry{Kind: "flag-other-next-hex", Args: []string{"--name", g.from(hexs, 40)}, Secret: true, Ctx: CtxArg})
	out = append(out, Entry{Kind: "env-commit-wrong-len", Key: "GIT_COMMIT", Value: g.from(hexs, 32), Secret: true, Ctx: CtxEnv | CtxHeader})
	out = append(out, Entry{Kind: "env-tokenish-key-hex40", Key: "COMMIT_TOKEN", Value: g.from(hexs, 40), Secret: true, Ctx: CtxEnv | CtxHeader})

	// identifiers that merely look like words
	out = append(out, Entry{Kind: "digit-heavy-dashed", Value: "a1b2-c3d4-e5f6-g7h8-i9j0-k1l2", Secret: true, Ctx: anyCtx})
	out = append(out, Entry{Kind: "random-underscored", Value: g.mixed(alnum, 12) + "_" + g.mixed(alnum, 12) + "_" + g.mixed(alnum, 12), Secret: true, Ctx: anyCtx})
	out = append(out, Entry{Kind: "random-dashed", Value: g.mixed(alnum, 8) + "-" + g.mixed(alnum, 8) + "-" + g.mixed(alnum, 8) + "-" + g.mixed(alnum, 8), Secret: true, Ctx: anyCtx})

	// secrets disguised as paths (known prefix anywhere in a component)
	for i := 0; i < 3; i++ {
		add("disguised-tmp-sk", "/tmp/sk-"+g.from(alnum, 48), CtxArg|CtxEnv|CtxText)
	}
	for i := 0; i < 3; i++ {
		add("disguised-mid-ghp", "/opt/b/ghp_"+g.from(alnum, 36)+"/c", CtxArg|CtxEnv|CtxText)
	}
	for i := 0; i < 2; i++ {
		add("disguised-jwt-component", "/srv/app/eyJ"+g.from(b64u, 34)+".eyJ"+g.from(b64u, 40)+"."+g.from(b64u, 43)+"/x", CtxArg|CtxEnv|CtxText)
	}
	for i := 0; i < 2; i++ {
		add("disguised-config-akia", "~/.config/AKIA"+g.from(upnum, 16), CtxArg|CtxEnv|CtxText)
	}
	add("disguised-rel-glpat", "./a/glpat-"+g.from(b64u, 20)+"/b", CtxArg|CtxEnv|CtxText)
	add("disguised-parent-npm", "../x/npm_"+g.from(alnum, 36)+"/y", CtxArg|CtxEnv|CtxText)
	add("disguised-win-npm", `C:\Users\u\npm_`+g.from(alnum, 36)+`\x`, CtxArg|CtxEnv|CtxText)
	add("disguised-win-ghp", `D:\work\ghp_`+g.from(alnum, 36)+`\x`, CtxArg|CtxEnv|CtxText)
	add("disguised-prefix-midcomponent", "/var/folders/gr/"+g.from(lowdig, 28)+"/T/foo-sk-"+g.from(alnum, 40)+"/x", CtxArg|CtxEnv|CtxText)
	add("disguised-mid-xox", "/opt/x/xoxb-"+g.from(digits, 12)+"-"+g.from(digits, 12)+"-"+g.from(alnum, 24)+"/y", CtxArg|CtxEnv|CtxText)
	add("disguised-token-kv-component", "/opt/app/token="+g.from(alnum, 20)+"/x", CtxArg|CtxEnv|CtxText)
	add("disguised-password-kv-component", "/opt/app/password="+g.from(alnum, 14)+"/x", CtxArg|CtxEnv|CtxText)
	add("disguised-sk-stripe", "/etc/app/sk_live_"+g.from(alnum, 24)+"/y", CtxArg|CtxEnv|CtxText)
	add("disguised-flagged-path", "--out=/etc/app/sk-"+g.from(alnum, 40)+"/y", CtxArg|CtxText)
	// URL-path-shaped value given as a bare argument: its first component is not a
	// known root, so it is not a file path
	for i := 0; i < 3; i++ {
		add("bare-urlpath", "/v1/"+g.mixed(alnum, 32)+"/mcp", CtxArg|CtxEnv|CtxText)
	}
	add("bare-random-components", "/"+g.mixed(alnum, 30)+"/"+g.mixed(alnum, 30), CtxArg|CtxEnv|CtxText)
	// more provider shapes in a path (prefix list shared by every context)
	for _, p := range []string{"whsec_", "rk_live_", "rk_test_", "sk_test_", "pk_live_", "xapp-", "shpat_", "dop_v1_", "glsa_"} {
		set := alnum
		n := 32
		if p == "shpat_" || p == "dop_v1_" {
			set, n = hexs, 32
		}
		add("path-"+p, "/opt/app/"+p+g.from(set, n)+"/x", CtxArg|CtxEnv|CtxText)
	}
	add("path-twilio-sk", "/tmp/SK"+g.from(hexs, 32), CtxArg|CtxEnv|CtxText)
	add("path-sendgrid", "/tmp/SG."+g.from(b64u, 22)+"."+g.from(b64u, 43), CtxArg|CtxEnv|CtxText)
	add("path-hf", "/home/u/hf_"+g.from(alnum, 34), CtxArg|CtxEnv|CtxText)
	add("path-ya29", "/home/u/ya29."+g.from(b64u, 60), CtxArg|CtxEnv|CtxText)
	// case variants and glued prefixes
	add("path-upper-GHP", "/tmp/GHP_"+g.from(alnum, 36), CtxArg|CtxEnv|CtxText)
	add("path-mixed-Sk", "/tmp/Sk-"+g.from(alnum, 48), CtxArg|CtxEnv|CtxText)
	add("path-upper-SK", "/tmp/SK-"+g.from(alnum, 48), CtxArg|CtxEnv|CtxText)
	add("path-upper-XOXB", "/tmp/XOXB-"+g.from(digits, 12)+"-"+g.from(digits, 12)+"-"+g.from(alnum, 24), CtxArg|CtxEnv|CtxText)
	add("path-upper-EYJ", "/tmp/EYJ"+g.from(b64u, 34)+".EYJ"+g.from(b64u, 40)+"."+g.from(b64u, 43), CtxArg|CtxEnv|CtxText)
	add("path-glued-ghp", "/tmp/prefixghp_"+g.from(alnum, 36), CtxArg|CtxEnv|CtxText)
	add("path-glued-sk_live", "/tmp/mysk_live_"+g.from(alnum, 24), CtxArg|CtxEnv|CtxText)
	add("path-glued-glpat", "/tmp/foo-glpat-"+g.from(b64u, 20)+"/x", CtxArg|CtxEnv|CtxText)
	// AWS secret key shape starting with "/" (env value or list element)
	for i := 0; i < 4; i++ {
		var v string
		for {
			v = "/" + g.from(alnum+"/+", 39)
			if strings.ContainsAny(v[1:], "+") && strings.ContainsAny(v, digits) && strings.ContainsAny(v, upper) && strings.ContainsAny(v, lower) {
				break
			}
		}
		add("aws-secret-slash", v, CtxArg|CtxEnv|CtxText)
	}
	// dictionary words with digits mixed in (a passphrase-like secret)
	words := []string{"correct", "horse", "battery", "staple", "orange", "monkey", "guitar", "window", "silver", "dragon", "planet", "winter"}
	for i := 0; i < 8; i++ {
		var ps []string
		for j := 0; j < 4; j++ {
			w := words[g.r.Intn(len(words))]
			if g.r.Intn(2) == 0 {
				w += g.from(digits, 1+g.r.Intn(2))
			}
			ps = append(ps, w)
		}
		sep := "-"
		if i%2 == 1 {
			sep = "_"
		}
		v := strings.Join(ps, sep)
		if !strings.ContainsAny(v[:len(v)-1], digits) {
			v = ps[0] + g.from(digits, 2) + sep + strings.Join(ps[1:], sep)
		}
		add("wordlike-digits-passphrase", v, CtxArg|CtxEnv|CtxText)
	}
	add("disguised-bearer-in-path", "/api/bearer "+g.mixed(alnum, 30), CtxArg|CtxText)
	return out
}

// residual lists fake secrets that the path/identifier exceptions let through on
// purpose (docs/security.md M5 residual risk). They are measured separately.
func (g *gen) residual() []Entry {
	return []Entry{
		{Kind: "residual-path-component-random40", Value: "/home/u/.config/" + g.mixed(alnum, 40), Secret: true, Residual: true, Ctx: CtxArg | CtxEnv | CtxText},
		{Kind: "residual-path-component-hex32", Value: "/home/u/.cache/" + g.from(hexs, 32) + "/x", Secret: true, Residual: true, Ctx: CtxArg | CtxEnv | CtxText},
		{Kind: "residual-root-random40", Value: "/tmp/" + g.mixed(alnum, 40), Secret: true, Residual: true, Ctx: CtxArg | CtxEnv | CtxText},
		{Kind: "residual-commit-key-api-hex40", Value: "commit=" + g.from(hexs, 40), Secret: true, Residual: true, Ctx: CtxArg | CtxText},
		{Kind: "residual-ref-flag-api-hex40", Args: []string{"--ref", g.from(hexs, 40)}, Secret: true, Residual: true, Ctx: CtxArg},
		{Kind: "residual-env-commit-api-hex64", Key: "GIT_COMMIT", Value: g.from(hexs, 64), Secret: true, Residual: true, Ctx: CtxEnv | CtxHeader},
		{Kind: "residual-sha256-label-hex64", Value: "sha256:" + g.from(hexs, 64), Secret: true, Residual: true, Ctx: CtxArg | CtxEnv | CtxText},
	}
}

func (g *gen) normals() []Entry {
	var out []Entry
	add := func(kind, v string, ctx int) {
		out = append(out, Entry{Kind: kind, Key: "SERVICE_ID", Value: v, Ctx: ctx})
	}
	pathCtx := CtxArg | CtxEnv | CtxText
	for i := 0; i < 8; i++ {
		add("path-macos-tmpdir", fmt.Sprintf("/var/folders/%s/%s/T/forge-demo.%s/src/variant", g.from(lowdig, 2), g.from(lowdig, 28), g.from(lowdig, 6)), pathCtx)
	}
	add("path-macos-tmpdir-real", "/var/folders/gr/q4n6pgs90gx_jny4zh7p58dw0000gn/T/forge-demo.abc123/src/variant", pathCtx)
	for i := 0; i < 3; i++ {
		add("path-private-var", fmt.Sprintf("/private/var/folders/%s/%s/T/x/y", g.from(lowdig, 2), g.from(lowdig, 28)), pathCtx)
	}
	add("path-private-tmp", "/private/tmp/forge-test/a.txt", pathCtx)
	add("path-tmp", "/tmp/forge-demo.1a2b3c/x/y", pathCtx)
	for i := 0; i < 4; i++ {
		add("path-npx", "~/.npm/_npx/"+g.from(hexs, 16)+"/node_modules/.bin/server", pathCtx)
	}
	for i := 0; i < 2; i++ {
		add("path-npx-abs", "/Users/me/.npm/_npx/"+g.from(hexs, 16)+"/node_modules/@scope/pkg/dist/index.js", pathCtx)
	}
	for i := 0; i < 4; i++ {
		add("path-pnpm", "/home/u/proj/node_modules/.pnpm/typescript@5.4.5_"+g.from(lowdig, 26)+"/node_modules/typescript/bin/tsc", pathCtx)
	}
	add("path-pnpm-rel", "./node_modules/.pnpm/@modelcontextprotocol+server-filesystem@0.6.2_"+g.from(lowdig, 30)+"/node_modules/x", pathCtx)
	for i := 0; i < 4; i++ {
		add("path-uuid-dir", "/var/lib/app/"+g.uuid()+"/logs/out.log", pathCtx)
	}
	for i := 0; i < 3; i++ {
		add("path-sha1-dir", "/var/cache/app/"+g.from(hexs, 40)+"/obj", pathCtx)
	}
	for i := 0; i < 2; i++ {
		add("path-sha256-dir", "/srv/store/"+g.from(hexs, 64)+"/layer.tar", pathCtx)
	}
	add("path-git-object", "./.git/objects/ab/"+g.from(hexs, 38), pathCtx)
	add("path-pnpm-store", "/home/u/.local/share/pnpm_store/v3/files/"+g.from(hexs, 2)+"/"+g.from(hexs, 40), pathCtx)
	add("path-node-modules-rel", "node_modules/.pnpm/@modelcontextprotocol+server-filesystem@0.6.2_"+g.from(lowdig, 30)+"/node_modules/x", pathCtx)
	add("path-long-home", "/Users/heonjinjeong/IdeaProjects/toyproject/forge/internal/mcp/secrets.go", pathCtx)
	add("path-long-names", "/home/someone/projects/my-long-directory-name-for-tests/subdir/another_directory_name_2024/file.json", pathCtx)
	add("path-home", "~/projects/forge/src/main.go", pathCtx)
	add("path-rel", "./scripts/run.sh", pathCtx)
	add("path-parent", "../shared/config/app.yaml", pathCtx)
	add("path-colon-mount", "/var/folders/gr/"+g.from(lowdig, 28)+"/T/data:/data", pathCtx)
	for i := 0; i < 3; i++ {
		add("path-windows", `C:\Users\heonjin\AppData\Local\Temp\forge-`+g.from(lowdig, 8)+`\src\variant.py`, pathCtx)
	}
	add("path-windows-fwd", "D:/work/projects/forge/build/output", pathCtx)
	add("path-flag-eq-config", "--config=/var/folders/gr/"+g.from(lowdig, 28)+"/T/forge/cfg.json", CtxArg|CtxText)
	add("path-flag-eq-out", "--out=/Users/me/.npm/_npx/"+g.from(hexs, 16)+"/out", CtxArg|CtxText)
	add("path-text-sentence", "cwd is /var/folders/gr/"+g.from(lowdig, 28)+"/T/forge-demo.abc123/src now", CtxText)
	add("path-text-quoted", `"/var/folders/gr/`+g.from(lowdig, 28)+`/T/forge-demo.abc123/src",`, CtxText)

	// digests and commit ids with a clear context
	for i := 0; i < 3; i++ {
		add("digest-sha256", "sha256:"+g.from(hexs, 64), CtxArg|CtxEnv|CtxText)
	}
	for _, img := range []string{"ghcr.io/org/image", "postgres", "docker.io/library/redis"} {
		add("digest-image", img+"@sha256:"+g.from(hexs, 64), CtxArg|CtxEnv|CtxText)
	}
	add("digest-image-tag", "ghcr.io/org/image:1.2.3@sha256:"+g.from(hexs, 64), CtxArg|CtxEnv|CtxText)
	for _, f := range []string{"--ref", "--rev", "--sha256", "--commit", "--digest"} {
		n := 40
		if f == "--sha256" || f == "--digest" {
			n = 64
		}
		out = append(out, Entry{Kind: "flag-hash-next", Args: []string{f, g.from(hexs, n)}, Ctx: CtxArg})
	}
	out = append(out, Entry{Kind: "flag-hash-next-text", Value: "git checkout --ref " + g.from(hexs, 40), Ctx: CtxText})
	for _, k := range []string{"commit", "rev", "sha", "ref", "checksum", "hash", "digest"} {
		add("keyvalue-"+k, k+"="+g.from(hexs, 40), CtxArg|CtxText)
	}
	add("flag-eq-ref", "--ref="+g.from(hexs, 40), CtxArg|CtxText)
	add("keyvalue-sha256-64", "checksum="+g.from(hexs, 64), CtxArg|CtxText)
	for _, k := range []string{"GIT_COMMIT", "COMMIT_SHA", "BUILD_REV", "IMAGE_DIGEST"} {
		out = append(out, Entry{Kind: "env-" + k, Key: k, Value: g.from(hexs, 40), Ctx: CtxEnv | CtxHeader})
	}
	out = append(out, Entry{Kind: "env-digest-64", Key: "IMAGE_DIGEST", Value: g.from(hexs, 64), Ctx: CtxEnv | CtxHeader})
	for i := 0; i < 4; i++ {
		add("uuid", g.uuid(), anyCtx)
	}
	add("uuid-upper", strings.ToUpper(g.uuid()), anyCtx)

	// long snake/kebab identifiers made of short words
	for _, v := range []string{
		"my_snake_case_identifier_name_v2_final", "release-candidate-build-for-staging-v2", "disk_usage_report_generator_service",
		"my-super-long-package-name-here-v3", "mcp-server-sequential-thinking-for-everyone", "task-runner-for-all-the-things",
		"database_migration_runner_tool_2024", "user_profile_settings_manager_v1", "weekly-status-report-generator-tool",
		"internal-tools-metrics-collector-job", "very_long_function_name_for_testing_purposes_v2",
	} {
		add("identifier", v, anyCtx)
	}
	for _, v := range []string{"1.2.3", "v20.11.1", "1.2.3-beta.4+build.5", "2024.10.24", "v1", "latest", "ko_KR.UTF-8", "C.UTF-8", "UTF-8"} {
		add("version-locale", v, anyCtx)
	}
	for _, v := range []string{
		"https://example.com/mcp", "https://example.com/v1/mcp?lang=ko&page=2", "http://127.0.0.1:8080/sse",
		"https://api.example.com/v2/servers/my-long-server-name-docs/mcp", "https://github.com/org/repo/tree/main/docs",
		"https://raw.githubusercontent.com/org/repo/main/README.md", "https://example.com/a/b/c?x=1#top",
	} {
		add("url", v, CtxURL|CtxArg|CtxText)
	}
	add("url-postgres", "postgres://localhost:5432/app", CtxArg|CtxText)
	for _, v := range []string{
		"--verbose", "--port=8080", "--log-level=debug", "--max-old-space-size=4096", "-y", "--timeout=30s",
		"--no-auth", "--model=gpt-4o-mini", "--password-stdin",
	} {
		add("flag", v, CtxArg|CtxText)
	}
	// flags whose names contain a secret word: the inline display masker masks them
	// by name (conservative, unrelated to value patterns), so registration only.
	for _, v := range []string{"--token-file=/etc/token", "--max-tokens=1000"} {
		add("flag-secretword-name", v, CtxArg)
	}
	for _, v := range []string{
		"debug", "true", "hello", "application/json", "text/event-stream", "info,warn,error", "@modelcontextprotocol/server-filesystem",
		"user@example.com", "localhost:8080", "8080:80", "main", "feature/new-thing", "HEAD~3", "node", "python3", "s.py", "x",
		"123456789012345678901234567890", "basic configuration needed here",
	} {
		add("plain", v, anyCtx)
	}
	return out
}
