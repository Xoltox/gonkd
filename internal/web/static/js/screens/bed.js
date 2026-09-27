// Bed: manual mesh leveling (5x5 heatmap + wizard), corner assistant, Z offset.
import { h, icon, region, setText, visiblePoll } from '../dom.js';
import { card, btn, iconBtn, seg, banner, sheet, note, spinner, empty } from '../ui.js';
import { phaseOf, act, jobActive, connected, toast, set } from '../store.js';
import { api } from '../api.js';
import { screenTitle, pageBanners, preheat } from './common.js';
import { DEG } from '../fmt.js';

const ready = (s) => connected(s) && !['killed', 'error'].includes(phaseOf(s));
const locked = (s) => jobActive(phaseOf(s));
const canBed = (s) => ready(s) && !locked(s);

const RANGE_WARN = 0.3; // mm; "needs corner leveling" hint (DESIGN-AUDIT 5, Bed leveling)
const Z_STEPS = [0.025, 0.05, 0.1];

// Module-level (not store) state: this screen's own view of the mesh and
// wizard, refreshed from GET /gonkd/mesh and the mesh/level responses
// rather than the app-wide Snapshot poll.
let mesh = null; // {active,zOffset,points,min,max,range} | null
let meshErr = '';
let meshLoading = false;
let wiz = null; // null | {point,total,z} | 'done'
let zStep = Z_STEPS[0];

const MESH_READ_KEY = 'gonkd-mesh-read-at';
function markMeshRead() {
  try { localStorage.setItem(MESH_READ_KEY, String(Date.now())); } catch { /* private mode: this visit only */ }
}
function lastMeshReadText() {
  try {
    const v = localStorage.getItem(MESH_READ_KEY);
    if (v) return new Date(Number(v)).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' });
  } catch { /* ignore */ }
  return 'earlier';
}

async function loadMesh() {
  meshLoading = true;
  set({});
  try {
    mesh = await api.mesh();
    meshErr = '';
    if (mesh && mesh.points && !mesh.cached) markMeshRead();
  } catch (e) {
    meshErr = e.message || 'Could not load the mesh';
  }
  meshLoading = false;
  set({});
}

async function runLevel(action) {
  try {
    return await api.meshLevelStep(action);
  } catch (e) {
    toast(e.message || 'Something went wrong', 'error');
    return null;
  }
}

export function mount(root) {
  const banners = pageBanners();
  const lockNote = region((s) => locked(s), (s) => locked(s) && banner('info', 'Bed tools are locked while a job runs',
    'Wait for the job to finish, or pause and cancel it, before levelling or moving to a corner.', { icon: 'pause' }));
  const heat = heatmapCard();
  const wizard = wizardCard();
  const corner = cornerCard();
  const zoff = zOffsetCard();
  root.append(screenTitle('Bed'), banners.el, lockNote.el,
    h('div', { class: 'control-grid' }, heat.el, wizard.el, corner.el, zoff.el));
  if (!mesh && !meshLoading) loadMesh();
  // POST /gonkd/position while actively levelling (the coordinates panel
  // itself lives on Control; this just keeps position fresh in that gap).
  visiblePoll(root, 3000, () => api.position().catch(() => {}), () => !!wiz);
  return (s) => {
    banners.update(s);
    lockNote.update(s);
    heat.update(s);
    wizard.update(s);
    corner.update(s);
    zoff.update(s);
  };
}

