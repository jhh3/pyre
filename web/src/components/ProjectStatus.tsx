// The project view's live status card: the lead's progress and items
// (contracts.ProjectStatus, written with update_project_page) next to what
// Pyre sees itself (working, idle and stuck sessions, open asks).
import { AlertTriangle, ArrowUpRight, ChevronRight } from 'lucide-react';
import type { ReactNode } from 'react';
import { Chip, DeadlineChip, Dot, Watching } from '@/components/bits';
import { askThreadId, sessionState, threadHref } from '@/lib/model';
import * as T from '@/lib/time';
import type { Ask, SessionCard, StatusCard, StatusItem } from '@/lib/types';
import { cn } from '@/lib/utils';

const SHIPPED_TONE: Record<string, 'ok' | 'brand' | 'neutral'> = { deployed: 'ok', released: 'ok', live: 'ok', merged: 'brand' };

function Section({ title, count, tone, children }: { title: string; count: number; tone?: 'warn' | 'bad'; children: ReactNode }) {
  return (
    <section className="min-w-0">
      <h3
        className={cn(
          'mb-1.5 flex items-center gap-1.5 text-[11.5px] font-semibold tracking-wider uppercase',
          tone === 'warn' ? 'text-warning' : tone === 'bad' ? 'text-destructive' : 'text-faint',
        )}
      >
        {title}
        <span className="tabular-nums">· {count}</span>
      </h3>
      <ul className="flex flex-col gap-0.5">{children}</ul>
    </section>
  );
}

function Session({ s, note }: { s: SessionCard; note?: string }) {
  const st = sessionState(s.state);
  return (
    <li className="py-1">
      <div className="flex items-center gap-2">
        <Dot tone={st.tone} pulse />
        <span className="min-w-0 truncate text-[13px] font-medium">{s.title}</span>
        {s.is_lead && <span className="rounded bg-brand-soft px-1.5 text-[10.5px] leading-4 font-semibold text-brand-text">Lead</span>}
        <Watching titles={s.watching} />
        {note && <span className="ml-auto shrink-0 text-[11.5px] text-faint">{note}</span>}
      </div>
      {s.last_activity && (
        <p className="line-clamp-1 pl-4 text-[12px] text-muted-foreground">
          {s.last_activity.line} <span className="text-faint">· {T.ago(s.last_activity.at)}</span>
        </p>
      )}
    </li>
  );
}

function Item({ it, tone }: { it: StatusItem; tone?: 'ok' | 'brand' | 'neutral' | 'warn' | 'bad' }) {
  const body = (
    <>
      <span className="flex items-center gap-2">
        <span className="min-w-0 truncate text-[13px] font-medium">{it.title}</span>
        {it.state && (
          <Chip tone={tone ?? 'neutral'} className="shrink-0">
            {it.state}
          </Chip>
        )}
        {it.url && <ArrowUpRight className="ml-auto size-3.5 shrink-0 text-faint" />}
      </span>
      {it.note && <span className="block truncate text-[12px] text-muted-foreground">{it.note}</span>}
    </>
  );
  return (
    <li>
      {it.url ? (
        <a href={it.url} target="_blank" rel="noopener noreferrer" className="block rounded-md py-1 transition-colors hover:bg-accent/60">
          {body}
        </a>
      ) : (
        <div className="py-1">{body}</div>
      )}
    </li>
  );
}

function AskRow({ a }: { a: Ask }) {
  return (
    <li>
      <a href={threadHref(askThreadId(a))} className="group block rounded-md py-1 transition-colors hover:bg-accent/60">
        <span className="flex items-center gap-2">
          <span className="min-w-0 flex-1 truncate text-[13px] font-medium">{a.title}</span>
          <ChevronRight className="size-3.5 shrink-0 text-faint group-hover:text-brand-text" />
        </span>
        <span className="mt-0.5 flex">
          <DeadlineChip ask={a} />
        </span>
      </a>
    </li>
  );
}

