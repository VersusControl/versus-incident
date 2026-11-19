import {
  accountName,
  fmtClockOrDay,
  gapReasonText,
  type ChangesView,
  type CompareResource,
  type CompareView,
} from "@/lib/awsChanges";
import { serviceLabel } from "@/lib/awsSettings";

// Compare wording kept out of the component so it can be read and tested on
// its own: ID hints without repeats, and left-out groups named with why.

/**
 * The part of the server's `id_hint` the line does not already print: the
 * hint may carry the account, the Region and an ID equal to the name.
 */
export function idHintText(r: CompareResource): string {
  if (!r.id_hint) return "";
  const said = new Set([r.account, r.region, r.name]);
  return r.id_hint
    .split(/\s+/)
    .filter((t) => t && !said.has(t))
    .join(" ");
}

/** Words for a known-unreadable service, as Settings names it. */
const EXCLUSION_TEXT: Record<string, string> = {
  service_denied: "not readable (permission)",
  not_on_plan: "not on this support plan",
  not_available_in_region: "not offered in this Region",
};

/**
 * The left-out groups, one line each. Known-unreadable services name the
 * service and why, once per account (they are the same in every Region);
 * the account leads only when several are in scope.
 */
export function partialLines(c: Pick<CompareView, "partial">, view: ChangesView): Array<{ key: string; text: string; excluded: boolean }> {
  const multi = view.accounts.length > 1;
  const seen = new Set<string>();
  const out: Array<{ key: string; text: string; excluded: boolean }> = [];
  for (const p of c.partial) {
    const known = p.reason ? EXCLUSION_TEXT[p.reason] : undefined;
    if (p.excluded || known) {
      const key = `x-${p.account}-${p.service}`;
      if (seen.has(key)) continue;
      seen.add(key);
      const why = known ?? gapReasonText(p.reason);
      out.push({ key, excluded: true, text: `${multi ? `${accountName(view, p.account)} ` : ""}${serviceLabel(p.service)}: ${why}` });
      continue;
    }
    const key = `${p.account}-${p.region}-${p.service}`;
    out.push({
      key,
      excluded: false,
      text: `${accountName(view, p.account)} ${p.region} ${serviceLabel(p.service)}: ${[
        p.reason ? gapReasonText(p.reason) : null,
        p.history_starts ? `not available before ${fmtClockOrDay(p.history_starts, view.window.until)}` : "no history yet",
      ]
        .filter(Boolean)
        .join("; ")}`,
    });
  }
  return out;
}
