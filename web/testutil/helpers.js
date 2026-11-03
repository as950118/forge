import http from 'node:http';

export const PASSWORD = 'correct-horse-battery';
export const TOKEN = 'fake-upstream-token-0123456789';
export const basic = (user, pass) => 'Basic ' + Buffer.from(`${user}:${pass}`).toString('base64');
export const goodAuth = basic('forge', PASSWORD);

export const baseEnv = (extra = {}) => ({
  DASHBOARD_PASSWORD: PASSWORD,
  FORGE_API_TOKEN: TOKEN,
  NODE_ENV: 'development',
  ...extra,
});

// Fake Render API on 127.0.0.1 (ephemeral port). `behavior(req, res)` decides the reply.
export async function fakeUpstream(behavior) {
  const seen = [];
  const server = http.createServer((req, res) => {
    seen.push({ url: req.url, authorization: req.headers.authorization });
    behavior(req, res);
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    seen,
    close: () => new Promise((r) => { server.closeAllConnections(); server.close(r); }),
  };
}

export const SUMMARY = {
  meta: { schema_version: 1, generated_at: '2026-11-03T01:02:03Z', forge_version: 'v0.0.1', day_basis: 'UTC' },
  items: [{ kind: 'skill', server: '', name: 'demo', calls: 3, ok: 2, failed: 1, unknown: 0, success_rate: 0.6667, last_call: '2026-11-03T00:00:00Z' }],
  daily: [{ day: '2026-11-03', kind: 'skill', calls: 3, ok: 2, failed: 1, unknown: 0 }],
};
