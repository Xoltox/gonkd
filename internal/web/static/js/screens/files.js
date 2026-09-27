// Files: SD list with thumbnails and metadata, print, rename, delete, upload.
import { h, icon, region, setText } from '../dom.js';
import { card, btn, iconBtn, bar, holdButton, banner, sheet, empty, spinner, note } from '../ui.js';
import { phaseOf, act, jobActive, loadFiles, startUpload, cancelUpload, clearUpload, fileFor, connected, set } from '../store.js';
import { api, thumbUrl } from '../api.js';
import { screenTitle, pageBanners } from './common.js';
import { dur, bytes, pct, displayName, DEG, DOT } from '../fmt.js';

const GCODE = /\.(gcode|gco|g)$/i;
const view = { q: '', open: '', print: false }; // kept across visits

export function mount(root) {
  const banners = pageBanners();
  const count = h('span', { class: 'count tnum' });
  const refresh = iconBtn('refresh', 'Read the file list from the card again', () => loadFiles(true));
  const upload = uploadPanel();
  const errs = region((s) => s.filesError, (s) => s.filesError &&
    banner('warn', 'Could not load files', s.filesError, { action: btn('Retry', { onClick: () => loadFiles() }) }));
  const search = h('input', { type: 'search', placeholder: 'Search files', value: view.q });
  search.addEventListener('input', () => { view.q = search.value; set({}); });
  const searchWrap = h('label', { class: 'field search' }, h('span', { class: 'sr-only', text: 'Search files' }), search);
  const list = region(listKey, buildList);
  root.append(
    h('div', { class: 'title-row' }, h('h1', { class: 'screen-title' }, 'Files', count), refresh),
    banners.el, upload.el, errs.el, searchWrap, list.el,
  );
  return (s) => {
    banners.update(s);
    setText(count, s.files ? ` (${s.files.length})` : '');
    refresh.disabled = !connected(s) || s.filesLoading || phaseOf(s) === 'uploading';
    refresh.classList.toggle('spin', s.filesLoading);
    upload.update(s);
    errs.update(s);
    searchWrap.hidden = !(s.files && s.files.length > 6);
    list.update(s);
  };
}

function canPrint(s) {
  const p = phaseOf(s);
  return connected(s) && !jobActive(p) && !['killed', 'error'].includes(p);
}

function printingShort(s) {
  const p = phaseOf(s);
  const f = jobActive(p) && s.snap.job ? fileFor(s, s.snap.job.filename) : null;
  return f ? f.short : '';
}

function listKey(s) {
  return JSON.stringify([s.files, s.filesError ? 1 : 0, view.q, view.open, canPrint(s), printingShort(s), connected(s), s.snap && s.snap.limits]);
}

function buildList(s) {
  if (s.files == null) return s.filesError ? null : card(null, null, spinner('Reading the card...'));
  if (!s.files.length) {
    return card(null, null, empty('files', connected(s)
      ? 'No G-code files on the card. Upload one to get going, or check that the card is in.'
      : 'The printer is offline, so the card cannot be read.',
    connected(s) && btn('Read card again', { icon: 'refresh', onClick: () => loadFiles(true) })));
  }
  const q = view.q.toLowerCase();
  const files = s.files
    .filter((f) => !q || displayName(f.long, f.short).toLowerCase().includes(q))
    .sort((a, b) => displayName(a.long, a.short).localeCompare(displayName(b.long, b.short)));
  const ul = h('ul', { class: 'file-list', 'aria-label': 'Files on the SD card' }, files.map((f) => fileRow(s, f)));
  if (!files.length) ul.append(h('li', { class: 'meta', text: `No file matches "${view.q}".` }));
  return ul;
}