function heatmapCard() {
  const body = h('div', { class: 'mesh-body' });
  const editSheet = sheet('Edit point');
  const el = card('Mesh', 'layers', [body]);
  function cellColor(v) {
    if (!mesh || mesh.range <= 0) return '';
    const mid = (mesh.min + mesh.max) / 2;
    const t = Math.min(1, Math.abs(v - mid) / (mesh.range / 2 || 1));
    const pct = Math.round(t * 65);
    return v >= mid
      ? `color-mix(in srgb, var(--hot-fill) ${pct}%, var(--surface-2))`
      : `color-mix(in srgb, var(--bed-fill) ${pct}%, var(--surface-2))`;
  }
  function editPoint(x, y) {
    const cur = mesh.points[y][x];
    const input = h('input', { type: 'number', step: '0.005', value: cur.toFixed(3), inputmode: 'decimal' });
    const save = (persist) => h('button', { type: 'submit', class: `btn ${persist ? 'btn-primary' : 'btn-secondary'}` },
      h('span', { text: persist ? 'Save (M500)' : 'Set' }));
    const setBtn = save(false);
    const saveBtn = save(true);
    const form = h('form', { class: 'stack' },
      h('p', { class: 'meta' }, `Point X${x} Y${y}, currently ${cur.toFixed(3)}mm.`),
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Z (mm)' }), input),
      h('div', { class: 'btn-row' }, setBtn, saveBtn));
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const z = Number(input.value);
      if (!isFinite(z)) { toast('Enter a number', 'warn'); return; }
      const persist = e.submitter === saveBtn;
      if (await act(() => api.meshPoint(x, y, z, persist), persist ? 'Point saved' : 'Point set')) {
        editSheet.close();
        loadMesh();
      }
    });
    editSheet.open(form);
    input.focus();
    input.select();
  }
  return {
    el,
    update(s) {
      const on = canBed(s);
      body.replaceChildren();
      if (meshLoading && !mesh) { body.append(spinner('Loading mesh')); return; }
      if (meshErr) { body.append(note('warn', meshErr), btn('Retry', { onClick: loadMesh })); return; }
      if (!mesh || !mesh.points) {
        body.append(empty('layers', 'No mesh yet. Run the levelling wizard below.'));
        return;
      }
      const rangeNote = mesh.range > RANGE_WARN
        ? note('warn', `Range ${mesh.range.toFixed(3)}mm is large. Try the corner assistant before fine mesh points.`)
        : null;
      const cachedNote = mesh.cached
        ? banner('info', 'Cached mesh', `Last read ${lastMeshReadText()}, live view after the print.`, { icon: 'clock' })
        : null;
      if (cachedNote) body.append(cachedNote);
      const grid = h('div', { class: 'mesh-grid' });
      for (let y = 0; y < mesh.points.length; y++) {
        for (let x = 0; x < mesh.points[y].length; x++) {
          const v = mesh.points[y][x];
          const cell = h('button', {
            type: 'button', class: 'mesh-cell tnum', style: { background: cellColor(v) },
            'aria-label': `Point X${x} Y${y}, ${v.toFixed(3)} millimetres. Tap to edit.`,
            disabled: !on,
            onclick: () => editPoint(x, y),
          }, v.toFixed(3));
          grid.append(cell);
        }
      }
      grid.style.setProperty('--cols', mesh.points[0].length);
      body.append(
        h('div', { class: 'mesh-stats' },
          h('span', { class: 'meta' }, 'Min ', h('span', { class: 'num-l tnum', text: mesh.min.toFixed(3) })),
          h('span', { class: 'meta' }, 'Max ', h('span', { class: 'num-l tnum', text: mesh.max.toFixed(3) })),
          h('span', { class: 'meta' }, 'Range ', h('span', { class: 'num-l tnum', text: mesh.range.toFixed(3) }))),
        grid,
      );
      if (rangeNote) body.append(rangeNote);
    },
  };
}

function wizardCard() {
  const body = h('div', { class: 'wizard-body' });
  const pre = preheat((s) => !canBed(s), false);
  const el = card('Mesh levelling', 'layers', [body]);
  async function start() {
    const r = await runLevel('start');
    if (r) { wiz = { point: r.point, total: r.total, z: r.z }; set({}); }
  }
  async function next() {
    const r = await runLevel('next');
    if (r) { wiz = r.done ? 'done' : { point: r.point, total: r.total, z: r.z }; set({}); }
  }
  async function abort() {
    if (await act(() => api.meshLevelEnd('abort'), 'Levelling aborted')) { wiz = null; set({}); }
  }
  async function finish(save) {
    if (await act(() => api.meshLevelEnd('finish', save), save ? 'Mesh saved' : 'Mesh discarded')) {
      wiz = null;
      loadMesh();
      set({});
    }
  }
  async function adjustZ(delta) {
    try {
      const r = await api.meshZAdjust(delta);
      if (wiz && wiz !== 'done') wiz.z = r.z;
      set({});
    } catch (e) {
      toast(e.message || 'Could not move Z', 'error');
    }
  }
  const steps = seg('Z step in millimetres', Z_STEPS.map((v) => [v, String(v)]), zStep, (v) => { zStep = v; });
  return {
    el,
    update(s) {
      const on = canBed(s);
      body.replaceChildren();
      if (wiz === 'done') {
        body.append(
          h('p', { text: 'All points probed. Save to keep this mesh, or discard it.' }),
          h('div', { class: 'btn-row' },
            btn('Save (M500)', { variant: 'primary', icon: 'check', onClick: () => finish(true) }),
            btn('Discard', { icon: 'trash', onClick: () => finish(false) })));
        return;
      }
      if (!wiz) {
        pre.update(s);
        body.append(
          h('p', { text: 'Paper test: at each of 25 points, jog Z until a slip of paper drags slightly under the nozzle.' }),
          pre.el,
          h('div', { class: 'btn-row' },
            btn('Start levelling', { variant: 'primary', icon: 'layers', disabled: !on, onClick: start })));
        return;
      }
      body.append(
        h('p', { class: 'meta' }, `Point ${wiz.point} of ${wiz.total}`),
        h('div', { class: 'wizard-z' }, h('span', { class: 'num-l tnum', text: wiz.z.toFixed(3) }), h('span', { class: 'unit', text: 'mm' })),
        h('div', { class: 'field-label', text: 'Z step (mm)' }),
        steps,
        h('div', { class: 'jog-z wizard-zbtn' },
          h('button', { type: 'button', class: 'btn btn-secondary', 'aria-label': 'Z up', onclick: () => adjustZ(zStep) }, icon('up'), h('span', { text: 'Z+' })),
          h('button', { type: 'button', class: 'btn btn-secondary', 'aria-label': 'Z down', onclick: () => adjustZ(-zStep) }, icon('down'), h('span', { text: 'Z-' }))),
        h('div', { class: 'btn-row' },
          btn('Next point', { variant: 'primary', icon: 'right', onClick: next }),
          btn('Abort', { variant: 'danger', icon: 'x', onClick: abort })),
      );
    },
  };
}

