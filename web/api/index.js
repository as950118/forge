// Vercel Node function: single entry for every path (see vercel.json rewrites and DEPLOY.md).
import { handle } from './_lib/handler.js';

export default async function handler(req, res) {
  let out;
  try {
    out = await handle({ method: req.method, url: req.url, headers: req.headers }, process.env);
  } catch {
    out = { status: 500, headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'private, no-store' }, body: '내부 오류입니다.\n' };
  }
  res.statusCode = out.status;
  for (const [k, v] of Object.entries(out.headers)) res.setHeader(k, v);
  res.end(out.body);
}
