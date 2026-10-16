import { CheckCheck, Flame, Inbox, Keyboard, LayoutGrid, Monitor, Moon, Sun } from 'lucide-react';
import type { ReactNode } from 'react';
import { Dot } from '@/components/bits';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { useInbox, useMe, useProjects } from '@/hooks/data';
import { useRoute } from '@/hooks/route';
import { projectHref, projectTone } from '@/lib/model';
import { useTheme, type ThemePref } from '@/lib/theme';
import { setUI, useUI, visibleProjects } from '@/lib/ui-store';
import { cn } from '@/lib/utils';

function NavItem({ href, icon, label, count, active, onNavigate }: { href: string; icon: ReactNode; label: string; count?: number; active: boolean; onNavigate?: () => void }) {
  return (
    <a
      href={href}
      onClick={onNavigate}
      aria-current={active ? 'page' : undefined}
      className={cn(
        'group flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-[13.5px] font-medium transition-colors lg:h-8 [&>svg]:size-4 [&>svg]:shrink-0',
        active ? 'bg-card text-foreground shadow-card ring-1 ring-border' : 'text-muted-foreground hover:bg-accent hover:text-foreground',
      )}
    >
      {icon}
      <span className="flex-1 truncate">{label}</span>
      {!!count && (
        <span className={cn('min-w-5 rounded-full px-1.5 text-center text-[11.5px] font-semibold tabular-nums', active ? 'bg-primary text-primary-foreground' : 'text-foreground/80')}>
          {count}
        </span>
      )}
    </a>
  );
}

const THEMES: [ThemePref, typeof Sun, string][] = [
  ['system', Monitor, 'Match system'],
  ['light', Sun, 'Light'],
  ['dark', Moon, 'Dark'],
];

export function RailNav({ onNavigate }: { onNavigate?: () => void }) {
  const route = useRoute();
  const box = useUI((s) => s.box);
  const open = useInbox('open');
  const projects = useProjects();
  const me = useMe();
  const { pref, setTheme } = useTheme();
  const showInactive = useUI((s) => s.showInactive);
  const inThread = route.name === 'thread';
  const inboxActive = (route.name === 'inbox' && !route.filter.project && !route.filter.session) || (inThread && box === 'open');
  const activeProject = route.name === 'project' ? route.id : route.name === 'inbox' || route.name === 'done' ? route.filter.project : undefined;

  return (
    <nav className="flex h-full flex-col px-3 pt-4 pb-3" aria-label="Main">
      <div className="mb-5 flex items-center gap-2.5 px-2">
        <span className="flex size-7 items-center justify-center rounded-lg bg-primary text-primary-foreground shadow-card">
          <Flame className="size-4" strokeWidth={2.25} />
        </span>
        <span className="text-[15px] font-semibold tracking-tight">Pyre</span>
      </div>

      <div className="flex flex-col gap-0.5">
        <NavItem href="#/inbox" icon={<Inbox />} label="Inbox" count={open.data?.length} active={inboxActive} onNavigate={onNavigate} />
        <NavItem href="#/done" icon={<CheckCheck />} label="Done" active={route.name === 'done' || (inThread && box === 'done')} onNavigate={onNavigate} />
        <NavItem href="#/projects" icon={<LayoutGrid />} label="Projects" active={route.name === 'projects'} onNavigate={onNavigate} />
      </div>

      <div className="mt-6 mb-1.5 px-2.5 text-[11px] font-semibold tracking-wider text-faint uppercase">Projects</div>
      <div className="scroll-quiet -mx-1 flex min-h-0 flex-1 flex-col gap-0.5 overflow-y-auto px-1">
        {visibleProjects(projects.data ?? [], showInactive, activeProject).shown.map((p) => (
          <a
            key={p.project_id}
            href={projectHref(p.project_id)}
            onClick={onNavigate}
            className={cn(
              'flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-[13.5px] transition-colors lg:h-8',
              activeProject === p.project_id ? 'bg-card font-medium text-foreground shadow-card ring-1 ring-border' : 'text-muted-foreground hover:bg-accent hover:text-foreground',
            )}
          >
            <Dot tone={projectTone(p)} />
            <span className="flex-1 truncate">{p.name}</span>
            {p.open_asks > 0 && <span className="text-[11.5px] font-semibold text-warning tabular-nums">{p.open_asks}</span>}
          </a>
        ))}
        {projects.isSuccess && !projects.data.length && <p className="px-2.5 py-1 text-[13px] text-faint">No projects yet.</p>}
        <InactiveToggle all={projects.data ?? []} show={showInactive} className="mt-1 px-2.5" />
      </div>

      <div className="mt-3 flex items-center gap-1 border-t pt-3">
        <div className="flex rounded-lg bg-muted p-0.5" role="radiogroup" aria-label="Theme">
          {THEMES.map(([value, Icon, label]) => (
            <Tooltip key={value}>
              <TooltipTrigger asChild>
                <button
                  role="radio"
                  aria-checked={pref === value}
                  aria-label={label}
                  onClick={() => setTheme(value)}
                  className={cn(
                    'flex size-7 items-center justify-center rounded-md transition-colors',
                    pref === value ? 'bg-card text-foreground shadow-card' : 'text-faint hover:text-foreground',
                  )}
                >
                  <Icon className="size-3.5" />
                </button>
              </TooltipTrigger>
              <TooltipContent>{label}</TooltipContent>
            </Tooltip>
          ))}
        </div>
        <div className="flex-1" />
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              aria-label="Keyboard shortcuts"
              onClick={() => setUI({ helpOpen: true, navOpen: false })}
              className="hidden size-8 items-center justify-center rounded-md text-faint transition-colors hover:bg-accent hover:text-foreground lg:flex"
            >
              <Keyboard className="size-4" />
            </button>
          </TooltipTrigger>
          <TooltipContent>Keyboard shortcuts (?)</TooltipContent>
        </Tooltip>
      </div>
      {me.data?.email && <p className="mt-2 truncate px-1 text-[11.5px] text-faint" title={me.data.email}>{me.data.email}</p>}
    </nav>
  );
}

// InactiveToggle shows or hides projects with no live sessions and no open
// asks (remembered per browser).
export function InactiveToggle({ all, show, className }: { all: { project_id: string; active: boolean }[]; show: boolean; className?: string }) {
  const { inactive } = visibleProjects(all, show);
  if (!inactive) return null;
  return (
    <button
      type="button"
      onClick={() => setUI({ showInactive: !show })}
      className={cn('h-8 text-left text-[12.5px] text-faint transition-colors hover:text-foreground', className)}
    >
      {show ? 'Hide inactive' : `Show ${inactive} inactive`}
    </button>
  );
}
