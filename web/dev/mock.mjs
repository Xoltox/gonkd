// Zero-dependency dev server for the Gonk'd UI: serves internal/web/static
// and mocks the gonkd API, including SSE with a simulated print.
// Never talks to a real printer.
//
//   node web/dev/mock.mjs            -> http://localhost:5173
//   PORT=8080 node web/dev/mock.mjs
//
// Force a state from the browser console or a tab:
//   /__mock?s=idle|heating|printing|paused|finished|error|killed|disconnected|empty|uploadfail|offline|online
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { extname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('../../internal/web/static/', import.meta.url));
const PORT = Number(process.env.PORT) || 5173;
const TYPES = {
  '.html': 'text/html; charset=utf-8', '.css': 'text/css; charset=utf-8', '.js': 'text/javascript; charset=utf-8',
  '.woff2': 'font/woff2', '.svg': 'image/svg+xml', '.png': 'image/png', '.json': 'application/json',
};

// ---- simulated printer ----
const LIMITS = { hotendMax: 260, bedMax: 110 };
let files = [
  { short: 'BENCHY~1.GCO', long: 'benchy_0.2mm_PLA.gcode', bytes: 2841233, meta: { thumb: true, estSec: 5220, filamentG: 14.8, filamentMm: 4960, layerHeight: 0.2, nozzle: 0.4, hotendC: 205, bedC: 60, slicer: 'OrcaSlicer 2.2.0' } },
  { short: 'CALCUB~1.GCO', long: 'calibration_cube_20mm.gcode', bytes: 512330, meta: { thumb: true, estSec: 1860, filamentG: 6.1, layerHeight: 0.2, nozzle: 0.4, hotendC: 200, bedC: 60, slicer: 'PrusaSlicer 2.8.1' } },
  { short: 'CABLEC~1.GCO', long: 'cable_clip_x8_PETG_long_name_that_wraps_on_small_phones.gcode', bytes: 1203880, meta: { estSec: 3420, filamentG: 9.4, layerHeight: 0.16, nozzle: 0.4, hotendC: 235, bedC: 80 } },
  { short: 'TEST.GCO', long: '', bytes: 4096 },
];
let presets = [{ name: 'PLA', hotend: 200, bed: 60 }, { name: 'PETG', hotend: 235, bed: 80 }];
const p = {
  state: 'idle', connected: true,
  temps: { HotendActual: 24.1, HotendTarget: 0, BedActual: 23.4, BedTarget: 0, FanPercent: 0 },
  job: null, tune: { speed: 100, flow: 100, fan: 0 },
  capabilities: ['AUTOREPORT_TEMP', 'AUTOREPORT_SD_STATUS', 'EEPROM', 'EMERGENCY_PARSER', 'SDCARD', 'LONG_FILENAME', 'BABYSTEPPING', 'PROMPT_SUPPORT', 'THERMAL_PROTECTION'],
  lastError: '', version: '0.1.0-dev', firmware: 'Marlin 2.1.2.7', limits: LIMITS,
};
let offline = false;
let uploadFail = false;
let upload = null; // {file, sent, total, print}

