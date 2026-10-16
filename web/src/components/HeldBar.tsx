// The persistent held-answers bar: visible whenever the hub holds any
// answers, with a live countdown, the list, per-item undo and Send now.
import { ChevronUp, Loader2, Pause, Undo2 } from 'lucide-react';
import { useState } from 'react';
import { sendHeldNow, undoHeld, useHeld, useInbox } from '@/hooks/data';
import { useNow } from '@/hooks/now';
import { heldItems, optionLabel, threadHref } from '@/lib/model';
import * as T from '@/lib/time';
import { setUI, useUI } from '@/lib/ui-store';
import { cn } from '@/lib/utils';

export function HeldBar() {
  const held = useHeld();
  const items = heldItems(held);
  const open = useUI((s) => s.heldOpen);
  const threads = useInbox('open').data ?? [];
  const [sending, setSending] = useState(false);
  useNow(1000);
  if (!items.length) return null;

  const n = items.length;
  const find = (askId: string) => threads.find((t) => t.ask?.ask_id === askId);
  const send = async () => {
    setSending(true);
    try {
      await sendHeldNow();
    } finally {
      setSending(false);
    }
  };

  return (
    <div className="shrink-0 px-2 pt-2 pb-[max(env(safe-area-inset-bottom),8px)] md:px-4 md:pb-3" role="region" aria-label="Held answers">
      <div className="mx-auto max-w-[880px] overflow-hidden rounded-2xl bg-inverse text-inverse-foreground shadow-float animate-rise">
        {open && (
          <ul className="max-h-[40vh] overflow-y-auto border-b border-inverse-foreground/10 px-2 py-2">
            {items.map((i) => {
              const t = find(i.ask_id);
              return (
                <li key={i.ask_id} className="flex items-center gap-3 rounded-xl px-2.5 py-2 hover:bg-inverse-foreground/[0.06]">
                  <a href={t ? threadHref(t.thread_id) : undefined} className="min-w-0 flex-1" onClick={() => setUI({ heldOpen: false })}>
                    <span className="block truncate text-[13.5px] font-medium">{t?.title ?? 'Question'}</span>
                    <span className="block truncate text-[12.5px] opacity-65">
                      {t?.project?.name ? t.project.name + ' · ' : ''}
                      {optionLabel(t?.ask, i.option_id) || i.text}
                    </span>
                  </a>
                  <button
                    onClick={() => undoHeld(i.ask_id, t?.thread_id)}
                    className="inline-flex h-8 shrink-0 items-center gap-1.5 rounded-lg px-2.5 text-[12.5px] font-medium opacity-80 transition hover:bg-inverse-foreground/10 hover:opacity-100"
                  >
                    <Undo2 className="size-3.5" /> Undo
                  </button>
                </li>
              );
            })}
          </ul>
        )}
        <div className="flex items-center gap-2 py-2 pr-2 pl-3 md:gap-3 md:pl-4">
          <button
            onClick={() => setUI({ heldOpen: !open })}
            aria-expanded={open}
            className="flex min-w-0 flex-1 items-center gap-3 rounded-xl py-1 text-left"
          >
            <span className="flex size-8 shrink-0 items-center justify-center rounded-full bg-inverse-foreground/12">
              <Pause className="size-3.5" strokeWidth={2.5} />
            </span>
            <span className="min-w-0">
              <span className="block text-[14px] leading-5 font-semibold">
                {n} answer{n === 1 ? '' : 's'} held
              </span>
              <span className="block truncate text-[12.5px] leading-4 opacity-70">
                {held.sends_at ? (
                  <>
                    Sending at {T.clock(held.sends_at)} <span className="tabular-nums">({T.countdown(held.sends_at)})</span>
                    <span className="hidden sm:inline"> unless you hold more</span>
                  </>
                ) : (
                  'Waiting to send'
                )}
              </span>
            </span>
            <ChevronUp className={cn('size-4 shrink-0 opacity-60 transition-transform', open && 'rotate-180')} />
          </button>
          <button
            onClick={send}
            disabled={sending}
            className="inline-flex h-10 shrink-0 items-center gap-1.5 rounded-xl bg-primary px-4 text-[13.5px] font-semibold text-primary-foreground shadow-card transition hover:brightness-110 active:scale-[0.98] disabled:opacity-70 md:h-9"
          >
            {sending && <Loader2 className="size-4 animate-spin" />}
            Send now
          </button>
        </div>
      </div>
    </div>
  );
}
