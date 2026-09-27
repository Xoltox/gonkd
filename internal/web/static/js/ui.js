// Shared widgets built with h(). Each returns a DOM element.
import { h, icon, nextId, setText } from './dom.js';

export function btn(label, opts = {}) {
  return h('button', {
    type: 'button',
    class: `btn btn-${opts.variant || 'secondary'} ${opts.class || ''}`,
    onclick: opts.onClick,
    disabled: opts.disabled,
    'aria-label': opts.aria,
  }, opts.icon && icon(opts.icon), label && h('span', null, label));
}

export function iconBtn(name, label, onClick, opts = {}) {
  return h('button', {
    type: 'button',
    class: `btn btn-secondary btn-icon ${opts.class || ''}`,
    'aria-label': label,
    title: label,
    onclick: onClick,
    disabled: opts.disabled,
  }, icon(name));
}

export const link = (label, href, iconName, variant = 'secondary') =>
  h('a', { class: `btn btn-${variant}`, href }, iconName && icon(iconName), h('span', null, label));

// Hold-to-confirm. Pointer or keyboard (Space/Enter held) for `ms`.
// The fill sweeps via the --p custom property; releasing early resets.
export function holdButton(label, onConfirm, opts = {}) {
  const ms = opts.ms || 1500;
  const hintId = nextId('hold');
  const text = h('span', null, label);
  const el = h('button', {
    type: 'button',
    class: `btn btn-${opts.variant || 'danger'} hold ${opts.class || ''}`,
    'aria-describedby': hintId,
    disabled: opts.disabled,
  },
  h('span', { class: 'hold-fill', 'aria-hidden': 'true' }),
  opts.icon && icon(opts.icon),
  text,
  h('span', { id: hintId, class: 'sr-only', text: opts.hint || `Hold ${ms / 1000} seconds to ${label.toLowerCase()}` }));

  let holding = false;
  let t0 = 0;
  let raf = 0;
  const vibrate = (p) => { try { navigator.vibrate && navigator.vibrate(p); } catch { /* ignore */ } };
  const frame = (t) => {
    if (!holding) return;
    const f = Math.min(1, (t - t0) / ms);
    el.style.setProperty('--p', f);
    if (f < 1) { raf = requestAnimationFrame(frame); return; }
    holding = false;
    el.style.setProperty('--p', 0);
    vibrate([30, 40, 30]);
    onConfirm();
  };
  const begin = () => {
    if (el.disabled || holding) return;
    holding = true;
    t0 = performance.now();
    setText(text, label);
    vibrate(15);
    raf = requestAnimationFrame(frame);
  };
  const end = (early) => {
    if (!holding) return;
    holding = false;
    cancelAnimationFrame(raf);
    el.style.setProperty('--p', 0);
    if (early) {
      setText(text, 'Keep holding');
      setTimeout(() => setText(text, label), 1500);
    }
  };
  el.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    try { el.setPointerCapture(e.pointerId); } catch { /* ignore */ }
    begin();
  });
  el.addEventListener('pointerup', () => end(true));
  el.addEventListener('pointercancel', () => end(false));
  el.addEventListener('lostpointercapture', () => end(false));
  el.addEventListener('keydown', (e) => {
    if ((e.key === ' ' || e.key === 'Enter') && !e.repeat) { e.preventDefault(); begin(); }
  });
  el.addEventListener('keyup', (e) => {
    if (e.key === ' ' || e.key === 'Enter') { e.preventDefault(); end(true); }
  });
  el.addEventListener('blur', () => end(false));
  el.addEventListener('contextmenu', (e) => e.preventDefault());
  return el;
}

// Second-tap confirm: the first tap arms it for 3 s ("Delete?"), the
// second tap acts. With opts.confirm === false it acts on the first tap.
export function confirmButton(label, armedLabel, onConfirm, opts = {}) {
  const text = h('span', null, label);
  const ic = opts.icon ? icon(opts.icon) : null;
  const base = `btn btn-${opts.variant || 'secondary'} ${opts.class || ''}`;
  const el = h('button', { type: 'button', class: base, disabled: opts.disabled }, ic, text);
  let armed = false;
  let timer = 0;
  const reset = () => {
    armed = false;
    el.className = base;
    setText(text, label);
  };
  el.addEventListener('click', () => {
    if (opts.confirm === false || armed) {
      clearTimeout(timer);
      reset();
      onConfirm();
      return;
    }
    armed = true;
    el.className = `btn btn-danger ${opts.class || ''}`;
    setText(text, armedLabel);
    timer = setTimeout(reset, 3000);
  });
  return el;
}

