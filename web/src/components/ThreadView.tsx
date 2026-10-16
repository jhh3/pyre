// The thread view: what is being asked (rendered context), the answer
// options with one obvious action, an unmistakable outcome, and the
// conversation with a reply composer.
import {
  AlertCircle, Archive, ArrowDown, ArrowRight, ArrowUp, Check, CheckCheck, ChevronDown, ChevronLeft, ChevronUp, Clock, CornerDownLeft,
  FolderClosed, Loader2, MessageSquareText, Pause, Undo2, X, Zap,
} from 'lucide-react';
import { useEffect, useLayoutEffect, useRef, type ReactNode } from 'react';
import { Avatar, Chip, DeadlineChip, Dot, EmptyState, ErrorState, Markdown, Time } from '@/components/bits';
import { Button } from '@/components/ui/button';
import { Kbd } from '@/components/ui/kbd';
import { Skeleton } from '@/components/ui/skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import {
  archiveThread, choose, currentList, discardReply, dismissAnswerError, markRead, sendHeldNow, sendReply, undoHeld, useHeld, useInbox, useMe, useThread,
} from '@/hooks/data';
import { useNow } from '@/hooks/now';
import { listHref, navigate } from '@/hooks/route';
import { canArchive, heldItems, isOpenAsk, OPEN_ASK_ARCHIVE, optionLabel, projectHref, sessionState, threadHref } from '@/lib/model';
import * as T from '@/lib/time';
import type { Ask, AskOption, HeldAnswers, Msg, Thread } from '@/lib/types';
import { getUI, setUI, useUI, withKey, type PendingReply } from '@/lib/ui-store';
import { cn } from '@/lib/utils';

const isMac = /Mac|iPhone|iPad/.test(navigator.platform);

// ---------- header / toolbar ----------

function Toolbar({ thread }: { thread?: Thread }) {
  const box = useUI((s) => s.box);
  const filter = useUI((s) => s.filter);
  const list = useInbox(box, filter).data ?? [];
  const idx = thread ? list.findIndex((t) => t.thread_id === thread.thread_id) : -1;
  const go = (d: number) => {
    const t = list[idx + d];
    if (t) navigate(threadHref(t.thread_id), true);
  };
  return (
    <div className="flex h-14 shrink-0 items-center gap-1 border-b px-2 pt-[env(safe-area-inset-top)] md:h-[52px] md:px-4">
      <a
        href={listHref(box, filter)}
        className="flex h-10 items-center gap-0.5 rounded-lg pr-3 pl-1.5 text-[15px] font-medium text-brand-text transition-colors hover:bg-accent md:hidden"
      >
        <ChevronLeft className="size-5" />
        {box === 'open' ? 'Inbox' : 'Done'}
      </a>
      {thread && (canArchive(thread) || isOpenAsk(thread)) && <ArchiveButton thread={thread} />}
      <div className="flex-1" />
      {idx >= 0 && <span className="mr-1 text-[12.5px] text-faint tabular-nums">{idx + 1} of {list.length}</span>}
      <NavButton label="Previous" keys="K" disabled={idx <= 0} onClick={() => go(-1)}>
        <ChevronUp />
      </NavButton>
      <NavButton label="Next" keys="J" disabled={idx < 0 || idx >= list.length - 1} onClick={() => go(1)}>
        <ChevronDown />
      </NavButton>
    </div>
  );
}

// Archive: an open ask cannot be archived, so its button explains why
// instead of acting (a disabled button would hide the tooltip).
function ArchiveButton({ thread }: { thread: Thread }) {
  const blocked = isOpenAsk(thread);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button
          variant="ghost"
          aria-label="Archive"
          aria-disabled={blocked || undefined}
          onClick={() => archiveThread(thread)}
          className={cn('size-10 gap-1.5 px-0 text-muted-foreground md:-ml-1 md:h-8 md:w-auto md:px-2.5 md:text-[13px]', blocked && 'opacity-50')}
        >
          <Archive className="size-[18px] md:size-4" />
          <span className="hidden md:inline">Archive</span>
        </Button>
      </TooltipTrigger>
      <TooltipContent className={blocked ? 'max-w-60' : undefined}>
        {blocked ? (
          OPEN_ASK_ARCHIVE
        ) : (
          <>
            Archive: move to Done <Kbd>E</Kbd>
          </>
        )}
      </TooltipContent>
    </Tooltip>
  );
}

