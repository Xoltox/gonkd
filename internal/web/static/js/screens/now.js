// Now: job progress, temps, tune, and the mid-print action bar.
import { h, icon, region, setText } from '../dom.js';
import { card, bar, btn, link, holdButton, sheet, spinner, empty } from '../ui.js';
import { phaseOf, act, fileFor, connected } from '../store.js';
import { api, thumbUrl } from '../api.js';
import { stateChip, tempState } from '../shell.js';
import { screenTitle, pageBanners, preheat, tuneCard } from './common.js';
import { dur, clock, t1, t0, pct, bytes, displayName, DEG } from '../fmt.js';

const RUNNING = ['printing', 'paused', 'heating', 'cancelling'];

export function mount(root) {
  const banners = pageBanners();
  const job = region(jobKey, buildJob);
  const hot = tempCard('hot');
  const bed = tempCard('bed');
  const tune = tuneCard((s) => !connected(s));
  const tuneWrap = h('div', { class: 'now-tune' }, tune.el);
  const actions = actionBar();
  const pre = preheat((s) => !canActIdle(s));
  const preCard = card(null, null, pre.el, { class: 'precard' });
  const grid = h('div', { class: 'now-grid' }, h('div', { class: 'now-job' }, job.el, actions.el, preCard), h('div', { class: 'now-temps' }, hot.el, bed.el), tuneWrap);
  const waiting = region((s) => s.link === 'offline', (s) => card(null, null, s.link === 'offline'
    ? empty('offline', 'Cannot reach the box. Check that it is powered and on the same Wi-Fi. Retrying in the background.')
    : spinner('Connecting to the box...')));
  root.append(screenTitle('Now'), banners.el, waiting.el, grid);

  return (s) => {
    const p = phaseOf(s);
    const running = RUNNING.includes(p);
    banners.update(s);
    waiting.el.hidden = !!s.snap;
    grid.hidden = !s.snap;
    root.classList.toggle('has-actionbar', running);
    if (!s.snap) return;
    job.update(s);
    preCard.hidden = running || p === 'uploading';
    if (!preCard.hidden) pre.update(s);
    hot.update(s.snap.temps.hotendActual, s.snap.temps.hotendTarget, s.hist.h);
    bed.update(s.snap.temps.bedActual, s.snap.temps.bedTarget, s.hist.b);
    tuneWrap.hidden = !running;
    if (running) tune.update(s);
    actions.update(s, p, running);
  };
}

// ---- job card ----

const canActIdle = (s) => connected(s) && ['idle', 'finished'].includes(phaseOf(s));

function jobKey(s) {
  const p = phaseOf(s);
  const j = s.snap && s.snap.job;
  const f = j && fileFor(s, j.filename);
  const kind = RUNNING.includes(p) ? 'run' : p === 'uploading' ? 'up' : 'idle';
  return [kind, p, j && j.filename, f && f.short, f && f.meta && f.meta.thumb, kind === 'idle' && j && Math.round(j.progress), connected(s), !!s.files].join('|');
}

function buildJob(s, live) {
  const p = phaseOf(s);
  if (RUNNING.includes(p)) return runningCard(s, p, live);
  if (p === 'uploading') return uploadingCard(s, live);
  return idleCard(s, p);
}

function thumb(s, name, size) {
  const f = fileFor(s, name);
  const ph = h('div', { class: `thumb thumb-${size} thumb-none`, 'aria-hidden': 'true' }, icon('cube', size === 'l' ? 36 : 28));
  if (!f || !f.meta || !f.meta.thumb) return ph;
  const img = h('img', { class: `thumb thumb-${size}`, src: thumbUrl(f.short), alt: '' });
  img.addEventListener('error', () => img.replaceWith(ph));
  return img;
}

// Finds the next pause point (M0/M1/M600/M601/M25/@pause, from the file's
// stored meta.pauses) ahead of the job's current byte position, and
// estimates when it will be reached from the current bytes/sec rate.
function nextPauseInfo(job, meta) {
  if (!job || !meta || !meta.pauses || !meta.pauses.length || !job.totalBytes) return null;
  const next = meta.pauses.find((pt) => pt.offset > job.sentBytes);
  if (!next) return null;
  let eta = null;
  if (job.sentBytes > 0 && job.elapsedSec > 0) {
    const rate = job.sentBytes / job.elapsedSec; // bytes/sec so far
    if (rate > 0) eta = Math.max(0, (next.offset - job.sentBytes) / rate);
  }
  return { layer: next.layer, msg: next.msg || next.cmd, eta };
}

