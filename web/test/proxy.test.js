import test from 'node:test';
import assert from 'node:assert/strict';
import { handle } from '../api/_lib/handler.js';
import { resolveBase } from '../api/_lib/upstream.js';
import { goodAuth, baseEnv, fakeUpstream, SUMMARY, TOKEN, PASSWORD } from '../testutil/helpers.js';

const call = (env, url, headers = { authorization: goodAuth }) => handle({ method: 'GET', url, headers }, env);
const dump = (r) => JSON.stringify(r.headers) + String(r.body);

test('proxies JSON unchanged, sends Bearer only upstream, token absent from response', async () => {
  const up = await fakeUpstream((req, res) => {
    res.setHeader('content-type', 'application/json');
    res.end(JSON.stringify(SUMMARY));
  });
  try {
    const env = baseEnv({ FORGE_API_URL: up.url });
    const r = await call(env, '/data/summary');
    assert.equal(r.status, 200);
    assert.deepEqual(JSON.parse(r.body), SUMMARY);
    assert.equal(up.seen[0].authorization, `Bearer ${TOKEN}`);
    assert.equal(up.seen[0].url, '/v1/summary');
    assert.ok(!dump(r).includes(TOKEN));
    assert.ok(!dump(r).includes(up.url));
    // static files never contain the token or env names that hold secrets
    for (const p of ['/', '/app.js', '/render.js']) assert.ok(!dump(await call(env, p)).includes(TOKEN));
  } finally { await up.close(); }
});

test('items and daily validate parameters before calling upstream', async () => {
  const up = await fakeUpstream((req, res) => res.end('[]'));
  try {
    const env = baseEnv({ FORGE_API_URL: up.url });
    assert.equal((await call(env, '/data/items?kind=mcp')).status, 200);
    assert.equal((await call(env, '/data/daily?days=7')).status, 200);
    assert.equal((await call(env, '/data/items?kind=../x')).status, 400);
    assert.equal((await call(env, '/data/daily?days=0')).status, 400);
    assert.equal((await call(env, '/data/daily?days=999')).status, 400);
    assert.equal((await call(env, '/data/daily?days=1e2')).status, 400);
    assert.deepEqual(up.seen.map((s) => s.url), ['/v1/items?kind=mcp', '/v1/daily?days=7']);
  } finally { await up.close(); }
});

test('upstream errors are generalized (no url, token, body)', async () => {
  const leak = `secret-internal-detail ${TOKEN} postgres://u:p@h/db`;
  const modes = {
    500: (req, res) => { res.statusCode = 500; res.end(leak); },
    401: (req, res) => { res.statusCode = 401; res.end(leak); },
    redirect: (req, res) => { res.statusCode = 302; res.setHeader('location', 'http://evil.example/'); res.end(); },
    notjson: (req, res) => res.end('<html>' + leak),
    echoToken: (req, res) => res.end(JSON.stringify({ t: TOKEN })),
    huge: (req, res) => res.end('"' + 'a'.repeat(5 * 1024 * 1024) + '"'),
    reset: (req, res) => req.socket.destroy(),
  };
  for (const [name, fn] of Object.entries(modes)) {
    const up = await fakeUpstream(fn);
    try {
      const r = await call(baseEnv({ FORGE_API_URL: up.url }), '/data/summary');
      assert.equal(r.status, 502, name);
      assert.deepEqual(JSON.parse(r.body), { error: 'upstream_error' }, name);
      const d = dump(r);
      for (const bad of [TOKEN, up.url, '127.0.0.1', 'secret-internal', 'postgres://']) assert.ok(!d.includes(bad), `${name} leaked ${bad}`);
    } finally { await up.close(); }
  }
});

