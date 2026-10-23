// Small lookups shared by the views.
import type { Ask, HeldAnswer, HeldAnswers, Msg, ProjectCard, Thread } from './types';

export const threadHref = (id: string) => '#/thread/' + encodeURIComponent(id);
export const projectHref = (id: string) => '#/projects/' + encodeURIComponent(id);
export const askThreadId = (ask: Ask) => ask.thread_id || 'ask:' + ask.ask_id;

/** What dismissing an open ask does (archiving one asks to confirm this first). */
export const DISMISS_ASK = 'No option is chosen and its default never applies. The session is told you dismissed it.';
export const isOpenAsk = (t: Thread) => t.kind === 'ask' && t.ask?.state === 'open';
/** Archiving would change something: the thread waits on you (an open ask is dismissed, after a confirmation). */
export const canArchive = (t: Thread) => t.needs_reply || isOpenAsk(t);
/** Bulk archive never dismisses open asks. */
export const canBulkArchive = (t: Thread) => canArchive(t) && !isOpenAsk(t);
/** Archive dismisses rather than files away: open asks and session items. */
export const archiveDismisses = (t: Thread) => isOpenAsk(t) || t.kind === 'attention';

export const heldItems = (held: HeldAnswers | undefined): HeldAnswer[] => held?.items ?? [];

export function optionLabel(ask: Ask | null | undefined, id: string | undefined) {
  return ask?.options.find((o) => o.id === id)?.label ?? '';
}

export type Tone = 'working' | 'idle' | 'attn' | 'bad' | 'off';

// Session states as the web API reports them (older run states kept too).
const STATES: Record<string, [string, Tone]> = {
  working: ['Working', 'working'],
  needs_you: ['Needs you', 'attn'],
  idle: ['Idle', 'idle'],
  ended: ['Ended', 'off'],
  archived: ['Archived', 'off'],
  starting: ['Starting', 'working'],
  running: ['Working', 'working'],
  waiting_input: ['Waiting for input', 'idle'],
  waiting_approval: ['Needs approval', 'attn'],
  waiting_on_background: ['Waiting on background work', 'working'],
  exiting: ['Stopping', 'off'],
  exited: ['Finished', 'off'],
  failed: ['Failed', 'bad'],
  unknown: ['Unreachable', 'off'],
  stopped: ['Stopped', 'off'],
};

export function sessionState(state: string | undefined) {
  const [label, tone] = STATES[state ?? ''] ?? [state ? state.replace(/_/g, ' ') : 'Unknown', 'off' as Tone];
  return { label, tone };
}

/** A project's dot: waiting on the owner > something working > quiet. */
export function projectTone(card: ProjectCard): Tone {
  if (card.waiting || card.open_asks > 0) return 'attn';
  if ((card.leads ?? []).some((l) => sessionState(l.state).tone === 'working')) return 'working';
  return 'idle';
}

export const isDelivered = (m: Msg | undefined) => !!m && ['delivered', 'read', 'replied'].includes(m.delivery.state);

/** The option chosen for one item of a batched ask (answered, defaulted or held). */
export function itemOption(ask: Ask, itemId: string, answer: { option_id?: string; items?: Record<string, string> | null } | null | undefined = ask.answer) {
  return answer?.items?.[itemId] ?? answer?.option_id ?? '';
}

/** A batched answer that differs by item. */
export const answeredPerItem = (ask: Ask) => !!ask.items?.length && Object.keys(ask.answer?.items ?? {}).length > 0;

/** The outcome of a decided ask, in words. */
export function outcome(ask: Ask) {
  const label = answeredPerItem(ask) ? 'per item' : optionLabel(ask, ask.answer?.option_id) || ask.answer?.text || '';
  switch (ask.state) {
    case 'answered':
      if (ask.grant_id) return { tone: 'ok' as const, chip: 'Granted', text: `Standing grant: ${label}` };
      if (ask.proposal?.state === 'applied')
        return { tone: 'ok' as const, chip: 'Approved', text: ask.proposal.kind === 'fact' ? 'Approved: added to the fact sheet' : 'Approved: go-ahead recorded' };
      if (ask.proposal?.state === 'failed') return { tone: 'neutral' as const, chip: 'Approved', text: `Approved, not recorded: ${ask.proposal.error ?? ''}` };
      if (ask.answer?.source === 'chat') return { tone: 'ok' as const, chip: 'Answered', text: `You answered in chat: ${ask.answer.text ?? ''}` };
      return { tone: 'ok' as const, chip: 'Answered', text: label ? `You chose: ${label}` : 'Answered' };
    case 'defaulted':
      return { tone: 'neutral' as const, chip: 'Defaulted', text: `Defaulted to: ${label}` };
    case 'expired':
      return { tone: 'neutral' as const, chip: 'Expired', text: 'Expired with no answer' };
    case 'withdrawn':
      if (ask.answer?.by?.kind === 'owner') return { tone: 'neutral' as const, chip: 'Dismissed', text: 'Dismissed by you' };
      return { tone: 'neutral' as const, chip: 'Withdrawn', text: 'Withdrawn by the session' };
    case 'superseded':
      return { tone: 'neutral' as const, chip: 'Replaced', text: 'Replaced by a newer question' };
    default:
      return { tone: 'neutral' as const, chip: '', text: '' };
  }
}

/** One line of plain text from a markdown preview (list rows show no markup). */
export function plainPreview(s: string) {
  return s
    .replace(/```[\s\S]*?```/g, ' ')
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/(\*\*|__|\*|_|~~|`)(\S(?:.*?\S)?)\1/g, '$2')
    .replace(/^\s{0,3}(#{1,6}\s+|>\s?|[-*+]\s+|\d+\.\s+)/gm, '')
    .replace(/\|/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}