function fileRow(s, f) {
  const m = f.meta || {};
  const name = displayName(f.long, f.short);
  const locked = printingShort(s) === f.short;
  const bits = [];
  if (m.estSec) bits.push(dur(m.estSec));
  if (m.filamentG) bits.push(`${m.filamentG.toFixed(1)} g`);
  if (m.layerHeight) bits.push(`${m.layerHeight} mm layers`);
  if (m.nozzle) bits.push(`${m.nozzle} mm nozzle`);
  if (m.hotendC || m.bedC) bits.push(`${m.hotendC ?? '--'} / ${m.bedC ?? '--'}${DEG}`);
  const limits = s.snap && s.snap.limits;
  const tooHot = m.hotendC && limits && m.hotendC > limits.hotendMax;

  const ph = h('div', { class: 'thumb thumb-s thumb-none', 'aria-hidden': 'true' }, icon('cube', 28));
  let th = ph;
  if (m.thumb) {
    th = h('img', { class: 'thumb thumb-s', src: thumbUrl(f.short), alt: '', loading: 'lazy', width: 56, height: 56 });
    th.addEventListener('error', () => th.replaceWith(ph));
  }
  const open = view.open === f.short;
  const more = iconBtn('more', `More actions for ${name}`, () => { view.open = open ? '' : f.short; set({}); });
  more.setAttribute('aria-expanded', open);
  return h('li', { class: `file ${locked ? 'is-locked' : ''}` },
    th,
    h('div', { class: 'file-body' },
      h('p', { class: 'file-name', title: name, text: name }),
      bits.length > 0 && h('p', { class: 'meta tnum', text: bits.join(DOT) }),
      h('p', { class: 'meta' }, h('span', { class: 'mono', text: f.short }), ' ', h('span', { class: 'tnum', text: bytes(f.bytes) }), m.slicer ? DOT + m.slicer : ''),
      locked && h('p', { class: 'meta c-hot' }, icon('play', 14), ' Printing now'),
      tooHot && note('warn', `Sliced for ${m.hotendC}${DEG}, above this printer's limit.`)),
    h('div', { class: 'file-actions' },
      btn('Print', { variant: 'primary', icon: 'play', disabled: !canPrint(s) || locked, onClick: () => act(() => api.print(f.short), `Printing ${name}`) }),
      more),
    open && h('div', { class: 'file-more' },
      btn('Rename', { icon: 'edit', disabled: locked, onClick: () => renameSheet(f) }),
      holdButton('Delete', async () => {
        if (await act(() => api.del(f.short), 'Deleted')) { view.open = ''; loadFiles(); }
      }, { icon: 'trash', disabled: locked, hint: `Hold 1.5 seconds to delete ${name} from the card` })),
  );
}

let rename;
function renameSheet(f) {
  rename = rename || sheet('Rename');
  const input = h('input', { value: displayName(f.long, f.short), maxlength: 120, 'aria-describedby': 'rn-help' });
  const bad = note('warn', 'Use a name without slashes.');
  bad.hidden = true;
  const save = h('button', { type: 'submit', class: 'btn btn-primary' }, h('span', { text: 'Save' }));
  const ok = () => input.value.trim() && !/[\/\\\x00-\x1f]/.test(input.value);
  input.addEventListener('input', () => { bad.hidden = !!ok(); save.disabled = !ok(); });
  const form = h('form', { class: 'stack' },
    h('p', { class: 'meta', id: 'rn-help' }, 'Changes the display name only. The card keeps the short name ', h('span', { class: 'mono', text: f.short }), '.'),
    h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Name' }), input),
    bad,
    h('div', { class: 'btn-row' }, btn('Cancel', { onClick: () => rename.close() }), save));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (!ok()) return;
    let n = input.value.trim();
    if (!GCODE.test(n)) n += '.gcode';
    save.disabled = true;
    if (await act(() => api.rename(f.short, n), 'Renamed')) { rename.close(); loadFiles(); }
    save.disabled = false;
  });
  rename.open(form);
  input.focus();
  input.select();
}

// ---- upload ----

