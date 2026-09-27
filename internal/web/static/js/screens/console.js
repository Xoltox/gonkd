// Console: filtered printer output, send G-code with history.
import { h, setText } from '../dom.js';
import { btn, card, empty } from '../ui.js';
import { act, pushLine, connected, toast } from '../store.js';
import { api } from '../api.js';
import { screenTitle, pageBanners } from './common.js';
import { hms } from '../fmt.js';

// Noise hidden by default: temperature autoreports, SD progress, bare ok, busy.
const NOISE = [
  /^ok$/i,
  /^(ok\s+)?T:\s*-?[\d.]+/i,
  /^\s*B:\s*-?[\d.]+/i,
  /^SD printing byte/i,
  /^Not SD printing/i,
  /^echo:busy:/i,
];
const isNoise = (t) => NOISE.some((r) => r.test(t.trim()));
const isError = (t) => /^(error|!!)/i.test(t.trim());
const isResend = (t) => /^resend/i.test(t.trim());

const QUICK = [['M115', 'Firmware info'], ['M114', 'Position'], ['M105', 'Temps'], ['M503', 'Settings'], ['G28', 'Home all']];
const HIST_KEY = 'gonkd-history';
const view = { all: false };

function loadHistory() {
  try { return JSON.parse(localStorage.getItem(HIST_KEY)) || []; } catch { return []; }
}
function saveHistory(list) {
  try { localStorage.setItem(HIST_KEY, JSON.stringify(list.slice(-50))); } catch { /* ignore */ }
}

export function mount(root) {
  const banners = pageBanners();
  const log = h('div', { class: 'log', role: 'log', 'aria-label': 'Printer output', tabindex: 0 });
  const hiddenCount = h('span', { class: 'meta tnum' });
  const allBox = h('input', { type: 'checkbox', checked: view.all });
  allBox.addEventListener('change', () => { view.all = allBox.checked; rebuild = true; update(last); });

  const input = h('input', { class: 'mono', placeholder: 'G-code, e.g. M114', autocomplete: 'off', autocapitalize: 'characters', spellcheck: false, 'aria-label': 'G-code command' });
  const send = h('button', { type: 'submit', class: 'btn btn-primary' }, h('span', { text: 'Send' }));
  const history = loadHistory();
  let hi = history.length;
  const submit = async (cmd) => {
    cmd = cmd.trim();
    if (!cmd) return;
    if (/^M112\b/i.test(cmd)) { toast('Use the Stop button for an emergency stop.', 'warn'); return; }
    if (history[history.length - 1] !== cmd) history.push(cmd);
    saveHistory(history);
    hi = history.length;
    pushLine(cmd, 'out');
    input.value = '';
    await act(() => api.send(cmd));
  };
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowUp' && hi > 0) { hi--; input.value = history[hi]; e.preventDefault(); }
    if (e.key === 'ArrowDown') { hi = Math.min(history.length, hi + 1); input.value = history[hi] || ''; e.preventDefault(); }
  });
  const form = h('form', { class: 'console-input' }, input, send);
  form.addEventListener('submit', (e) => { e.preventDefault(); submit(input.value); });
  const chips = h('div', { class: 'chips', 'aria-label': 'Quick commands' },
    QUICK.map(([c, d]) => h('button', { type: 'button', class: 'chip-btn', title: d, onclick: () => submit(c) },
      h('span', { class: 'mono', text: c }), h('span', { class: 'meta', text: d }))));
  const emptyEl = empty('console', 'No output yet. Send a command, or wait for the printer to talk.');

  root.append(screenTitle('Console'), banners.el,
    card(null, null, [
      h('div', { class: 'console-bar' },
        h('label', { class: 'check' }, allBox, h('span', { text: 'Show all lines' })),
        hiddenCount,
        h('span', { class: 'grow' }),
        btn('Clear view', { onClick: () => { clearedAt = lastId; rebuild = true; update(last); } })),
      log, emptyEl, form, chips,
    ], { class: 'console' }));

  let shownId = 0;   // last line id rendered
  let lastId = 0;
  let clearedAt = 0;
  let rebuild = true;
  let last = null;

  const lineEl = (l) => {
    const t = l.text;
    const kind = l.dir === 'out' ? 'out' : isError(t) ? 'err' : isResend(t) ? 'resend' : 'in';
    const pre = { out: '>', err: '!', resend: 'R', in: '<' }[kind];
    const row = h('div', { class: `line line-${kind}`, title: 'Click to copy' },
      h('span', { class: 'ts', text: l.ts ? hms(l.ts) : '--:--:--' }),
      h('span', { class: 'pre', 'aria-hidden': 'true', text: pre }),
      h('span', { class: 'txt', text: t }));
    row.addEventListener('click', () => {
      try { navigator.clipboard.writeText(t).then(() => toast('Copied', 'ok'), () => {}); } catch { /* ignore */ }
    });
    return row;
  };

  function update(s) {
    last = s;
    banners.update(s);
    send.disabled = input.disabled = !connected(s);
    const lines = s.lines.filter((l) => l.id > clearedAt);
    lastId = s.lines.length ? s.lines[s.lines.length - 1].id : 0;
    const visible = (l) => view.all || l.dir === 'out' || !isNoise(l.text);
    const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
    if (rebuild) {
      rebuild = false;
      log.replaceChildren(...lines.filter(visible).map(lineEl));
    } else {
      const add = lines.filter((l) => l.id > shownId && visible(l));
      if (add.length) log.append(...add.map(lineEl));
      while (log.childElementCount > 500) log.firstElementChild.remove();
    }
    shownId = lastId;
    const hidden = view.all ? 0 : lines.filter((l) => !visible(l)).length;
    setText(hiddenCount, hidden ? `${hidden} routine lines hidden` : '');
    emptyEl.hidden = log.childElementCount > 0;
    log.hidden = !emptyEl.hidden;
    if (atBottom) log.scrollTop = log.scrollHeight;
  }
  return update;
}
