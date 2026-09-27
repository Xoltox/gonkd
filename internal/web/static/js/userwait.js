// Global "printer waiting for user" alert: shown on every screen while
// Marlin is blocked at an M0/M1 (or a HOST_PROMPT_SUPPORT dialog it
// opened). System notifications need HTTPS, and gonkd is plain HTTP on a
// LAN box, so this never uses the Notification API -- the banner plus a
// blinking title, a repeating WebAudio beep and navigator.vibrate is the
// whole alert.
import { h, icon, setText } from './dom.js';
import { act } from './store.js';
import { api } from './api.js';
import { ago } from './fmt.js';

const SOUND_KEY = 'gonkd-sound-alerts';
export function getSoundAlerts() {
  try { return localStorage.getItem(SOUND_KEY) !== '0'; } catch { return true; }
}
export function setSoundAlerts(on) {
  try { localStorage.setItem(SOUND_KEY, on ? '1' : '0'); } catch { /* private mode: this visit only */ }
}

// Browsers require a user gesture before audio can play; a page load alone
// never counts. The first touch or key press anywhere unlocks audio; until
// then the banner offers an "Enable sound" button (soundReady() is false).
let audioCtx = null;
function unlock() {
  if (!audioCtx) {
    try { audioCtx = new (window.AudioContext || window.webkitAudioContext)(); } catch { return; /* no WebAudio */ }
  }
  if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
}
['pointerdown', 'keydown'].forEach((ev) => document.addEventListener(ev, unlock, { passive: true }));

export function soundReady() {
  return !!audioCtx && audioCtx.state === 'running';
}

// beep plays three short tones. Mobile browsers may suspend the context when
// the tab is hidden, so it tries to resume first.
export function beep() {
  if (!getSoundAlerts() || !audioCtx) return;
  if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
  const t = audioCtx.currentTime + 0.05;
  for (let i = 0; i < 3; i++) {
    const t0 = t + i * 0.35;
    const osc = audioCtx.createOscillator();
    const gain = audioCtx.createGain();
    osc.frequency.value = 880;
    gain.gain.setValueAtTime(0.0001, t0);
    gain.gain.exponentialRampToValueAtTime(0.4, t0 + 0.02);
    gain.gain.exponentialRampToValueAtTime(0.0001, t0 + 0.25);
    osc.connect(gain).connect(audioCtx.destination);
    osc.start(t0);
    osc.stop(t0 + 0.3);
  }
}

const BASE_TITLE = document.title;
let blinkTimer = 0;
function startBlink() {
  if (blinkTimer) return;
  let on = false;
  blinkTimer = setInterval(() => {
    on = !on;
    document.title = on ? 'Waiting for you...' : BASE_TITLE;
  }, 1000);
}
function stopBlink() {
  if (!blinkTimer) return;
  clearInterval(blinkTimer);
  blinkTimer = 0;
  document.title = BASE_TITLE;
}

const BEEP_PERIOD = 30000;

export function mountUserWait(root) {
  let activeSince = '';
  let beepTimer = 0;
  const msg = h('p', { class: 'userwait-msg' });
  const time = h('p', { class: 'meta tnum' });
  const goBtn = h('button', {
    type: 'button', class: 'btn btn-primary userwait-continue',
    onclick: async () => {
      goBtn.disabled = true;
      await act(() => api.continue(), 'Sent. Resuming shortly.');
      goBtn.disabled = false;
    },
  }, icon('play'), h('span', { text: 'Continue print' }));
  const soundBtn = h('button', {
    type: 'button', class: 'btn',
    onclick: () => { unlock(); beep(); setTimeout(update, 300); },
  }, h('span', { text: 'Enable sound' }));
  const update = () => { soundBtn.hidden = !getSoundAlerts() || soundReady(); };
  root.append(
    h('div', { class: 'userwait-panel' },
      icon('pause', 28),
      h('div', { class: 'userwait-body' },
        h('strong', { text: 'Printer is waiting for you' }),
        msg,
        time,
        h('p', { class: 'meta', text: 'Continues the print. Make sure the new filament is loaded first.' })),
      h('div', { class: 'userwait-actions' }, goBtn, soundBtn)),
  );
  root.hidden = true;

  return (s) => {
    const uw = s.snap && s.snap.userWait;
    root.hidden = !uw;
    if (!uw) {
      if (activeSince) {
        activeSince = '';
        stopBlink();
        clearInterval(beepTimer);
        beepTimer = 0;
      }
      return;
    }
    update();
    setText(msg, uw.message || 'Waiting for user');
    const since = new Date(uw.since).getTime();
    setText(time, isFinite(since) ? `Waiting since ${ago(Math.max(0, s.now - since))}` : '');
    if (uw.since !== activeSince) {
      activeSince = uw.since;
      startBlink();
      try { navigator.vibrate && navigator.vibrate([200, 100, 200]); } catch { /* ignore */ }
      beep();
      clearInterval(beepTimer);
      beepTimer = setInterval(beep, BEEP_PERIOD);
    }
  };
}
