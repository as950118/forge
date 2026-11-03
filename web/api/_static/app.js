import { renderDashboard, showStatus, ERROR_MESSAGE, NO_SNAPSHOT_MESSAGE } from './render.js';

const byId = (id) => document.getElementById(id);
const ui = {
  meta: byId('meta'),
  status: byId('status'),
  daily: { section: byId('daily-section'), body: byId('daily') },
  skill: { section: byId('skill-section'), body: byId('skill') },
  tool: { section: byId('tool-section'), body: byId('tool') },
  mcp: { section: byId('mcp-section'), body: byId('mcp') },
};

async function load() {
  try {
    const res = await fetch('/data/summary', { headers: { accept: 'application/json' }, cache: 'no-store' });
    if (res.status === 404) {
      ui.meta.textContent = '';
      showStatus(ui, NO_SNAPSHOT_MESSAGE);
      return;
    }
    if (!res.ok) throw new Error('status');
    renderDashboard(document, ui, await res.json());
  } catch {
    ui.meta.textContent = '';
    showStatus(ui, ERROR_MESSAGE);
  }
}

load();