export function card(title, iconName, body, opts = {}) {
  return h('section', { class: `card ${opts.tint ? 'card-' + opts.tint : ''} ${opts.class || ''}` },
    title && h('header', { class: 'card-head' },
      h('h2', { class: 'card-title' }, iconName && icon(iconName), title),
      opts.extra),
    body);
}

export function banner(kind, title, body, opts = {}) {
  return h('div', { class: `banner banner-${kind}`, role: kind === 'danger' ? 'alert' : 'status' },
    icon(opts.icon || (kind === 'ok' ? 'check' : 'alert')),
    h('div', { class: 'banner-body' }, h('strong', null, title), body && h('div', { class: 'banner-text' }, body)),
    opts.action && h('div', { class: 'banner-action' }, opts.action));
}

// Progress bar. Returns {el, set(value, text)}.
export function bar(label, kind) {
  const fill = h('div', { class: 'bar-fill' });
  const track = h('div', { class: `bar ${kind ? 'bar-' + kind : ''}`, role: 'progressbar', 'aria-label': label, 'aria-valuemin': 0, 'aria-valuemax': 100 }, fill);
  const txt = h('span', { class: 'tnum' });
  const el = h('div', { class: 'barwrap' }, h('div', { class: 'bar-label' }, h('span', null, label), txt), track);
  return {
    el,
    set(v, text) {
      v = Math.max(0, Math.min(100, v || 0));
      fill.style.width = v + '%';
      track.setAttribute('aria-valuenow', Math.round(v));
      setText(txt, text ?? '');
    },
  };
}

// Segmented choice (radio group of buttons).
export function seg(label, options, value, onChange, disabled) {
  const wrap = h('div', { class: 'seg', role: 'radiogroup', 'aria-label': label });
  const btns = options.map(([v, t]) => {
    const b = h('button', { type: 'button', role: 'radio', disabled, text: t });
    b.addEventListener('click', () => { select(v); onChange(v); });
    b.dataset.v = String(v);
    return b;
  });
  const select = (v) => btns.forEach((b) => {
    const on = b.dataset.v === String(v);
    b.classList.toggle('on', on);
    b.setAttribute('aria-checked', on);
  });
  select(value);
  wrap.append(...btns);
  return wrap;
}

// [-] value [+] stepper. Returns {el, set(value, pending, disabled)}.
export function stepper(label, unit, step, min, max, onSet) {
  let cur = null;
  const val = h('span', { class: 'tnum' }, '--');
  const out = h('output', { class: 'num-l', 'aria-live': 'polite' }, val, h('span', { class: 'unit' }, unit));
  const change = (d) => { if (cur != null) onSet(Math.max(min, Math.min(max, cur + d))); };
  const minus = iconBtn('minus', `Lower ${label.toLowerCase()}`, () => change(-step));
  const plus = iconBtn('plus', `Raise ${label.toLowerCase()}`, () => change(step));
  const el = h('div', { class: 'stepper' }, h('div', { class: 'stepper-label', text: label }), h('div', { class: 'stepper-row' }, minus, out, plus));
  return {
    el,
    set(v, pending, disabled) {
      cur = v;
      setText(val, v == null ? '--' : v);
      out.classList.toggle('pending', !!pending);
      minus.disabled = disabled || v == null || v <= min;
      plus.disabled = disabled || v == null || v >= max;
    },
  };
}

// Native <dialog>: focus trap and Esc for free. Bottom sheet on phones.
export function sheet(title) {
  const body = h('div', { class: 'sheet-body' });
  const dlg = h('dialog', { class: 'sheet', 'aria-label': title },
    h('div', { class: 'sheet-inner' },
      h('header', { class: 'sheet-head' }, h('h2', { class: 'card-title', text: title }), iconBtn('x', 'Close', () => dlg.close())),
      body));
  dlg.addEventListener('click', (e) => { if (e.target === dlg) dlg.close(); });
  document.body.append(dlg);
  return {
    open(content) { body.replaceChildren(content); if (!dlg.open) dlg.showModal(); },
    close() { if (dlg.open) dlg.close(); },
    body,
    dlg,
  };
}

export function empty(iconName, text, action) {
  return h('div', { class: 'empty' }, icon(iconName, 40), h('p', { text }), action);
}

export const spinner = (text) => h('div', { class: 'empty' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }), h('p', { text }));

export const note = (kind, text) => h('p', { class: `note ${kind}` }, icon('alert', 18), h('span', { text }));
