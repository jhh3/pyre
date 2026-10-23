import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Kbd } from '@/components/ui/kbd';
import { setUI, useUI } from '@/lib/ui-store';

const GROUPS: [string, [string[], string][]][] = [
  ['Inbox', [[['J'], 'Next conversation'], [['K'], 'Previous conversation'], [['↵'], 'Open'], [['E'], 'Archive (dismiss an open question or a session item)'], [['Esc'], 'Back to the list'], [['G', 'I'], 'Go to Inbox'], [['G', 'D'], 'Go to Done'], [['G', 'P'], 'Go to Projects']]],
  ['Answering', [[['1', '–', '4'], 'Choose an option'], [['H'], 'Switch Send now / Hold & batch'], [['U'], 'Undo a held answer'], [['R'], 'Reply'], [['⌘', '↵'], 'Send reply'], [['Esc'], 'Leave the reply box']]],
];

export function HelpDialog() {
  const open = useUI((s) => s.helpOpen);
  return (
    <Dialog open={open} onOpenChange={(o) => setUI({ helpOpen: o })}>
      <DialogContent className="max-w-md gap-0 p-0">
        <DialogHeader className="border-b px-6 pt-5 pb-4">
          <DialogTitle className="text-[16px]">Keyboard shortcuts</DialogTitle>
          <DialogDescription className="text-[13px]">Press ? anywhere to see these again.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-6 px-6 py-5">
          {GROUPS.map(([title, rows]) => (
            <div key={title}>
              <h3 className="mb-2 text-[11.5px] font-semibold tracking-wider text-faint uppercase">{title}</h3>
              <ul className="space-y-1.5">
                {rows.map(([keys, label]) => (
                  <li key={label} className="flex items-center justify-between gap-4 text-[13.5px]">
                    <span>{label}</span>
                    <span className="flex gap-1">
                      {keys.map((k, i) => (k === '–' ? <span key={i} className="text-faint">–</span> : <Kbd key={i}>{k}</Kbd>))}
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
      </DialogContent>
    </Dialog>
  );
}
