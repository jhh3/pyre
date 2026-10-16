// Client-only UI state that must survive refetches and view changes:
// in-flight answers, optimistic replies, drafts, the answer mode, the
// keyboard cursor. A tiny external store (no extra dependency).
import { useSyncExternalStore } from 'react';
import { local } from './local';
import type { AnswerMode } from './types';
import type { Filter } from '@/hooks/route';

export type Answering = { phase: 'sending' | 'error'; option_id: string; mode: AnswerMode; op: string; message?: string };
export type PendingReply = { key: string; text: string; at: string; phase: 'sending' | 'error'; message?: string };

export type UI = {
  answering: Record<string, Answering>; // by ask_id
  fresh: Record<string, number>; // ask_id -> when answered from this tab
  pending: Record<string, PendingReply[]>; // by thread_id
  drafts: Record<string, string>; // by thread_id
  mode: AnswerMode;
  cursor: string | null;
  navOpen: boolean;
  helpOpen: boolean;
  heldOpen: boolean;
  box: 'open' | 'done'; // the list the thread view sits beside
  filter: Filter;
  showInactive: boolean; // list projects with no live sessions and no open asks
};

let state: UI = {
  answering: {},
  fresh: {},
  pending: {},
  drafts: {},
  mode: local.get('answerMode', 'now') === 'hold' ? 'hold' : 'now',
  cursor: null,
  navOpen: false,
  helpOpen: false,
  heldOpen: false,
  box: 'open',
  filter: {},
  showInactive: local.get('showInactive', '') === '1',
};
const listeners = new Set<() => void>();

export const getUI = () => state;

export function setUI(patch: Partial<UI> | ((s: UI) => Partial<UI>)) {
  const p = typeof patch === 'function' ? patch(state) : patch;
  state = { ...state, ...p };
  if (p.mode) local.set('answerMode', p.mode);
  if (p.showInactive !== undefined) local.set('showInactive', p.showInactive ? '1' : '');
  listeners.forEach((fn) => fn());
}

const subscribe = (fn: () => void) => {
  listeners.add(fn);
  return () => listeners.delete(fn);
};

export function useUI<T>(select: (s: UI) => T): T {
  return useSyncExternalStore(subscribe, () => select(state));
}

/** Immutable record helpers. */
export function withKey<V>(rec: Record<string, V>, key: string, value: V | undefined): Record<string, V> {
  const next = { ...rec };
  if (value === undefined) delete next[key];
  else next[key] = value;
  return next;
}

// visibleProjects: active projects (and keep, e.g. the one on screen) unless
// inactive ones are shown; hidden is how many were left out.
export function visibleProjects<T extends { project_id: string; active: boolean }>(all: T[], show: boolean, keep?: string) {
  const shown = show ? all : all.filter((p) => p.active || p.project_id === keep);
  return { shown, hidden: all.length - shown.length, inactive: all.filter((p) => !p.active).length };
}
