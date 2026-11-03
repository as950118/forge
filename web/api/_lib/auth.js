// HTTP Basic gate. Fail closed: no usable DASHBOARD_PASSWORD means nothing is served.
import { createHash, timingSafeEqual } from 'node:crypto';

export const MIN_PASSWORD_LENGTH = 12;
const MAX_AUTH_HEADER = 1024;

const sha256 = (s) => createHash('sha256').update(s, 'utf8').digest();

export function isConfigured(env) {
  const p = env.DASHBOARD_PASSWORD;
  return typeof p === 'string' && p.length >= MIN_PASSWORD_LENGTH;
}

// Returns 'ok' | 'unconfigured' | 'unauthorized'. `headers` is a Node-style object
// (lower-case names, string or string[] values). Only one Authorization header is accepted.
export function checkAuth(headers, env) {
  if (!isConfigured(env)) return 'unconfigured';
  const raw = headers.authorization;
  if (typeof raw !== 'string' || raw.length === 0 || raw.length > MAX_AUTH_HEADER) return 'unauthorized';
  const m = /^basic ([A-Za-z0-9+/]+={0,2})$/i.exec(raw);
  let user = '';
  let pass = '';
  let parsed = false;
  if (m) {
    const decoded = Buffer.from(m[1], 'base64').toString('utf8');
    const i = decoded.indexOf(':');
    if (i >= 0) {
      user = decoded.slice(0, i);
      pass = decoded.slice(i + 1);
      parsed = true;
    }
  }
  const wantUser = typeof env.DASHBOARD_USER === 'string' && env.DASHBOARD_USER !== '' ? env.DASHBOARD_USER : 'forge';
  // Always hash and compare both fields (no early exit), constant-time on fixed-length digests.
  const userOk = timingSafeEqual(sha256(user), sha256(wantUser));
  const passOk = timingSafeEqual(sha256(pass), sha256(env.DASHBOARD_PASSWORD));
  return parsed && userOk && passOk ? 'ok' : 'unauthorized';
}
