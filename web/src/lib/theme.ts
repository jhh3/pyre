// Theme: follows the system by default; the owner can pin light or dark
// (remembered per browser). ?theme=light|dark forces one without saving
// (used for screenshots).
import { useSyncExternalStore } from 'react';
import { local } from './local';

export type ThemePref = 'system' | 'light' | 'dark';

const forced = new URLSearchParams(location.search).get('theme') as ThemePref | null;
let pref: ThemePref = forced ?? (local.get('theme', 'system') as ThemePref);
const media = window.matchMedia('(prefers-color-scheme: dark)');
const listeners = new Set<() => void>();

const resolved = () => (pref === 'system' ? (media.matches ? 'dark' : 'light') : pref);

function apply() {
  const dark = resolved() === 'dark';
  document.documentElement.classList.toggle('dark', dark);
  document.documentElement.style.colorScheme = dark ? 'dark' : 'light';
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', dark ? '#16171b' : '#f9f9fb');
  listeners.forEach((fn) => fn());
}

media.addEventListener('change', apply);
apply();

export function setTheme(next: ThemePref) {
  pref = next;
  if (!forced) local.set('theme', next);
  apply();
}

const subscribe = (fn: () => void) => {
  listeners.add(fn);
  return () => listeners.delete(fn);
};

export function useTheme() {
  const p = useSyncExternalStore(subscribe, () => pref);
  const r = useSyncExternalStore(subscribe, resolved);
  return { pref: p, resolved: r as 'light' | 'dark', setTheme };
}
