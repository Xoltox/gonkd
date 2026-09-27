// App shell: status strip, e-stop, nav, toasts.
import { h, icon, setText, region } from './dom.js';
import { holdButton } from './ui.js';
import { phaseOf, isStale, act, set, toast } from './store.js';
import { api } from './api.js';
import { t1, t0, pct, ago, DEG } from './fmt.js';

export const ROUTES = [
  ['now', 'Now', 'now'],
  ['files', 'Files', 'files'],
  ['control', 'Control', 'control'],
  ['console', 'Console', 'console'],
  ['settings', 'Settings', 'settings'],
];

const PHASE = {
  offline: ['Box offline', 'offline', 'warn'],
  connecting: ['Connecting', 'refresh', 'neutral'],
  disconnected: ['Printer offline', 'offline', 'warn'],
  idle: ['Ready', 'check', 'neutral'],
  heating: ['Heating', 'flame', 'hot'],
  printing: ['Printing', 'play', 'hot'],
  paused: ['Paused', 'pause', 'warn'],
  finished: ['Finished', 'check', 'ok'],
  cancelling: ['Cancelling', 'x', 'warn'],
  uploading: ['Writing to card', 'upload', 'bed'],
  error: ['Error', 'alert', 'danger'],
  killed: ['Halted', 'stop', 'danger'],
};

export function stateChip(phase) {
  const [t, ic, tone] = PHASE[phase] || PHASE.idle;
  return h('span', { class: `chip chip-${tone} ${phase === 'connecting' ? 'spin' : ''}` }, icon(ic, 18), t);
}

export function tempState(a, t) {
  if (t <= 0) return a > 45 ? 'cooling' : 'off';
  if (Math.abs(a - t) <= 2) return 'at temp';
  return a < t ? 'heating' : 'cooling';
}

export function logo(cls) {
  return h('a', { class: `logo ${cls}`, href: '#/now', 'aria-label': "Gonk'd, go to Now" },
    h('svg:svg', { class: 'logo-mark', viewBox: '0 0 24 24', 'aria-hidden': 'true', html: '<rect x="4" y="5" width="16" height="13" rx="1.5"/><path d="M8 18v3M16 18v3M7 9h10M9 13h2M13 13h2"/>' }),
    h('span', { class: 'logo-text', text: "Gonk'd" }));
}

function stripTemp(kind) {
  const val = h('span', { class: 'tnum' });
  const tgt = h('span', { class: 'dim tnum' });
  const dot = h('span', { class: 'dot-ok', 'aria-hidden': 'true', hidden: true });
  const el = h('span', { class: `strip-temp t-${kind}`, role: 'img' },
    icon(kind === 'hot' ? 'flame' : 'bed', 18), h('span', { 'aria-hidden': 'true' }, val, tgt), dot);
  const name = kind === 'hot' ? 'Hotend' : 'Bed';
  return {
    el,
    set(a, t) {
      const st = tempState(a, t);
      setText(val, t1(a));
      setText(tgt, '/' + (t > 0 ? t0(t) : 'off'));
      dot.hidden = st !== 'at temp';
      el.setAttribute('aria-label', `${name} ${t1(a)}${DEG}, target ${t > 0 ? t0(t) + DEG : 'off'}, ${st}`);
    },
  };
}

function estop() {
  return holdButton('Stop', async () => {
    if (await act(() => api.estop())) {
      set({ estopped: true, homed: false });
      toast('Emergency stop sent. Reset the printer to continue.', 'error');
    }
  }, { class: 'estop', icon: 'stop', hint: 'Emergency stop: hold 1.5 seconds. Halts the printer with M112; it needs a reset afterwards.' });
}

export function mountStrip(root) {
  const chipPhase = (s) => (s.link === 'offline' ? 'offline' : phaseOf(s));
  const chip = region(chipPhase, (s) => stateChip(chipPhase(s)));
  const linkNote = h('span', { class: 'link-note', 'aria-live': 'polite' });
  const hot = stripTemp('hot');
  const bed = stripTemp('bed');
  const prog = h('span', { class: 'strip-prog tnum' });
  const vals = h('div', { class: 'strip-row strip-vals' }, hot.el, bed.el, prog);
  root.append(
    h('div', { class: 'strip-row' }, logo('logo-strip'), chip.el, linkNote, h('span', { class: 'grow' }), estop()),
    vals,
  );
  return (s) => {
    chip.update(s);
    const stale = isStale(s) || s.link === 'offline';
    root.classList.toggle('is-stale', stale);
    let note = '';
    if (s.link === 'offline') note = s.lastUpdate ? `No link, last seen ${ago(s.now - s.lastUpdate)}` : 'No link to box';
    else if (isStale(s)) note = `Stale, ${ago(s.now - s.lastUpdate)}`;
    else if (s.link === 'polling') note = 'Polling';
    setText(linkNote, note);
    linkNote.classList.toggle('warn', stale);
    vals.hidden = !s.snap;
    if (!s.snap) return;
    const t = s.snap.temps;
    hot.set(t.hotendActual, t.hotendTarget);
    bed.set(t.bedActual, t.bedTarget);
    const p = phaseOf(s);
    const job = s.snap.job;
    const showProg = job && ['printing', 'paused', 'heating', 'uploading'].includes(p);
    prog.hidden = !showProg;
    if (showProg) {
      const v = p === 'uploading' ? (job.totalBytes ? (job.sentBytes / job.totalBytes) * 100 : 0) : job.progress;
      setText(prog, pct(v) + '%');
      prog.setAttribute('aria-label', `Progress ${pct(v)} percent`);
    }
  };
}

export function mountNav(root) {
  const items = ROUTES.map(([id, label, ic]) => {
    const a = h('a', { href: `#/${id}`, class: 'nav-item' }, icon(ic), h('span', { text: label }));
    a.dataset.route = id;
    return a;
  });
  const badge = h('span', { class: 'nav-badge', hidden: true, role: 'img', 'aria-label': 'upload in progress' });
  items[1].append(badge);
  root.append(logo('logo-rail'), ...items);
  return (s, route) => {
    for (const a of items) {
      const on = a.dataset.route === route;
      a.classList.toggle('on', on);
      if (on) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    }
    badge.hidden = !(s.upload && s.upload.stage !== 'failed');
  };
}

export function mountToast(root) {
  let shown = 0;
  return (s) => {
    const t = s.toast;
    if (!t) { if (shown) { root.replaceChildren(); shown = 0; } return; }
    if (t.id === shown) return;
    shown = t.id;
    root.setAttribute('role', t.kind === 'error' ? 'alert' : 'status');
    const ic = t.kind === 'ok' ? 'check' : t.kind === 'info' ? 'console' : 'alert';
    root.replaceChildren(h('div', { class: `toast toast-${t.kind}` },
      icon(ic), h('span', { text: t.msg }),
      h('button', { type: 'button', class: 'toast-x', 'aria-label': 'Dismiss', onclick: () => set({ toast: null }) }, icon('x', 18))));
  };
}

