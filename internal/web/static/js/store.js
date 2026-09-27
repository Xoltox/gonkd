// App state, live link (SSE with polling fallback) and derived helpers.
import { api, normalizeSnapshot, uploadFile, ApiError, DEFAULT_LIMITS } from './api.js';

export const state = {
  snap: null,          // last Snapshot
  link: 'connecting',  // connecting | live | polling | offline
  lastUpdate: 0,
  now: Date.now(),
  lines: [],           // console: {id, ts, text, dir: 'in'|'out'}
  files: null,         // SDFile[] or null while loading
  filesError: '',
  filesLoading: false,
  presets: [],
  upload: null,        // {name, stage: sending|writing|failed, sent, total, error, file, print}
  toast: null,         // {id, msg, kind}
  homed: false,
  estopped: false,
  hist: { h: [], b: [] },
};

const subs = new Set();
export const subscribe = (fn) => subs.add(fn);
let queued = false;

// Merge a patch and notify subscribers once per frame.
export function set(patch) {
  Object.assign(state, patch);
  if (queued) return;
  queued = true;
  requestAnimationFrame(() => {
    queued = false;
    subs.forEach((fn) => fn(state));
  });
}

// ---- toasts ----
let toastId = 0;
let toastTimer;
export function toast(msg, kind = 'info') {
  clearTimeout(toastTimer);
  set({ toast: { id: ++toastId, msg, kind } });
  if (kind !== 'error') toastTimer = setTimeout(() => set({ toast: null }), 4000);
}

// Run an API call; failures become an error toast. Resolves to success.
export async function act(fn, okMsg) {
  try {
    await fn();
    if (okMsg) toast(okMsg, 'ok');
    return true;
  } catch (e) {
    toast(e instanceof ApiError ? e.message : 'Something went wrong', 'error');
    return false;
  }
}

// ---- console ----
let lineId = 0;
const MAX_LINES = 500;
export function pushLine(text, dir, ts = Date.now()) {
  const lines = state.lines.length >= MAX_LINES ? state.lines.slice(1 - MAX_LINES) : state.lines.slice();
  lines.push({ id: ++lineId, ts, text, dir });
  set({ lines });
}

// ---- status ----
const HIST_MAX = 120; // 10 minutes at one sample per 5 s
let lastHist = 0;
let prevState = '';
let errBefore = '';

function applySnap(raw) {
  const snap = normalizeSnapshot(raw);
  const now = Date.now();
  const patch = { snap, lastUpdate: now, now };
  if (now - lastHist >= 5000) {
    lastHist = now;
    patch.hist = {
      h: state.hist.h.concat(snap.temps.hotendActual).slice(-HIST_MAX),
      b: state.hist.b.concat(snap.temps.bedActual).slice(-HIST_MAX),
    };
  }
  if (!snap.connected) patch.homed = false;
  if (state.estopped && snap.state === 'idle') patch.estopped = false;

  // Upload stage 2: the box writes the file to the card (state uploading).
  const up = state.upload;
  if (up && up.stage === 'writing') {
    if (snap.state === 'uploading' && snap.job) {
      patch.upload = { ...up, sent: snap.job.sentBytes, total: snap.job.totalBytes || up.total };
    } else if (prevState === 'uploading') {
      const newErr = snap.lastError && snap.lastError !== errBefore;
      if (snap.state === 'error' || (newErr && snap.state !== 'printing')) {
        patch.upload = { ...up, stage: 'failed', error: snap.lastError || 'Card write failed' };
      } else {
        patch.upload = null;
        toast(up.print ? 'Upload done, print starting' : 'Upload done', 'ok');
        loadFiles();
      }
    }
  }
  if (['printing', 'cancelling'].includes(prevState) && snap.state === 'idle') loadFiles();
  prevState = snap.state;
  set(patch);
}

let es = null;
let pollTimer = 0;
let openTimer = 0;
let retryTimer = 0;

function startPolling() {
  if (pollTimer) return;
  const tick = async () => {
    try {
      applySnap(await api.status());
      if (state.link !== 'live') set({ link: 'polling' });
    } catch {
      set({ link: 'offline' });
    }
  };
  tick();
  pollTimer = setInterval(tick, 3000);
}

function stopPolling() {
  clearInterval(pollTimer);
  pollTimer = 0;
}

