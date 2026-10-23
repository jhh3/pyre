import { Archive, CheckCheck, Inbox as InboxIcon, Menu, MessageCircle, X, Zap } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { Chip, DeadlineChip, EmptyState, ErrorState, HeldChip, Time } from '@/components/bits';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Kbd } from '@/components/ui/kbd';
import { Skeleton } from '@/components/ui/skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { archiveThread, archiveThreads, useHeld, useInbox, useProjects } from '@/hooks/data';
import { listHref, useRoute, type Filter } from '@/hooks/route';
import { archiveDismisses, canArchive, canBulkArchive, heldItems, isOpenAsk, outcome, plainPreview, threadHref } from '@/lib/model';
import { dayKey } from '@/lib/time';
import type { Box, HeldAnswers, Thread } from '@/lib/types';
import { setUI, useUI } from '@/lib/ui-store';
import { cn } from '@/lib/utils';

export function MenuButton() {
  return (
    <button
      aria-label="Open navigation"
      onClick={() => setUI({ navOpen: true })}
      className="-ml-1.5 flex size-10 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground active:bg-accent lg:hidden"
    >
      <Menu className="size-5" />
    </button>
  );
}

function FilterChip({ box, filter }: { box: Box; filter: Filter }) {
  const projects = useProjects();
  const list = useInbox(box, filter);
  let label = '';
  if (filter.project) {
    const p = projects.data?.find((x) => x.project_id === filter.project);
    label = 'Project: ' + (p?.name ?? filter.project);
  } else if (filter.session) {
    const lead =
      projects.data?.flatMap((p) => p.leads ?? []).find((l) => l.id === filter.session)?.title ??
      list.data?.find((t) => t.session?.id === filter.session)?.session?.title;
    label = 'Session: ' + (lead ?? filter.session);
  }
  if (!label) return null;
  return (
    <a
      href={listHref(box)}
      className="group inline-flex h-7 max-w-full items-center gap-1 rounded-full border bg-card pr-1.5 pl-2.5 text-[12.5px] font-medium text-foreground shadow-card transition-colors hover:border-border-strong"
      aria-label={`Remove filter ${label}`}
    >
      <span className="truncate">{label}</span>
      <X className="size-3.5 text-faint transition-colors group-hover:text-foreground" />
    </a>
  );
}

function Row({ t, held, box, selected, cursor }: { t: Thread; held: HeldAnswers; box: Box; selected: boolean; cursor: boolean }) {
  const ref = useRef<HTMLAnchorElement>(null);
  const heldItem = t.ask ? heldItems(held).find((i) => i.ask_id === t.ask!.ask_id) : undefined;
  const preview = plainPreview(t.preview);
  const decided = t.ask && t.ask.state !== 'open' ? outcome(t.ask) : null;
  // The outcome chip, unless the preview already says the same thing.
  const out = decided && !preview.startsWith(decided.text) ? decided : null;
  // Open threads get an archive button (hover on desktop, always on
  // touch); for an open ask or a session item it dismisses (an open ask
  // confirms first).
  const archivable = box === 'open' && canArchive(t);
  const dismiss = archiveDismisses(t);
  useEffect(() => {
    if (cursor) ref.current?.scrollIntoView({ block: 'nearest' });
  }, [cursor]);
  return (
    <div className="group relative mx-2">
      <a
        ref={ref}
        href={threadHref(t.thread_id)}
        onClick={() => setUI({ box, cursor: t.thread_id })}
        aria-current={selected ? 'true' : undefined}
        data-thread={t.thread_id}
        className={cn(
          'flex gap-3 rounded-xl px-3 py-3 transition-colors',
          selected ? 'bg-accent' : 'group-hover:bg-accent/60 active:bg-accent',
          cursor && !selected && 'ring-1 ring-ring/40',
        )}
      >
        <span className="flex w-2 shrink-0 justify-center pt-[7px]">
          {t.unread && <span className="size-2 rounded-full bg-primary" aria-label="Unread" />}
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-1.5 text-[12px] leading-4 text-muted-foreground">
            {t.project && <span className="truncate font-medium text-foreground/70">{t.project.name}</span>}
            {t.project && t.session && <span className="text-faint">·</span>}
            {t.session && <span className="truncate">{t.session.title}</span>}
            <Time at={t.last_at} className={cn('ml-auto shrink-0 pl-2 text-[12px]', t.unread ? 'font-medium text-foreground' : 'text-faint')} />
          </div>
          <div className={cn('mt-1 truncate text-[14.5px] leading-5 tracking-[-0.006em]', archivable && 'pr-8', t.unread ? 'font-semibold text-foreground' : 'font-medium text-foreground/85')}>
            {t.title}
          </div>
          <div className={cn('mt-0.5 line-clamp-1 text-[13px] leading-5 text-muted-foreground', archivable && 'pr-8')}>{preview}</div>
          {(t.ask?.state === 'open' || heldItem || t.needs_reply || out) && (
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              {t.ask?.urgent && t.ask.state === 'open' && (
                <Chip tone="bad" icon={<Zap />}>
                  Urgent
                </Chip>
              )}
              {heldItem ? <HeldChip held={heldItem} sendsAt={held.sends_at} ask={t.ask} /> : t.ask && <DeadlineChip ask={t.ask} />}
              {t.kind === 'message' && t.needs_reply && (
                <Chip tone="brand" icon={<MessageCircle />}>
                  Waiting for your reply
                </Chip>
              )}
              {t.kind === 'attention' && t.needs_reply && (
                <Chip tone="brand" icon={<MessageCircle />}>
                  {t.attention?.kind === 'waiting' ? 'Waiting on you in its chat' : 'Needs you'}
                </Chip>
              )}
              {out && <Chip tone={out.tone === 'ok' ? 'ok' : 'neutral'}>{out.text}</Chip>}
            </div>
          )}
        </div>
      </a>
      {archivable && (
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              aria-label={`${dismiss ? 'Dismiss' : 'Archive'} ${t.title}`}
              onClick={() => archiveThread(t)}
              className="absolute top-8 right-2 flex size-8 items-center justify-center rounded-lg text-muted-foreground opacity-0 transition-[opacity,color,background-color] group-hover:opacity-100 hover:bg-background hover:text-foreground focus-visible:opacity-100 focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:outline-none pointer-coarse:opacity-100"
            >
              {dismiss ? <X className="size-4" /> : <Archive className="size-4" />}
            </button>
          </TooltipTrigger>
          <TooltipContent>
            {isOpenAsk(t) ? 'Dismiss (no option, no default)' : dismiss ? 'Dismiss' : 'Archive'} <Kbd>E</Kbd>
          </TooltipContent>
        </Tooltip>
      )}
    </div>
  );
}

