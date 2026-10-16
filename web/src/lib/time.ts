// All times are shown in Pacific time (the owner's zone), whatever the
// browser's zone is.
const TZ = 'America/Los_Angeles';
const fClock = new Intl.DateTimeFormat('en-US', { timeZone: TZ, hour: 'numeric', minute: '2-digit' });
const fDay = new Intl.DateTimeFormat('en-CA', { timeZone: TZ, year: 'numeric', month: '2-digit', day: '2-digit' });
const fMonthDay = new Intl.DateTimeFormat('en-US', { timeZone: TZ, month: 'short', day: 'numeric' });
const fYear = new Intl.DateTimeFormat('en-US', { timeZone: TZ, year: 'numeric' });
const fFull = new Intl.DateTimeFormat('en-US', { timeZone: TZ, month: 'short', day: 'numeric', year: 'numeric' });
const fWeekday = new Intl.DateTimeFormat('en-US', { timeZone: TZ, weekday: 'short' });
const fTitle = new Intl.DateTimeFormat('en-US', {
  timeZone: TZ, weekday: 'long', month: 'long', day: 'numeric', year: 'numeric',
  hour: 'numeric', minute: '2-digit', timeZoneName: 'short',
});

const DAY = 86400000;
type T = string | number | Date | null | undefined;

const toDate = (v: T): Date | null => (v == null || v === '' ? null : v instanceof Date ? v : new Date(v));
export const dayKey = (v: T) => fDay.format(toDate(v) ?? new Date());
export const isToday = (v: T) => dayKey(v) === dayKey(new Date());
const isYesterday = (v: T) => dayKey(v) === dayKey(new Date(Date.now() - DAY));

/** "9:41 AM" */
export function clock(v: T) {
  const d = toDate(v);
  return d ? fClock.format(d) : '';
}

/** Compact list time: "9:41 AM", "Yesterday", "Tue", "Oct 12", "Oct 12, 2025". */
export function short(v: T) {
  const d = toDate(v);
  if (!d) return '';
  if (isToday(d)) return clock(d);
  if (isYesterday(d)) return 'Yesterday';
  const age = Date.now() - d.getTime();
  if (age > 0 && age < 6 * DAY) return fWeekday.format(d);
  if (fYear.format(d) === fYear.format(new Date())) return fMonthDay.format(d);
  return fFull.format(d);
}

/** "9:41 AM", "Yesterday 4:10 PM", "Oct 12, 4:10 PM". */
export function long(v: T) {
  const d = toDate(v);
  if (!d) return '';
  if (isToday(d)) return clock(d);
  if (isYesterday(d)) return 'Yesterday ' + clock(d);
  return fMonthDay.format(d) + ', ' + clock(d);
}

/** Tooltip text with the zone spelled out. */
export const full = (v: T) => {
  const d = toDate(v);
  return d ? fTitle.format(d) : '';
};

/** "just now", "5 min ago", "2h ago", "3d ago", then short(). */
export function ago(v: T) {
  const d = toDate(v);
  if (!d) return '';
  const s = (Date.now() - d.getTime()) / 1000;
  if (s < 45) return 'just now';
  if (s < 3600) return Math.round(s / 60) + ' min ago';
  if (s < 86400) return Math.round(s / 3600) + 'h ago';
  if (s < 7 * 86400) return Math.round(s / 86400) + 'd ago';
  return short(d);
}

/** Time left: "45 sec", "23 min", "1 hr 5 min", "2 days"; null once past. */
export function until(v: T) {
  const d = toDate(v);
  if (!d) return null;
  const s = Math.round((d.getTime() - Date.now()) / 1000);
  if (s <= 0) return null;
  if (s < 60) return s + ' sec';
  const m = Math.round(s / 60);
  if (m < 60) return m + ' min';
  const h = Math.floor(m / 60);
  if (h < 24) return h + ' hr' + (m % 60 ? ' ' + (m % 60) + ' min' : '');
  const days = Math.round(h / 24);
  return days + (days === 1 ? ' day' : ' days');
}

export const minutesLeft = (v: T) => {
  const d = toDate(v);
  return d ? (d.getTime() - Date.now()) / 60000 : Infinity;
};

/** "4:07" countdown; "0:00" once past. */
export function countdown(v: T) {
  const d = toDate(v);
  if (!d) return '';
  const s = Math.max(0, Math.round((d.getTime() - Date.now()) / 1000));
  return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
}
