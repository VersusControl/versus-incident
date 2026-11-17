import clsx from "clsx";
import { Check, ShieldCheck, X } from "lucide-react";
import type { AwsSettings } from "@/lib/api";
import { aiModelLine, privacyLabel, selfTestLine } from "@/lib/awsSettings";

// AwsPrivacyCard — the plain-language half of the screen: what leaves for the
// model, what never does, which tag values are shared, and the last leak check.
// It renders identically for admins and viewers because it is read-only.
export function AwsPrivacyCard({
  privacy,
  now = new Date(),
}: {
  privacy: AwsSettings["privacy"];
  now?: Date;
}) {
  const selfTest = selfTestLine(privacy.self_test, now);
  const passing = privacy.self_test.passing;

  return (
    <section className="card min-w-0" aria-labelledby="aws-privacy-title">
      <div className="card-header">
        <h2 id="aws-privacy-title" className="card-title">
          Privacy
        </h2>
        <span className="min-w-0 truncate text-2xs text-ink-300">
          {aiModelLine(privacy)}
        </span>
      </div>
      <div className="card-body space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="min-w-0">
            <h3 className="text-2xs uppercase tracking-wider text-ink-300">
              Sent to AI
            </h3>
            {privacy.sent.length === 0 ? (
              <p className="mt-1 text-xs text-ink-300">Nothing is sent.</p>
            ) : (
              <ul className="mt-1 space-y-1 text-xs text-ink-100">
                {privacy.sent.map((code) => (
                  <li key={code} className="flex items-start gap-1.5">
                    <Check
                      size={13}
                      aria-hidden="true"
                      className="mt-0.5 shrink-0 text-sev-ok"
                    />
                    <span className="min-w-0">{privacyLabel(code)}</span>
                  </li>
                ))}
              </ul>
            )}
          </div>
          <div className="min-w-0">
            <h3 className="text-2xs uppercase tracking-wider text-ink-300">
              Never sent
            </h3>
            {privacy.never_sent.length === 0 ? (
              <p className="mt-1 text-xs text-ink-300">Every category above.</p>
            ) : (
              <ul className="mt-1 space-y-1 text-xs text-ink-100">
                {privacy.never_sent.map((code) => (
                  <li key={code} className="flex items-start gap-1.5">
                    <X
                      size={13}
                      aria-hidden="true"
                      className="mt-0.5 shrink-0 text-ink-400"
                    />
                    <span className="min-w-0">{privacyLabel(code)}</span>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>

        <div className="min-w-0">
          <h3 className="text-2xs uppercase tracking-wider text-ink-300">
            Tag values shared
          </h3>
          {privacy.tag_value_allowlist.length === 0 ? (
            <p className="mt-1 text-xs text-ink-300">No tag values are shared.</p>
          ) : (
            <ul aria-label="Tag value allowlist" className="mt-1 flex flex-wrap gap-1">
              {privacy.tag_value_allowlist.map((tag) => (
                <li key={tag}>
                  <span className="pill font-mono">{tag}</span>
                </li>
              ))}
            </ul>
          )}
          <p className="mt-1 text-2xs text-ink-300">
            Only these tag keys share their values; every other tag value stays
            in your account.
          </p>
        </div>

        <p
          className={clsx(
            "flex items-center gap-1.5 text-2xs font-medium",
            passing ? "text-sev-ok" : "text-sev-critical",
          )}
        >
          <ShieldCheck size={13} aria-hidden="true" />
          {selfTest}
        </p>
      </div>
    </section>
  );
}
