// Server state (TanStack Query) and the actions that change it. Live
// events invalidate the affected queries; refetches keep the rendered tree
// (and so scroll positions and half-typed replies) in place.
import { QueryClient, useQuery } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { toast } from 'sonner';
import { api, ApiError, connectEvents, newOpId, type LiveStatus } from '@/lib/api';
import { archiveDismisses, canArchive, isOpenAsk, threadHref } from '@/lib/model';
import { clock } from '@/lib/time';
import type { AnswerMode, Ask, Box, HeldAnswers, Msg, Thread, ThreadResponse } from '@/lib/types';
import { getUI, setUI, withKey } from '@/lib/ui-store';
import { filterKey, getRoute, listHref, navigate, type Filter } from './route';

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      retry: (n, err) => n < 2 && !(err instanceof ApiError && err.status >= 400 && err.status < 500),
      refetchOnWindowFocus: true,
    },
  },
});

const EMPTY_HELD: HeldAnswers = { items: [], sends_at: null };

function setHeld(held: HeldAnswers | undefined) {
  if (held) queryClient.setQueryData(['held'], { items: held.items ?? [], sends_at: held.sends_at ?? null });
}

export function useInbox(box: Box, filter: Filter = {}) {
  return useQuery({
    queryKey: ['inbox', box, filterKey(filter)],
    queryFn: async () => {
      const r = await api.inbox(box, filter);
      setHeld(r.held);
      return r.threads ?? [];
    },
  });
}

/** The held-answers queue (filled by inbox fetches, events and mutations). */
export function useHeld(): HeldAnswers {
  const q = useQuery({ queryKey: ['held'], queryFn: () => EMPTY_HELD, enabled: false, initialData: EMPTY_HELD });
  return q.data;
}

export function useThread(id: string | null) {
  return useQuery({
    queryKey: ['thread', id],
    queryFn: () => api.thread(id!),
    enabled: !!id,
  });
}

export function useProjects() {
  return useQuery({ queryKey: ['projects'], queryFn: async () => (await api.projects()).projects ?? [] });
}

export function useProject(id: string | null) {
  return useQuery({ queryKey: ['project', id], queryFn: () => api.project(id!), enabled: !!id });
}

export function useMe() {
  return useQuery({ queryKey: ['me'], queryFn: api.me, staleTime: Infinity });
}

/** Subscribe to /api/events for the app's lifetime; returns the stream status. */
export function useLive(): LiveStatus {
  const [status, setStatus] = useState<LiveStatus>('connecting');
  useEffect(
    () =>
      connectEvents(
        (name, data) => {
          switch (name) {
            case 'inbox':
              queryClient.invalidateQueries({ queryKey: ['inbox'] });
              queryClient.invalidateQueries({ queryKey: ['thread'] });
              break;
            case 'held':
              setHeld(data as HeldAnswers);
              break;
            case 'thread':
              queryClient.invalidateQueries({ queryKey: ['thread', data?.thread_id] });
              break;
            case 'project':
              queryClient.invalidateQueries({ queryKey: ['projects'] });
              queryClient.invalidateQueries({ queryKey: ['project', data?.project_id] });
              break;
          }
        },
        (s, resync) => {
          setStatus(s);
          if (resync) queryClient.invalidateQueries();
        },
      ),
    [],
  );
  return status;
}

// ---- actions ----

function patchThreadAsk(threadId: string, ask: Ask) {
  queryClient.setQueryData<ThreadResponse>(['thread', threadId], (d) => (d ? { ...d, thread: { ...d.thread, ask } } : d));
}

function refreshAfterAnswer(threadId?: string) {
  queryClient.invalidateQueries({ queryKey: ['inbox'] });
  queryClient.invalidateQueries({ queryKey: ['projects'] });
  queryClient.invalidateQueries({ queryKey: ['project'] });
  if (threadId) queryClient.invalidateQueries({ queryKey: ['thread', threadId] });
}

/**
 * Choose an option: send it now, or hold it in the batch. A retry of a
 * failed attempt (same option and mode) reuses its operation ID.
 */
/** Answer an ask with one option (for every item of a batched ask), or per item. */
export async function choose(thread: Thread, optionId: string, mode: AnswerMode, items?: Record<string, string>) {
  const ask = thread.ask!;
  const prev = getUI().answering[ask.ask_id];
  if (prev?.phase === 'sending') return;
  const same = prev && prev.option_id === optionId && prev.mode === mode && JSON.stringify(prev.items ?? null) === JSON.stringify(items ?? null);
  const op = same ? prev.op : newOpId();
  setUI((s) => ({ answering: withKey(s.answering, ask.ask_id, { phase: 'sending', option_id: optionId, items, mode, op }) }));
  try {
    const r = await api.answer(ask.ask_id, { option_id: optionId, items, mode }, op);
    setHeld(r.held);
    if (r.ask) patchThreadAsk(thread.thread_id, r.ask);
    setUI((s) => ({
      answering: withKey(s.answering, ask.ask_id, undefined),
      fresh: mode === 'now' ? withKey(s.fresh, ask.ask_id, Date.now()) : s.fresh,
    }));
  } catch (e) {
    const err = e as ApiError;
    if (err.code === 'conflict') {
      setUI((s) => ({ answering: withKey(s.answering, ask.ask_id, undefined) }));
      toast.error('Already decided', { description: err.message });
    } else {
      setUI((s) => ({
        answering: withKey(s.answering, ask.ask_id, { phase: 'error', option_id: optionId, items, mode, op, message: err.message }),
      }));
    }
  } finally {
    refreshAfterAnswer(thread.thread_id);
  }
}