const DAY_MS = 24 * 60 * 60 * 1000;

/** "Archive all older than 1 day" under the Open list, with a confirmation. */
function ArchiveOlder({ threads }: { threads: Thread[] }) {
  const [open, setOpen] = useState(false);
  const older = threads.filter((t) => canBulkArchive(t) && Date.now() - Date.parse(t.last_at) > DAY_MS);
  if (!older.length) return null;
  const n = older.length;
  const noun = `conversation${n === 1 ? '' : 's'}`;
  return (
    <div className="flex justify-center px-4 pt-4">
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-[13px] font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
      >
        <Archive className="size-3.5" />
        Archive all older than 1 day
      </button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-w-sm">
          <DialogHeader>
            <DialogTitle className="text-[16px]">Archive {n} {noun}?</DialogTitle>
            <DialogDescription className="text-[13.5px]">
              {n === 1 ? 'One message thread' : `${n} message threads`} with no activity for more than a day move to Done. Open questions stay in
              the Inbox. A new message from the session brings a thread back.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button
              autoFocus
              onClick={() => {
                setOpen(false);
                archiveThreads(older.map((t) => t.thread_id));
              }}
            >
              Archive {n}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function group(threads: Thread[]) {
  const today = dayKey(new Date());
  const groups: { label: string; items: Thread[] }[] = [];
  for (const t of threads) {
    const label = dayKey(t.last_at) === today ? 'Today' : 'Earlier';
    const g = groups[groups.length - 1];
    if (g && g.label === label) g.items.push(t);
    else groups.push({ label, items: [t] });
  }
  return groups;
}

function ListSkeleton() {
  return (
    <div className="space-y-1 px-2 pt-2" aria-label="Loading">
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className="flex gap-3 rounded-xl px-3 py-3">
          <span className="w-2" />
          <div className="flex-1 space-y-2">
            <Skeleton className="h-3 w-2/5" />
            <Skeleton className="h-4 w-4/5" />
            <Skeleton className="h-3 w-3/5" />
          </div>
        </div>
      ))}
    </div>
  );
}

export function InboxList({ box, filter, className }: { box: Box; filter: Filter; className?: string }) {
  const route = useRoute();
  const q = useInbox(box, filter);
  const held = useHeld();
  const cursor = useUI((s) => s.cursor);
  const selected = route.name === 'thread' ? route.id : null;
  const threads = q.data ?? [];
  const filtered = !!(filter.project || filter.session);

  return (
    <section className={cn('flex min-h-0 flex-col bg-card', className)} aria-label={box === 'open' ? 'Inbox' : 'Done'}>
      <header className="flex shrink-0 flex-col gap-2 px-4 pt-[max(env(safe-area-inset-top),12px)] pb-3 md:px-5 md:pt-5">
        <div className="flex h-10 items-center gap-1">
          <MenuButton />
          <h1 className="text-[19px] font-semibold tracking-[-0.018em]">{box === 'open' ? 'Inbox' : 'Done'}</h1>
          <span className="ml-2 text-[13px] text-faint tabular-nums">
            {q.isSuccess && (box === 'open' ? `${threads.length} open` : 'Last 14 days')}
          </span>
        </div>
        {filtered && (
          <div>
            <FilterChip box={box} filter={filter} />
          </div>
        )}
      </header>
      <div className="scroll-quiet min-h-0 flex-1 overflow-y-auto overscroll-contain pb-6">
        {q.isPending ? (
          <ListSkeleton />
        ) : q.isError ? (
          <ErrorState title={box === 'open' ? "Couldn't load your inbox" : "Couldn't load Done"} error={q.error} onRetry={() => q.refetch()} />
        ) : !threads.length ? (
          box === 'open' ? (
            <EmptyState icon={<InboxIcon />} title={filtered ? 'Nothing open here' : "You're all caught up"}>
              {filtered ? 'No open questions or messages match this filter.' : 'New questions and messages from your sessions land here.'}
            </EmptyState>
          ) : (
            <EmptyState icon={<CheckCheck />} title="Nothing decided yet">
              Answered questions and handled messages from the last 14 days show up here.
            </EmptyState>
          )
        ) : (
          group(threads).map((g) => (
            <div key={g.label}>
              <div className="sticky top-0 z-10 bg-card/90 px-5 pt-3 pb-1.5 text-[11px] font-semibold tracking-wider text-faint uppercase backdrop-blur-sm">{g.label}</div>
              <div className="flex flex-col gap-0.5">
                {g.items.map((t) => (
                  <Row key={t.thread_id} t={t} held={held} box={box} selected={selected === t.thread_id} cursor={cursor === t.thread_id} />
                ))}
              </div>
            </div>
          ))
        )}
        {box === 'open' && q.isSuccess && <ArchiveOlder threads={threads} />}
      </div>
    </section>
  );
}
