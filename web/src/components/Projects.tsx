import { ArrowUpRight, ChevronLeft, FileText, FolderOpen, Inbox, NotebookPen } from 'lucide-react';
import type { ReactNode } from 'react';
import { Avatar, Chip, Dot, EmptyState, ErrorState, Markdown, Time, Watching } from '@/components/bits';
import { MenuButton } from '@/components/InboxList';
import { ProjectStatusCard } from '@/components/ProjectStatus';
import { Skeleton } from '@/components/ui/skeleton';
import { revokeGrant, useProject, useProjects } from '@/hooks/data';
import { Button } from '@/components/ui/button';
import { listHref } from '@/hooks/route';
import { askThreadId, outcome, projectHref, projectTone, sessionState, threadHref } from '@/lib/model';
import * as T from '@/lib/time';
import type { ProjectCard, SessionCard, StandingGrant } from '@/lib/types';
import { cn } from '@/lib/utils';
import { InactiveToggle } from '@/components/Rail';
import { useUI, visibleProjects } from '@/lib/ui-store';

function Card({ p }: { p: ProjectCard }) {
  const leads = p.leads ?? [];
  return (
    <a
      href={projectHref(p.project_id)}
      className="group flex min-w-0 flex-col rounded-2xl border bg-card p-5 shadow-card transition-[border-color,box-shadow,transform] duration-150 hover:border-border-strong hover:shadow-float active:scale-[0.995]"
    >
      <div className="flex items-center gap-2.5">
        <Dot tone={projectTone(p)} />
        <h3 className="min-w-0 flex-1 truncate text-[15.5px] font-semibold tracking-[-0.01em]">{p.name}</h3>
        {(p.waiting || p.open_asks > 0) && (
          <Chip tone="warn">{p.open_asks > 0 ? `${p.open_asks} waiting on you` : 'Needs you'}</Chip>
        )}
        {p.stale && (
          <Chip tone="neutral" title="Work is running but the lead hasn't updated the page lately">
            Stale
          </Chip>
        )}
      </div>
      {p.description && <p className="mt-1 truncate pl-[18px] text-[13px] text-muted-foreground">{p.description}</p>}
      {p.progress != null && (
        <div className="mt-3 flex items-center gap-2 pl-[18px]">
          <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
            <div className="h-full rounded-full bg-primary" style={{ width: `${p.progress}%` }} />
          </div>
          <span className="text-[12px] font-semibold text-muted-foreground tabular-nums">{p.progress}%</span>
        </div>
      )}
      {p.phase && <p className="mt-2 truncate pl-[18px] text-[13px] font-medium text-foreground/85">{p.phase}</p>}
      <p className={cn('mt-3.5 line-clamp-3 flex-1 text-[13.5px] leading-relaxed', p.page_summary ? 'text-foreground/80' : 'text-faint italic')}>
        {p.page_summary || "The lead hasn't written this page yet."}
      </p>
      <div className="mt-4 flex items-center gap-2 border-t pt-3.5 text-[12.5px] text-muted-foreground">
        {leads.length ? (
          <>
            <span className="flex -space-x-1.5">
              {leads.slice(0, 3).map((l) => (
                <Avatar key={l.id} name={l.title} className="size-6 text-[9.5px] ring-2 ring-card" />
              ))}
            </span>
            <span className="min-w-0 truncate">
              {leads[0].title}
              <span className="text-faint"> · {sessionState(leads[0].state).label}</span>
            </span>
          </>
        ) : (
          <span className="text-faint">No lead</span>
        )}
        <span className="ml-auto shrink-0 text-faint">Active {T.ago(p.last_activity_at)}</span>
      </div>
    </a>
  );
}

