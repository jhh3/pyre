// Fixture backend for ?fixtures=1: an in-memory fake of the web API built
// from /fixtures/*.json. Timestamps in the files are relative to their
// "_now" field and are shifted to the real clock on load, so deadlines and
// "2h ago" stay meaningful whenever the page is opened. Mutations behave
// like the server (answer, hold, undo, send held, reply) and emit the same
// events, with delivery progressing a moment later. Archive acknowledges
// a message thread (it moves to Done); an open ask is dismissed when the
// request says so (else 409), and a session item is dismissed. ?fail=1 makes answers
// and replies fail so the error states can be seen.
import { ApiError, type EventName } from './api';
import { clock } from './time';
import type { HeldAnswers, Msg, ProjectCard, ProjectDetail, Thread } from './types';

const ISO = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/;
const QUIET_MS = 5 * 60 * 1000;

type Entry = { thread: Thread; messages: Msg[] };

async function load<T>(name: string): Promise<T> {
  const res = await fetch('/fixtures/' + name, { cache: 'no-store' });
  const text = await res.text();
  const shift = Date.now() - Date.parse(JSON.parse(text)._now);
  return JSON.parse(text, (_k, v) =>
    typeof v === 'string' && ISO.test(v) ? new Date(Date.parse(v) + shift).toISOString() : v,
  );
}

const copy = <T,>(v: T): T => JSON.parse(JSON.stringify(v));
const now = () => new Date().toISOString();

