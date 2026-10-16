// Small shared pieces: dots, avatars, chips, markdown, empty/error states.
import { AlertTriangle, Clock, Pause, RotateCw } from 'lucide-react';
import type { ComponentProps, ReactNode } from 'react';
import ReactMarkdown from 'react-markdown';
import rehypeSanitize from 'rehype-sanitize';
import remarkBreaks from 'remark-breaks';
import remarkGfm from 'remark-gfm';
import { Button } from '@/components/ui/button';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { useNow } from '@/hooks/now';
import { optionLabel, type Tone } from '@/lib/model';
import * as T from '@/lib/time';
import type { Ask, HeldAnswer } from '@/lib/types';
import { cn } from '@/lib/utils';

const DOT: Record<Tone, string> = {
  working: 'bg-success',
  idle: 'bg-faint',
  attn: 'bg-warning',
  bad: 'bg-destructive',
  off: 'bg-border-strong',
};

export function Dot({ tone, pulse, className }: { tone: Tone; pulse?: boolean; className?: string }) {
  return (
    <span className={cn('relative inline-flex size-2 shrink-0 rounded-full', DOT[tone], className)} aria-hidden>
      {pulse && tone === 'working' && <span className={cn('absolute inset-0 rounded-full opacity-60 animate-ping', DOT[tone])} />}
    </span>
  );
}

function initials(name: string) {
  const words = name.replace(/[^\p{L}\p{N} ]/gu, ' ').trim().split(/\s+/).filter(Boolean);
  if (!words.length) return '?';
  return (words[0][0] + (words.length > 1 ? words[words.length - 1][0] : '')).toUpperCase();
}

function hue(name: string) {
  let x = 0;
  for (const c of name) x = (x * 31 + c.codePointAt(0)!) % 360;
  return x;
}

export function Avatar({ name, owner, className }: { name: string; owner?: boolean; className?: string }) {
  return (
    <span
      aria-hidden
      className={cn(
        'inline-flex size-8 shrink-0 items-center justify-center rounded-full text-[11px] font-semibold tracking-wide',
        owner ? 'bg-primary text-primary-foreground' : 'bg-[oklch(0.93_0.04_var(--h))] text-[oklch(0.42_0.1_var(--h))] dark:bg-[oklch(0.32_0.06_var(--h))] dark:text-[oklch(0.86_0.07_var(--h))]',
        className,
      )}
      style={{ ['--h' as string]: hue(name) }}
    >
      {owner ? (name.match(/\p{L}/u)?.[0] ?? 'Y').toUpperCase() : initials(name)}
    </span>
  );
}

type ChipTone = 'neutral' | 'warn' | 'brand' | 'ok' | 'bad';
const CHIP: Record<ChipTone, string> = {
  neutral: 'bg-muted text-muted-foreground',
  warn: 'bg-warning-soft text-warning',
  brand: 'bg-brand-soft text-brand-text',
  ok: 'bg-success-soft text-success',
  bad: 'bg-destructive-soft text-destructive',
};

export function Chip({ tone = 'neutral', icon, children, className, title }: { tone?: ChipTone; icon?: ReactNode; children: ReactNode; className?: string; title?: string }) {
  return (
    <span
      title={title}
      className={cn(
        'inline-flex max-w-full min-w-0 items-center gap-1 rounded-md px-1.5 py-0.5 text-[11.5px] leading-4 font-medium [&>svg]:size-3 [&>svg]:shrink-0',
        CHIP[tone],
        className,
      )}
    >
      {icon}
      <span className="truncate">{children}</span>
    </span>
  );
}

/** "Default in 23 min: Wait until Monday" / "Expires in 2 hr". */
export function deadlineText(ask: Ask) {
  const left = T.until(ask.deadline_at);
  const def = optionLabel(ask, ask.default_option);
  if (!ask.deadline_at) return null;
  if (!left) return def ? `Applying default: ${def}` : 'Expiring now';
  return def ? `Default in ${left}: ${def}` : `Expires in ${left}`;
}

