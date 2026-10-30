import { useMemo, useState } from "react";
import { Copy, Download, Search } from "lucide-react";
import type { KubernetesResource } from "@/lib/api";
import { projectedResourceYaml, yamlKeyEnd } from "@/lib/kubernetesProjection";
import { downloadText } from "@/lib/kubernetesResourceDetail";

export function ResourceYaml({ resource }: { resource: KubernetesResource }) {
  const yaml = useMemo(() => projectedResourceYaml(resource), [resource]);
  const lines = yaml.trimEnd().split("\n");
  const blocks = lines.flatMap((line, index) => {
    if (!/^\s*[\w.-]+:\s*$/.test(line) && !/^\s*-\s+name:\s*.+$/.test(line)) return [];
    const indent = line.search(/\S/);
    let end = index + 1;
    while (end < lines.length && lines[end].search(/\S/) > indent) end++;
    return end > index + 1
      ? [{ start: index, end, name: line.trim().replace(/:$/, ""), indent }]
      : [];
  });
  const containerBlock = blocks.find((block) => block.name === "containers");
  const containerEntries = blocks.filter((block) => containerBlock && block.indent === containerBlock.indent + 2 && block.start > containerBlock.start && block.end <= containerBlock.end);
  const [folded, setFolded] = useState<Set<number>>(() => new Set(blocks.filter((block) => ["ownerReferences", "status", "observations"].includes(block.name) || containerEntries.slice(1).some((entry) => entry.start === block.start)).map((block) => block.start)));
  const [activeLine, setActiveLine] = useState(blocks.find((block) => block.name === "spec")?.start ?? 0);
  const [find, setFind] = useState("");
  const [match, setMatch] = useState(0);
  const [status, setStatus] = useState("");
  const matches = find
    ? lines.flatMap((line, index) =>
        line.toLowerCase().includes(find.toLowerCase()) ? [index] : [],
      )
    : [];
  const goTo = (line: number) => {
    setActiveLine(line);
    setFolded(
      (current) =>
        new Set(
          [...current].filter(
            (start) =>
              !blocks.some(
                (block) =>
                  block.start === start && line > start && line < block.end,
              ),
          ),
        ),
    );
    requestAnimationFrame(() =>
      document
        .getElementById(`resource-yaml-${line}`)
        ?.scrollIntoView?.({ block: "center" }),
    );
  };
  return (
    <section aria-label="Projected YAML" className="resource-yaml space-y-3">
      <div className="yaml-toolbar flex flex-wrap items-center gap-2">
        <span className="pill">Projected, read-only</span>
        <div className="yaml-find flex items-center gap-1"><Search size={13} aria-hidden="true" />
          <input className="input min-w-0 flex-1" aria-label="Find in YAML" placeholder="Find in YAML" value={find} onChange={(event) => { setFind(event.target.value); setMatch(0); }} onKeyDown={(event) => { if (event.key === "Enter" && matches.length) { goTo(matches[match % matches.length]); setMatch((value) => value + 1); } }} />
          <span aria-live="polite" className="yaml-find-count">{find ? `${matches.length}` : ""}</span>
          <button className="btn" type="button" aria-label="Next match" title="Next match" disabled={!matches.length} onClick={() => { goTo(matches[match % matches.length]); setMatch((value) => value + 1); }}>↓</button>
        </div>
        <button
          type="button"
          className="btn"
          onClick={() => setFolded(new Set(blocks.map((block) => block.start)))}
        >
          Fold all
        </button>
        <button
          type="button"
          className="btn"
          onClick={() => setFolded(new Set())}
        >
          Unfold all
        </button>
        <button
          type="button"
          className="btn"
          onClick={() =>
            void navigator.clipboard.writeText(yaml).then(
              () => setStatus("YAML copied."),
              () => setStatus("Clipboard unavailable."),
            )
          }
        >
          <Copy size={13} />
          Copy
        </button>
        <button
          type="button"
          className="btn"
          onClick={() =>
            downloadText(yaml, `${resource.name}.yaml`, "text/yaml")
          }
        >
          <Download size={13} />
          Download
        </button>
      </div>
      <div className="yaml-workspace flex min-w-0 flex-col gap-3 sm:flex-row">
        <div className="yaml-editor min-w-0 flex-1 overflow-auto rounded-control border border-ink-700 bg-ink-950 py-2 font-mono text-xs leading-[19px]" onScroll={(event) => { const top = event.currentTarget.getBoundingClientRect().top; const first = Array.from(event.currentTarget.querySelectorAll<HTMLElement>("[data-line]")).find((row) => row.getBoundingClientRect().bottom >= top); if (first) setActiveLine(Number(first.dataset.line)); }}>
          {lines.map((line, index) => {
            if (
              blocks.some(
                (block) =>
                  folded.has(block.start) &&
                  index > block.start &&
                  index < block.end,
              )
            )
              return null;
            const block = blocks.find((item) => item.start === index);
            const colon = yamlKeyEnd(line);
            return (
              <div
                id={`resource-yaml-${index}`}
                key={index}
                data-line={index}
                onFocus={() => setActiveLine(index)}
                className={`flex min-w-max ${matches.includes(index) ? "bg-accent/10" : ""}`}
              >
                <span
                  className="inline-block w-10 shrink-0 pr-2 text-right text-ink-500"
                  aria-hidden="true"
                >
                  {index + 1}
                </span>
                {folded.has(index) && block ? <button className="yaml-fold-pill" type="button" aria-label={`Unfold ${block.name} at line ${index + 1}`} aria-expanded={false} onClick={() => setFolded((current) => new Set([...current].filter((start) => start !== index)))}>▸ {block.name} ({block.end - block.start - 1} lines)</button> : <><span className="inline-block w-5 shrink-0">
                  {block && (
                    <button
                      type="button"
                      aria-label={`${folded.has(index) ? "Unfold" : "Fold"} ${block.name} at line ${index + 1}`}
                      aria-expanded={!folded.has(index)}
                      onClick={() =>
                        setFolded((current) => {
                          const next = new Set(current);
                          if (next.has(index)) next.delete(index);
                          else next.add(index);
                          return next;
                        })
                      }
                    >
                      {folded.has(index) ? "▸" : "▾"}
                    </button>
                  )}
                </span>
                <code className="whitespace-pre pr-3">
                  {colon >= 0 ? (
                    <>
                      <span className="text-link">
                        {line.slice(0, colon + 1)}
                      </span>
                      <span className={line.slice(colon + 1).includes("‹redacted›") ? "yaml-redacted" : /^\s*-?\d+(?:\.\d+)?\s*$/.test(line.slice(colon + 1)) ? "text-sev-warning" : "text-sev-ok"}>
                        {line.slice(colon + 1)}
                      </span>
                    </>
                  ) : (
                    line
                  )}
                </code>
                </>}
              </div>
            );
          })}
        </div>
        <nav aria-label="YAML outline" className="h-fit shrink-0 rounded-control border border-ink-700 bg-surface-sunken p-3 text-xs sm:w-32">
          <h3 className="mb-2 text-2xs uppercase text-ink-400">Outline</h3>
          {blocks
            .filter((block, index, all) => (block.indent === 0 || block.name === "containers") && all.findIndex((candidate) => candidate.name === block.name) === index)
            .map((block) => (
              <button
                key={block.start}
                type="button"
                className="yaml-outline-item block w-full py-1 text-left"
                aria-current={activeLine >= block.start && activeLine < block.end && !blocks.some((child) => child.name === "containers" && child.start > block.start && activeLine >= child.start && activeLine < child.end) ? "location" : undefined}
                onClick={() => goTo(block.start)}
              >
                {block.name}{" "}
                <span className="text-ink-400">{block.start + 1}</span>
              </button>
            ))}
        </nav>
      </div>
      <p role="status" className="text-2xs text-ink-400">
        {status}
      </p>
    </section>
  );
}

