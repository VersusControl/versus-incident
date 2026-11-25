/**
 * Clock times on the Kubernetes pages. A time from a day other than the
 * reader's today carries its date ("Nov 21, 23:37"), so yesterday's crash or
 * an outage that crossed midnight is never read as today's. The year is added
 * only when it differs too. Both "today" and the date are in the reader's
 * time zone.
 */
export function datedClockTime(at: Date, now: number, time: Intl.DateTimeFormatOptions): string {
  const today = new Date(now);
  const clock = at.toLocaleTimeString([], time);
  if (at.toDateString() === today.toDateString()) return clock;
  const date = at.toLocaleDateString([], { month: "short", day: "numeric", ...(at.getFullYear() !== today.getFullYear() ? { year: "numeric" } : {}) });
  return `${date}, ${clock}`;
}

/** Hours and minutes in the reader's locale. */
export const hourMinute: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit" };
/** A 24-hour clock with seconds, as break and change times are shown. */
export const fullClock: Intl.DateTimeFormatOptions = { hour12: false };

/**
 * `datedClockTime` for a timestamp from the API or a query: an ISO string or
 * epoch milliseconds. Anything that is not a time gives `undefined`, so each
 * caller keeps its own wording for a missing time.
 */
export function datedClock(value: string | number | undefined | null, time: Intl.DateTimeFormatOptions = hourMinute, now: number = Date.now()): string | undefined {
  if (value === undefined || value === null || value === "") return undefined;
  const at = new Date(value);
  if (!Number.isFinite(at.getTime())) return undefined;
  return datedClockTime(at, now, time);
}

/** Whether a time falls on a day other than the reader's today. */
export function notToday(value: string | number, now: number = Date.now()): boolean {
  const at = new Date(value);
  return Number.isFinite(at.getTime()) && at.toDateString() !== new Date(now).toDateString();
}

/** `datedClock` for a time that is expected to be valid, with a plain word when it is not. */
export function clockLabel(value: string | number | undefined | null, time: Intl.DateTimeFormatOptions = hourMinute, fallback = "unavailable"): string {
  return datedClock(value, time) ?? fallback;
}
