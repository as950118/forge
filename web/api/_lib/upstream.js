// Server-side calls to the Render read-only API. The token never leaves this module's caller.
export const MAX_UPSTREAM_BYTES = 4 * 1024 * 1024;
export const UPSTREAM_TIMEOUT_MS = 8000;
const LOOPBACK = new Set(['127.0.0.1', 'localhost', '[::1]']);

// Returns a base URL string or null. https only; http loopback only when NODE_ENV=development.
export function resolveBase(env) {
  const raw = env.FORGE_API_URL;
  if (typeof raw !== 'string' || raw === '') return null;
  let u;
  try {
    u = new URL(raw);
  } catch {
    return null;
  }
  if (u.username || u.password || u.search || u.hash) return null;
  if (u.pathname !== '/') return null;
  if (u.protocol === 'https:') return u.origin;
  if (u.protocol === 'http:' && env.NODE_ENV === 'development' && LOOPBACK.has(u.hostname)) return u.origin;
  return null;
}

// Result: { ok: true, body } | { ok: false, status, error }. `error` is a fixed generic code.
export async function fetchUpstream(env, path, query, fetchImpl = fetch) {
  const base = resolveBase(env);
  const token = env.FORGE_API_TOKEN;
  if (!base || typeof token !== 'string' || token === '') return { ok: false, status: 503, error: 'not_configured' };
  const url = new URL(path, base);
  for (const [k, v] of query) url.searchParams.set(k, v);
  let res;
  try {
    res = await fetchImpl(url, {
      method: 'GET',
      headers: { authorization: `Bearer ${token}`, accept: 'application/json' },
      redirect: 'manual',
      signal: AbortSignal.timeout(UPSTREAM_TIMEOUT_MS),
    });
  } catch (e) {
    const timeout = e && (e.name === 'TimeoutError' || e.name === 'AbortError');
    return { ok: false, status: timeout ? 504 : 502, error: timeout ? 'upstream_timeout' : 'upstream_error' };
  }
  try {
    if (res.status === 404) {
      await res.body?.cancel();
      return { ok: false, status: 404, error: 'no_data' };
    }
    if (res.status !== 200) {
      await res.body?.cancel();
      return { ok: false, status: 502, error: 'upstream_error' };
    }
    const len = Number(res.headers.get('content-length'));
    if (Number.isFinite(len) && len > MAX_UPSTREAM_BYTES) {
      await res.body?.cancel();
      return { ok: false, status: 502, error: 'upstream_error' };
    }
    const chunks = [];
    let total = 0;
    for await (const c of res.body) {
      total += c.length;
      if (total > MAX_UPSTREAM_BYTES) {
        return { ok: false, status: 502, error: 'upstream_error' };
      }
      chunks.push(c);
    }
    const text = Buffer.concat(chunks).toString('utf8');
    if (text.includes(token)) return { ok: false, status: 502, error: 'upstream_error' };
    JSON.parse(text); // must be JSON; passed through unchanged
    return { ok: true, body: text };
  } catch {
    return { ok: false, status: 502, error: 'upstream_error' };
  }
}