export function ProjectsGrid() {
  const q = useProjects();
  const showInactive = useUI((s) => s.showInactive);
  const all = q.data ?? [];
  const projects = visibleProjects(all, showInactive).shown;
  return (
    <div className="scroll-quiet min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto max-w-[1180px] px-4 pt-[max(env(safe-area-inset-top),12px)] pb-12 md:px-8 md:pt-7">
        <header className="mb-5 flex h-10 items-center gap-1 md:mb-7">
          <MenuButton />
          <h1 className="text-[19px] font-semibold tracking-[-0.018em] md:text-[22px]">Projects</h1>
          {q.isSuccess && <span className="ml-2 text-[13px] text-faint tabular-nums">{projects.length}</span>}
          <InactiveToggle all={all} show={showInactive} className="ml-auto px-2" />
        </header>
        {q.isPending ? (
          <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
            {Array.from({ length: 6 }, (_, i) => (
              <Skeleton key={i} className="h-52 rounded-2xl" />
            ))}
          </div>
        ) : q.isError ? (
          <ErrorState title="Couldn't load projects" error={q.error} onRetry={() => q.refetch()} />
        ) : !all.length ? (
          <EmptyState icon={<FolderOpen />} title="No projects yet">
            Projects group sessions around a goal. Create one with <code className="text-[12.5px]">pyre project new</code>.
          </EmptyState>
        ) : (
          <div className="grid grid-cols-[minmax(0,1fr)] gap-4 sm:grid-cols-2 xl:grid-cols-3">
            {projects.map((p) => (
              <Card key={p.project_id} p={p} />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function Side({ title, count, children, className }: { title: string; count?: number; children: ReactNode; className?: string }) {
  return (
    <section className={cn('rounded-2xl border bg-card p-4 shadow-card', className)}>
      <h2 className="mb-2.5 flex items-center gap-2 px-1 text-[12px] font-semibold tracking-wider text-faint uppercase">
        {title}
        {!!count && <span className="tabular-nums">· {count}</span>}
      </h2>
      {children}
    </section>
  );
}

function SessionRow({ s }: { s: SessionCard }) {
  const st = sessionState(s.state);
  return (
    <li className="rounded-xl px-1 py-2">
      <div className="flex items-center gap-2">
        <Dot tone={st.tone} pulse />
        <span className="min-w-0 truncate text-[13.5px] font-medium">{s.title}</span>
        {s.is_lead && <span className="rounded bg-brand-soft px-1.5 text-[10.5px] leading-4 font-semibold text-brand-text">Lead</span>}
        <Watching titles={s.watching} />
        <span className="ml-auto shrink-0 text-[12px] text-faint">{st.label}</span>
      </div>
      {s.last_activity && (
        <p className="mt-0.5 line-clamp-2 pl-4 text-[12.5px] leading-snug text-muted-foreground">
          {s.last_activity.line} <span className="text-faint">· {T.ago(s.last_activity.at)}</span>
        </p>
      )}
    </li>
  );
}

/** Where a grant came from: the owner's own words only, else "approved". */
function grantSource(g: StandingGrant) {
  if (g.ask_id && g.quote) return <>You: “{g.quote}”</>;
  if (g.ask_id) return <>You approved a proposal (no words)</>;
  if (g.quote) return <>Set by you: “{g.quote}”</>;
  return <>Set by you</>;
}

function GrantRow({ g, projectId }: { g: StandingGrant; projectId: string }) {
  return (
    <li className="rounded-xl px-1 py-2">
      <div className="flex items-start gap-2">
        <span className="min-w-0 flex-1 text-[13.5px] leading-snug">
          <span className="font-medium">{g.action}</span>
          <span className="text-muted-foreground"> → “{g.option}”</span>
        </span>
        <Button
          variant="outline"
          size="xs"
          aria-label={`Revoke the ${g.action} go-ahead`}
          onClick={() => {
            if (confirm(`Revoke the standing go-ahead "${g.action} → ${g.option}"? Asks for it will reach you again.`)) revokeGrant(g.grant_id, projectId);
          }}
        >
          Revoke
        </Button>
      </div>
      <p className="mt-0.5 line-clamp-2 text-[12.5px] leading-snug text-muted-foreground">{grantSource(g)}</p>
      {g.condition && <p className="mt-0.5 line-clamp-2 text-[12px] leading-snug text-faint">Note (not checked): {g.condition}</p>}
      <p className="mt-0.5 text-[12px] text-faint">
        {g.scope === 'session' ? 'A session tree' : 'Whole project'} · expires {T.short(g.expires_at)}
        {g.used_count > 0 && <> · used {g.used_count}×</>}
      </p>
    </li>
  );
}

export function ProjectPage({ id }: { id: string }) {
  const q = useProject(id);
  const cards = useProjects().data;
  const card = cards?.find((c) => c.project_id === id);
  const d = q.data;
  const asks = d?.asks ?? [];
  const open = asks.filter((a) => a.state === 'open');
  const decided = asks.filter((a) => a.state !== 'open');
  const sessions = d?.sessions ?? [];
  const artifacts = d?.artifacts ?? [];
  const grants = d?.grants ?? [];
  const page = d?.page;
  const author = page?.revision?.author;
  const updated = page?.revision?.created_at ?? page?.document?.updated_at;

  return (
    <div className="scroll-quiet min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto max-w-[1180px] px-4 pt-[max(env(safe-area-inset-top),8px)] pb-14 md:px-8 md:pt-6">
        <div className="flex h-12 items-center">
          <a href="#/projects" className="-ml-2 inline-flex h-10 items-center gap-0.5 rounded-lg pr-3 pl-1.5 text-[14px] font-medium text-brand-text transition-colors hover:bg-accent md:text-[13px] md:text-muted-foreground md:hover:text-foreground">
            <ChevronLeft className="size-5 md:size-4" /> Projects
          </a>
        </div>
        {q.isPending ? (
          <div className="mt-4 space-y-4">
            <Skeleton className="h-8 w-72" />
            <Skeleton className="h-4 w-96 max-w-full" />
            <Skeleton className="mt-6 h-96 w-full rounded-2xl" />
          </div>
        ) : q.isError ? (
          <ErrorState title="Couldn't load this project" error={q.error} onRetry={() => q.refetch()} />
        ) : d ? (
          <>
            <header className="mt-1 mb-7">
              <div className="flex items-center gap-3">
                {card && <Dot tone={projectTone(card)} className="size-2.5" />}
                <h1 className="text-[24px] leading-tight font-semibold tracking-[-0.024em] md:text-[28px]">{d.project.name}</h1>
              </div>
              {d.project.description && <p className="mt-1.5 text-[14.5px] text-muted-foreground">{d.project.description}</p>}
              <div className="mt-4 flex flex-wrap items-center gap-2">
                {open.length > 0 && <Chip tone="warn">{open.length} waiting on you</Chip>}
                <a
                  href={listHref('open', { project: id })}
                  className="inline-flex h-7 items-center gap-1.5 rounded-md border bg-card px-2.5 text-[12.5px] font-medium text-foreground/80 shadow-card transition-colors hover:border-border-strong hover:text-foreground"
                >
                  <Inbox className="size-3.5 text-faint" /> Inbox for this project
                </a>
              </div>
            </header>

            {d.status && <ProjectStatusCard status={d.status} openAsks={open} />}

            <div className="grid grid-cols-[minmax(0,1fr)] gap-5 lg:grid-cols-[minmax(0,1fr)_330px] lg:grid-rows-[auto_1fr] lg:gap-7">

              <article className="min-w-0 rounded-2xl border bg-card shadow-card lg:col-start-1 lg:row-span-2 lg:row-start-1">
                {page?.content ? (
                  <>
                    <div className="flex items-center gap-2 border-b px-5 py-3 text-[12.5px] text-muted-foreground md:px-10">
                      <NotebookPen className="size-3.5 text-faint" />
                      <span className="truncate">
                        Updated {updated ? <Time at={updated} mode="ago" /> : 'recently'}
                        {author?.name ? (
                          <>
                            {' '}by <span className="font-medium text-foreground/80">{author.name}</span>
                          </>
                        ) : null}
                      </span>
                    </div>
                    <Markdown text={page.content} className="md-page px-5 py-6 md:px-10 md:py-9" />
                  </>
                ) : (
                  <EmptyState icon={<NotebookPen />} title="The lead hasn't written this page yet." className="py-20">
                    Leads keep this page current: the goal, status, results, open decisions and what's next.
                  </EmptyState>
                )}
              </article>

              <div className="flex min-w-0 flex-col gap-5 lg:col-start-2 lg:row-span-2 lg:row-start-1">
                <Side title="Sessions" count={sessions.length}>
                  {sessions.length ? (
                    <ul className="divide-y divide-border">
                      {sessions.map((s) => (
                        <SessionRow key={s.id} s={s} />
                      ))}
                    </ul>
                  ) : (
                    <p className="px-1 text-[13px] text-faint">No live sessions.</p>
                  )}
                </Side>
                {grants.length > 0 && (
                  <Side title="Standing go-aheads" count={grants.length}>
                    <ul className="divide-y divide-border">
                      {grants.map((g) => (
                        <GrantRow key={g.grant_id} g={g} projectId={id} />
                      ))}
                    </ul>
                  </Side>
                )}
                {decided.length > 0 && (
                  <Side title="Recent decisions">
                    <ul className="flex flex-col">
                      {decided.map((a) => {
                        const o = outcome(a);
                        return (
                          <li key={a.ask_id}>
                            <a href={threadHref(askThreadId(a))} className="block rounded-lg px-1 py-2 transition-colors hover:bg-accent/60">
                              <span className="block text-[13.5px] leading-snug font-medium">{a.title}</span>
                              <span className="mt-0.5 block truncate text-[12.5px] text-muted-foreground">
                                <span className={cn(o.tone === 'ok' && 'text-success')}>{o.text}</span>
                                <span className="text-faint"> · {T.short(a.answer?.at ?? a.updated_at)}</span>
                              </span>
                            </a>
                          </li>
                        );
                      })}
                    </ul>
                  </Side>
                )}
                {artifacts.length > 0 && (
                  <Side title="Artifacts">
                    <ul className="flex flex-col">
                      {artifacts.map((a) => (
                        <li key={a.id}>
                          <a
                            href={a.url}
                            target="_blank"
                            rel="noopener noreferrer"
                            className="group flex items-start gap-2.5 rounded-lg px-1 py-2 transition-colors hover:bg-accent/60"
                          >
                            <FileText className="mt-0.5 size-4 shrink-0 text-faint" />
                            <span className="min-w-0 flex-1">
                              <span className="block text-[13.5px] leading-snug font-medium">{a.title}</span>
                              <span className="block text-[12px] text-faint">{T.ago(a.updated_at)}</span>
                            </span>
                            <ArrowUpRight className="mt-0.5 size-3.5 shrink-0 text-faint opacity-0 transition-opacity group-hover:opacity-100" />
                          </a>
                        </li>
                      ))}
                    </ul>
                  </Side>
                )}
              </div>
            </div>
          </>
        ) : null}
      </div>
    </div>
  );
}
