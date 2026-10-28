import { useEffect, useRef } from "react";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { Lock } from "lucide-react";
import clsx from "clsx";
import { TopBar } from "@/components/TopBar";
import { ReadOnlyNotice } from "@/components/settings/ReadOnlyValues";
import {
  groupSections,
  resolveSection,
  type LegacyMaps,
  type SettingsSectionDef,
} from "@/components/settings/sections";

interface SettingsLayoutProps {
  title: string;
  subtitle?: string;
  navLabel: string;
  sections: readonly SettingsSectionDef[];
  legacy?: LegacyMaps;
  // headerActions renders in the top bar (license / role chips).
  headerActions?: React.ReactNode;
  // lockedEnterprise marks Enterprise sections with a lock in the nav.
  lockedEnterprise?: boolean;
  renderSection: (id: string) => React.ReactNode;
}

// SettingsLayout shows one section at a time, selected from a grouped section
// nav and synced to ?section= so every section is deep-linkable.
export function SettingsLayout({
  title,
  subtitle,
  navLabel,
  sections,
  legacy,
  headerActions,
  lockedEnterprise = false,
  renderSection,
}: SettingsLayoutProps) {
  const [params] = useSearchParams();
  const location = useLocation();
  const navigate = useNavigate();
  const resolved = resolveSection(
    sections,
    { section: params.get("section"), tab: params.get("tab"), hash: location.hash },
    legacy,
  );
  const active = sections.find((s) => s.id === resolved.id) ?? sections[0];
  const headingRef = useRef<HTMLHeadingElement>(null);
  const previous = useRef(active.id);

  useEffect(() => {
    if (resolved.fromLegacy) {
      navigate({ search: `?section=${resolved.id}`, hash: "" }, { replace: true });
    }
  }, [navigate, resolved.fromLegacy, resolved.id]);

  useEffect(() => {
    if (previous.current !== active.id) {
      previous.current = active.id;
      headingRef.current?.focus();
    }
  }, [active.id]);

  const groups = groupSections(sections);

  return (
    <>
      <TopBar title={title} subtitle={subtitle} actions={headerActions} />
      <main className="flex-1 overflow-auto">
        <div className="mx-auto grid w-full max-w-6xl gap-5 p-4 sm:p-6 lg:grid-cols-[13rem_minmax(0,1fr)] lg:gap-10">
          <div className="lg:hidden">
            <label className="field-label" htmlFor="settings-section-picker">
              Section
            </label>
            <select
              id="settings-section-picker"
              className="input"
              value={active.id}
              onChange={(e) => navigate({ search: `?section=${e.target.value}` })}
            >
              {groups.map((g) => (
                <optgroup key={g.group} label={g.group}>
                  {g.sections.map((s) => (
                    <option key={s.id} value={s.id}>
                      {s.label}
                    </option>
                  ))}
                </optgroup>
              ))}
            </select>
          </div>

          <nav aria-label={navLabel} className="hidden lg:sticky lg:top-6 lg:block lg:self-start">
            {groups.map((g) => (
              <div key={g.group} className="mb-5">
                <p className="mb-1.5 px-2.5 text-2xs font-medium uppercase tracking-wider text-ink-400">
                  {g.group}
                </p>
                <ul className="space-y-0.5">
                  {g.sections.map((s) => {
                    const current = s.id === active.id;
                    return (
                      <li key={s.id}>
                        <Link
                          to={{ search: `?section=${s.id}` }}
                          aria-current={current ? "page" : undefined}
                          className={clsx(
                            "flex items-center justify-between gap-2 rounded-control px-2.5 py-1.5 text-xs transition-colors",
                            current
                              ? "bg-accent-subtle font-medium text-ink-50"
                              : "text-ink-300 hover:bg-ink-600/50 hover:text-ink-100",
                          )}
                        >
                          {s.label}
                          {lockedEnterprise && s.enterprise && (
                            <Lock size={11} aria-label="Enterprise" className="shrink-0 text-ink-400" />
                          )}
                        </Link>
                      </li>
                    );
                  })}
                </ul>
              </div>
            ))}
          </nav>

          <section aria-labelledby="settings-section-title" className="min-w-0 max-w-3xl">
            <header className="mb-5">
              <p className="text-2xs font-medium uppercase tracking-wider text-ink-400">
                {active.group}
              </p>
              <div className="mt-1 flex flex-wrap items-center gap-2">
                <h2
                  id="settings-section-title"
                  ref={headingRef}
                  tabIndex={-1}
                  className="text-lg font-semibold text-ink-50 focus:outline-none"
                >
                  {active.label}
                </h2>
                {active.enterprise && <span className="pill pill-accent">Enterprise</span>}
                {active.readOnly && (
                  <span className="pill">
                    <Lock size={10} aria-hidden />
                    Configured in YAML
                  </span>
                )}
              </div>
              <p className="mt-1 text-xs text-ink-300">{active.description}</p>
            </header>
            {active.readOnly && <ReadOnlyNotice docsUrl={active.docsUrl} />}
            <div key={active.id}>{renderSection(active.id)}</div>
          </section>
        </div>
      </main>
    </>
  );
}
