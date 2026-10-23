// "Dismiss this question?": archiving an open ask dismisses it (no option,
// its default never applies, the session is told), so it asks first.
import { archiveThread } from '@/hooks/data';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { DISMISS_ASK } from '@/lib/model';
import { setUI, useUI } from '@/lib/ui-store';

export function DismissDialog() {
  const thread = useUI((s) => s.dismissing);
  const close = () => setUI({ dismissing: null });
  return (
    <Dialog open={!!thread} onOpenChange={(o) => !o && close()}>
      <DialogContent className="max-w-sm">
        <DialogHeader>
          <DialogTitle className="text-[16px]">Dismiss this question?</DialogTitle>
          <DialogDescription className="text-[13.5px]">
            <span className="font-medium text-foreground">{thread?.title}</span>
            <br />
            {DISMISS_ASK} To answer in your own words instead, write in the reply box and choose Answer with this.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" onClick={close}>
            Cancel
          </Button>
          <Button
            autoFocus
            variant="destructive"
            onClick={() => {
              const t = thread;
              close();
              if (t) archiveThread(t, true);
            }}
          >
            Dismiss
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
