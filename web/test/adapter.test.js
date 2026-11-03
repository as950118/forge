import test from 'node:test';
import assert from 'node:assert/strict';
import handler from '../api/index.js';
import { goodAuth, PASSWORD } from '../testutil/helpers.js';

function run(headers, url = '/') {
  return new Promise((resolve) => {
    const res = { headers: {}, setHeader(k, v) { this.headers[k.toLowerCase()] = v; }, end(body) { resolve({ status: this.statusCode, headers: this.headers, body: String(body) }); } };
    handler({ method: 'GET', url, headers }, res);
  });
}

test('Vercel adapter writes status, headers and body; fails closed without env', async () => {
  const saved = { ...process.env };
  try {
    delete process.env.DASHBOARD_PASSWORD;
    assert.equal((await run({ authorization: goodAuth })).status, 503);
    process.env.DASHBOARD_PASSWORD = PASSWORD;
    assert.equal((await run({})).status, 401);
    const ok = await run({ authorization: goodAuth });
    assert.equal(ok.status, 200);
    assert.equal(ok.headers['x-frame-options'], 'DENY');
    assert.match(ok.body, /<html/);
  } finally {
    process.env = saved;
  }
});
