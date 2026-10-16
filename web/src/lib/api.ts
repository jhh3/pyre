// The web JSON API (/api/...) and its live event stream. With ?fixtures=1
// the same calls are answered by an in-browser fake built from
// /fixtures/*.json (see fixtures.ts), so the UI can be developed and
// screenshotted without a hub.
import type {
  AnswerMode, AnswerResult, ArchiveResult, Ask, Box, HeldAnswers, InboxResponse, Msg, ProjectCard, ProjectDetail, Thread, ThreadResponse,
} from './types';

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

const params = new URLSearchParams(location.search);
export const FIXTURES = params.get('fixtures') === '1';

type Backend = Awaited<ReturnType<typeof import('./fixtures').create>>;
let backend: Promise<Backend> | null = null;
function fixtureBackend() {
  backend ??= import('./fixtures').then((m) => m.create({ fail: params.get('fail') === '1' }));
  return backend;
}

async function call<R>(method: 'GET' | 'POST', path: string, body?: unknown): Promise<R> {
  if (FIXTURES) return (await fixtureBackend()).call(method, path, body) as Promise<R>;
  let res: Response;
  try {
    res = await fetch('/api' + path, {
      method,
      credentials: 'same-origin',
      headers: body !== undefined ? { 'Content-Type': 'application/json', Accept: 'application/json' } : { Accept: 'application/json' },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new ApiError(0, 'network', "Couldn't reach Pyre. Check your connection and try again.");
  }
  let data: any = null;
  try {
    data = await res.json();
  } catch {
    /* empty or non-JSON body */
  }
  if (!res.ok) {
    const e = data?.error;
    throw new ApiError(res.status, e?.code || 'http_' + res.status, e?.message || `Pyre answered with an error (${res.status}).`);
  }
  return (data ?? {}) as R;
}

const enc = encodeURIComponent;

/**
 * A fresh ID per user action. Mutations carry it as operation_id and a
 * retry of the same action reuses it, so the server never applies one
 * action twice (an answer whose response was lost is not sent again).
 */
export function newOpId(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

export const api = {
  me: () => call<{ email: string }>('GET', '/me'),
  // project/session filters are applied by the server (session = a lead
  // and its child sessions).
  inbox: (box: Box, filter: { project?: string; session?: string } = {}) =>
    call<InboxResponse>(
      'GET',
      '/inbox?box=' + box + (filter.project ? '&project=' + enc(filter.project) : '') + (filter.session ? '&session=' + enc(filter.session) : ''),
    ),
  thread: (id: string) => call<ThreadResponse>('GET', '/threads/' + enc(id)),
  reply: (id: string, text: string, opId: string) =>
    call<{ message: Msg }>('POST', '/threads/' + enc(id) + '/reply', { text, operation_id: opId }),
  read: (id: string) => call<object>('POST', '/threads/' + enc(id) + '/read', { operation_id: newOpId() }),
  archive: (id: string, opId: string) => call<{ thread: Thread }>('POST', '/threads/' + enc(id) + '/archive', { operation_id: opId }),
  archiveMany: (ids: string[], opId: string) =>
    call<{ results: ArchiveResult[] | null }>('POST', '/inbox/archive', { thread_ids: ids, operation_id: opId }),
  answer: (askId: string, body: { option_id?: string; text?: string; mode: AnswerMode }, opId: string) =>
    call<{ ask: Ask; held: HeldAnswers }>('POST', '/asks/' + enc(askId) + '/answer', { ...body, operation_id: opId }),
  undoHeld: (askId: string) => call<{ held: HeldAnswers }>('POST', '/held/' + enc(askId) + '/undo', { operation_id: newOpId() }),
  sendHeld: () => call<{ results: AnswerResult[] | null; held: HeldAnswers }>('POST', '/held/send', { operation_id: newOpId() }),
  projects: () => call<{ projects: ProjectCard[] | null }>('GET', '/projects'),
  project: (id: string) => call<ProjectDetail>('GET', '/projects/' + enc(id)),
};

export type EventName = 'inbox' | 'held' | 'project' | 'thread';
export type LiveStatus = 'connecting' | 'live' | 'reconnecting';

/**
 * connectEvents calls on(name, data) for each server event and
 * onStatus(status, resync) as the stream drops and returns; resync is true
 * when events may have been missed (every reopen after the first).
 */
export function connectEvents(on: (name: EventName, data: any) => void, onStatus: (s: LiveStatus, resync: boolean) => void) {
  if (FIXTURES) {
    fixtureBackend().then((b) => b.subscribe(on));
    onStatus('live', false);
    return () => {};
  }
  let es: EventSource | null = null;
  let retry = 1000;
  let downTimer: ReturnType<typeof setTimeout> | null = null;
  let down = false;
  let opened = false;
  let stopped = false;

  const open = () => {
    if (stopped) return;
    es = new EventSource('/api/events');
    es.onopen = () => {
      retry = 1000;
      if (downTimer) clearTimeout(downTimer);
      downTimer = null;
      down = false;
      onStatus('live', opened);
      opened = true;
    };
    es.onerror = () => {
      // Brief blips are normal (proxies recycle connections); only say
      // "reconnecting" when the stream stays down.
      if (!downTimer && !down) {
        downTimer = setTimeout(() => {
          downTimer = null;
          down = true;
          onStatus('reconnecting', false);
        }, 2500);
      }
      if (es && es.readyState === EventSource.CLOSED) {
        es.close();
        setTimeout(open, retry);
        retry = Math.min(retry * 2, 30000);
      }
    };
    for (const name of ['inbox', 'held', 'project', 'thread'] as EventName[]) {
      es.addEventListener(name, (ev) => {
        let data: any = {};
        try {
          data = (ev as MessageEvent).data ? JSON.parse((ev as MessageEvent).data) : {};
        } catch {
          /* a malformed payload still means "refetch" */
        }
        on(name, data);
      });
    }
  };
  const onVisible = () => {
    // Phones suspend background tabs; reconnect promptly on return.
    if (document.visibilityState === 'visible' && es && es.readyState === EventSource.CLOSED) {
      retry = 1000;
      open();
    }
  };
  open();
  document.addEventListener('visibilitychange', onVisible);
  return () => {
    stopped = true;
    es?.close();
    document.removeEventListener('visibilitychange', onVisible);
  };
}
