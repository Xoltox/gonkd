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

// Shared colour ramp for the mesh: low = dark and cool, high = light and
// warm. Used by the 2D cells, the 3D faces and the legend bar so all three
// always agree. t is 0..1 (0 = min, 1 = max).
function meshColor(t) {
  const pct = Math.round(Math.max(0, Math.min(1, t)) * 100);
  return `color-mix(in oklch shorter hue, #fdc58a ${pct}%, #16264d)`;
}
// Text colour to put on top of meshColor(t): light on the dark (low) half,
// dark on the light (high) half.
const meshTextColor = (t) => (t >= 0.5 ? '#1c1916' : '#f4eee6');
// A multi-stop CSS gradient built from meshColor so the legend bar traces
// the same oklch path instead of a naive 2-stop blend.
function meshLegendGradient(steps = 10) {
  const stops = [];
  for (let i = 0; i <= steps; i++) {
    const t = i / steps;
    stops.push(`${meshColor(t)} ${Math.round(t * 100)}%`);
  }
  return `linear-gradient(to right, ${stops.join(', ')})`;
}

const MESH_VIEW_KEY = 'gonkd-mesh-view';
function loadMeshView() {
  try { return localStorage.getItem(MESH_VIEW_KEY) === '3d' ? '3d' : '2d'; } catch { return '2d'; }
}
function saveMeshView(v) {
  try { localStorage.setItem(MESH_VIEW_KEY, v); } catch { /* private mode: this visit only */ }
}
let meshView = loadMeshView(); // '2d' | '3d'
let meshRotation = 0; // 0..3, quarter turns applied to the 3D view

