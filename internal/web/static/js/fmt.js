// Formatting helpers. Degree sign kept as an escape so sources stay ASCII.
export const DEG = '\u00b0C';
export const DOT = ' \u00b7 ';

export const t1 = (v) => (Math.round(v * 10) / 10).toFixed(1);
export const t0 = (v) => String(Math.round(v));
export const pct = (v) => (v >= 99.95 ? '100' : Math.max(0, v).toFixed(1));

export function dur(sec) {
  if (!isFinite(sec) || sec < 0) return '--';
  const s = Math.round(sec);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m`;
  if (m > 0) return `${m}m`;
  return `${s}s`;
}

export const clock = (ms) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

export function hms(ms) {
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export function bytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`;
  return `${(n / 1048576).toFixed(1)} MB`;
}

export function ago(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  return s < 60 ? `${s}s ago` : `${Math.floor(s / 60)}m ago`;
}

export const displayName = (long, short) => (long && long.trim()) || short || 'Untitled';