export async function create({ fail = false } = {}) {
  const [me, inbox, projects, details] = await Promise.all([
    load<{ email: string }>('me.json'),
    load<{ threads: Entry[]; held: HeldAnswers }>('threads.json'),
    load<{ projects: ProjectCard[] }>('projects.json'),
    load<{ details: Record<string, ProjectDetail> }>('project-details.json'),
  ]);
  const threads = inbox.threads;
  // Delivery summaries name a clock time; keep it in step with the shift.
  for (const t of threads)
    for (const m of t.messages)
      m.delivery.summary = m.delivery.summary.replace(/at \d{1,2}:\d{2} [AP]M/, 'at ' + clock(Date.parse(m.at) + 60_000));
  let held: HeldAnswers = inbox.held ?? { items: [] };
  const listeners = new Set<(name: EventName, data: unknown) => void>();
  let seq = 0;

  const items = () => held.items ?? [];
  const emit = (name: EventName, data: unknown = {}) => setTimeout(() => listeners.forEach((fn) => fn(name, data)), 30);
  const byId = (id: string) => threads.find((t) => t.thread.thread_id === id);
  const byAsk = (askId: string) => threads.find((t) => t.thread.ask?.ask_id === askId);
  const heldFor = (askId: string) => items().find((i) => i.ask_id === askId) ?? null;
  const sessionName = (t: Entry) => t.thread.session?.title ?? 'the session';
  const isOpen = (t: Entry) => (t.thread.kind === 'ask' ? t.thread.ask!.state === 'open' : t.thread.needs_reply);
  // Fixture lineage: a project's lead is the parent of its other sessions.
  const childOf = (sessionId: string | undefined, leadId: string) =>
    !!sessionId &&
    Object.values(details.details).some(
      (d) => (d.sessions ?? []).some((s) => s.is_lead && s.id === leadId) && (d.sessions ?? []).some((s) => s.id === sessionId),
    );
  const view = (t: Entry): Thread => ({ ...copy(t.thread), held: t.thread.ask ? heldFor(t.thread.ask.ask_id) : null });

  function deliverLater(t: Entry, msg: Msg, ms = 1500) {
    setTimeout(() => {
      msg.delivery = { state: 'delivered', summary: `Delivered to ${sessionName(t)} at ${clock(new Date())}` };
      emit('thread', { thread_id: t.thread.thread_id });
    }, ms);
  }

  function ownerMessage(t: Entry, text: string, askId?: string): Msg {
    const msg: Msg = {
      message_id: 'msg_fx_new' + ++seq,
      from: { kind: 'owner', id: 'owner', label: 'You' },
      text,
      at: now(),
      reply_to: '',
      delivery: { state: 'queued', summary: `Queued for ${sessionName(t)}` },
    };
    if (askId) msg.ask_id = askId;
    t.messages.push(msg);
    t.thread.last_at = msg.at;
    t.thread.message_count = t.messages.length;
    return msg;
  }

  function decide(t: Entry, optionId?: string, text?: string, perItem?: Record<string, string> | null) {
    const ask = t.thread.ask!;
    if (ask.state !== 'open') throw new ApiError(409, 'conflict', 'This question was already decided.');
    const opt = ask.options.find((o) => o.id === optionId);
    const byItem = perItem && Object.keys(perItem).length ? perItem : null;
    if (byItem && !optionId && (ask.items ?? []).some((it) => !byItem[it.id]))
      throw new ApiError(400, 'invalid_argument', 'Answer every item or give an option for the rest.');
    ask.state = 'answered';
    ask.answer = { option_id: optionId ?? '', items: byItem, text: text ?? '', by: { kind: 'owner', id: 'owner', name: 'You' }, at: now() };
    const label = byItem ? 'an answer per item' : opt ? opt.label : text;
    const msg = ownerMessage(t, `Decision: **${label}**`, ask.ask_id);
    ask.decision_message_id = msg.message_id;
    t.thread.preview = (opt || byItem ? 'You chose: ' : 'You answered: ') + label;
    t.thread.unread = false;
    deliverLater(t, msg);
    return copy(ask);
  }

  function setHeld(next: HeldAnswers) {
    held = next;
    if (!items().length) held.sends_at = null;
    emit('held', copy(held));
  }

  function flush() {
    const results = items().map((item) => {
      const t = byAsk(item.ask_id)!;
      try {
        return { ask_id: item.ask_id, ask: decide(t, item.option_id, item.text, item.items) };
      } catch (e) {
        const err = e as ApiError;
        return { ask_id: item.ask_id, error: { code: err.code, message: err.message } };
      }
    });
    setHeld({ items: [] });
    emit('inbox');
    for (const r of results) emit('thread', { thread_id: byAsk(r.ask_id)!.thread.thread_id });
    return results;
  }

  // Like the server: an open ask is dismissed only when asked to (no
  // option, no default); anything else stops waiting on you.
  function archive(id: string, dismiss = false): Thread {
    const t = byId(id);
    if (!t) throw new ApiError(404, 'not_found', `No conversation ${id} in your inbox.`);
    if (t.thread.kind === 'ask' && t.thread.ask!.state === 'open') {
      if (!dismiss) throw new ApiError(409, 'conflict', 'Answer it, or dismiss it: open questions are archived only by dismissing them');
      const ask = t.thread.ask!;
      ask.state = 'withdrawn';
      ask.answer = { text: 'dismissed by the owner', by: { kind: 'owner', id: 'owner', name: 'You' }, at: now() };
      t.thread.preview = 'Dismissed by you';
      if (heldFor(ask.ask_id)) setHeld({ ...held, items: items().filter((i) => i.ask_id !== ask.ask_id) });
    }
    if (t.thread.attention) t.thread.attention.state = 'handled';
    t.thread.needs_reply = false;
    t.thread.unread = false;
    return view(t);
  }

  setInterval(() => {
    if (items().length && held.sends_at && Date.parse(held.sends_at) <= Date.now()) flush();
  }, 1000);

  type Handler = (args: string[], body: any) => unknown;
  const routes: [string, RegExp, Handler][] = [
    ['GET', /^\/me$/, () => me],
    ['GET', /^\/inbox\?box=(open|done)(?:&project=([^&]*))?(?:&session=([^&]*))?$/, ([box, project, session]) => ({
      threads: threads
        .filter((t) => (box === 'open') === isOpen(t))
        .filter((t) => !project || t.thread.project?.id === project)
        .filter((t) => !session || t.thread.session?.id === session || childOf(t.thread.session?.id, session))
        .sort((a, b) => Date.parse(b.thread.last_at) - Date.parse(a.thread.last_at))
        .map(view),
      held: copy(held),
    })],
    ['GET', /^\/threads\/([^/]+)$/, ([id]) => {
      const t = byId(id);
      if (!t) throw new ApiError(404, 'not_found', 'That conversation no longer exists.');
      return { thread: view(t), messages: copy(t.messages) };
    }],
    ['POST', /^\/threads\/([^/]+)\/read$/, ([id]) => {
      const t = byId(id);
      if (t?.thread.unread) {
        t.thread.unread = false;
        emit('inbox');
      }
      return {};
    }],
    ['POST', /^\/threads\/([^/]+)\/archive$/, ([id], body) => {
      if (fail) throw new ApiError(502, 'unavailable', "The hub didn't answer. Nothing was archived.");
      const thread = archive(id, !!body?.dismiss);
      emit('inbox');
      emit('thread', { thread_id: id });
      return { thread };
    }],
    ['POST', /^\/inbox\/archive$/, (_, body) => {
      if (fail) throw new ApiError(502, 'unavailable', "The hub didn't answer. Nothing was archived.");
      const results = [...new Set<string>(body.thread_ids ?? [])].map((id) => {
        try {
          return { thread_id: id, thread: archive(id) };
        } catch (e) {
          const err = e as ApiError;
          return { thread_id: id, error: { code: err.code, message: err.message } };
        }
      });
      emit('inbox');
      return { results };
    }],
    ['POST', /^\/threads\/([^/]+)\/reply$/, ([id], body) => {
      const t = byId(id)!;
      if (fail) throw new ApiError(502, 'unavailable', "The hub didn't answer. Your message was not sent.");
      const msg = ownerMessage(t, body.text);
      t.thread.needs_reply = false;
      t.thread.preview = 'You: ' + body.text;
      deliverLater(t, msg);
      setTimeout(() => {
        t.messages.push({
          message_id: 'msg_fx_new' + ++seq,
          from: { kind: 'session', id: t.thread.session!.id, label: sessionName(t) },
          text: 'Good question. Short version: nothing ships until you pick an option, and any of them can be rolled back in under ten minutes. I added the details to the project page under **Rollback**.',
          at: now(),
          reply_to: msg.message_id,
          delivery: { state: 'delivered', summary: 'Delivered to you' },
        });
        msg.delivery = { state: 'replied', summary: `${sessionName(t)} replied at ${clock(new Date())}` };
        t.thread.last_at = now();
        t.thread.message_count = t.messages.length;
        emit('thread', { thread_id: id });
        emit('inbox');
      }, 7000);
      emit('inbox');
      return { message: copy(msg) };
    }],
    ['POST', /^\/asks\/([^/]+)\/answer$/, ([askId], body) => {
      const t = byAsk(askId);
      if (!t) throw new ApiError(404, 'not_found', 'That question no longer exists.');
      if (fail) throw new ApiError(502, 'unavailable', "The hub didn't answer. Your choice was not sent.");
      if (body.mode === 'hold') {
        if (t.thread.ask!.state !== 'open') throw new ApiError(409, 'conflict', 'This question was already decided.');
        const rest = items().filter((i) => i.ask_id !== askId);
        rest.push({ ask_id: askId, option_id: body.option_id ?? '', items: body.items ?? null, text: body.text ?? '', held_at: now() });
        setHeld({ items: rest, sends_at: new Date(Date.now() + QUIET_MS).toISOString() });
        emit('inbox');
        emit('thread', { thread_id: t.thread.thread_id });
        return { ask: copy(t.thread.ask), held: copy(held) };
      }
      const ask = decide(t, body.option_id, body.text, body.items);
      if (heldFor(askId)) setHeld({ ...held, items: items().filter((i) => i.ask_id !== askId) });
      emit('inbox');
      emit('thread', { thread_id: t.thread.thread_id });
      return { ask, held: copy(held) };
    }],
    ['POST', /^\/held\/([^/]+)\/undo$/, ([askId]) => {
      setHeld({ ...held, items: items().filter((i) => i.ask_id !== askId) });
      const t = byAsk(askId);
      emit('inbox');
      if (t) emit('thread', { thread_id: t.thread.thread_id });
      return { held: copy(held) };
    }],
    ['POST', /^\/held\/send$/, () => ({ results: flush(), held: copy(held) })],
    ['GET', /^\/projects$/, () => ({
      projects: projects.projects.map((p) => ({
        ...p,
        open_asks: threads.filter((t) => t.thread.project?.id === p.project_id && t.thread.ask?.state === 'open').length,
      })),
    })],
    ['POST', /^\/grants\/([^/]+)\/revoke$/, ([id]) => {
      for (const [pid, d] of Object.entries(details.details)) {
        const g = (d.grants ?? []).find((x) => x.grant_id === id);
        if (!g) continue;
        d.grants = (d.grants ?? []).filter((x) => x.grant_id !== id);
        emit('project', { project_id: pid });
        return { grant: { ...g, revoked_at: now() } };
      }
      throw new ApiError(404, 'not_found', 'That go-ahead no longer exists.');
    }],
    ['GET', /^\/projects\/([^/]+)$/, ([id]) => {
      const d = details.details[id];
      if (!d) throw new ApiError(404, 'not_found', 'That project no longer exists.');
      // Asks reflect what happened in the inbox since load.
      const asks = (d.asks ?? []).map((a) => byAsk(a.ask_id)?.thread.ask ?? a);
      asks.sort((a, b) => Number(a.state !== 'open') - Number(b.state !== 'open'));
      return { ...d, asks };
    }],
  ];

  return {
    async call(method: string, path: string, body: unknown) {
      await new Promise((r) => setTimeout(r, 120 + Math.random() * 150));
      for (const [m, re, fn] of routes) {
        const match = method === m ? path.match(re) : null;
        if (match) return copy(fn(match.slice(1).map((x) => (x ? decodeURIComponent(x) : '')), body ?? {}) ?? {});
      }
      throw new ApiError(404, 'not_found', `No fixture for ${method} ${path}`);
    },
    subscribe(fn: (name: EventName, data: unknown) => void) {
      listeners.add(fn);
    },
  };
}
