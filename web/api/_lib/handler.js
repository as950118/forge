// Framework-free request handler: ({method,url,headers}, env) -> {status, headers, body}.
// Every request (static files included) passes the Basic-auth gate first.
import { readFile } from 'node:fs/promises';
import { checkAuth } from './auth.js';
import { fetchUpstream } from './upstream.js';

export const SECURITY_HEADERS = {
  'x-content-type-options': 'nosniff',
  'x-frame-options': 'DENY',
  'referrer-policy': 'no-referrer',
  'cache-control': 'private, no-store',
  'content-security-policy':
    "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
};

const STATIC = {
  '/': ['index.html', 'text/html; charset=utf-8'],
  '/index.html': ['index.html', 'text/html; charset=utf-8'],
  '/app.js': ['app.js', 'text/javascript; charset=utf-8'],
  '/render.js': ['render.js', 'text/javascript; charset=utf-8'],
  '/style.css': ['style.css', 'text/css; charset=utf-8'],
};
const KINDS = new Set(['skill', 'tool', 'mcp']);
const MAX_URL = 2048;

const respond = (status, type, body, extra = {}) => ({
  status,
  headers: { ...SECURITY_HEADERS, 'content-type': type, ...extra },
  body,
});
const text = (status, msg, extra) => respond(status, 'text/plain; charset=utf-8', msg + '\n', extra);
const json = (status, obj) => respond(status, 'application/json; charset=utf-8', JSON.stringify(obj));

async function readStatic(name) {
  return readFile(new URL(`../_static/${name}`, import.meta.url));
}

// Route is the URL path; behind the Vercel rewrite the original path is carried in ?p=.
function routeOf(rawUrl) {
  const u = new URL(rawUrl, 'http://x');
  if (u.pathname === '/api/index' || u.pathname === '/api/index.js') {
    const p = u.searchParams.get('p');
    return { path: p && p.startsWith('/') ? p : '/', params: u.searchParams };
  }
  return { path: u.pathname, params: u.searchParams };
}

function dataRoute(path, params) {
  if (path === '/data/summary') return ['/v1/summary', []];
  if (path === '/data/items') {
    const kind = params.get('kind');
    if (kind === null) return ['/v1/items', []];
    return KINDS.has(kind) ? ['/v1/items', [['kind', kind]]] : 'bad';
  }
  if (path === '/data/daily') {
    const d = params.get('days');
    if (d === null) return ['/v1/daily', []];
    return /^[0-9]{1,3}$/.test(d) && +d >= 1 && +d <= 366 ? ['/v1/daily', [['days', d]]] : 'bad';
  }
  return null;
}

export async function handle(req, env, deps = {}) {
  const auth = checkAuth(req.headers || {}, env);
  if (auth === 'unconfigured') return text(503, '대시보드가 아직 구성되지 않았습니다. 관리자에게 문의하세요.', { 'retry-after': '600' });
  if (auth !== 'ok') {
    return text(401, '인증이 필요합니다.', { 'www-authenticate': 'Basic realm="Forge", charset="UTF-8"' });
  }
  if (req.method !== 'GET' && req.method !== 'HEAD') return text(405, '허용되지 않는 메서드입니다.', { allow: 'GET, HEAD' });
  if (typeof req.url !== 'string' || req.url.length > MAX_URL) return text(414, '요청 주소가 너무 깁니다.');
  let route;
  try {
    route = routeOf(req.url);
  } catch {
    return text(400, '잘못된 요청입니다.');
  }
  const out = await dispatch(route, env, deps);
  return req.method === 'HEAD' ? { ...out, body: '' } : out;
}

async function dispatch({ path, params }, env, deps) {
  const file = Object.hasOwn(STATIC, path) ? STATIC[path] : null;
  if (file) {
    try {
      return respond(200, file[1], await (deps.readStatic || readStatic)(file[0]));
    } catch {
      return text(500, '내부 오류입니다.');
    }
  }
  const dr = dataRoute(path, params);
  if (dr === 'bad') return json(400, { error: 'bad_request' });
  if (dr) {
    const r = await fetchUpstream(env, dr[0], dr[1], deps.fetch);
    return r.ok ? respond(200, 'application/json; charset=utf-8', r.body) : json(r.status, { error: r.error });
  }
  return text(404, '찾을 수 없습니다.');
}