function uploadPanel() {
  const input = h('input', { type: 'file', accept: '.gcode,.gco,.g', class: 'sr-only', tabindex: -1, 'aria-hidden': 'true' });
  const pick = (list) => {
    const file = list && list[0];
    if (!file) return;
    if (!GCODE.test(file.name)) {
      set({ upload: { name: file.name, stage: 'failed', sent: 0, total: 0, error: 'Only .gcode files can be printed.' } });
      return;
    }
    startUpload(file, view.print);
  };
  input.addEventListener('change', () => { pick(input.files); input.value = ''; });
  const choose = btn('Upload G-code', { variant: 'primary', icon: 'upload', onClick: () => input.click() });
  const printBox = h('input', { type: 'checkbox', checked: view.print });
  printBox.addEventListener('change', () => { view.print = printBox.checked; });
  const why = h('p', { class: 'meta' });
  const drop = h('div', { class: 'drop' }, input, choose,
    h('label', { class: 'check' }, printBox, h('span', { text: 'Print when done' })),
    h('span', { class: 'meta hide-phone', text: 'or drop a file here' }));
  let disabled = false;
  drop.addEventListener('dragover', (e) => { if (!disabled) { e.preventDefault(); el.classList.add('is-drag'); } });
  drop.addEventListener('dragleave', () => el.classList.remove('is-drag'));
  drop.addEventListener('drop', (e) => { e.preventDefault(); el.classList.remove('is-drag'); if (!disabled) pick(e.dataTransfer.files); });
  const prog = region((s) => s.upload ? s.upload.stage + s.upload.name : '', buildProgress);
  const el = card(null, null, [drop, why, prog.el], { class: 'upload' });
  return {
    el,
    update(s) {
      const p = phaseOf(s);
      const busy = !!s.upload && ['sending', 'writing'].includes(s.upload.stage);
      const reason = !connected(s) ? 'The printer is not connected.'
        : p === 'uploading' ? 'A file is being written to the card.'
        : jobActive(p) ? 'A print is running. Upload when it ends.'
        : p === 'killed' ? 'Reset the printer first.'
        : busy ? 'One upload at a time. Wait for this one to finish.' : '';
      disabled = !!reason;
      choose.disabled = printBox.disabled = disabled;
      setText(why, reason);
      why.hidden = !reason;
      prog.update(s);
    },
  };
}

function buildProgress(s, live) {
  const up = s.upload;
  if (!up) return null;
  if (up.stage === 'failed') {
    return banner('danger', `Upload failed: ${up.name}`, up.error, {
      action: h('div', { class: 'btn-row' },
        up.file && btn('Retry', { icon: 'refresh', onClick: () => startUpload(up.file, up.print) }),
        iconBtn('x', 'Dismiss', clearUpload)),
    });
  }
  const b1 = bar('Sending to box');
  const b2 = bar('Writing to card', 'bed');
  live((s) => {
    const u = s.upload;
    if (!u) return;
    const v1 = u.stage === 'sending' ? (u.total ? (u.sent / u.total) * 100 : 0) : 100;
    b1.set(v1, `${pct(v1)}%`);
    const v2 = u.stage === 'writing' && u.total ? (u.sent / u.total) * 100 : 0;
    b2.set(v2, u.stage === 'writing' ? `${pct(v2)}% (${bytes(u.sent)} / ${bytes(u.total)})` : 'waiting');
  });
  return h('div', { class: 'upload-prog', 'aria-live': 'polite' },
    h('p', { class: 'file-name', text: up.name }),
    b1.el, b2.el,
    h('div', { class: 'btn-row' }, up.stage === 'sending'
      ? btn('Cancel upload', { icon: 'x', onClick: cancelUpload })
      : holdButton('Cancel', () => act(() => api.cancel(), 'Stopping card write'), { icon: 'x', hint: 'Hold 1.5 seconds to stop writing to the card' })));
}
