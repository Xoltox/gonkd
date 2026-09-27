// Entry: mount the shell, route by hash, push state to the visible screen.
import { state, subscribe, start } from './store.js';
import { mountStrip, mountNav, mountToast, ROUTES } from './shell.js';
import * as now from './screens/now.js';
import * as files from './screens/files.js';
import * as control from './screens/control.js';
import * as consoleScreen from './screens/console.js';
import * as settings from './screens/settings.js';

const SCREENS = { now, files, control, console: consoleScreen, settings };
const TITLES = Object.fromEntries(ROUTES.map(([id, label]) => [id, label]));

const main = document.getElementById('main');
const updateStrip = mountStrip(document.getElementById('strip'));
const updateNav = mountNav(document.getElementById('nav'));
const updateToast = mountToast(document.getElementById('toast'));

let route = '';
let updateScreen = () => {};

function currentRoute() {
  const id = location.hash.replace(/^#\/?/, '');
  return SCREENS[id] ? id : 'now';
}

function go() {
  const next = currentRoute();
  if (next === route) return;
  const first = !route;
  route = next;
  const root = document.createElement('div');
  root.className = `screen screen-${route}`;
  main.replaceChildren(root);
  updateScreen = SCREENS[route].mount(root);
  document.title = `${TITLES[route]} - Gonk'd`;
  render(state);
  if (!first) {
    window.scrollTo(0, 0);
    main.focus({ preventScroll: true });
  }
}

function render(s) {
  updateStrip(s);
  updateNav(s, route);
  updateToast(s);
  updateScreen(s);
}

// Skip link: focus main without touching the hash router.
document.querySelector('.skip').addEventListener('click', (e) => { e.preventDefault(); main.focus(); });

subscribe(render);
window.addEventListener('hashchange', go);
go();
start();