function NavButton({ label, keys, disabled, onClick, children }: { label: string; keys: string; disabled: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button variant="ghost" size="icon" aria-label={label} disabled={disabled} onClick={onClick} className="size-10 text-muted-foreground md:size-8">
          {children}
        </Button>
      </TooltipTrigger>
      <TooltipContent>
        {label} <Kbd>{keys}</Kbd>
      </TooltipContent>
    </Tooltip>
  );
}

function Header({ thread }: { thread: Thread }) {
  const ask = thread.ask;
  const st = sessionState(thread.session?.state);
  return (
    <header className="pt-6 md:pt-9">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-[13px]">
        {thread.project && (
          <a
            href={projectHref(thread.project.id)}
            className="inline-flex h-7 items-center gap-1.5 rounded-md border bg-card px-2 font-medium text-foreground/80 shadow-card transition-colors hover:border-border-strong hover:text-foreground"
          >
            <FolderClosed className="size-3.5 text-faint" />
            {thread.project.name}
          </a>
        )}
        {thread.session && (
          <span className="inline-flex min-w-0 items-center gap-2 text-muted-foreground">
            <Dot tone={st.tone} pulse />
            <span className="truncate font-medium text-foreground/80">{thread.session.title}</span>
            <span className="text-faint">·</span>
            <span className="truncate">{st.label}</span>
            {thread.session.node && <span className="hidden text-faint sm:inline">on {thread.session.node}</span>}
          </span>
        )}
      </div>
      <h1 className="mt-4 text-[22px] leading-[1.25] font-semibold tracking-[-0.022em] text-balance md:text-[26px]">{thread.title}</h1>
      <div className="mt-3 flex flex-wrap items-center gap-2 text-[13px] text-muted-foreground">
        {ask ? (
          <span>
            Asked <Time at={ask.created_at} mode="long" />
            {ask.count > 1 && <span className="text-faint"> · asked {ask.count} times</span>}
          </span>
        ) : (
          <span>
            {thread.message_count} message{thread.message_count === 1 ? '' : 's'} · last <Time at={thread.last_at} mode="long" />
          </span>
        )}
        {ask?.urgent && ask.state === 'open' && (
          <Chip tone="bad" icon={<Zap />}>
            Urgent
          </Chip>
        )}
        {ask && <DeadlineChip ask={ask} />}
        {ask?.state === 'open' && (
          <button
            onClick={() => document.getElementById('answer')?.scrollIntoView({ behavior: 'smooth', block: 'start' })}
            className="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[12.5px] font-medium text-brand-text transition-colors hover:bg-brand-soft"
          >
            Jump to options <ArrowDown className="size-3.5" />
          </button>
        )}
      </div>
    </header>
  );
}

// ---------- the ask ----------

