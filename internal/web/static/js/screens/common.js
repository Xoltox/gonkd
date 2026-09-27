// Pieces shared by several screens: page banners, preheat, tune.
import { h, region } from '../dom.js';
import { banner, btn, card, stepper } from '../ui.js';
import { phaseOf, act, set } from '../store.js';
import { api } from '../api.js';
import { DEG } from '../fmt.js';

export function screenTitle(text) {
  return h('h1', { class: 'screen-title', text });
}

// Killed / error / printer offline / job note banners.
export function pageBanners() {
  return region(
    (s) => [phaseOf(s), s.snap && s.snap.lastError, s.snap && s.snap.job && s.snap.job.note].join('|'),
    (s) => {
      const p = phaseOf(s);
      const snap = s.snap;
      const out = [];
      if (p === 'killed') out.push(banner('danger', 'Printer halted',
        'The emergency stop fired. Reset the printer (reset button or power cycle) to continue. Heaters are off and position is lost.', { icon: 'stop' }));
      if (p === 'error') out.push(banner('danger', 'Printer error', snap.lastError || 'The printer reported an error.'));
      if (p === 'disconnected') out.push(banner('warn', 'Printer not connected',
        'The box is up but has no serial link to the printer. Check the USB cable and that the printer is on.', { icon: 'offline' }));
      if (snap && snap.job && snap.job.note && ['printing', 'paused', 'disconnected'].includes(p)) out.push(banner('warn', 'Note', snap.job.note));
      return out;
    },
  );
}

// Preset buttons plus cooldown. Rebuilt when presets or heater state change.
export function preheat(disabledFn, withCooldown = true) {
  return region(
    (s) => JSON.stringify([s.presets, disabledFn(s), heatersOn(s)]),
    (s) => {
      const dis = disabledFn(s);
      const row = h('div', { class: 'btn-row wrap' });
      for (const p of s.presets) {
        row.append(h('button', {
          type: 'button', class: 'btn btn-secondary preset', disabled: dis,
          onclick: () => act(() => api.heat({ hotend: p.hotend, bed: p.bed }), `Preheating ${p.name}`),
        }, h('span', { text: p.name }), h('span', { class: 'meta tnum' },
          h('span', { class: 'c-hot', text: p.hotend }), ' / ', h('span', { class: 'c-bed', text: p.bed }), DEG)));
      }
      if (withCooldown && heatersOn(s)) row.append(btn('Cooldown', { icon: 'snow', disabled: dis, onClick: () => act(() => api.heat({ hotend: 0, bed: 0 }), 'Heaters off') }));
      if (!s.presets.length) row.append(h('span', { class: 'meta', text: 'No presets yet. Add them in Settings.' }));
      return h('div', { class: 'preheat' }, h('p', { class: 'overline', text: 'Preheat' }), row);
    },
  );
}

export const heatersOn = (s) => !!s.snap && (s.snap.temps.hotendTarget > 0 || s.snap.temps.bedTarget > 0);

// Speed / flow / fan. A requested value shows dimmed (pending) until the
// snapshot reports it.
export function tuneCard(disabledFn) {
  const want = {};
  const mk = (key, label, step, min, max) => stepper(label, '%', step, min, max, async (v) => {
    want[key] = v;
    set({});
    if (!(await act(() => api.tune({ [key]: v })))) { delete want[key]; set({}); }
  });
  const sp = { speed: mk('speed', 'Speed', 5, 10, 500), flow: mk('flow', 'Flow', 1, 50, 200), fan: mk('fan', 'Fan', 10, 0, 100) };
  const hint = h('p', { class: 'meta', text: 'Values show once the box reports them.' });
  const el = card('Tune', 'fan', [h('div', { class: 'tune' }, sp.speed.el, sp.flow.el, sp.fan.el), hint]);
  return {
    el,
    update(s) {
      const tune = s.snap && s.snap.tune;
      hint.hidden = !!tune;
      const dis = disabledFn(s);
      for (const k of Object.keys(sp)) {
        if (tune && want[k] === tune[k]) delete want[k];
        const v = want[k] ?? (tune ? tune[k] : null);
        sp[k].set(v, want[k] != null, dis);
      }
    },
  };
}