/**
 * Answer an ask with free text from the composer ("Answer with this"): the
 * text is the answer, delivered to the session with the decision.
 */
export async function answerWithText(thread: Thread, text: string) {
  const ask = thread.ask!;
  if (getUI().answering[ask.ask_id]?.phase === 'sending') return false;
  const op = newOpId();
  setUI((s) => ({ answering: withKey(s.answering, ask.ask_id, { phase: 'sending', option_id: '', mode: 'now', op }) }));
  try {
    const r = await api.answer(ask.ask_id, { text, mode: 'now' }, op);
    setHeld(r.held);
    if (r.ask) patchThreadAsk(thread.thread_id, r.ask);
    setUI((s) => ({ answering: withKey(s.answering, ask.ask_id, undefined), fresh: withKey(s.fresh, ask.ask_id, Date.now()) }));
    return true;
  } catch (e) {
    setUI((s) => ({ answering: withKey(s.answering, ask.ask_id, undefined) }));
    const err = e as ApiError;
    toast.error(err.code === 'conflict' ? 'Already decided' : 'Answer not sent', { description: err.message });
    return false;
  } finally {
    refreshAfterAnswer(thread.thread_id);
  }
}

export function dismissAnswerError(askId: string) {
  setUI((s) => ({ answering: withKey(s.answering, askId, undefined) }));
}

export async function undoHeld(askId: string, threadId?: string) {
  try {
    const r = await api.undoHeld(askId);
    setHeld(r.held);
  } catch (e) {
    toast.error("Couldn't undo", { description: (e as Error).message });
  } finally {
    refreshAfterAnswer(threadId);
  }
}

export async function sendHeldNow() {
  try {
    const r = await api.sendHeld();
    setHeld(r.held);
    const results = r.results ?? [];
    const ok = results.filter((x) => !x.error);
    const failed = results.filter((x) => x.error);
    setUI((s) => {
      let fresh = s.fresh;
      for (const x of ok) fresh = withKey(fresh, x.ask_id, Date.now());
      return { fresh, heldOpen: false };
    });
    if (ok.length) toast.success(`Sent ${ok.length} answer${ok.length === 1 ? '' : 's'} at ${clock(new Date())}`);
    for (const x of failed) toast.error(`Not sent: ${x.ask?.title ?? 'one answer'}`, { description: x.error!.message });
  } catch (e) {
    toast.error('Held answers were not sent', { description: (e as Error).message });
  } finally {
    refreshAfterAnswer();
    queryClient.invalidateQueries({ queryKey: ['thread'] });
  }
}

/**
 * Send a reply into a thread, showing it at once with its delivery state.
 * The pending entry's key is the operation ID, so Retry cannot send twice.
 */
export async function sendReply(threadId: string, text: string, retryKey?: string) {
  const key = retryKey ?? newOpId();
  const entry = { key, text, at: new Date().toISOString(), phase: 'sending' as const };
  setUI((s) => {
    const list = (s.pending[threadId] ?? []).filter((p) => p.key !== key);
    return { pending: withKey(s.pending, threadId, [...list, entry]) };
  });
  try {
    const r = await api.reply(threadId, text, key);
    queryClient.setQueryData<ThreadResponse>(['thread', threadId], (d) => {
      if (!d) return d;
      const msgs: Msg[] = d.messages ?? [];
      return msgs.some((m) => m.message_id === r.message.message_id) ? d : { ...d, messages: [...msgs, r.message] };
    });
    setUI((s) => ({ pending: withKey(s.pending, threadId, (s.pending[threadId] ?? []).filter((p) => p.key !== key)) }));
  } catch (e) {
    setUI((s) => ({
      pending: withKey(
        s.pending,
        threadId,
        (s.pending[threadId] ?? []).map((p) => (p.key === key ? { ...p, phase: 'error' as const, message: (e as Error).message } : p)),
      ),
    }));
  } finally {
    queryClient.invalidateQueries({ queryKey: ['thread', threadId] });
    queryClient.invalidateQueries({ queryKey: ['inbox'] });
  }
}

