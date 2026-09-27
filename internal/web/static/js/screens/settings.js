// Settings (minimal): theme, versions, capabilities, presets editor.
import { h, region } from '../dom.js';
import { card, btn, iconBtn, seg, note } from '../ui.js';
import { act, limitsOf, loadPresets } from '../store.js';
import { api } from '../api.js';
import { screenTitle } from './common.js';
import { DEG } from '../fmt.js';

const THEME_KEY = 'gonkd-theme';

export function getTheme() {
  try { return localStorage.getItem(THEME_KEY) || 'system'; } catch { return 'system'; }
}

export function applyTheme(t) {
  if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
  else document.documentElement.removeAttribute('data-theme');
  try {
    if (t === 'system') localStorage.removeItem(THEME_KEY);
    else localStorage.setItem(THEME_KEY, t);
  } catch { /* private mode: applies for this visit only */ }
}

export function mount(root) {
  const theme = card('Appearance', 'settings', [
    h('div', { class: 'field-label', text: 'Theme' }),
    seg('Theme', [['system', 'System'], ['light', 'Light'], ['dark', 'Dark']], getTheme(), applyTheme),
    h('p', { class: 'meta', text: 'System follows your device. Saved in this browser only.' }),
  ]);
  const about = region((s) => JSON.stringify([s.snap && s.snap.version, s.snap && s.snap.firmware, s.snap && s.snap.connected, s.link]), (s) => {
    const snap = s.snap || {};
    const row = (k, v) => h('div', null, h('dt', { text: k }), h('dd', { text: v }));
    return card('About', 'console', h('dl', { class: 'kv' },
      row("Gonk'd", snap.version || 'unknown'),
      row('Firmware', snap.firmware || 'not reported yet'),
      row('Printer link', snap.connected ? 'Connected' : 'Not connected'),
      row('Live updates', { live: 'Streaming', polling: 'Polling every 3 s', offline: 'No link', connecting: 'Connecting' }[s.link])));
  });
  const caps = region((s) => JSON.stringify(s.snap && s.snap.capabilities), (s) => {
    const list = (s.snap && s.snap.capabilities) || [];
    return card('Printer capabilities', 'control', list.length
      ? h('ul', { class: 'caps' }, list.map((c) => h('li', { class: 'mono', text: c })))
      : h('p', { class: 'meta', text: 'Reported by the firmware (M115) once the printer connects.' }));
  });
  const presets = presetEditor();
  root.append(screenTitle('Settings'), h('div', { class: 'settings-grid' }, theme, presets.el, about.el, caps.el));
  return (s) => { about.update(s); caps.update(s); presets.update(s); };
}

// Editable copy of the presets; saved with PUT /gonkd/presets.
function presetEditor() {
  let rows = null;      // working copy
  let source = '';      // JSON of the server list the copy came from
  let lim = { hotendMax: 260, bedMax: 110 };
  const list = h('div', { class: 'presets' });
  const msg = h('p', { class: 'meta' });
  const add = btn('Add preset', { icon: 'plus', onClick: () => { rows.push({ name: '', hotend: 200, bed: 60 }); draw(); } });
  const save = btn('Save presets', { variant: 'primary', onClick: async () => {
    const clean = rows.map((r) => ({ name: r.name.trim(), hotend: Number(r.hotend), bed: Number(r.bed) }));
    save.disabled = true;
    if (await act(() => api.savePresets(clean), 'Presets saved')) { source = ''; await loadPresets(); }
    save.disabled = false;
  } });
  const el = card('Preheat presets', 'flame', [
    h('p', { class: 'meta', text: `Up to 8. Hotend 0 to ${lim.hotendMax}${DEG}, bed 0 to ${lim.bedMax}${DEG}.` }),
    list, msg, h('div', { class: 'btn-row' }, add, save),
  ]);
  const errorOf = (r) => {
    if (!r.name.trim()) return 'Name it';
    if (!(Number(r.hotend) >= 0 && Number(r.hotend) <= lim.hotendMax)) return `Hotend 0 to ${lim.hotendMax}`;
    if (!(Number(r.bed) >= 0 && Number(r.bed) <= lim.bedMax)) return `Bed 0 to ${lim.bedMax}`;
    return '';
  };
  const validate = () => {
    const errs = rows.map(errorOf).filter(Boolean);
    msg.textContent = errs.length ? `Fix before saving: ${errs[0]}.` : '';
    msg.className = errs.length ? 'note warn' : 'meta';
    save.disabled = errs.length > 0;
    add.disabled = rows.length >= 8;
  };
  const field = (r, key, label, type, max) => {
    const input = h('input', { type, value: r[key], 'aria-label': label, min: type === 'number' ? 0 : null, max, maxlength: type === 'text' ? 16 : null, inputmode: type === 'number' ? 'numeric' : null });
    input.addEventListener('input', () => { r[key] = input.value; validate(); });
    return input;
  };
  function draw() {
    list.replaceChildren(...rows.map((r, i) => h('div', { class: 'preset-row' },
      field(r, 'name', `Preset ${i + 1} name`, 'text'),
      h('label', { class: 'mini c-hot' }, h('span', { text: 'Hotend' }), field(r, 'hotend', `Preset ${i + 1} hotend ${DEG}`, 'number', lim.hotendMax)),
      h('label', { class: 'mini c-bed' }, h('span', { text: 'Bed' }), field(r, 'bed', `Preset ${i + 1} bed ${DEG}`, 'number', lim.bedMax)),
      iconBtn('trash', `Remove preset ${r.name || i + 1}`, () => { rows.splice(i, 1); draw(); }))));
    validate();
  }
  return {
    el,
    update(s) {
      lim = limitsOf(s);
      const src = JSON.stringify(s.presets);
      if (src !== source && (!rows || !el.contains(document.activeElement))) {
        source = src;
        rows = s.presets.map((p) => ({ ...p }));
        draw();
      }
    },
  };
}