function runningCard(s, p, live) {
  const job = s.snap.job;
  if (!job) return card(null, null, h('p', { text: 'Waiting for job details...' }));
  const f = fileFor(s, job.filename);
  const name = displayName(f && f.long, job.filename);
  const heating = p === 'heating';
  const num = h('span', { class: 'display-xl' });
  const big = h('div', { class: 'big-pct' }, num, h('span', { class: 'pct-sign', text: '%' }));
  const pb = bar('Print progress', heating ? 'indeterminate' : p === 'paused' ? 'paused' : '');
  const nextPause = h('p', { class: 'meta' });
  const time = (label) => {
    const dd = h('dd', { class: 'num-l tnum', text: '--' });
    return { el: h('div', null, h('dt', { text: label }), dd), dd };
  };
  const el = time('Elapsed');
  const ls = time('Left (slicer)');
  const ll = time('Left (live)');
  const da = time('Done at');
  const est = f && f.meta && f.meta.estSec;
  live((s) => {
    const j = s.snap.job;
    if (!j) return;
    setText(num, heating ? '--' : pct(j.progress));
    big.setAttribute('aria-label', heating ? 'Heating' : `${pct(j.progress)} percent`);
    pb.set(j.progress);
    setText(el.dd, dur(j.elapsedSec));
    const leftSlicer = est ? Math.max(0, est - j.elapsedSec) : null;
    ls.el.hidden = leftSlicer == null;
    if (leftSlicer != null) setText(ls.dd, dur(leftSlicer));
    const showLive = j.progress >= 5 && j.etaSec > 0;
    setText(ll.dd, showLive ? dur(j.etaSec) : '--');
    const left = showLive ? j.etaSec : leftSlicer;
    setText(da.dd, left != null ? clock(Date.now() + left * 1000) : '--');
    const np = nextPauseInfo(j, f && f.meta);
    nextPause.hidden = !np;
    if (np) setText(nextPause, `Next pause: layer ${np.layer || '?'}, ${np.msg}${np.eta != null ? ` - about ${dur(np.eta)}` : ''}`);
  });
  return card(null, null, [
    h('div', { class: 'job-top' },
      thumb(s, job.filename, 'l'),
      h('div', { class: 'job-info' }, stateChip(p), h('p', { class: 'job-name', title: name, text: name }),
        f && f.long && h('p', { class: 'mono meta', text: f.short })),
      big),
    pb.el,
    heating && h('p', { class: 'meta', text: 'Heating up before the first layer. The print starts on its own.' }),
    p === 'paused' && h('p', { class: 'meta' }, icon('pause', 16), ' Paused. The nozzle may ooze; resume soon or cancel.'),
    nextPause,
    h('dl', { class: 'times' }, el.el, ls.el, ll.el, da.el),
  ], { class: 'job' });
}

function uploadingCard(s, live) {
  const job = s.snap.job;
  const f = job && fileFor(s, job.filename);
  const pb = bar('Writing to card', 'bed');
  live((s) => {
    const j = s.snap.job;
    if (!j) return;
    const v = j.totalBytes ? (j.sentBytes / j.totalBytes) * 100 : 0;
    pb.set(v, `${pct(v)}% (${bytes(j.sentBytes)} / ${bytes(j.totalBytes)})`);
  });
  return card('Writing to card', 'upload', [
    h('p', { class: 'job-name', text: displayName(f && f.long, job && job.filename) }),
    pb.el,
    h('p', { class: 'meta', text: "The box is copying the file to the printer's SD card. Motion is locked until it finishes." }),
  ]);
}

function idleCard(s, p) {
  const job = s.snap.job;
  const f = job && fileFor(s, job.filename);
  const canAct = canActIdle(s);
  const finished = p === 'finished';
  const title = finished ? 'Done. Nice one.' : p === 'idle' ? 'Ready for a job' : p === 'killed' ? 'Halted' : 'Not ready';
  return card(null, null, [
    h('div', { class: 'idle-head' },
      h('div', null, h('p', { class: 'overline', text: finished ? 'Print finished' : 'Printer' }), h('p', { class: 'title', text: title })),
      stateChip(p)),
    job && h('div', { class: 'lastjob' },
      thumb(s, job.filename, 's'),
      h('div', { class: 'grow' },
        h('p', { class: 'overline', text: 'Last job' }),
        h('p', { class: 'job-name', text: displayName(f && f.long, job.filename) }),
        h('p', { class: 'meta tnum', text: `${job.progress >= 99.9 ? 'Finished' : `Stopped at ${pct(job.progress)}%`}, ${dur(job.elapsedSec)}` }))),
    h('div', { class: 'btn-row' },
      job && btn('Print again', { variant: 'primary', icon: 'play', disabled: !canAct || !f, onClick: () => act(() => api.print(f.short), 'Print started') }),
      link('Pick a file', '#/files', 'files')),
    job && !f && s.files && h('p', { class: 'meta', text: 'That file is no longer on the card.' }),
  ], { class: 'idle' });
}

// ---- temps ----

