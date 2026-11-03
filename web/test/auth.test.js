import test from 'node:test';
import assert from 'node:assert/strict';
import { handle } from '../api/_lib/handler.js';
import { PASSWORD, basic, goodAuth, baseEnv, TOKEN } from '../testutil/helpers.js';

const get = (headers = {}, url = '/', env = baseEnv(), method = 'GET') => handle({ method, url, headers }, env);

test('valid credentials pass for static files', async () => {
  for (const p of ['/', '/app.js', '/render.js', '/style.css']) {
    const r = await get({ authorization: goodAuth }, p);
    assert.equal(r.status, 200, p);
  }
});

test('no header, wrong scheme, empty or wrong password are 401', async () => {
  const cases = [
    {},
    { authorization: '' },
    { authorization: 'Bearer ' + PASSWORD },
    { authorization: 'Digest abc' },
    { authorization: 'Basic' },
    { authorization: 'Basic !!!notbase64' },
    { authorization: basic('forge', '') },
    { authorization: basic('forge', 'wrong-password-123') },
    { authorization: basic('admin', PASSWORD) },
    { authorization: 'Basic ' + Buffer.from('nocolon').toString('base64') },
    { authorization: basic('forge', PASSWORD + 'x') },
    { authorization: basic('forge', PASSWORD.slice(0, -1)) },
  ];
  for (const h of cases) {
    const r = await get(h);
    assert.equal(r.status, 401, JSON.stringify(h));
    assert.match(r.headers['www-authenticate'], /^Basic /);
    assert.ok(!r.body.includes(TOKEN) && !r.body.includes(PASSWORD));
  }
});

test('unauthenticated requests never reach data or static routes, any method', async () => {
  for (const method of ['GET', 'HEAD', 'POST', 'PUT', 'DELETE', 'OPTIONS']) {
    for (const url of ['/', '/data/summary', '/app.js', '/nope', '/api/index?p=/data/summary']) {
      assert.equal((await get({}, url, baseEnv(), method)).status, 401, `${method} ${url}`);
    }
  }
});

test('multiple Authorization headers (array) and comma-joined values are rejected', async () => {
  assert.equal((await get({ authorization: [goodAuth, goodAuth] })).status, 401);
  assert.equal((await get({ authorization: `${goodAuth}, ${goodAuth}` })).status, 401);
  assert.equal((await get({ authorization: goodAuth + ' ' })).status, 401);
});

test('very long Authorization input is rejected', async () => {
  assert.equal((await get({ authorization: basic('forge', 'a'.repeat(5000)) })).status, 401);
  assert.equal((await get({ authorization: 'Basic ' + 'A'.repeat(100000) })).status, 401);
});

test('password in query string, cookie or other headers is ignored', async () => {
  assert.equal((await get({}, `/?password=${PASSWORD}&user=forge`)).status, 401);
  assert.equal((await get({ cookie: `password=${PASSWORD}`, 'x-password': PASSWORD }, '/')).status, 401);
  assert.equal((await get({ authorization: `Bearer ${PASSWORD}` }, `/?p=${PASSWORD}`)).status, 401);
});

test('scheme is case-insensitive, custom user works, default user is forge', async () => {
  assert.equal((await get({ authorization: goodAuth.replace('Basic', 'basic') })).status, 200);
  const env = baseEnv({ DASHBOARD_USER: 'alice' });
  assert.equal((await get({ authorization: goodAuth }, '/', env)).status, 401);
  assert.equal((await get({ authorization: basic('alice', PASSWORD) }, '/', env)).status, 200);
  assert.equal((await get({ authorization: basic('forge', PASSWORD) }, '/', baseEnv({ DASHBOARD_USER: '' }))).status, 200);
});

test('missing, empty or short (<12) DASHBOARD_PASSWORD fails closed with 503 for everything', async () => {
  for (const pw of [undefined, '', 'short', '12345678901']) {
    const env = baseEnv();
    if (pw === undefined) delete env.DASHBOARD_PASSWORD; else env.DASHBOARD_PASSWORD = pw;
    for (const headers of [{}, { authorization: basic('forge', pw ?? '') }, { authorization: goodAuth }]) {
      for (const url of ['/', '/data/summary', '/app.js']) {
        const r = await get(headers, url, env);
        assert.equal(r.status, 503, `${pw} ${url}`);
        assert.ok(!r.body.includes(TOKEN));
      }
    }
  }
  const ok = baseEnv({ DASHBOARD_PASSWORD: '123456789012' });
  assert.equal((await get({ authorization: basic('forge', '123456789012') }, '/', ok)).status, 200);
});

test('after auth: unknown paths 404, bad methods 405, HEAD has no body, traversal is not served', async () => {
  const h = { authorization: goodAuth };
  assert.equal((await get(h, '/nope')).status, 404);
  assert.equal((await get(h, '/../../etc/passwd')).status, 404);
  assert.equal((await get(h, '/%2e%2e/package.json')).status, 404);
  assert.equal((await get(h, '/package.json')).status, 404);
  assert.equal((await get(h, '/', baseEnv(), 'POST')).status, 405);
  const head = await get(h, '/', baseEnv(), 'HEAD');
  assert.equal(head.status, 200);
  assert.equal(head.body, '');
  assert.equal((await get(h, '/x' + 'a'.repeat(3000))).status, 414);
});

test('rewrite form: original path carried in ?p=', async () => {
  const h = { authorization: goodAuth };
  assert.equal((await get(h, '/api/index?p=/app.js')).headers['content-type'].startsWith('text/javascript'), true);
  assert.equal((await get(h, '/api/index?p=/nope')).status, 404);
});
