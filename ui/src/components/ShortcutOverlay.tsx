import { Modal } from "./Modal";

const GROUPS: Array<{ title: string; keys: Array<[string, string]> }> = [
  {
    title: "Global",
    keys: [
      ["/", "Focus the page search"],
      ["g n", "Go to Now"],
      ["g i", "Go to Incidents"],
      ["g p", "Go to Patterns"],
      ["?", "Open this overlay (Esc closes)"],
      ["Esc", "Close panel / clear selection"],
    ],
  },
  {
    title: "Tables",
    keys: [
      ["j / k", "Move down / up"],
      ["Enter", "Open the active row"],
    ],
  },
  {
    title: "Patterns",
    keys: [
      ["x", "Select / deselect row"],
      ["K", "Mark known"],
      ["S", "Mark spike"],
    ],
  },
];

export function ShortcutOverlay({ onClose }: { onClose: () => void }) {
  return (
    <Modal title="Keyboard shortcuts" onClose={onClose} size="md">
      <div className="space-y-4">
        {GROUPS.map((g) => (
          <div key={g.title}>
            <div className="mb-1.5 text-2xs uppercase tracking-wider text-ink-300">
              {g.title}
            </div>
            <dl className="space-y-1">
              {g.keys.map(([k, desc]) => (
                <div key={k} className="flex items-center justify-between gap-4">
                  <dt>
                    <kbd className="rounded border border-ink-500 bg-surface-sunken px-1.5 py-0.5 font-mono text-2xs text-ink-100">
                      {k}
                    </kbd>
                  </dt>
                  <dd className="text-xs text-ink-200">{desc}</dd>
                </div>
              ))}
            </dl>
          </div>
        ))}
      </div>
    </Modal>
  );
}
