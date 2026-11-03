import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { buildItemsTable, buildDailyChart, renderDashboard, showStatus, formatRate, formatTime, metaText } from '../api/_static/render.js';

const STATIC_DIR = new URL('../api/_static/', import.meta.url).pathname;
const FORBIDDEN = [/\binnerHTML\b/, /\bouterHTML\b/, /\binsertAdjacentHTML\b/, /\bdocument\.write\b/, /\beval\s*\(/, /\bnew Function\b/, /\bsetAttribute\(\s*['"]on/i, /\bsrcdoc\b/, /\bsetTimeout\(\s*['"]/];

test('client sources never use HTML-injecting or eval-like APIs', () => {
  for (const f of readdirSync(STATIC_DIR).filter((n) => n.endsWith('.js'))) {
    const src = readFileSync(join(STATIC_DIR, f), 'utf8');
    for (const re of FORBIDDEN) assert.ok(!re.test(src), `${f} matches ${re}`);
  }
});

test('index.html has no inline script, style, handlers or external resources', () => {
  const html = readFileSync(join(STATIC_DIR, 'index.html'), 'utf8');
  for (const m of html.matchAll(/<script\b([^>]*)>/gi)) assert.match(m[1], /\bsrc="\/[^"]+"/);
  assert.ok(!/<script[^>]*>\s*\S/.test(html.replace(/<script[^>]*src[^>]*><\/script>/g, '')));
  assert.ok(!/<style\b/i.test(html));
  assert.ok(!/\son\w+\s*=/i.test(html));
  assert.ok(!/\sstyle\s*=/i.test(html));
  assert.ok(!/(src|href)="https?:/i.test(html));
  assert.match(html, /lang="ko"/);
  assert.match(html, /name="viewport"/);
});

test('CSS supports dark mode and narrow screens', () => {
  const css = readFileSync(join(STATIC_DIR, 'style.css'), 'utf8');
  assert.match(css, /prefers-color-scheme:\s*dark/);
  assert.match(css, /max-width:\s*480px/);
});

// Fake DOM: records textContent and attributes; any innerHTML-style write throws.
class FakeNode {
  constructor(tag) {
    this.tag = tag; this.children = []; this.attrs = {}; this._text = ''; this.hidden = false;
    for (const bad of ['innerHTML', 'outerHTML']) {
      Object.defineProperty(this, bad, { set() { throw new Error(`${bad} used`); }, get() { return ''; } });
    }
  }
  set textContent(v) { this._text = String(v); this.children = []; }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  appendChild(c) { this.children.push(c); return c; }
  removeChild(c) { this.children.splice(this.children.indexOf(c), 1); }
  get firstChild() { return this.children[0] || null; }
  insertAdjacentHTML() { throw new Error('insertAdjacentHTML used'); }
}
const fakeDoc = { createElement: (t) => new FakeNode(t), createElementNS: (_ns, t) => new FakeNode(t) };
const walk = (n, fn) => { fn(n); n.children.forEach((c) => walk(c, fn)); };

const EVIL = '<img src=x onerror=alert(1)>';

test('hostile names and servers appear only as text, never as markup or attributes', () => {
  const items = [
    { kind: 'skill', server: '', name: EVIL, calls: 2, ok: 1, failed: 1, unknown: 0, success_rate: 0.5, last_call: '2026-11-03T00:00:00Z' },
    { kind: 'mcp', server: '"><script>x</script>', name: EVIL, calls: 1, ok: 1, failed: 0, unknown: 0, success_rate: 1, last_call: 'not-a-date' },
  ];
  const table = buildItemsTable(fakeDoc, items);
  let found = 0;
  walk(table, (n) => {
    assert.ok(!/[<>]/.test(Object.values(n.attrs).join('')), 'markup in attribute');
    assert.ok(!['img', 'script'].includes(n.tag));
    if (n.textContent === EVIL || n._text.includes(EVIL)) found++;
  });
  assert.ok(found >= 2);
  const chart = buildDailyChart(fakeDoc, [{ day: EVIL, kind: 'skill', calls: 1, ok: 1, failed: 0, unknown: 0 }]);
  walk(chart, (n) => assert.ok(!/[<>]/.test(Object.values(n.attrs).join('')), 'markup in chart attribute'));
});

function makeUi() {
  const mk = () => new FakeNode('div');
  return { meta: mk(), status: mk(), daily: { section: mk(), body: mk() }, skill: { section: mk(), body: mk() }, tool: { section: mk(), body: mk() }, mcp: { section: mk(), body: mk() } };
}

test('renderDashboard fills sections, meta shows time and version; empty state; error text', () => {
  const ui = makeUi();
  renderDashboard(fakeDoc, ui, {
    meta: { generated_at: '2026-11-03T01:02:03Z', forge_version: EVIL, day_basis: 'UTC' },
    items: [{ kind: 'skill', server: '', name: 'a', calls: 1, ok: 1, failed: 0, unknown: 0, success_rate: 1, last_call: '2026-11-03T00:00:00Z' }],
    daily: [{ day: '2026-11-03', kind: 'skill', calls: 1, ok: 1, failed: 0, unknown: 0 }],
  });
  assert.equal(ui.skill.section.hidden, false);
  assert.equal(ui.tool.section.hidden, true);
  assert.equal(ui.daily.section.hidden, false);
  assert.match(ui.meta.textContent, /2026-11-03 01:02 UTC/);
  assert.ok(ui.meta.textContent.includes(EVIL));

  const empty = makeUi();
  renderDashboard(fakeDoc, empty, { meta: {}, items: [], daily: [] });
  assert.equal(empty.status.hidden, false);
  assert.match(empty.status.textContent, /데이터가 없습니다/);

  const err = makeUi();
  showStatus(err, '데이터를 불러오지 못했습니다.');
  assert.equal(err.status.hidden, false);
});

test('formatters tolerate null and garbage', () => {
  assert.equal(formatRate(null), '-');
  assert.equal(formatRate(0.5), '50.0%');
  assert.equal(formatTime('garbage'), '-');
  assert.equal(formatTime(undefined), '-');
  assert.ok(metaText(undefined).includes('-'));
});
