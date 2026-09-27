// Small DOM helpers. No framework: build elements with h(), refresh
// live values in place, rebuild a region only when its structure changes.

// h('button', {class: 'btn', onclick: fn}, 'Label', child, ...)
// Attribute rules: on* keys become listeners, `class`/`text`/`html` are
// shortcuts, false/null/undefined skip the attribute, true sets it empty.
export function h(tag, attrs, ...kids) {
  const el = tag.startsWith('svg:')
    ? document.createElementNS('http://www.w3.org/2000/svg', tag.slice(4))
    : document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (k === 'text') el.textContent = v;
      else if (k === 'html') el.innerHTML = v;
      else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
      else if (k === 'value' || k === 'checked') el[k] = v;
      else el.setAttribute(k, v === true ? '' : v);
    }
  }
  append(el, kids);
  return el;
}

function append(el, kids) {
  for (const k of kids) {
    if (k == null || k === false || k === '') continue;
    if (Array.isArray(k)) append(el, k);
    else el.append(k instanceof Node ? k : String(k));
  }
}

// <svg><use href="#i-name"></svg> from the sprite in index.html.
export function icon(name, size = 24) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('class', 'icon');
  svg.setAttribute('width', size);
  svg.setAttribute('height', size);
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', '#i-' + name);
  svg.append(use);
  return svg;
}

// A region rebuilds its children only when key(s) changes. Inside build,
// call live(fn) to register a callback that refreshes values on every
// update without touching structure (keeps focus and hold state intact).
export function region(key, build) {
  const el = h('div', { class: 'region' });
  let last;
  let lives = [];
  return {
    el,
    update(s) {
      const k = key(s);
      if (k !== last) {
        last = k;
        lives = [];
        const out = build(s, (fn) => lives.push(fn));
        el.replaceChildren(...[].concat(out ?? []).filter(Boolean));
      }
      for (const fn of lives) fn(s);
    },
  };
}

// Set text only when it changed (avoids needless layout and screen reader chatter).
export function setText(el, t) {
  t = String(t);
  if (el.textContent !== t) el.textContent = t;
}

export function toggle(el, cls, on) {
  el.classList.toggle(cls, !!on);
}

let uid = 0;
export const nextId = (p = 'u') => p + ++uid;

// Poll fn() every ms while root is attached to the document, the tab is
// visible (document.visibilityState), and active() is true (default:
// always). Fires once immediately if those hold. Cleans itself up once
// root leaves the DOM (the router swaps screens by replacing children).
export function visiblePoll(root, ms, fn, active = () => true) {
  const ok = () => root.isConnected && document.visibilityState === 'visible' && active();
  let timer = 0;
  function stop() {
    clearInterval(timer);
    document.removeEventListener('visibilitychange', onVis);
  }
  function tick() {
    if (!root.isConnected) { stop(); return; }
    if (ok()) fn();
  }
  function onVis() { tick(); }
  tick();
  timer = setInterval(tick, ms);
  document.addEventListener('visibilitychange', onVis);
}