export function ProjectStatusCard({ status, openAsks }: { status: StatusCard; openAsks: Ask[] }) {
  const lead = status.lead;
  const working = status.working ?? [];
  const needsYou = status.needs_you ?? [];
  const idle = status.idle ?? [];
  const stuck = status.stuck ?? [];
  const inFlight = lead?.in_flight ?? [];
  const ready = lead?.ready_for_owner ?? [];
  const shipped = lead?.shipped ?? [];
  const blocked = lead?.blocked ?? [];
  const progress = lead?.progress;
  const readyCount = openAsks.length + needsYou.length + ready.length;
  const flightCount = working.length + inFlight.length;
  const troubleCount = stuck.length + idle.length + blocked.length;
  const empty = !flightCount && !readyCount && !shipped.length && !troubleCount;

  return (
    <section className={cn('mb-6 rounded-2xl border bg-card p-4 shadow-card md:p-5', status.stale && 'border-warning/40')} aria-label="Project status">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <h2 className="text-[12px] font-semibold tracking-wider text-faint uppercase">Status</h2>
        {progress != null && (
          <div className="flex min-w-[160px] flex-1 items-center gap-2.5 sm:max-w-[320px]">
            <div className="h-2 flex-1 overflow-hidden rounded-full bg-muted" role="progressbar" aria-valuenow={progress} aria-valuemin={0} aria-valuemax={100}>
              <div className="h-full rounded-full bg-primary transition-[width]" style={{ width: `${progress}%` }} />
            </div>
            <span className="text-[13px] font-semibold tabular-nums">{progress}%</span>
          </div>
        )}
        {lead?.eta && <Chip>ETA {lead.eta}</Chip>}
        <span className="ml-auto text-[12px] text-faint">
          {status.updated_at ? `Lead updated ${T.ago(status.updated_at)}` : 'The lead has not written a status yet'}
        </span>
      </div>
      {lead?.phase && <p className="mt-2 text-[14px] leading-snug text-foreground/90">{lead.phase}</p>}
      {status.stale && (
        <p className="mt-2.5 flex items-center gap-1.5 rounded-lg bg-warning-soft px-2.5 py-1.5 text-[12.5px] font-medium text-warning">
          <AlertTriangle className="size-3.5 shrink-0" />
          Work is running but the lead hasn't updated this in over {status.stale_after_minutes} min; it may be out of date.
        </p>
      )}

      {empty ? (
        <p className="mt-3 text-[13px] text-faint">Nothing running, nothing waiting on you.</p>
      ) : (
        <div className="mt-4 grid grid-cols-[minmax(0,1fr)] gap-x-6 gap-y-4 sm:grid-cols-2 xl:grid-cols-4">
          {readyCount > 0 && (
            <Section title="Ready for you" count={readyCount} tone="warn">
              {openAsks.map((a) => (
                <AskRow key={a.ask_id} a={a} />
              ))}
              {ready.map((it, i) => (
                <Item key={'r' + i} it={it} tone="warn" />
              ))}
              {needsYou.map((s) => (
                <Session key={s.id} s={s} />
              ))}
            </Section>
          )}
          {flightCount > 0 && (
            <Section title="In flight" count={flightCount}>
              {working.map((s) => (
                <Session key={s.id} s={s} />
              ))}
              {inFlight.map((it, i) => (
                <Item key={'f' + i} it={it} />
              ))}
            </Section>
          )}
          {shipped.length > 0 && (
            <Section title="Shipped" count={shipped.length}>
              {shipped.map((it, i) => (
                <Item key={'s' + i} it={it} tone={SHIPPED_TONE[(it.state ?? '').toLowerCase()] ?? 'neutral'} />
              ))}
            </Section>
          )}
          {troubleCount > 0 && (
            <Section title="Idle or stuck" count={troubleCount} tone={stuck.length || blocked.length ? 'bad' : undefined}>
              {stuck.map((s) => (
                <Session key={s.id} s={s} note={s.state === 'failed' ? 'Failed' : 'No activity'} />
              ))}
              {blocked.map((it, i) => (
                <Item key={'b' + i} it={it} tone="bad" />
              ))}
              {idle.map((s) => (
                <Session key={s.id} s={s} note="Idle" />
              ))}
            </Section>
          )}
        </div>
      )}
    </section>
  );
}
