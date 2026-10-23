import { Loader2 } from 'lucide-react';
import { useEffect } from 'react';
import { DismissDialog } from '@/components/DismissDialog';
import { HelpDialog } from '@/components/HelpDialog';
import { HeldBar } from '@/components/HeldBar';
import { InboxList } from '@/components/InboxList';
import { ProjectPage, ProjectsGrid } from '@/components/Projects';
import { RailNav } from '@/components/Rail';
import { NoThread, ThreadPane } from '@/components/ThreadView';
import { Sheet, SheetContent, SheetTitle } from '@/components/ui/sheet';
import { Toaster } from '@/components/ui/sonner';
import { useLive } from '@/hooks/data';
import { useKeyboard } from '@/hooks/keys';
import { useRoute } from '@/hooks/route';
import { setUI, useUI } from '@/lib/ui-store';
import { cn } from '@/lib/utils';

export default function App() {
  const route = useRoute();
  const live = useLive();
  const navOpen = useUI((s) => s.navOpen);
  const box = useUI((s) => s.box);
  const filter = useUI((s) => s.filter);
  useKeyboard();

  // The list a thread opens beside is the last list shown.
  useEffect(() => {
    if (route.name === 'inbox' || route.name === 'done') setUI({ box: route.name === 'done' ? 'done' : 'open', filter: route.filter });
  }, [route]);

  const listView = route.name === 'inbox' || route.name === 'done' || route.name === 'thread';
  const listBox = route.name === 'done' ? 'done' : route.name === 'inbox' ? 'open' : box;
  const listFilter = route.name === 'inbox' || route.name === 'done' ? route.filter : filter;

  return (
    <div className="flex h-dvh overflow-hidden bg-background">
      <aside className="hidden w-[244px] shrink-0 border-r bg-rail lg:block">
        <RailNav />
      </aside>
      <Sheet open={navOpen} onOpenChange={(o) => setUI({ navOpen: o })}>
        <SheetContent side="left" showCloseButton={false} onOpenAutoFocus={(e) => e.preventDefault()} className="w-[288px] max-w-[85vw] gap-0 bg-rail p-0 pt-[env(safe-area-inset-top)]">
          <SheetTitle className="sr-only">Navigation</SheetTitle>
          <RailNav onNavigate={() => setUI({ navOpen: false })} />
        </SheetContent>
      </Sheet>

      <div className="flex min-w-0 flex-1 flex-col">
        <main className="flex min-h-0 flex-1">
          {listView ? (
            <>
              <InboxList
                box={listBox}
                filter={listFilter}
                className={cn('w-full md:w-[360px] md:shrink-0 md:border-r xl:w-[400px]', route.name === 'thread' && 'hidden md:flex')}
              />
              {route.name === 'thread' ? <ThreadPane id={route.id} className="flex-1" /> : <NoThread className="hidden flex-1 md:flex" />}
            </>
          ) : route.name === 'projects' ? (
            <ProjectsGrid />
          ) : (
            <ProjectPage id={route.id} />
          )}
        </main>
        <HeldBar />
      </div>

      {live === 'reconnecting' && (
        <div
          role="status"
          className="fixed top-[max(env(safe-area-inset-top),10px)] left-1/2 z-50 inline-flex -translate-x-1/2 items-center gap-2 rounded-full border bg-popover px-3 py-1.5 text-[12.5px] font-medium text-muted-foreground shadow-float animate-rise"
        >
          <Loader2 className="size-3.5 animate-spin" /> Reconnecting…
        </div>
      )}
      <HelpDialog />
      <DismissDialog />
      <Toaster position="top-center" />
    </div>
  );
}
