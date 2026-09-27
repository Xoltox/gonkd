// Control: jog, home, motors off, extrude, heat, presets, tune.
import { h, icon, region, setText, visiblePoll } from '../dom.js';
import { card, btn, iconBtn, seg, banner, confirmButton, holdButton, note } from '../ui.js';
import { phaseOf, act, jobActive, limitsOf, set, toast, connected } from '../store.js';
import { api } from '../api.js';
import { screenTitle, pageBanners, preheat, tuneCard, heatersOn } from './common.js';
import { t1, ago, DEG } from '../fmt.js';

const COLD_MIN = 170;
const ui = { step: 10, elen: 5, efeed: 120 }; // kept across visits

const ready = (s) => connected(s) && !['killed', 'error'].includes(phaseOf(s));
const locked = (s) => jobActive(phaseOf(s));
const canMove = (s) => ready(s) && !locked(s);

export function mount(root) {
  const banners = pageBanners();
  const lockNote = region((s) => locked(s), (s) => locked(s) && banner('info', 'Motion is locked while a job runs',
    'Jog, home and extrude are off until the job ends. Use Babystep on Now for Z tweaks.', { icon: 'pause' }));
  const move = moveCard();
  const ext = extruderCard();
  const heat = heatCard();
  const tune = tuneCard((s) => !ready(s));
  const coords = coordsCard();
  root.append(screenTitle('Control'), banners.el, lockNote.el,
    h('div', { class: 'control-grid' }, move.el, ext.el, heat.el, tune.el, coords.el));
  // POST /gonkd/position on open and every 3s while this screen and tab
  // are visible; stops on its own once the router swaps the screen out.
  visiblePoll(root, 3000, () => api.position().catch(() => {}));
  return (s) => {
    banners.update(s);
    lockNote.update(s);
    move.update(s);
    ext.update(s);
    heat.update(s);
    tune.update(s);
    coords.update(s);
  };
}

// ---- live coordinates ----

function coordsCard() {
  const axisDD = () => h('dd', { class: 'num-l tnum', text: '--' });
  const vals = { x: axisDD(), y: axisDD(), z: axisDD(), e: axisDD() };
  const row = (k, label) => h('div', null, h('dt', { text: label }), vals[k]);
  const age = h('p', { class: 'meta' });
  const el = card('Position', 'control', [
    h('dl', { class: 'times' }, row('x', 'X'), row('y', 'Y'), row('z', 'Z'), row('e', 'E')),
    age,
  ]);
  return {
    el,
    update(s) {
      const pos = s.snap && s.snap.position;
      const fmt = (v) => (v == null ? '--' : v.toFixed(2));
      setText(vals.x, fmt(pos && pos.x));
      setText(vals.y, fmt(pos && pos.y));
      setText(vals.z, fmt(pos && pos.z));
      setText(vals.e, fmt(pos && pos.e));
      setText(age, pos ? `As of ${ago(s.now - new Date(pos.at).getTime())}` : 'No position yet. Jog or home to fetch it.');
    },
  };
}

// Enable/disable a list of controls in one go.
const enable = (els, on) => els.forEach((e) => { e.disabled = !on; });
const segButtons = (el) => [...el.querySelectorAll('button')];

function moveCard() {
  const jog = (axis, dir) => act(() => api.jog(axis, dir * ui.step, axis === 'Z' ? 600 : 3000));
  const home = async (cmd) => {
    if ((await act(() => api.send(cmd), 'Homing')) && cmd === 'G28') set({ homed: true });
  };
  const motorsOff = () => act(() => api.send('M84'), 'Motors off').then((ok) => ok && set({ homed: false }));
  const steps = seg('Step in millimetres', [0.1, 1, 10, 50].map((v) => [v, String(v)]), ui.step, (v) => { ui.step = v; });
  const zBtn = (dir) => h('button', { type: 'button', class: 'btn btn-secondary', 'aria-label': `Z ${dir > 0 ? 'up' : 'down'}`, onclick: () => jog('Z', dir) },
    icon(dir > 0 ? 'up' : 'down'), h('span', { text: dir > 0 ? 'Z+' : 'Z-' }));
  const pad = [
    iconBtn('up', 'Y plus', () => jog('Y', 1)),
    iconBtn('left', 'X minus', () => jog('X', -1)),
    iconBtn('home', 'Home X and Y', () => home('G28 X Y')),
    iconBtn('right', 'X plus', () => jog('X', 1)),
    iconBtn('down', 'Y minus', () => jog('Y', -1)),
  ];
  const z = [zBtn(1), zBtn(-1)];
  const homeAll = btn('Home all', { variant: 'primary', icon: 'home', onClick: () => home('G28') });
  const homeZ = btn('Home Z', { onClick: () => home('G28 Z') });
  // Motors off: second tap when idle, hold while a job runs (it ruins the print).
  const m84 = region((s) => locked(s), (s) => locked(s)
    ? holdButton('Motors off', motorsOff, { variant: 'secondary', icon: 'power', hint: 'Hold 1.5 seconds. Motors off during a job ruins the print.' })
    : confirmButton('Motors off', 'Motors off?', motorsOff, { icon: 'power' }));
  const homedChip = h('span', { class: 'chip chip-warn' }, icon('alert', 16), 'Not homed');
  const homedHint = h('p', { class: 'meta', text: 'Position is unknown until you home. Moves still work; mind the limits.' });
  const el = card('Move', 'control', [
    homedHint,
    h('div', { class: 'field-label', text: 'Step (mm)' }),
    steps,
    h('div', { class: 'jog' },
      h('div', { class: 'jog-xy' }, h('span'), pad[0], h('span'), pad[1], pad[2], pad[3], h('span'), pad[4], h('span')),
      h('div', { class: 'jog-z' }, ...z)),
    h('div', { class: 'btn-row wrap' }, homeAll, homeZ, m84.el),
  ], { extra: homedChip });
  return {
    el,
    update(s) {
      const on = canMove(s);
      enable([...pad, ...z, homeAll, homeZ, ...segButtons(steps)], on);
      m84.update(s);
      const b = m84.el.querySelector('button');
      if (b) b.disabled = !ready(s);
      homedChip.hidden = homedHint.hidden = !on || s.homed;
    },
  };
}