test('upstream 404 maps to no_data, timeout maps to 504', async () => {
  const up = await fakeUpstream((req, res) => { res.statusCode = 404; res.end('{"error":"x"}'); });
  try {
    const r = await call(baseEnv({ FORGE_API_URL: up.url }), '/data/summary');
    assert.equal(r.status, 404);
    assert.deepEqual(JSON.parse(r.body), { error: 'no_data' });
  } finally { await up.close(); }
  const slow = await fakeUpstream(() => { /* never answers */ });
  try {
    const orig = AbortSignal.timeout;
    AbortSignal.timeout = () => orig.call(AbortSignal, 150);
    try {
      const r = await call(baseEnv({ FORGE_API_URL: slow.url }), '/data/summary');
      assert.equal(r.status, 504);
      assert.deepEqual(JSON.parse(r.body), { error: 'upstream_timeout' });
      assert.ok(!dump(r).includes(TOKEN));
    } finally { AbortSignal.timeout = orig; }
  } finally { await slow.close(); }
});

test('FORGE_API_URL: https only; loopback http only with NODE_ENV=development', () => {
  assert.equal(resolveBase({ FORGE_API_URL: 'https://forge-api.onrender.com' }), 'https://forge-api.onrender.com');
  assert.equal(resolveBase({ FORGE_API_URL: 'http://127.0.0.1:8080', NODE_ENV: 'development' }), 'http://127.0.0.1:8080');
  const bad = [
    ['http://127.0.0.1:8080', 'production'], ['http://127.0.0.1:8080', undefined],
    ['http://example.com', 'development'], ['ftp://x.com', 'development'],
    ['https://u:p@x.com', 'production'], ['https://x.com/path', 'production'],
    ['https://x.com/?a=1', 'production'], ['not a url', 'production'], ['', 'production'],
  ];
  for (const [u, e] of bad) assert.equal(resolveBase({ FORGE_API_URL: u, NODE_ENV: e }), null, `${u} ${e}`);
  assert.equal(resolveBase({}), null);
});

test('missing FORGE_API_URL or FORGE_API_TOKEN gives generic 503 on data routes only', async () => {
  const r1 = await call(baseEnv({ FORGE_API_URL: undefined }), '/data/summary');
  assert.equal(r1.status, 503);
  assert.deepEqual(JSON.parse(r1.body), { error: 'not_configured' });
  const r2 = await call(baseEnv({ FORGE_API_URL: 'https://x.example', FORGE_API_TOKEN: '' }), '/data/summary');
  assert.equal(r2.status, 503);
  assert.equal((await call(baseEnv({ FORGE_API_URL: undefined }), '/')).status, 200);
});

test('response headers: security, no-store, CSP without inline script, content types', async () => {
  for (const [url, ct] of [['/', 'text/html'], ['/app.js', 'text/javascript'], ['/style.css', 'text/css'], ['/data/summary', 'application/json'], ['/nope', 'text/plain']]) {
    const r = await call(baseEnv({ FORGE_API_URL: undefined }), url);
    assert.ok(r.headers['content-type'].startsWith(ct), url);
    assert.equal(r.headers['x-content-type-options'], 'nosniff');
    assert.equal(r.headers['x-frame-options'], 'DENY');
    assert.equal(r.headers['referrer-policy'], 'no-referrer');
    assert.match(r.headers['cache-control'], /private/);
    assert.match(r.headers['cache-control'], /no-store/);
    const csp = r.headers['content-security-policy'];
    assert.match(csp, /default-src 'none'/);
    assert.match(csp, /script-src 'self'/);
    assert.match(csp, /frame-ancestors 'none'/);
    assert.ok(!/unsafe-inline|unsafe-eval|\*/.test(csp));
  }
  const unauth = await handle({ method: 'GET', url: '/', headers: {} }, baseEnv());
  assert.equal(unauth.headers['x-frame-options'], 'DENY');
  const off = await handle({ method: 'GET', url: '/', headers: {} }, { NODE_ENV: 'production' });
  assert.equal(off.status, 503);
  assert.equal(off.headers['x-frame-options'], 'DENY');
  assert.ok(!off.body.includes(PASSWORD));
});