const clients = new Set();
const snapshot = () => ({ ...p, job: p.job ? { ...p.job } : undefined });
function emit(event, data) {
  const msg = `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
  for (const res of clients) res.write(msg);
}
const say = (line) => emit('console', { line, ts: Date.now() });
const pushStatus = () => emit('status', snapshot());

function newJob(f) {
  return { filename: f.short, mode: 'sd', totalBytes: f.bytes, sentBytes: 0, progress: 0, startedAt: new Date().toISOString(), elapsedSec: 0, etaSec: 0, babystepMm: 0 };
}
function startPrint(f) {
  p.state = 'printing';
  p.job = newJob(f);
  p.temps.HotendTarget = f.meta?.hotendC || 200;
  p.temps.BedTarget = f.meta?.bedC || 60;
  p.lastError = '';
  say(`echo:Now fresh file: ${f.short}`);
  say('File opened: ' + f.short + ' Size: ' + f.bytes);
  say('File selected');
}

function approach(cur, target, rate) {
  const goal = target > 0 ? target : 23;
  const d = goal - cur;
  return Math.abs(d) < 0.3 ? goal + (Math.random() - 0.5) * 0.3 : cur + Math.sign(d) * Math.min(Math.abs(d), rate) + (Math.random() - 0.5) * 0.2;
}

let tick = 0;
setInterval(() => {
  tick++;
  const t = p.temps;
  if (p.state !== 'disconnected' && p.state !== 'error') {
    t.HotendActual = approach(t.HotendActual, t.HotendTarget, 6);
    t.BedActual = approach(t.BedActual, t.BedTarget, 2.5);
  }
  if (p.state === 'printing' && p.job) {
    const hot = Math.abs(t.HotendActual - t.HotendTarget) < 3 && Math.abs(t.BedActual - t.BedTarget) < 2;
    if (hot || p.job.progress > 0) {
      p.job.progress = Math.min(100, p.job.progress + 0.25 * (p.tune.speed / 100));
      p.job.elapsedSec += 30;
      p.job.sentBytes = Math.round(p.job.totalBytes * p.job.progress / 100);
      p.job.etaSec = p.job.progress > 1 ? (p.job.elapsedSec / p.job.progress) * (100 - p.job.progress) : 0;
      if (tick % 4 === 0) say(`SD printing byte ${p.job.sentBytes}/${p.job.totalBytes}`);
      if (p.job.progress >= 100) {
        p.state = 'idle';
        t.HotendTarget = 0; t.BedTarget = 0;
        say('Done printing file');
      }
    } else p.job.elapsedSec += 0.5;
  }
  if (upload) {
    upload.sent = Math.min(upload.total, upload.sent + upload.total / 10);
    p.job.sentBytes = upload.sent;
    p.job.progress = upload.sent / upload.total * 100;
    if (uploadFail && upload.sent > upload.total / 2) {
      p.state = 'idle'; p.job = null; p.lastError = 'card write failed: timeout waiting for ok'; upload = null;
      say('Error:No SD card');
    } else if (upload.sent >= upload.total) {
      files.push(upload.file);
      const pr = upload.print;
      p.job = null; upload = null;
      p.state = 'idle';
      say('Done saving file.');
      if (pr) startPrint(files[files.length - 1]);
    }
  }
  if (tick % 4 === 0 && p.connected) say(`T:${t.HotendActual.toFixed(2)} /${t.HotendTarget.toFixed(2)} B:${t.BedActual.toFixed(2)} /${t.BedTarget.toFixed(2)} @:0 B@:0`);
  pushStatus();
}, 500);

function scenario(s) {
  offline = false; uploadFail = false;
  const f = files[0];
  p.connected = true; p.lastError = '';
  const reset = () => { p.job = null; p.temps.HotendTarget = 0; p.temps.BedTarget = 0; };
  switch (s) {
    case 'idle': p.state = 'idle'; reset(); break;
    case 'heating': startPrint(f); p.temps.HotendActual = 60; p.temps.BedActual = 30; break;
    case 'printing': startPrint(f); Object.assign(p.temps, { HotendActual: 205, BedActual: 60 }); Object.assign(p.job, { progress: 42.3, elapsedSec: 2210, etaSec: 3010, babystepMm: -0.02 }); break;
    case 'paused': scenario('printing'); p.state = 'paused'; break;
    case 'finished': p.state = 'idle'; p.job = { ...newJob(f), progress: 100, elapsedSec: 5301 }; p.temps.HotendTarget = 0; p.temps.BedTarget = 0; break;
    case 'error': p.state = 'error'; p.lastError = 'Printer reported: Error:Thermal Runaway, system stopped! Heater_ID: 0'; break;
    case 'killed': p.state = 'error'; p.lastError = 'Printer halted. kill() called!'; reset(); break;
    case 'disconnected': p.state = 'disconnected'; p.connected = false; reset(); break;
    case 'empty': files = []; p.state = 'idle'; reset(); break;
    case 'uploadfail': uploadFail = true; p.state = 'idle'; reset(); break;
    case 'offline': offline = true; for (const c of clients) c.destroy(); clients.clear(); break;
    case 'online': break;
  }
  pushStatus();
}

// ---- HTTP ----
const readBody = (req) => new Promise((resolve) => {
  const chunks = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => resolve(Buffer.concat(chunks)));
});
const json = (res, code, data) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(data)); };
const fail = (res, code, msg) => { res.writeHead(code, { 'Content-Type': 'text/plain' }); res.end(msg + '\n'); };
const done = (res) => { res.writeHead(204); res.end(); };
const findFile = (name) => files.find((f) => f.short.toLowerCase() === String(name).toLowerCase());
const busy = () => ['printing', 'paused', 'uploading', 'cancelling'].includes(p.state);

async function api(req, res, url) {
  const path = url.pathname;
  const body = req.method === 'POST' || req.method === 'PUT' ? await readBody(req) : null;
  const j = () => { try { return JSON.parse(body.toString() || '{}'); } catch { return {}; } };

  if (path === '/gonkd/events') {
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'keep-alive' });
    res.write(`event: status\ndata: ${JSON.stringify(snapshot())}\n\n`);
    clients.add(res);
    const ping = setInterval(() => res.write(': ping\n\n'), 15000);
    req.on('close', () => { clearInterval(ping); clients.delete(res); });
    return;
  }
  if (path === '/gonkd/status') return json(res, 200, snapshot());
  if (path === '/gonkd/console') return json(res, 200, { lines: ['start', 'echo:Marlin 2.1.2.7', 'FIRMWARE_NAME:Marlin 2.1.2.7 (Sep 20 2026) SOURCE_CODE_URL:github.com/MarlinFirmware/Marlin PROTOCOL_VERSION:1.0 MACHINE_TYPE:Ender-3 4.2.2 EXTRUDER_COUNT:1', 'ok'] });
  if (path === '/gonkd/files') return p.connected ? json(res, 200, { files }) : json(res, 200, { files: [] });
  if (path === '/gonkd/files/thumb') {
    const f = findFile(url.searchParams.get('name'));
    if (!f || !f.meta?.thumb) return fail(res, 404, 'no thumbnail');
    const hue = f.short.charCodeAt(0) * 7 % 360;
    res.writeHead(200, { 'Content-Type': 'image/svg+xml', 'Cache-Control': 'max-age=86400' });
    return res.end(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" fill="#2a2621"/><path d="M14 44h36l-6-16H22z" fill="hsl(${hue} 70% 55%)"/><rect x="26" y="18" width="12" height="10" fill="hsl(${hue} 70% 45%)"/></svg>`);
  }
  if (path === '/gonkd/presets' && req.method === 'GET') return json(res, 200, presets);
  if (path === '/gonkd/presets' && req.method === 'PUT') {
    const list = j();
    if (!Array.isArray(list) || list.length > 8 || list.some((x) => !x.name || x.hotend < 0 || x.hotend > LIMITS.hotendMax || x.bed < 0 || x.bed > LIMITS.bedMax)) return fail(res, 400, 'invalid presets');
    presets = list;
    return done(res);
  }
  if (req.method !== 'POST') return fail(res, 405, 'method not allowed');

  switch (path) {
    case '/gonkd/send': {
      const cmd = String(j().cmd || '').trim().toUpperCase();
      if (!cmd) return fail(res, 400, 'bad request');
      setTimeout(() => {
        if (cmd.startsWith('M114')) say('X:0.00 Y:0.00 Z:10.00 E:0.00 Count X:0 Y:0 Z:4000');
        else if (cmd.startsWith('M115')) say('FIRMWARE_NAME:Marlin 2.1.2.7 (Sep 20 2026) PROTOCOL_VERSION:1.0 MACHINE_TYPE:Ender-3 4.2.2 EXTRUDER_COUNT:1');
        else if (cmd.startsWith('M105')) say(`ok T:${p.temps.HotendActual.toFixed(2)} /${p.temps.HotendTarget.toFixed(2)} B:${p.temps.BedActual.toFixed(2)} /${p.temps.BedTarget.toFixed(2)} @:0 B@:0`);
        else if (cmd.startsWith('M503')) { say('echo:  G21    ; Units in mm (mm)'); say('echo:  M92 X80.00 Y80.00 Z400.00 E93.00'); }
        else if (/^[GM]\d/.test(cmd)) { /* fine */ } else say(`echo:Unknown command: "${cmd}"`);
        say('ok');
      }, 150);
      return done(res);
    }
    case '/gonkd/files/delete': {
      const f = findFile(j().name);
      if (!f) return fail(res, 404, 'no such file');
      files = files.filter((x) => x !== f);
      return done(res);
    }
    case '/gonkd/files/rename': {
      const b = j();
      const f = findFile(b.name);
      if (!f || !b.long) return fail(res, 400, 'bad request');
      f.long = b.long;
      return done(res);
    }
    case '/gonkd/job/print': {
      const f = findFile(j().name);
      if (!f) return fail(res, 404, 'no such file');
      if (busy()) return fail(res, 409, 'a job is already active');
      startPrint(f);
      return done(res);
    }
    case '/gonkd/job/pause': if (p.state !== 'printing') return fail(res, 409, 'not printing'); p.state = 'paused'; say('// action:paused'); return done(res);
    case '/gonkd/job/resume': if (p.state !== 'paused') return fail(res, 409, 'not paused'); p.state = 'printing'; say('// action:resumed'); return done(res);
    case '/gonkd/job/cancel':
      if (!busy()) return fail(res, 409, 'no active job');
      p.state = 'cancelling'; upload = null;
      setTimeout(() => { p.state = 'idle'; p.temps.HotendTarget = 0; p.temps.BedTarget = 0; say('echo:Print cancelled'); }, 1500);
      return done(res);
    case '/gonkd/emergency': p.state = 'error'; p.lastError = 'Printer halted. kill() called!'; p.temps.HotendTarget = 0; p.temps.BedTarget = 0; say('Error:Printer halted. kill() called!'); return done(res);
    case '/gonkd/babystep': if (p.job) p.job.babystepMm = Math.round((p.job.babystepMm + (j().deltaMm || 0)) * 1000) / 1000; return done(res);
    case '/gonkd/jog': {
      if (busy()) return fail(res, 409, 'move disabled while a job is active');
      const b = j();
      say(`echo:G91 G1 ${b.axis}${b.dist} F${b.feed} G90`); say('ok');
      return done(res);
    }
    case '/gonkd/heat': {
      const b = j();
      if (b.hotend != null && (b.hotend < 0 || b.hotend > LIMITS.hotendMax)) return fail(res, 400, `hotend must be 0 to ${LIMITS.hotendMax}`);
      if (b.bed != null && (b.bed < 0 || b.bed > LIMITS.bedMax)) return fail(res, 400, `bed must be 0 to ${LIMITS.bedMax}`);
      if (b.hotend != null) p.temps.HotendTarget = b.hotend;
      if (b.bed != null) p.temps.BedTarget = b.bed;
      return done(res);
    }
    case '/gonkd/tune': {
      const b = j();
      if (b.speed != null && (b.speed < 10 || b.speed > 500)) return fail(res, 400, 'speed out of range');
      if (b.flow != null && (b.flow < 50 || b.flow > 200)) return fail(res, 400, 'flow out of range');
      if (b.fan != null && (b.fan < 0 || b.fan > 100)) return fail(res, 400, 'fan out of range');
      setTimeout(() => Object.assign(p.tune, b), 400); // confirm a bit later: shows pending state
      if (b.fan != null) p.temps.FanPercent = b.fan;
      return done(res);
    }
    case '/api/files/local': {
      if (busy()) return fail(res, 409, 'printer busy');
      if (!p.connected) return fail(res, 503, 'printer not connected');
      const text = body.toString('latin1');
      const name = (text.match(/filename="([^"]+)"/) || [])[1] || 'upload.gcode';
      const print = /name="print"\r\n\r\ntrue/.test(text);
      const base = name.replace(/\.[^.]+$/, '').replace(/[^A-Za-z0-9]/g, '').toUpperCase().slice(0, 6) || 'FILE';
      const f = { short: `${base}~${files.length + 1}.GCO`, long: name, bytes: body.length, meta: { estSec: 2400, filamentG: 8.2, layerHeight: 0.2, nozzle: 0.4, hotendC: 210, bedC: 60 } };
      upload = { file: f, sent: 0, total: f.bytes, print };
      p.state = 'uploading';
      p.job = { ...newJob(f), filename: name };
      return json(res, 201, { done: true });
    }
  }
  return fail(res, 404, 'not found');
}

http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x');
  if (url.pathname === '/__mock') {
    scenario(url.searchParams.get('s'));
    return json(res, 200, { ok: true, state: p.state });
  }
  if (url.pathname.startsWith('/gonkd/') || url.pathname.startsWith('/api/')) {
    if (offline) return req.socket.destroy();
    return api(req, res, url);
  }
  // Static files; unknown paths fall back to index.html like the Go server.
  let rel = normalize(decodeURIComponent(url.pathname)).replace(/^([/\\])+/, '');
  if (rel.includes('..')) return fail(res, 400, 'bad path');
  if (!rel) rel = 'index.html';
  try {
    const data = await readFile(join(ROOT, rel));
    res.writeHead(200, { 'Content-Type': TYPES[extname(rel)] || 'application/octet-stream', 'Cache-Control': 'no-cache' });
    res.end(data);
  } catch {
    const data = await readFile(join(ROOT, 'index.html'));
    res.writeHead(200, { 'Content-Type': TYPES['.html'] });
    res.end(data);
  }
}).listen(PORT, () => console.log(`gonkd mock UI on http://localhost:${PORT}`));