function extruderCard() {
  const len = seg('Extrude length in millimetres', [1, 5, 10, 25].map((v) => [v, String(v)]), ui.elen, (v) => { ui.elen = v; });
  const speed = seg('Extrude speed', [[120, 'Slow 2 mm/s'], [300, 'Normal 5 mm/s']], ui.efeed, (v) => { ui.efeed = v; });
  const retract = btn('Retract', { icon: 'up', onClick: () => act(() => api.jog('E', -ui.elen, ui.efeed)) });
  const extrude = btn('Extrude', { icon: 'down', variant: 'primary', onClick: () => act(() => api.jog('E', ui.elen, ui.efeed)) });
  const coldNote = note('warn', '');
  const coldText = coldNote.querySelector('span');
  const el = card('Extruder', 'drop', [
    h('div', { class: 'field-label', text: 'Length (mm)' }), len,
    h('div', { class: 'field-label', text: 'Speed' }), speed,
    coldNote,
    h('div', { class: 'btn-row' }, retract, extrude),
  ]);
  return {
    el,
    update(s) {
      const hot = s.snap ? s.snap.temps.hotendActual : 0;
      const cold = hot < COLD_MIN;
      const on = canMove(s);
      enable([...segButtons(len), ...segButtons(speed)], on);
      enable([retract, extrude], on && !cold);
      coldNote.hidden = !(on && cold);
      setText(coldText, `Heat the nozzle to ${COLD_MIN}${DEG} first (now ${t1(hot)}${DEG}).`);
    },
  };
}

function heatCard() {
  const rows = { hotend: heatRow('hotend'), bed: heatRow('bed') };
  const ptfe = note('warn', `Above 240${DEG} can damage a PTFE-lined hotend.`);
  const pre = preheat((s) => !ready(s), false);
  const preWrap = h('div', null, pre.el);
  // Cooldown: one tap when idle, second tap while a job runs.
  const cool = region((s) => locked(s), (s) => confirmButton('Cooldown', 'Cool mid-print?',
    () => act(() => api.heat({ hotend: 0, bed: 0 }), 'Heaters off'), { icon: 'snow', confirm: locked(s) }));
  const el = card('Heat', 'flame', [rows.hotend.el, ptfe, rows.bed.el, preWrap, h('div', { class: 'btn-row' }, cool.el)]);
  return {
    el,
    update(s) {
      const lim = limitsOf(s);
      rows.hotend.update(s, lim.hotendMax);
      rows.bed.update(s, lim.bedMax);
      const hv = Number(rows.hotend.input.value);
      ptfe.hidden = !(rows.hotend.input.value !== '' && hv > 240 && hv <= lim.hotendMax);
      preWrap.hidden = locked(s);
      if (!preWrap.hidden) pre.update(s);
      cool.update(s);
      cool.el.querySelector('button').disabled = !ready(s) || !heatersOn(s);
    },
  };
}

function heatRow(key) {
  const label = key === 'hotend' ? 'Hotend' : 'Bed';
  const tkey = key === 'hotend' ? 'hotendTarget' : 'bedTarget';
  let max = 0;
  const cap = h('span', { class: `field-label c-${key === 'hotend' ? 'hot' : 'bed'}` });
  const input = h('input', { type: 'number', inputmode: 'numeric', min: 0, step: 1 });
  const valid = () => input.value !== '' && Number(input.value) >= 0 && Number(input.value) <= max;
  const setBtn = btn('Set', { variant: 'primary', onClick: () => {
    if (!valid()) { toast(`Enter 0 to ${max}`, 'warn'); return; }
    const v = Math.round(Number(input.value));
    act(() => api.heat({ [key]: v }), `${label} target ${v}${DEG}`).then((ok) => { if (ok) input.value = ''; });
  } });
  const offBtn = btn('Off', { onClick: () => act(() => api.heat({ [key]: 0 }), `${label} off`) });
  input.addEventListener('input', () => set({}));
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter') setBtn.click(); });
  const el = h('div', { class: 'heat-row' }, h('label', { class: 'field' }, cap, input), setBtn, offBtn);
  return {
    el,
    input,
    update(s, lim) {
      max = lim;
      setText(cap, `${label} (0 to ${lim}${DEG})`);
      input.max = lim;
      const target = s.snap ? s.snap.temps[tkey] : 0;
      input.placeholder = target > 0 ? `now ${Math.round(target)}` : 'off';
      input.setAttribute('aria-invalid', input.value !== '' && !valid());
      setBtn.disabled = !ready(s) || !valid();
      offBtn.disabled = !ready(s) || !target;
    },
  };
}