export function discardReply(threadId: string, key: string) {
  setUI((s) => ({ pending: withKey(s.pending, threadId, (s.pending[threadId] ?? []).filter((p) => p.key !== key)) }));
}

const marked = new Set<string>();
/**
 * Mark a thread read once per view: unread threads, and message threads
 * still waiting on the owner (opening one acknowledges its messages at the
 * hub, so it moves to Done even when another surface already marked it read).
 */
export function markRead(thread: Thread) {
  const pendingMail = thread.kind === 'message' && thread.needs_reply;
  if ((!thread.unread && !pendingMail) || marked.has(thread.thread_id)) return;
  marked.add(thread.thread_id);
  api
    .read(thread.thread_id)
    .then(() => {
      for (const box of ['open', 'done'] as const) {
        queryClient.setQueriesData<Thread[]>({ queryKey: ['inbox', box] }, (list) =>
          list?.map((t) => (t.thread_id === thread.thread_id ? { ...t, unread: false } : t)),
        );
      }
    })
    .catch(() => marked.delete(thread.thread_id))
    .finally(() => setTimeout(() => marked.delete(thread.thread_id), 5000));
}

/** Threads of the list the current view sits beside. */
export function currentList(): Thread[] {
  const { box, filter } = getUI();
  return queryClient.getQueryData<Thread[]>(['inbox', box, filterKey(filter)]) ?? [];
}

// ---- archive ----

/** Take threads out of the cached open lists at once (the server agrees a moment later). */
function dropFromOpen(ids: Set<string>) {
  queryClient.setQueriesData<Thread[]>({ queryKey: ['inbox', 'open'] }, (list) => list?.filter((t) => !ids.has(t.thread_id)));
}

const archiving = new Set<string>();

/**
 * Archive a thread like email: the server acknowledges its unanswered
 * messages so it moves to Done. When it is the open conversation, the view
 * moves on to the next one in the list (else the previous, else the list).
 * The hub has no unarchive; a new message from the session reopens it.
 * An open ask is dismissed (no option, no default) once confirmed: without
 * confirmed this only opens the confirmation (DismissDialog). A session
 * item ("waiting on you", its own flag) is dismissed at once.
 */
export async function archiveThread(thread: Thread, confirmed = false) {
  const id = thread.thread_id;
  const dismiss = isOpenAsk(thread);
  if (dismiss && !confirmed) {
    setUI({ dismissing: thread });
    return;
  }
  if (!canArchive(thread) || archiving.has(id)) return;
  archiving.add(id);
  const ui = getUI();
  const list = currentList();
  const idx = list.findIndex((t) => t.thread_id === id);
  const next = idx >= 0 ? (list[idx + 1] ?? list[idx - 1]) : undefined;
  const route = getRoute();
  if (route.name === 'thread' && route.id === id) navigate(next ? threadHref(next.thread_id) : listHref(ui.box, ui.filter), true);
  if (ui.cursor === id) setUI({ cursor: next?.thread_id ?? null });
  dropFromOpen(new Set([id]));
  try {
    const r = await api.archive(id, newOpId(), dismiss);
    queryClient.setQueryData<ThreadResponse>(['thread', id], (d) => (d ? { ...d, thread: r.thread } : d));
    toast(archiveDismisses(thread) ? 'Dismissed' : 'Archived', { duration: 2000 });
  } catch (e) {
    toast.error("Couldn't archive", { description: (e as Error).message });
  } finally {
    archiving.delete(id);
    queryClient.invalidateQueries({ queryKey: ['inbox'] });
    queryClient.invalidateQueries({ queryKey: ['thread', id] });
  }
}

/** Revoke a standing grant (project page). Asks for its action reach the owner again. */
export async function revokeGrant(grantId: string, projectId: string) {
  try {
    await api.revokeGrant(grantId, newOpId());
    toast('Go-ahead revoked', { duration: 2500 });
  } catch (e) {
    toast.error("Couldn't revoke", { description: (e as Error).message });
  } finally {
    queryClient.invalidateQueries({ queryKey: ['project', projectId] });
  }
}

/** Archive several threads at once (one request; per-thread results). */
export async function archiveThreads(ids: string[]) {
  if (!ids.length) return;
  dropFromOpen(new Set(ids));
  try {
    const r = await api.archiveMany(ids, newOpId());
    const results = r.results ?? [];
    const ok = results.filter((x) => !x.error).length;
    const failed = results.filter((x) => x.error);
    if (ok) toast(`Archived ${ok} conversation${ok === 1 ? '' : 's'}`, { duration: 2500 });
    if (failed.length)
      toast.error(`${failed.length} not archived`, { description: failed[0].error!.message });
  } catch (e) {
    toast.error("Couldn't archive", { description: (e as Error).message });
  } finally {
    queryClient.invalidateQueries({ queryKey: ['inbox'] });
    queryClient.invalidateQueries({ queryKey: ['thread'] });
  }
}
