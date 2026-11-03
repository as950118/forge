// Pure rendering helpers. Dynamic text is inserted ONLY via textContent / createTextNode;
// attributes only receive fixed strings or numbers. `doc` is a document (or a test double).
const SVG_NS = 'http://www.w3.org/2000/svg';

export function formatRate(rate) {
  if (typeof rate !== 'number' || !Number.isFinite(rate)) return '-';
  return `${(rate * 100).toFixed(1)}%`;
}

export function formatTime(iso) {
  if (typeof iso !== 'string' || iso === '') return '-';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '-';
  return `${d.toISOString().slice(0, 16).replace('T', ' ')} UTC`;
}

const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

function el(doc, tag, cls, txt) {
  const e = doc.createElement(tag);
  if (cls) e.setAttribute('class', cls);
  if (txt !== undefined) e.textContent = String(txt);
  return e;
}

export function itemLabel(item) {
  const name = String(item && item.name != null ? item.name : '');
  return item && item.kind === 'mcp' && item.server ? `${item.server} / ${name}` : name;
}

export function buildItemsTable(doc, items) {
  const wrap = el(doc, 'div', 'card');
  const table = el(doc, 'table');
  const head = el(doc, 'tr');
  [['이름', ''], ['호출 수', 'num'], ['성공률', 'num'], ['실패', 'num'], ['최근 호출', '']].forEach(([t, c]) => head.appendChild(el(doc, 'th', c, t)));
  const thead = el(doc, 'thead');
  thead.appendChild(head);
  table.appendChild(thead);
  const tbody = el(doc, 'tbody');
  for (const it of items) {
    const tr = el(doc, 'tr');
    tr.appendChild(el(doc, 'td', 'name', itemLabel(it)));
    tr.appendChild(el(doc, 'td', 'num', num(it.calls)));
    const r = it.success_rate;
    const cls = typeof r === 'number' ? (r >= 0.9 ? 'num rate-ok' : r < 0.5 ? 'num rate-bad' : 'num') : 'num';
    tr.appendChild(el(doc, 'td', cls, formatRate(r)));
    tr.appendChild(el(doc, 'td', 'num', num(it.failed)));
    tr.appendChild(el(doc, 'td', '', formatTime(it.last_call)));
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  wrap.appendChild(table);
  return wrap;
}

// Sum all kinds per day; returns [{day, ok, failed, unknown, calls}] sorted by day.
export function aggregateDaily(daily) {
  const by = new Map();
  for (const d of daily) {
    const day = String(d.day);
    const a = by.get(day) || { day, ok: 0, failed: 0, unknown: 0, calls: 0 };
    a.ok += num(d.ok);
    a.failed += num(d.failed);
    a.unknown += num(d.unknown);
    a.calls += num(d.calls);
    by.set(day, a);
  }
  return [...by.values()].sort((x, y) => (x.day < y.day ? -1 : x.day > y.day ? 1 : 0));
}

export function buildDailyChart(doc, daily, maxDays = 30) {
  const rows = aggregateDaily(daily).slice(-maxDays);
  const wrap = el(doc, 'div', 'card');
  const W = 640, H = 200, padL = 36, padB = 24, padT = 8;
  const svg = doc.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('class', 'chart');
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', '일별 호출 수 막대 그래프');
  const max = Math.max(1, ...rows.map((r) => r.calls));
  const plotH = H - padB - padT;
  const slot = (W - padL - 8) / Math.max(1, rows.length);
  const bw = Math.max(2, slot * 0.7);
  const base = H - padB;
  const mk = (tag, attrs, txt) => {
    const e = doc.createElementNS(SVG_NS, tag);
    for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, String(v));
    if (txt !== undefined) e.textContent = String(txt);
    return e;
  };
  svg.appendChild(mk('line', { x1: padL, y1: base, x2: W - 8, y2: base }));
  svg.appendChild(mk('text', { x: 4, y: padT + 10 }, max));
  svg.appendChild(mk('text', { x: 4, y: base }, 0));
  rows.forEach((r, i) => {
    const x = padL + i * slot + (slot - bw) / 2;
    let y = base;
    for (const [key, cls] of [['ok', 'bar-ok'], ['failed', 'bar-fail'], ['unknown', 'bar-unk']]) {
      const h = (r[key] / max) * plotH;
      if (h <= 0) continue;
      y -= h;
      const rect = mk('rect', { x: x.toFixed(2), y: y.toFixed(2), width: bw.toFixed(2), height: h.toFixed(2), class: cls });
      rect.appendChild(mk('title', {}, `${r.day}: ${r.calls}`));
      svg.appendChild(rect);
    }
  });
  if (rows.length > 0) {
    svg.appendChild(mk('text', { x: padL, y: H - 6 }, rows[0].day));
    svg.appendChild(mk('text', { x: W - 8, y: H - 6, 'text-anchor': 'end' }, rows[rows.length - 1].day));
  }
  wrap.appendChild(svg);
  const legend = el(doc, 'div', 'legend');
  legend.appendChild(el(doc, 'span', 'lg-ok', '성공'));
  legend.appendChild(el(doc, 'span', 'lg-fail', '실패'));
  legend.appendChild(el(doc, 'span', 'lg-unk', '결과 미관찰'));
  wrap.appendChild(legend);
  return wrap;
}

export function metaText(meta) {
  const m = meta || {};
  return `생성 시각: ${formatTime(m.generated_at)} · Forge ${String(m.forge_version || '-')} · 일 기준 ${String(m.day_basis || 'UTC')}`;
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

// Fills the page. `ids` maps names to elements: meta, status, and per-section {section, body}.
export function renderDashboard(doc, ui, data) {
  const items = Array.isArray(data && data.items) ? data.items : [];
  const daily = Array.isArray(data && data.daily) ? data.daily : [];
  ui.meta.textContent = metaText(data && data.meta);
  if (items.length === 0 && daily.length === 0) {
    showStatus(ui, '아직 표시할 데이터가 없습니다. 로컬에서 forge export --to-postgres --yes 로 스냅샷을 내보내세요.');
    return;
  }
  ui.status.hidden = true;
  const sections = { daily: ui.daily };
  for (const kind of ['skill', 'tool', 'mcp']) sections[kind] = ui[kind];
  clear(sections.daily.body);
  sections.daily.section.hidden = daily.length === 0;
  if (daily.length > 0) sections.daily.body.appendChild(buildDailyChart(doc, daily));
  for (const kind of ['skill', 'tool', 'mcp']) {
    const list = items.filter((i) => i.kind === kind).sort((a, b) => num(b.calls) - num(a.calls));
    clear(sections[kind].body);
    sections[kind].section.hidden = list.length === 0;
    if (list.length > 0) sections[kind].body.appendChild(buildItemsTable(doc, list));
  }
}

export function showStatus(ui, message) {
  ui.status.textContent = message;
  ui.status.hidden = false;
}

export const ERROR_MESSAGE = '데이터를 불러오지 못했습니다. 잠시 후 다시 시도하세요.';
export const NO_SNAPSHOT_MESSAGE = '아직 내보낸 스냅샷이 없습니다. 로컬에서 forge export --to-postgres --yes 를 실행하세요.';