export function DeadlineChip({ ask, className }: { ask: Ask; className?: string }) {
  useNow(15_000);
  const text = deadlineText(ask);
  if (!text || ask.state !== 'open') return null;
  const soon = T.minutesLeft(ask.deadline_at) < 60;
  return (
    <Chip tone={soon ? 'warn' : 'neutral'} icon={<Clock />} className={className} title={`Deadline ${T.full(ask.deadline_at)}`}>
      {text}
    </Chip>
  );
}

export function HeldChip({ held, sendsAt, ask }: { held: HeldAnswer; sendsAt?: string | null; ask?: Ask | null }) {
  const label = optionLabel(ask, held.option_id);
  return (
    <Chip tone="brand" icon={<Pause />} title={label ? `Held answer: ${label}` : 'Held answer'}>
      Held{sendsAt ? ` — sends ${T.clock(sendsAt)}` : ''}
    </Chip>
  );
}

export function Time({ at, mode = 'short', className }: { at: string; mode?: 'short' | 'long' | 'ago'; className?: string }) {
  useNow(mode === 'ago' ? 30_000 : 60_000);
  const text = mode === 'long' ? T.long(at) : mode === 'ago' ? T.ago(at) : T.short(at);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <time dateTime={at} className={cn('tabular-nums', className)}>
          {text}
        </time>
      </TooltipTrigger>
      <TooltipContent>{T.full(at)}</TooltipContent>
    </Tooltip>
  );
}

// Agents write plain text with single newlines (lists like "D1 …\nD2 …"),
// so remark-breaks keeps each line on its own line instead of one paragraph.
// react-markdown never renders raw HTML (no rehype-raw); rehype-sanitize
// is a second line of defence for URLs and attributes.
const mdComponents: ComponentProps<typeof ReactMarkdown>['components'] = {
  a: ({ node: _n, ...p }) => <a {...p} target="_blank" rel="noopener noreferrer" />,
  table: ({ node: _n, ...p }) => (
    <div className="table-wrap">
      <table {...p} />
    </div>
  ),
};

export function Markdown({ text, className }: { text: string; className?: string }) {
  return (
    <div className={cn('md', className)}>
      <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]} rehypePlugins={[rehypeSanitize]} components={mdComponents}>
        {text}
      </ReactMarkdown>
    </div>
  );
}

export function EmptyState({ icon, title, children, className }: { icon: ReactNode; title: string; children?: ReactNode; className?: string }) {
  return (
    <div className={cn('flex flex-col items-center justify-center px-6 py-16 text-center animate-rise', className)}>
      <div className="mb-4 flex size-12 items-center justify-center rounded-2xl border bg-card text-faint shadow-card [&>svg]:size-5">{icon}</div>
      <p className="text-[15px] font-semibold tracking-tight">{title}</p>
      {children && <div className="mt-1.5 max-w-[34ch] text-[13.5px] leading-relaxed text-muted-foreground">{children}</div>}
    </div>
  );
}

export function ErrorState({ title, error, onRetry, className }: { title: string; error: unknown; onRetry?: () => void; className?: string }) {
  return (
    <div className={cn('flex flex-col items-center px-6 py-14 text-center', className)}>
      <div className="mb-4 flex size-12 items-center justify-center rounded-2xl bg-destructive-soft text-destructive">
        <AlertTriangle className="size-5" />
      </div>
      <p className="text-[15px] font-semibold tracking-tight">{title}</p>
      <p className="mt-1.5 max-w-[38ch] text-[13.5px] text-muted-foreground">{(error as Error)?.message ?? 'Something went wrong.'}</p>
      {onRetry && (
        <Button variant="outline" size="sm" className="mt-4" onClick={onRetry}>
          <RotateCw /> Try again
        </Button>
      )}
    </div>
  );
}