function goLive() {
  clearTimeout(openTimer);
  stopPolling();
  if (state.link !== 'live') set({ link: 'live' });
}

function connectSSE() {
  clearTimeout(retryTimer);
  if (es) es.close();
  if (!('EventSource' in window)) return startPolling();
  es = new EventSource('/gonkd/events');
  openTimer = setTimeout(startPolling, 5000); // slow to open: poll meanwhile
  es.onopen = goLive;
  es.addEventListener('status', (e) => {
    try { applySnap(JSON.parse(e.data)); } catch { /* bad frame */ }
    goLive();
  });
  es.addEventListener('console', (e) => {
    try {
      const d = JSON.parse(e.data);
      if (typeof d.line === 'string') pushLine(d.line, 'in', d.ts || Date.now());
    } catch { /* ignore */ }
  });
  es.onerror = () => {
    if (!es) return;
    startPolling();
    if (es.readyState === EventSource.CLOSED) {
      // Refused (for example 503, too many clients): poll, retry in 30 s.
      es = null;
      retryTimer = setTimeout(connectSSE, 30000);
    }
    // Otherwise the browser reconnects by itself.
  };
}

export async function loadFiles(refresh = false) {
  set({ filesLoading: true });
  try {
    const r = await api.files(refresh);
    set({ files: (r && r.files) || [], filesError: '', filesLoading: false });
  } catch (e) {
    set({ filesError: e.message || 'Could not load files', filesLoading: false });
  }
}

export async function loadPresets() {
  try {
    const p = await api.presets();
    if (Array.isArray(p)) set({ presets: p });
  } catch { /* keep what we have */ }
}

export function start() {
  connectSSE();
  api.console()
    .then((r) => { if (!state.lines.length) (r.lines || []).forEach((l) => pushLine(l, 'in', 0)); })
    .catch(() => {});
  loadFiles();
  loadPresets();
  setInterval(() => set({ now: Date.now() }), 1000);
}

// ---- upload ----
let abortUpload = null;
export function startUpload(file, print) {
  errBefore = (state.snap && state.snap.lastError) || '';
  set({ upload: { name: file.name, stage: 'sending', sent: 0, total: file.size, file, print } });
  const u = uploadFile(file, print, (sent, total) => {
    if (state.upload && state.upload.stage === 'sending') set({ upload: { ...state.upload, sent, total } });
  });
  abortUpload = u.abort;
  u.promise.then(
    () => {
      if (state.upload) set({ upload: { ...state.upload, stage: 'writing', sent: 0, total: file.size } });
      prevState = 'uploading'; // a small file may finish before we see it
    },
    (e) => {
      if (state.upload) set({ upload: { ...state.upload, stage: 'failed', error: e.message } });
    },
  ).finally(() => { abortUpload = null; });
}
export const cancelUpload = () => abortUpload && abortUpload();
export const clearUpload = () => set({ upload: null });

// ---- derived ----
// Phases: offline connecting disconnected idle heating printing paused
// finished cancelling uploading error killed
export function phaseOf(s) {
  const snap = s.snap;
  if (!snap) return s.link === 'offline' ? 'offline' : 'connecting';
  if (s.estopped || (snap.state === 'error' && /kill|halt|m112|emergency/i.test(snap.lastError || ''))) return 'killed';
  if (snap.state === 'printing') {
    const t = snap.temps;
    const cold = (t.hotendTarget > 0 && t.hotendActual < t.hotendTarget - 3) || (t.bedTarget > 0 && t.bedActual < t.bedTarget - 2);
    return (snap.job ? snap.job.progress : 0) < 1 && cold ? 'heating' : 'printing';
  }
  if (snap.state === 'idle') return snap.job && snap.job.progress >= 99.9 ? 'finished' : 'idle';
  return snap.state;
}

export const jobActive = (p) => ['printing', 'paused', 'heating', 'uploading', 'cancelling'].includes(p);
export const limitsOf = (s) => (s.snap && s.snap.limits) || DEFAULT_LIMITS;
export const isStale = (s) => s.lastUpdate > 0 && s.now - s.lastUpdate > 12000;
export const connected = (s) => !!(s.snap && s.snap.connected);

// SD entry for a job filename (short or long).
export function fileFor(s, name) {
  if (!name || !s.files) return undefined;
  const n = name.toLowerCase();
  return s.files.find((f) => f.short.toLowerCase() === n || (f.long || '').toLowerCase() === n);
}