// Rotate a rows x cols matrix 90 degrees clockwise.
function rotateMatrix90(m) {
  const rows = m.length, cols = m[0].length;
  const out = [];
  for (let c = 0; c < cols; c++) {
    const row = [];
    for (let r = rows - 1; r >= 0; r--) row.push(m[r][c]);
    out.push(row);
  }
  return out;
}
// nearestEdge names the bed side drawn at the bottom. The 2D grid never
// rotates. In 3D, row 0 of the rotated matrix is drawn nearest, and each
// clockwise turn brings the next side there: front, left, back, right.
function nearestEdge() {
  if (meshView !== '3d') return 'Front of bed';
  return ['Front', 'Left side', 'Back', 'Right side'][((meshRotation % 4) + 4) % 4] + ' of bed';
}
function rotatedMesh(m, times) {
  let r = m;
  for (let i = 0; i < ((times % 4) + 4) % 4; i++) r = rotateMatrix90(r);
  return r;
}

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
  let lastState = null;
  const viewSeg = seg('Mesh view', [['2d', '2D'], ['3d', '3D']], meshView, (v) => {
    meshView = v;
    saveMeshView(v);
    applyView();
  });
  const rotateBtn = iconBtn('refresh', 'Rotate 3D view', () => {
    meshRotation = (meshRotation + 1) % 4;
    spin();
  });
  const headExtra = h('div', { class: 'mesh-view-toggle' }, viewSeg, rotateBtn);
  const el = card('Mesh', 'layers', [body], { extra: headExtra });
  // Live parts of the drawn mesh, kept so the 2D/3D toggle and rotate can
  // change them in place: a rebuilt element starts in its end state and the
  // browser has nothing to animate.
  let ui = null; // {stage, layer3d, svg, edge, height, key}
  function applyView() {
    rotateBtn.hidden = meshView !== '3d';
    if (!ui) return;
    ui.stage.classList.toggle('is-3d', meshView === '3d');
    ui.stage.classList.toggle('is-2d', meshView !== '3d');
    setText(ui.edge, nearestEdge());
    ui.height.hidden = meshView !== '3d' || !ui.height.textContent;
  }
  function spin() {
    if (!ui || !mesh) return;
    const all = mesh.points.flat();
    const mn = Math.min(...all);
    const { svg } = build3D(rotatedMesh(mesh.points, meshRotation), mn, Math.max(...all) - mn);
    const old = ui.svg;
    old.classList.add('mesh-svg-out');
    svg.classList.add('mesh-svg-in');
    ui.layer3d.append(svg);
    ui.svg = svg;
    setTimeout(() => old.remove(), 400);
    setText(ui.edge, nearestEdge());
  }
  // Fixed (not theme-token) colours so both themes read the ramp the same
  // way: cool and dark at the low end, warm and light at the high end.
  function cellStyle(v) {
    if (!mesh) return {};
    const all = mesh.points.flat();
    const mn = Math.min(...all), span = Math.max(...all) - mn;
    const t = span > 0 ? (v - mn) / span : 0.5;
    return { background: meshColor(t), color: meshTextColor(t) };
  }
  // Isometric 3D surface. Row 0 of `matrix` is drawn nearest the viewer
  // (bottom of the drawing), matching the 2D grid's "front at the bottom".
  // Rotating the mesh matrix itself (rather than the projection) means the
  // same draw logic always keeps the current front row at the near edge.
  function build3D(matrix, mn, span) {
    const rows = matrix.length, cols = matrix[0].length;
    const halfW = 18, halfH = 9;
    const range = span > 0 ? span : 0;
    // Plane's own vertical extent (ignoring height), used to size the
    // exaggeration so the height reads clearly without swamping the shape.
    const planeSpan = (rows - 1 + cols - 1) * halfH || halfH;
    const heightScale = range > 0 ? (0.25 * planeSpan) / range : 0;
    // Exaggeration relative to one grid step's own screen size (scale-
    // invariant: both are computed before the final viewBox fit).
    const exaggeration = heightScale > 0 ? heightScale / halfH : 0;
    const pts = [];
    for (let i = 0; i < rows; i++) {
      const row = [];
      for (let j = 0; j < cols; j++) {
        const depth = rows - 1 - i; // 0 = back row, max = front row
        const z = matrix[i][j];
        const x = (j - depth) * halfW;
        const yFlat = (j + depth) * halfH;
        const y = yFlat - (z - mn) * heightScale;
        row.push({ x, y, yFlat, z });
      }
      pts.push(row);
    }
    const quads = [];
    for (let i = 0; i < rows - 1; i++) {
      for (let j = 0; j < cols - 1; j++) {
        const c = [pts[i][j], pts[i][j + 1], pts[i + 1][j + 1], pts[i + 1][j]];
        const avgZ = (c[0].z + c[1].z + c[2].z + c[3].z) / 4;
        quads.push({ c, depthKey: (rows - 1 - i) + j, avgZ });
      }
    }
    quads.sort((a, b) => a.depthKey - b.depthKey); // back-to-front (painter's algorithm)
    const all = pts.flat();
    const minX = Math.min(...all.map((p) => p.x)), maxX = Math.max(...all.map((p) => p.x));
    const minY = Math.min(...all.map((p) => p.y)), maxYFlat = Math.max(...all.map((p) => p.yFlat));
    const pad = 3;
    const w = (maxX - minX) + pad * 2, hgt = (maxYFlat - minY) + pad * 2;
    const ox = -minX + pad, oy = -minY + pad;
    const svg = h('svg:svg', { viewBox: `0 0 ${w} ${hgt}`, class: 'mesh-3d', 'aria-label': 'Mesh, isometric view' });
    const g = h('svg:g', { transform: `translate(${ox},${oy})` });
    // Base outline: the flat perimeter at each edge's lowest point, so the
    // surface reads as floating above a ground plane.
    const top = pts[0];
    const right = pts.map((r) => r[cols - 1]);
    const bottom = [...pts[rows - 1]].reverse();
    const left = [...pts.map((r) => r[0])].reverse();
    const perim = [...top, ...right.slice(1), ...bottom.slice(1), ...left.slice(1, -1)];
    const baseD = perim.map((p, k) => `${k ? 'L' : 'M'} ${p.x} ${p.yFlat}`).join(' ') + ' Z';
    g.append(h('svg:path', { d: baseD, class: 'mesh-3d-base' }));
    // Corner stems ground the surface to the base plane.
    for (const p of [pts[0][0], pts[0][cols - 1], pts[rows - 1][0], pts[rows - 1][cols - 1]]) {
      g.append(h('svg:line', { x1: p.x, y1: p.yFlat, x2: p.x, y2: p.y, class: 'mesh-3d-stem' }));
    }
    for (const q of quads) {
      const t = range > 0 ? (q.avgZ - mn) / range : 0.5;
      const d = `M ${q.c[0].x} ${q.c[0].y} L ${q.c[1].x} ${q.c[1].y} L ${q.c[2].x} ${q.c[2].y} L ${q.c[3].x} ${q.c[3].y} Z`;
      g.append(h('svg:path', { d, class: 'mesh-3d-face', style: { fill: meshColor(t) } }));
    }
    for (let i = 0; i < rows; i++) {
      for (let j = 0; j < cols; j++) {
        const p = pts[i][j];
        const t = range > 0 ? (p.z - mn) / range : 0.5;
        g.append(h('svg:circle', {
          cx: p.x, cy: p.y, r: 3, class: 'mesh-3d-dot', style: { fill: meshColor(t) },
        }, h('svg:title', { text: `X${j} Y${i}: ${p.z.toFixed(3)}mm` })));
      }
    }
    svg.append(g);
    return { svg, exaggeration };
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
  function draw(s) {
    lastState = s;
    const on = canBed(s);
    rotateBtn.hidden = meshView !== '3d';
    const key = [mesh, on, meshLoading, meshErr];
    if (ui && ui.key.every((k, i) => k === key[i])) return;
    ui = null;
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
    // Marlin row 0 is the front (Y0): draw it last so the map faces the
    // viewer like the printer does, matching the corner assistant.
    for (let y = mesh.points.length - 1; y >= 0; y--) {
      for (let x = 0; x < mesh.points[y].length; x++) {
        const v = mesh.points[y][x];
        const cell = h('button', {
          type: 'button', class: 'mesh-cell tnum', style: cellStyle(v),
          'aria-label': `Point X${x} Y${y}, ${v.toFixed(3)} millimetres. Tap to edit.`,
          disabled: !on,
          onclick: () => editPoint(x, y),
        }, v.toFixed(3));
        grid.append(cell);
      }
    }
    grid.style.setProperty('--cols', mesh.points[0].length);
    // Derive the stats from the points: never trust optional fields.
    const all = mesh.points.flat();
    const mn = Math.min(...all), mx = Math.max(...all);
    const span = mx - mn;
    const matrix = rotatedMesh(mesh.points, meshRotation);
    const { svg, exaggeration } = build3D(matrix, mn, span);
    const stage = h('div', { class: `mesh-stage is-${meshView}` },
      h('div', { class: 'mesh-2d-layer' }, grid),
      h('div', { class: 'mesh-3d-layer' }, svg));
    const legendBar = h('span', { class: 'mesh-legend-bar', style: { background: meshLegendGradient() } });
    const heightNote = h('p', { class: 'meta mesh-height-note', text: span > 0 ? `Height x${exaggeration.toFixed(0)} (exaggerated for readability)` : '' });
    const edge = h('p', { class: 'meta mesh-front', text: nearestEdge() });
    body.append(
      h('div', { class: 'mesh-stats' },
        h('span', { class: 'meta' }, 'Min ', h('span', { class: 'num-l tnum', text: mn.toFixed(3) })),
        h('span', { class: 'meta' }, 'Max ', h('span', { class: 'num-l tnum', text: mx.toFixed(3) })),
        h('span', { class: 'meta' }, 'Range ', h('span', { class: 'num-l tnum', text: span.toFixed(3) }))),
      stage,
      edge,
      heightNote,
      h('div', { class: 'mesh-legend meta' },
        h('span', { text: 'Low' }), legendBar, h('span', { text: 'High' })),
    );
    if (rangeNote) body.append(rangeNote);
    ui = { stage, layer3d: stage.lastChild, svg, edge, height: heightNote, key };
    applyView();
  }
  return { el, update: draw };
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