const CORNERS = [
  ['bl', 'Back left'], null, ['br', 'Back right'],
  null, ['center', 'Center'], null,
  ['fl', 'Front left'], null, ['fr', 'Front right'],
];

function cornerCard() {
  const btns = CORNERS.map((c) => c && btn(c[1], {
    onClick: () => act(() => api.bedCorner(c[0]), `Moved to ${c[1].toLowerCase()}`),
  }));
  const grid = h('div', { class: 'corner-grid' }, ...btns.map((b, i) => b || h('span', { key: i })));
  const done = btn('Done: lift and turn the mesh back on', {
    onClick: () => act(() => api.bedCorner('done'), 'Mesh back on').then((ok) => ok && loadMesh()),
  });
  const el = card('Corner assistant', 'home', [
    h('p', { class: 'meta', text: 'Homes if needed, turns the mesh off, moves over each levelling screw, then lowers for the paper test.' }),
    grid,
    done,
  ]);
  return {
    el,
    update(s) {
      const on = canBed(s);
      for (const b of btns) if (b) b.disabled = !on;
      done.disabled = !on;
    },
  };
}

function zOffsetCard() {
  const cur = h('span', { class: 'num-l tnum' }, '--');
  const minus = iconBtn('minus', 'Lower Z offset by 0.025mm', () => nudge(-0.025));
  const plus = iconBtn('plus', 'Raise Z offset by 0.025mm', () => nudge(0.025));
  const saveBtn = btn('Save (M500)', { variant: 'primary', onClick: () => save(true) });
  const babyBtn = btn('Save babystep as Z offset', { icon: 'edit', onClick: saveBabystep });
  const el = card('Z offset', 'down', [
    h('p', { class: 'meta', text: 'Fine-tunes the whole mesh up or down (G29 S4). Positive moves the nozzle away from the bed.' }),
    h('div', { class: 'stepper' }, h('div', { class: 'stepper-label', text: 'Z offset' }),
      h('div', { class: 'stepper-row' }, minus, h('output', { class: 'num-l', 'aria-live': 'polite' }, cur, h('span', { class: 'unit', text: 'mm' })), plus)),
    h('div', { class: 'btn-row wrap' }, saveBtn, babyBtn),
  ]);
  async function nudge(delta) {
    if (!mesh) return;
    try {
      await api.meshZOffset(mesh.zOffset + delta, false);
      await loadMesh();
    } catch (e) {
      toast(e.message || 'Could not adjust Z offset', 'error');
    }
  }
  async function save(persist) {
    if (!mesh) return;
    if (await act(() => api.meshZOffset(mesh.zOffset, persist), 'Z offset saved')) loadMesh();
  }
  let lastBaby = 0;
  async function saveBabystep() {
    if (await act(() => api.meshZOffset((mesh ? mesh.zOffset : 0) + lastBaby, true), 'Babystep saved as Z offset')) loadMesh();
  }
  return {
    el,
    update(s) {
      const on = canBed(s);
      lastBaby = s.snap && s.snap.job ? s.snap.job.babystepMm : 0;
      minus.disabled = plus.disabled = saveBtn.disabled = !on || !mesh;
      babyBtn.disabled = !on || !mesh || !lastBaby;
      setText(cur, mesh ? mesh.zOffset.toFixed(3) : '--');
    },
  };
}
