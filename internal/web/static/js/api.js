// HTTP helpers for the gonkd API (contract: notes/design/ui-contract.md).

export const DEFAULT_LIMITS = { hotendMax: 260, bedMax: 110 };

const num = (v) => (typeof v === 'number' && isFinite(v) ? v : 0);

// Temps may arrive as HotendActual (Go field names, no json tags) or
// hotendActual. Accept both and normalize to camelCase.
export function normalizeSnapshot(raw) {
  const t = (raw && raw.temps) || {};
  const pick = (k) => num(t[k] ?? t[k[0].toUpperCase() + k.slice(1)]);
  return {
    ...raw,
    temps: {
      hotendActual: pick('hotendActual'),
      hotendTarget: pick('hotendTarget'),
      bedActual: pick('bedActual'),
      bedTarget: pick('bedTarget'),
      fanPercent: pick('fanPercent'),
    },
  };
}

export class ApiError extends Error {}

async function req(method, path, body) {
  let res;
  try {
    res = await fetch(path, {
      method,
      headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new ApiError('Box not reachable');
  }
  if (!res.ok) {
    let msg = '';
    try { msg = (await res.text()).trim(); } catch { /* ignore */ }
    throw new ApiError(msg || `Request failed (${res.status})`);
  }
  return res;
}

const post = (path, body = {}) => req('POST', path, body).then(() => undefined);
const getJSON = (path) => req('GET', path).then((r) => r.json());

export const api = {
  status: () => getJSON('/gonkd/status').then(normalizeSnapshot),
  console: () => getJSON('/gonkd/console'),
  files: (refresh) => getJSON('/gonkd/files' + (refresh ? '?refresh=1' : '')),
  presets: () => getJSON('/gonkd/presets'),
  savePresets: (list) => req('PUT', '/gonkd/presets', list),
  send: (cmd) => post('/gonkd/send', { cmd }),
  del: (name) => post('/gonkd/files/delete', { name }),
  rename: (name, long) => post('/gonkd/files/rename', { name, long }),
  print: (name) => post('/gonkd/job/print', { name }),
  pause: () => post('/gonkd/job/pause'),
  resume: () => post('/gonkd/job/resume'),
  continue: () => post('/gonkd/job/continue'),
  cancel: () => post('/gonkd/job/cancel'),
  estop: () => post('/gonkd/emergency'),
  babystep: (deltaMm) => post('/gonkd/babystep', { deltaMm }),
  jog: (axis, dist, feed) => post('/gonkd/jog', { axis, dist, feed }),
  heat: (body) => post('/gonkd/heat', body),
  tune: (body) => post('/gonkd/tune', body),
  mesh: () => getJSON('/gonkd/mesh'),
  // start/next reply with the leveling state as JSON; abort/finish reply 204.
  meshLevelStep: (action) => req('POST', '/gonkd/mesh/level', { action }).then((r) => r.json()),
  meshLevelEnd: (action, save) => post('/gonkd/mesh/level', { action, save: !!save }),
  meshZAdjust: (mm) => req('POST', '/gonkd/mesh/zadjust', { mm }).then((r) => r.json()),
  meshZOffset: (z, save) => post('/gonkd/mesh/zoffset', { z, save: !!save }),
  meshPoint: (x, y, z, save) => post('/gonkd/mesh/point', { x, y, z, save: !!save }),
  bedCorner: (corner) => post('/gonkd/bed/corner', { corner }),
  position: () => post('/gonkd/position'),
};

export const thumbUrl = (short) => '/gonkd/files/thumb?name=' + encodeURIComponent(short);

// Stage 1 of an upload: bytes to the box, with progress. XHR because
// fetch has no upload progress.
export function uploadFile(file, print, onProgress) {
  const xhr = new XMLHttpRequest();
  const promise = new Promise((resolve, reject) => {
    xhr.open('POST', '/api/files/local');
    xhr.upload.onprogress = (e) => onProgress(e.loaded, e.lengthComputable ? e.total : file.size);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) resolve();
      else reject(new ApiError((xhr.responseText || '').trim() || `Upload failed (${xhr.status})`));
    };
    xhr.onerror = () => reject(new ApiError('Connection lost during upload'));
    xhr.onabort = () => reject(new ApiError('Upload cancelled'));
    const fd = new FormData();
    fd.append('file', file, file.name);
    fd.append('print', print ? 'true' : 'false');
    xhr.send(fd);
  });
  return { promise, abort: () => xhr.abort() };
}