function Context({ ask }: { ask: Ask }) {
  return (
    <div className="mt-7">
      <Markdown text={ask.context} className="text-[15px] md:text-[14.5px]" />
      {!!ask.updates?.length && (
        <div className="mt-6 space-y-2.5">
          {ask.updates.map((u) => (
            <div key={u.seq} className="rounded-xl border-l-[3px] border-primary/50 bg-muted/60 px-4 py-3">
              <div className="mb-1 text-[12px] font-semibold text-muted-foreground">
                Follow-up · <Time at={u.at} mode="long" />
              </div>
              <Markdown text={u.text} className="text-[14px]" />
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function ModeToggle() {
  const mode = useUI((s) => s.mode);
  const opts = [
    { value: 'now' as const, icon: Zap, label: 'Send now', tip: 'Tapping an option sends it right away.' },
    { value: 'hold' as const, icon: Pause, label: 'Hold & batch', tip: 'Tapping an option holds it; held answers go out together 5 minutes after your last one.' },
  ];
  return (
    <div role="radiogroup" aria-label="When to send" className="inline-flex shrink-0 rounded-lg bg-muted p-0.5">
      {opts.map((o) => (
        <Tooltip key={o.value}>
          <TooltipTrigger asChild>
            <button
              role="radio"
              aria-checked={mode === o.value}
              onClick={() => setUI({ mode: o.value })}
              className={cn(
                'inline-flex h-8 items-center gap-1.5 rounded-md px-2.5 text-[12.5px] font-medium transition-all',
                mode === o.value ? 'bg-card text-foreground shadow-card ring-1 ring-border' : 'text-muted-foreground hover:text-foreground',
              )}
            >
              <o.icon className="size-3.5" />
              {o.label}
            </button>
          </TooltipTrigger>
          <TooltipContent className="max-w-60">{o.tip} <Kbd>H</Kbd> toggles.</TooltipContent>
        </Tooltip>
      ))}
    </div>
  );
}

type OptState = 'idle' | 'sending' | 'held' | 'chosen' | 'dim';

function OptionCard({ opt, index, isDefault, state, actionLabel, disabled, onChoose }: { opt: AskOption; index: number; isDefault: boolean; state: OptState; actionLabel: string; disabled: boolean; onChoose?: () => void }) {
  return (
    <button
      type="button"
      onClick={onChoose}
      disabled={disabled}
      aria-pressed={state === 'held' || state === 'chosen'}
      aria-keyshortcuts={String(index + 1)}
      className={cn(
        'group relative flex w-full items-start gap-3.5 rounded-xl border px-4 py-3.5 text-left transition-[background-color,border-color,box-shadow,transform,opacity] duration-150',
        'disabled:cursor-default',
        state === 'idle' && 'bg-card shadow-card hover:border-primary/45 hover:bg-brand-soft/50 hover:shadow-[0_0_0_3px_color-mix(in_oklch,var(--primary)_10%,transparent)] active:scale-[0.993]',
        state === 'sending' && 'border-primary/60 bg-brand-soft/60',
        state === 'held' && 'border-primary/50 bg-brand-soft shadow-[0_0_0_3px_color-mix(in_oklch,var(--primary)_10%,transparent)]',
        state === 'chosen' && 'border-success/45 bg-success-soft',
        state === 'dim' && 'bg-card opacity-55',
      )}
    >
      <span
        className={cn(
          'mt-px flex size-6 shrink-0 items-center justify-center rounded-md border text-[12px] font-semibold tabular-nums transition-colors',
          state === 'idle' && 'bg-muted text-muted-foreground group-hover:border-primary/40 group-hover:bg-card group-hover:text-brand-text',
          (state === 'sending' || state === 'held') && 'border-primary/40 bg-card text-brand-text',
          state === 'chosen' && 'border-success/40 bg-success text-white',
          state === 'dim' && 'bg-muted text-faint',
        )}
      >
        {state === 'chosen' ? <Check className="size-3.5" strokeWidth={3} /> : state === 'sending' ? <Loader2 className="size-3.5 animate-spin" /> : index + 1}
      </span>
      <span className="min-w-0 flex-1">
        <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className="text-[15px] leading-6 font-semibold tracking-[-0.006em] md:text-[14.5px]">{opt.label}</span>
          {isDefault && <span className="rounded border px-1.5 text-[11px] leading-4 font-medium text-muted-foreground">Default</span>}
        </span>
        {opt.detail && <span className="mt-0.5 block text-[13.5px] leading-snug text-muted-foreground md:text-[13px]">{opt.detail}</span>}
      </span>
      {state === 'idle' && !disabled && (
        <span className="mt-0.5 hidden shrink-0 items-center gap-1 rounded-md bg-primary px-2 py-1 text-[12px] font-medium text-primary-foreground opacity-0 transition-opacity group-hover:opacity-100 md:inline-flex">
          {actionLabel}
          {actionLabel === 'Send' ? <ArrowRight className="size-3" /> : <Pause className="size-3" />}
        </span>
      )}
      {state === 'held' && <span className="mt-0.5 shrink-0 text-[12px] font-semibold text-brand-text">Held</span>}
      {state === 'sending' && <span className="mt-0.5 shrink-0 text-[12px] font-medium text-brand-text">Sending…</span>}
    </button>
  );
}

function HeldBanner({ ask, held }: { ask: Ask; held: HeldAnswers }) {
  useNow(1000);
  const items = heldItems(held);
  const mine = items.find((i) => i.ask_id === ask.ask_id)!;
  const others = items.length - 1;
  return (
    <div className="mb-4 flex flex-col gap-3 rounded-xl bg-brand-soft px-4 py-3.5 animate-rise sm:flex-row sm:items-center">
      <div className="flex min-w-0 flex-1 items-start gap-3">
        <span className="mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-full bg-primary text-primary-foreground">
          <Pause className="size-3.5" strokeWidth={2.5} />
        </span>
        <div className="min-w-0">
          <p className="text-[14px] font-semibold text-foreground">Held: {optionLabel(ask, mine.option_id) || mine.text}</p>
          <p className="mt-0.5 text-[13px] text-muted-foreground">
            {held.sends_at ? (
              <>
                Sends at <b className="font-semibold text-foreground">{T.clock(held.sends_at)}</b>{' '}
                <span className="tabular-nums">(in {T.countdown(held.sends_at)})</span>
                {others > 0 ? ` with ${others} other answer${others === 1 ? '' : 's'}` : ''}, unless you hold more.
              </>
            ) : (
              'Waiting to send with the batch.'
            )}
          </p>
        </div>
      </div>
      <div className="flex shrink-0 gap-2 pl-10 sm:pl-0">
        <Button variant="ghost" size="sm" onClick={() => undoHeld(ask.ask_id, ask.thread_id)} className="h-9 sm:h-8">
          <Undo2 /> Undo
        </Button>
        <Button size="sm" onClick={sendHeldNow} className="h-9 sm:h-8">
          {others > 0 ? `Send all ${items.length} now` : 'Send now'}
        </Button>
      </div>
    </div>
  );
}

function DeliveryLine({ msg, className }: { msg?: Msg; className?: string }) {
  if (!msg)
    return (
      <span className={cn('inline-flex items-center gap-1.5 text-muted-foreground', className)}>
        <Loader2 className="size-3.5 animate-spin" /> Waiting for delivery…
      </span>
    );
  const s = msg.delivery.state;
  const Icon = s === 'failed' ? AlertCircle : s === 'read' ? CheckCheck : s === 'replied' ? CornerDownLeft : s === 'delivered' ? Check : Clock;
  return (
    <span
      className={cn(
        'inline-flex min-w-0 items-center gap-1.5',
        s === 'failed' ? 'font-medium text-destructive' : ['delivered', 'read', 'replied'].includes(s) ? 'text-success' : 'text-muted-foreground',
        className,
      )}
    >
      <Icon className={cn('size-3.5 shrink-0', (s === 'queued' || s === 'stored') && 'animate-pulse-soft')} strokeWidth={2.25} />
      <span className="min-w-0">{msg.delivery.summary || s}</span>
    </span>
  );
}

function NextQuestion({ thread }: { thread: Thread }) {
  const open = useInbox('open').data ?? [];
  const held = useHeld();
  const next = open.find(
    (t) => t.thread_id !== thread.thread_id && t.ask?.state === 'open' && !heldItems(held).some((h) => h.ask_id === t.ask!.ask_id),
  );
  if (!next) return null;
  return (
    <a
      href={threadHref(next.thread_id)}
      className="group mt-3 flex items-center gap-2 rounded-lg px-1 py-1 text-[13px] text-muted-foreground transition-colors hover:text-foreground"
    >
      <span className="shrink-0 font-medium text-brand-text">Next question</span>
      <span className="truncate">{next.title}</span>
      <ArrowRight className="size-3.5 shrink-0 transition-transform group-hover:translate-x-0.5" />
    </a>
  );
}

function Outcome({ thread, messages }: { thread: Thread; messages: Msg[] }) {
  const ask = thread.ask!;
  const fresh = useUI((s) => s.fresh[ask.ask_id]);
  const decision = messages.find((m) => m.message_id === ask.decision_message_id);
  const failed = decision?.delivery.state === 'failed';
  const label = optionLabel(ask, ask.answer?.option_id) || ask.answer?.text || '';
  const session = thread.session?.title ?? 'the session';
  let icon: ReactNode, title: ReactNode, body: ReactNode, tone: 'ok' | 'bad' | 'neutral';
  switch (ask.state) {
    case 'answered':
      tone = failed ? 'bad' : 'ok';
      icon = failed ? <AlertCircle className="size-4" strokeWidth={2.5} /> : <Check className="size-4" strokeWidth={3} />;
      title = failed ? 'Recorded, but not delivered' : <>Sent {T.clock(ask.answer?.at)}</>;
      body = (
        <>
          {ask.answer?.by?.kind === 'owner' || !ask.answer?.by?.kind ? 'You chose ' : 'Answered: '}
          <b className="font-semibold text-foreground">{label}</b>
        </>
      );
      break;
    case 'defaulted':
      tone = failed ? 'bad' : 'neutral';
      icon = <Clock className="size-4" strokeWidth={2.5} />;
      title = <>Defaulted at {T.clock(ask.answer?.at ?? ask.deadline_at)}</>;
      body = (
        <>
          Nobody answered in time, so Pyre applied the default: <b className="font-semibold text-foreground">{label}</b>
        </>
      );
      break;
    case 'expired':
      tone = 'neutral';
      icon = <Clock className="size-4" strokeWidth={2.5} />;
      title = <>Expired {ask.deadline_at ? T.long(ask.deadline_at) : ''}</>;
      body = 'Nobody answered and there was no default, so nothing was sent.';
      break;
    case 'withdrawn':
      tone = 'neutral';
      icon = <Undo2 className="size-4" strokeWidth={2.5} />;
      title = 'Withdrawn';
      body = ask.updates?.length ? ask.updates[ask.updates.length - 1].text : `${session} took this question back.`;
      break;
    default:
      tone = 'neutral';
      icon = <ArrowRight className="size-4" strokeWidth={2.5} />;
      title = 'Replaced by a newer question';
      body = 'The session asked a newer version of this question.';
  }
  const showDelivery = ask.state === 'answered' || ask.state === 'defaulted';
  return (
    <div
      role="status"
      className={cn(
        'rounded-xl border px-4 py-4',
        tone === 'ok' && 'border-success/30 bg-success-soft',
        tone === 'bad' && 'border-destructive/30 bg-destructive-soft',
        tone === 'neutral' && 'bg-muted/60',
        fresh && Date.now() - fresh < 60_000 && 'animate-pop',
      )}
    >
      <div className="flex items-start gap-3">
        <span
          className={cn(
            'flex size-8 shrink-0 items-center justify-center rounded-full',
            tone === 'ok' && 'bg-success text-white',
            tone === 'bad' && 'bg-destructive text-white',
            tone === 'neutral' && 'bg-card text-muted-foreground ring-1 ring-border',
          )}
        >
          {icon}
        </span>
        <div className="min-w-0 flex-1">
          <p className={cn('text-[16px] leading-6 font-semibold tracking-[-0.01em]', tone === 'ok' && 'text-success', tone === 'bad' && 'text-destructive')}>{title}</p>
          <p className="mt-0.5 text-[14px] text-muted-foreground">{body}</p>
          {showDelivery && (
            <p className="mt-2 text-[13px]">
              <DeliveryLine msg={decision} />
            </p>
          )}
          {failed && (
            <p className="mt-2 text-[13px] text-muted-foreground">Your answer is saved. Reply below to reach {session} another way.</p>
          )}
        </div>
      </div>
    </div>
  );
}

function AnswerPanel({ thread, messages }: { thread: Thread; messages: Msg[] }) {
  const ask = thread.ask!;
  const mode = useUI((s) => s.mode);
  const answering = useUI((s) => s.answering[ask.ask_id]);
  const held = useHeld();
  const heldItem = heldItems(held).find((i) => i.ask_id === ask.ask_id);
  const open = ask.state === 'open';
  const session = thread.session?.title ?? 'the session';
  const sending = answering?.phase === 'sending';
  const def = optionLabel(ask, ask.default_option);
  useNow(30_000);
  const left = T.until(ask.deadline_at);

  const stateFor = (o: AskOption): OptState => {
    if (!open) return ask.answer?.option_id === o.id ? 'chosen' : 'dim';
    if (sending) return answering.option_id === o.id ? 'sending' : 'dim';
    if (heldItem) return heldItem.option_id === o.id ? 'held' : 'idle';
    return 'idle';
  };

  return (
    <section id="answer" aria-label="Your answer" className="mt-9 scroll-mt-4 rounded-2xl border bg-card p-4 shadow-card md:p-5">
      {open ? (
        <>
          <div className="mb-3.5 flex flex-wrap items-center justify-between gap-3">
            <div>
              <h2 className="text-[15px] font-semibold tracking-[-0.01em]">Your answer</h2>
              <p className="mt-0.5 text-[13px] text-muted-foreground">
                {mode === 'now' ? <>Tap an option to send it to {session} right away.</> : <>Tap an option to hold it. Held answers go out together.</>}
              </p>
            </div>
            <ModeToggle />
          </div>
          {heldItem && <HeldBanner ask={ask} held={held} />}
          {answering?.phase === 'error' && (
            <div role="alert" className="mb-4 flex flex-col gap-3 rounded-xl bg-destructive-soft px-4 py-3.5 animate-rise sm:flex-row sm:items-center">
              <div className="flex min-w-0 flex-1 items-start gap-3">
                <span className="mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-full bg-destructive text-white">
                  <AlertCircle className="size-4" strokeWidth={2.5} />
                </span>
                <div className="min-w-0">
                  <p className="text-[14px] font-semibold text-destructive">Not sent: {optionLabel(ask, answering.option_id)}</p>
                  <p className="mt-0.5 text-[13px] text-muted-foreground">{answering.message}</p>
                </div>
              </div>
              <div className="flex shrink-0 gap-2 pl-10 sm:pl-0">
                <Button variant="ghost" size="sm" className="h-9 sm:h-8" onClick={() => dismissAnswerError(ask.ask_id)}>
                  Dismiss
                </Button>
                <Button variant="destructive" size="sm" className="h-9 sm:h-8" onClick={() => choose(thread, answering.option_id, answering.mode)}>
                  Retry
                </Button>
              </div>
            </div>
          )}
        </>
      ) : (
        <div className="mb-4">
          <Outcome thread={thread} messages={messages} />
        </div>
      )}
      <div className="flex flex-col gap-2">
        {ask.options.map((o, i) => {
          const st = stateFor(o);
          return (
            <OptionCard
              key={o.id}
              opt={o}
              index={i}
              isDefault={o.id === ask.default_option}
              state={st}
              actionLabel={mode === 'now' ? 'Send' : 'Hold'}
              disabled={!open || sending || st === 'held'}
              onChoose={open ? () => choose(thread, o.id, mode) : undefined}
            />
          );
        })}
      </div>
      {open && (
        <p className="mt-3.5 flex items-start gap-2 text-[12.5px] leading-snug text-muted-foreground">
          <Clock className="mt-px size-3.5 shrink-0 text-faint" />
          <span>
            {ask.deadline_at ? (
              def ? (
                <>
                  If you don't answer, {session} goes with <b className="font-medium text-foreground">{def}</b> at {T.clock(ask.deadline_at)}
                  {left ? ` (in ${left})` : ''}.
                </>
              ) : (
                <>
                  No default: this question expires at {T.clock(ask.deadline_at)}
                  {left ? ` (in ${left})` : ''} and nothing is sent.
                </>
              )
            ) : (
              'No deadline.'
            )}{' '}
            Need more context? Ask in the reply box below.
          </span>
        </p>
      )}
      {!open && ask.state === 'answered' && <NextQuestion thread={thread} />}
    </section>
  );
}

// ---------- conversation ----------

function MessageItem({ m, me, decision }: { m: Msg; me: string; decision: boolean }) {
  const owner = m.from.kind === 'owner';
  return (
    <li className="flex gap-3 animate-rise">
      <Avatar name={owner ? me || 'You' : m.from.label} owner={owner} className="mt-0.5" />
      <div className={cn('min-w-0 flex-1 rounded-2xl rounded-tl-md border px-4 py-3', owner ? 'border-primary/15 bg-brand-soft/60' : 'bg-card shadow-card')}>
        <div className="flex items-baseline gap-2 text-[13px]">
          <span className="truncate font-semibold">{owner ? 'You' : m.from.label}</span>
          {decision && <span className="rounded bg-card px-1.5 text-[11px] font-medium text-muted-foreground ring-1 ring-border">Decision</span>}
          <Time at={m.at} mode="long" className="ml-auto shrink-0 text-[12px] text-faint" />
        </div>
        <Markdown text={m.text} className="mt-1 text-[14.5px] md:text-[14px]" />
        {owner && (
          <div className="mt-2 text-[12.5px]">
            <DeliveryLine msg={m} />
          </div>
        )}
      </div>
    </li>
  );
}

function PendingItem({ p, threadId, me }: { p: PendingReply; threadId: string; me: string }) {
  return (
    <li className="flex gap-3 animate-rise">
      <Avatar name={me || 'You'} owner className="mt-0.5" />
      <div className={cn('min-w-0 flex-1 rounded-2xl rounded-tl-md border px-4 py-3', p.phase === 'error' ? 'border-destructive/30 bg-destructive-soft' : 'border-primary/15 bg-brand-soft/60')}>
        <div className="flex items-baseline gap-2 text-[13px]">
          <span className="font-semibold">You</span>
          <span className="ml-auto text-[12px] text-faint">{T.clock(p.at)}</span>
        </div>
        <Markdown text={p.text} className="mt-1 text-[14.5px] md:text-[14px]" />
        <div className="mt-2 text-[12.5px]">
          {p.phase === 'sending' ? (
            <span className="inline-flex items-center gap-1.5 text-muted-foreground">
              <Loader2 className="size-3.5 animate-spin" /> Sending…
            </span>
          ) : (
            <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
              <span className="inline-flex items-center gap-1.5 font-medium text-destructive">
                <AlertCircle className="size-3.5" /> Not sent. {p.message}
              </span>
              <span className="flex gap-1.5">
                <Button size="xs" variant="destructive" onClick={() => sendReply(threadId, p.text, p.key)}>
                  Retry
                </Button>
                <Button
                  size="xs"
                  variant="ghost"
                  onClick={() => {
                    discardReply(threadId, p.key);
                    setUI((s) => ({ drafts: withKey(s.drafts, threadId, p.text) }));
                  }}
                >
                  Edit
                </Button>
                <Button size="icon-xs" variant="ghost" aria-label="Discard" onClick={() => discardReply(threadId, p.key)}>
                  <X />
                </Button>
              </span>
            </div>
          )}
        </div>
      </div>
    </li>
  );
}

function Conversation({ thread, messages }: { thread: Thread; messages: Msg[] }) {
  const me = useMe().data?.email ?? '';
  const pending = useUI((s) => s.pending[thread.thread_id]) ?? [];
  const ask = thread.ask;
  // The ask itself is shown above; skip the message that carried it.
  const shown = messages.filter((m) => !(ask && (m.message_id === ask.message_id || (m.ask_id === ask.ask_id && m.from.kind === 'session' && m === messages[0]))));
  return (
    <section aria-label="Conversation" className="mt-10">
      <h2 className="mb-3 flex items-center gap-2 text-[12px] font-semibold tracking-wider text-faint uppercase">
        Conversation
        {shown.length + pending.length > 0 && <span className="tabular-nums">· {shown.length + pending.length}</span>}
      </h2>
      {shown.length + pending.length === 0 ? (
        <p className="rounded-xl border border-dashed px-4 py-4 text-[13.5px] text-muted-foreground">
          No messages yet. Ask {thread.session?.title ?? 'the session'} for more context below; the reply lands here.
        </p>
      ) : (
        <ol className="flex flex-col gap-3">
          {shown.map((m) => (
            <MessageItem key={m.message_id} m={m} me={me} decision={!!ask && m.message_id === ask.decision_message_id} />
          ))}
          {pending.map((p) => (
            <PendingItem key={p.key} p={p} threadId={thread.thread_id} me={me} />
          ))}
        </ol>
      )}
    </section>
  );
}

// ---------- composer ----------

function Composer({ thread }: { thread: Thread }) {
  const id = thread.thread_id;
  const draft = useUI((s) => s.drafts[id]) ?? '';
  const ref = useRef<HTMLTextAreaElement>(null);
  const session = thread.session?.title;
  const setDraft = (v: string) => setUI((s) => ({ drafts: withKey(s.drafts, id, v || undefined) }));

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = Math.min(el.scrollHeight, window.innerHeight * 0.4) + 'px';
  }, [draft]);

  const send = () => {
    const text = (getUI().drafts[id] ?? '').trim();
    if (!text) return;
    setDraft('');
    sendReply(id, text);
  };

  if (!session) return null;
  return (
    <div className="shrink-0 border-t bg-card px-3 pt-3 pb-[max(env(safe-area-inset-bottom),12px)] md:px-6">
      <form
        className="mx-auto flex max-w-[760px] items-end gap-1 rounded-[22px] border bg-background p-1 transition-[border-color,box-shadow] focus-within:border-ring/50 focus-within:shadow-[0_0_0_3px_color-mix(in_oklch,var(--ring)_14%,transparent)] md:block md:rounded-2xl md:p-0"
        onSubmit={(e) => {
          e.preventDefault();
          send();
        }}
      >
        <label htmlFor="composer" className="sr-only">
          Reply to {session}
        </label>
        <textarea
          id="composer"
          ref={ref}
          rows={1}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
              e.preventDefault();
              send();
            } else if (e.key === 'Escape') {
              (e.target as HTMLTextAreaElement).blur();
            }
          }}
          placeholder={thread.kind === 'ask' ? 'Ask for more context…' : `Reply to ${session}…`}
          className="block max-h-[40vh] min-h-10 w-full min-w-0 flex-1 resize-none bg-transparent px-3.5 py-2 text-[16px] leading-6 outline-none placeholder:text-faint md:min-h-11 md:px-4 md:pt-3 md:pb-1 md:text-[14.5px]"
        />
        <div className="flex shrink-0 items-center gap-2 md:pr-2 md:pb-2 md:pl-4">
          <span className="hidden min-w-0 flex-1 truncate text-[12px] text-faint md:block">
            To {session} · <Kbd className="h-[18px] text-[11px]">{isMac ? '⌘' : 'Ctrl'}</Kbd> <Kbd className="h-[18px] text-[11px]">↵</Kbd> to send
          </span>
          <Button type="submit" disabled={!draft.trim()} aria-label="Send" className="size-10 rounded-full p-0 md:h-8 md:w-auto md:rounded-lg md:px-3.5">
            <span className="hidden md:inline">Send</span> <ArrowUp className="size-[18px] md:size-4" />
          </Button>
        </div>
      </form>
    </div>
  );
}

// ---------- pane ----------

function ThreadSkeleton() {
  return (
    <div className="pt-9" aria-label="Loading">
      <Skeleton className="h-6 w-48" />
      <Skeleton className="mt-5 h-8 w-4/5" />
      <Skeleton className="mt-3 h-4 w-40" />
      <div className="mt-8 space-y-2.5">
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-4 w-11/12" />
        <Skeleton className="h-4 w-3/4" />
      </div>
      <Skeleton className="mt-9 h-48 w-full rounded-2xl" />
    </div>
  );
}

export function ThreadPane({ id, className }: { id: string; className?: string }) {
  const q = useThread(id);
  const scroller = useRef<HTMLDivElement>(null);
  useEffect(() => {
    scroller.current?.scrollTo({ top: 0 });
    setUI({ cursor: id });
  }, [id]);

  useEffect(() => {
    if (q.data?.thread) markRead(q.data.thread);
  }, [q.data?.thread]);

  const thread = q.data?.thread;
  const messages = q.data?.messages ?? [];
  return (
    <section className={cn('flex min-h-0 min-w-0 flex-col bg-card', className)} aria-label="Conversation">
      <Toolbar thread={thread} />
      <div ref={scroller} className="scroll-quiet min-h-0 flex-1 overflow-y-auto overscroll-contain">
        <div className="mx-auto max-w-[760px] px-4 pb-12 md:px-8">
          {q.isPending ? (
            <ThreadSkeleton />
          ) : q.isError ? (
            <ErrorState title="Couldn't open this conversation" error={q.error} onRetry={() => q.refetch()} className="mt-10" />
          ) : thread ? (
            <article key={thread.thread_id}>
              <Header thread={thread} />
              {thread.ask && <Context ask={thread.ask} />}
              {thread.ask && <AnswerPanel thread={thread} messages={messages} />}
              <Conversation thread={thread} messages={messages} />
            </article>
          ) : null}
        </div>
      </div>
      {thread && <Composer thread={thread} />}
    </section>
  );
}

export function NoThread({ className }: { className?: string }) {
  return (
    <section className={cn('flex min-h-0 flex-col items-center justify-center bg-card', className)}>
      <EmptyState icon={<MessageSquareText />} title="Pick a conversation">
        <span className="inline-flex flex-wrap items-center justify-center gap-1">
          <Kbd>J</Kbd> <Kbd>K</Kbd> to move, <Kbd>↵</Kbd> to open, <Kbd>?</Kbd> for all shortcuts.
        </span>
      </EmptyState>
    </section>
  );
}

/** Open the next/previous thread in the current list (j/k). */
export function stepThread(delta: number, fromId: string | null) {
  const list = currentList();
  if (!list.length) return;
  const idx = fromId ? list.findIndex((t) => t.thread_id === fromId) : -1;
  const next = list[Math.max(0, Math.min(list.length - 1, idx + delta))];
  if (next) navigate(threadHref(next.thread_id), true);
}

