// SettingsCard is the plain surface every admin control renders inside; the
// section title and description come from the surrounding SettingsLayout.
export function SettingsCard({
  children,
  id,
  testId,
}: {
  children: React.ReactNode;
  id?: string;
  testId?: string;
}) {
  return (
    <div id={id} className="card" data-testid={testId}>
      <div className="card-body">{children}</div>
    </div>
  );
}

// SaveBar keeps a card's primary actions pinned to the bottom of the scroll
// area while a long form is edited. Place it as the card's last child.
export function SaveBar({ children }: { children: React.ReactNode }) {
  return (
    <div className="sticky bottom-0 z-10 -mx-4 -mb-4 mt-2 flex flex-wrap items-center justify-end gap-2 rounded-b-card border-t border-ink-500/40 bg-surface/95 px-4 py-3 backdrop-blur">
      {children}
    </div>
  );
}
