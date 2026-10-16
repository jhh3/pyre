// Desktop keyboard shortcuts (see HelpDialog for the list).
import { useEffect } from 'react';
import { stepThread } from '@/components/ThreadView';
import { archiveThread, choose, currentList, queryClient, undoHeld } from './data';
import { getRoute, listHref, navigate } from './route';
import { threadHref } from '@/lib/model';
import type { ThreadResponse } from '@/lib/types';
import { getUI, setUI } from '@/lib/ui-store';

function typing(t: EventTarget | null) {
  const el = t as HTMLElement | null;
  return !!el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable);
}

export function useKeyboard() {
  useEffect(() => {
    let g = 0;
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || typing(e.target)) return;
      const ui = getUI();
      if (ui.helpOpen || ui.navOpen) return;
      const route = getRoute();
      const k = e.key;

      if (g && Date.now() - g < 1200) {
        g = 0;
        const to = k === 'i' ? '#/inbox' : k === 'd' ? '#/done' : k === 'p' ? '#/projects' : null;
        if (to) {
          e.preventDefault();
          navigate(to);
        }
        return;
      }
      if (k === 'g') {
        g = Date.now();
        return;
      }
      if (k === '?') {
        e.preventDefault();
        setUI({ helpOpen: true });
        return;
      }

      if (route.name === 'inbox' || route.name === 'done') {
        const list = currentList();
        const idx = list.findIndex((t) => t.thread_id === ui.cursor);
        if (k === 'j' || k === 'ArrowDown' || k === 'k' || k === 'ArrowUp') {
          e.preventDefault();
          const d = k === 'j' || k === 'ArrowDown' ? 1 : -1;
          const next = list[Math.max(0, Math.min(list.length - 1, idx < 0 ? 0 : idx + d))];
          if (next) setUI({ cursor: next.thread_id });
        } else if (k === 'e' && route.name === 'inbox') {
          const t = list[idx];
          if (t) {
            e.preventDefault();
            archiveThread(t);
          }
        } else if (k === 'Enter' || k === 'o') {
          const t = list[idx] ?? list[0];
          if (t) {
            e.preventDefault();
            navigate(threadHref(t.thread_id));
          }
        }
        return;
      }

      if (route.name === 'thread') {
        const data = queryClient.getQueryData<ThreadResponse>(['thread', route.id]);
        const ask = data?.thread.ask;
        if (k === 'j' || k === 'k') {
          e.preventDefault();
          stepThread(k === 'j' ? 1 : -1, route.id);
        } else if (k === 'Escape') {
          e.preventDefault();
          navigate(listHref(ui.box, ui.filter));
        } else if (k === 'e' && data) {
          e.preventDefault();
          archiveThread(data.thread);
        } else if (k === 'r') {
          const el = document.getElementById('composer');
          if (el) {
            e.preventDefault();
            el.focus();
          }
        } else if (k === 'h') {
          setUI({ mode: ui.mode === 'now' ? 'hold' : 'now' });
        } else if (k === 'u' && ask) {
          undoHeld(ask.ask_id, data!.thread.thread_id);
        } else if (/^[1-9]$/.test(k) && ask?.state === 'open' && data) {
          const opt = ask.options[Number(k) - 1];
          if (opt && ui.answering[ask.ask_id]?.phase !== 'sending') {
            e.preventDefault();
            choose(data.thread, opt.id, ui.mode);
          }
        }
        return;
      }

      if (route.name === 'project' && (k === 'Escape' || k === 'e')) navigate('#/projects');
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
}