function tempCard(kind) {
  const name = kind === 'hot' ? 'Hotend' : 'Bed';
  const st = h('span', { class: 'temp-st' });
  const val = h('span', { class: `num-xl tnum c-${kind}` });
  const tgt = h('span', { class: 'temp-target tnum' });
  const spark = h('svg:svg', { class: `spark c-${kind}`, viewBox: '0 0 100 30', preserveAspectRatio: 'none', 'aria-hidden': 'true' });
  const el = card(null, null, [
    h('div', { class: 'temp-head' }, h('span', { class: `temp-name c-${kind}` }, icon(kind === 'hot' ? 'flame' : 'bed'), name), st),
    h('div', { class: 'temp-vals' }, val, tgt),
    spark,
  ], { tint: kind, class: 'temp' });
  let lastSt = '';
  let lastHist = 0;
  return {
    el,
    update(a, t, hist) {
      setText(val, t1(a));
      setText(tgt, '/ ' + (t > 0 ? t0(t) + DEG : 'off'));
      let s = tempState(a, t);
      let ic = { off: 'minus', 'at temp': 'check', heating: 'up', cooling: 'down' }[s];
      let tone = s === 'at temp' ? 'ok' : s === 'heating' ? kind : '';
      if (a < -10 || a > 300) { s = 'check thermistor'; ic = 'alert'; tone = 'danger'; }
      const key = s + tone;
      if (key !== lastSt) {
        lastSt = key;
        st.className = `temp-st ${tone ? 'c-' + tone : ''}`;
        st.replaceChildren(icon(ic, 18), s[0].toUpperCase() + s.slice(1));
      }
      if (hist.length !== lastHist) {
        lastHist = hist.length;
        spark.innerHTML = sparkPath(hist);
      }
    },
  };
}

function sparkPath(data) {
  if (data.length < 2) return '';
  const min = Math.min(...data) - 2;
  const max = Math.max(...data) + 2;
  const pts = data.map((v, i) => `${((i / (data.length - 1)) * 100).toFixed(1)},${(29 - ((v - min) / (max - min)) * 28).toFixed(1)}`).join(' ');
  return `<polyline points="${pts}" fill="none" stroke="currentColor" stroke-width="1.5" vector-effect="non-scaling-stroke"/>`;
}

// ---- sticky action bar + babystep sheet ----

function actionBar() {
  const baby = sheet('Babystep Z');
  const off = h('span', { class: 'tnum' });
  const step = (d, label) => btn(null, { aria: label, onClick: () => act(() => api.babystep(d)) });
  const babyBody = h('div', { class: 'babystep' },
    h('p', { class: 'meta', text: 'Nudge the nozzle while printing. Minus moves it closer to the bed.' }),
    h('div', { class: 'baby-val' }, h('span', { class: 'overline', text: 'Offset this print' }), h('span', { class: 'num-xl' }, off, h('span', { class: 'unit', text: 'mm' }))),
    h('div', { class: 'baby-grid' },
      withText(step(-0.05, 'Lower nozzle 0.05 mm'), '-0.05'),
      withText(step(-0.01, 'Lower nozzle 0.01 mm'), '-0.01'),
      withText(step(0.01, 'Raise nozzle 0.01 mm'), '+0.01'),
      withText(step(0.05, 'Raise nozzle 0.05 mm'), '+0.05')),
    h('div', { class: 'baby-labels meta' }, h('span', { text: 'Closer' }), h('span', { text: 'Farther' })));

  const inner = region((s) => phaseOf(s), (s) => {
    const p = phaseOf(s);
    const busy = p === 'cancelling';
    return [
      p === 'paused'
        ? btn('Resume', { variant: 'primary', icon: 'play', disabled: busy, onClick: () => act(() => api.resume(), 'Resuming') })
        : btn('Pause', { icon: 'pause', disabled: busy || p === 'heating', onClick: () => act(() => api.pause(), 'Pausing after the current move') }),
      btn('Babystep', { icon: 'layers', disabled: busy, onClick: () => baby.open(babyBody) }),
      holdButton('Cancel', () => act(() => api.cancel(), 'Cancelling print'), { icon: 'x', disabled: busy, hint: 'Hold 1.5 seconds to cancel the print' }),
    ];
  });
  const el = h('div', { class: 'actionbar', role: 'region', 'aria-label': 'Print controls' }, inner.el);
  return {
    el,
    update(s, p, running) {
      el.hidden = !running;
      if (!running) { baby.close(); return; }
      inner.update(s);
      const v = (s.snap.job && s.snap.job.babystepMm) || 0;
      setText(off, (v > 0 ? '+' : '') + v.toFixed(2));
    },
  };
}

function withText(b, t) {
  b.append(h('span', { class: 'tnum', text: t }));
  return b;
}
