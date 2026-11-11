import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import clsx from "clsx";
import {
  ArrowDownRight,
  ArrowUpRight,
  CheckCircle2,
  CircleDot,
  CircleSlash,
  Equal,
  Info,
  Plug,
} from "lucide-react";
import { Link } from "react-router-dom";
import type { FinOpsWhy } from "@/lib/api";
import { STATUS_WORDS, changeTone, fmtMoney, whyKind, whyText } from "@/lib/finops";
import { NOTE_GUTTER, NOTE_WIDTH, placeNote } from "./notePlacement";

// Small building blocks shared by the FinOps page and the increase detail.

/** Money in JetBrains Mono with tabular figures. */
export function Money({
  micros,
  signed,
  currency,
  className,
}: {
  micros: number;
  signed?: boolean;
  currency?: string;
  className?: string;
}) {
  return (
    <span className={clsx("font-mono tabular-nums", className)}>
      {fmtMoney(micros, { signed, currency })}
    </span>
  );
}

/**
 * One ⓘ per card: the measure, data lag or source. A click or Enter opens a
 * small popover; Escape or a second click closes it.
 */
export function InfoTip({ label, children }: { label: string; children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState<{ left: number; top: number; width: number } | null>(null);
  const id = useId();
  const ref = useRef<HTMLSpanElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const note = useRef<HTMLSpanElement>(null);
  const place = useCallback(() => {
    if (!button.current) return;
    const r = button.current.getBoundingClientRect();
    const width = document.documentElement.clientWidth || window.innerWidth;
    const height = document.documentElement.clientHeight || window.innerHeight;
    setPos(placeNote(r, note.current?.getBoundingClientRect().height ?? 0, { width, height }));
  }, []);
  useLayoutEffect(() => {
    if (open) place();
    else setPos(null);
  }, [open, place]);
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onDown);
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onDown);
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [open, place]);
  return (
    <span ref={ref} className="relative inline-flex align-middle">
      <button
        ref={button}
        type="button"
        aria-label={label}
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        className={clsx(
          "inline-flex rounded-full p-0.5 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent",
          open ? "text-accent" : "text-ink-400 hover:text-ink-100",
        )}
        onClick={(e) => {
          // Never let a surrounding row or link act on the ⓘ.
          e.preventDefault();
          e.stopPropagation();
          setOpen((v) => !v);
        }}
      >
        <Info size={12} aria-hidden="true" />
      </button>
      {open && (
        <span
          ref={note}
          id={id}
          role="note"
          onClick={(e) => e.stopPropagation()}
          style={pos ? { position: "fixed", left: pos.left, top: pos.top, width: pos.width } : { position: "fixed", visibility: "hidden", width: NOTE_WIDTH, maxWidth: `calc(100vw - ${2 * NOTE_GUTTER}px)` }}
          className="z-dropdown rounded-control border border-ink-500 bg-surface-raised p-2.5 text-left text-2xs font-normal normal-case leading-relaxed text-ink-200 shadow-overlay"
        >
          {children}
        </span>
      )}
    </span>
  );
}

/**
 * Resource-level lines were too many to read for the period: rows name no
 * resource, but the amounts are complete. A status word plus an ⓘ carrying
 * the server's note, so missing names never read as "no resources" or zero.
 * On paper an ⓘ cannot open, so the print variant marks the word with "*"
 * and the caller prints the note once as a footnote (ResourceFootnote).
 * The compact variant is the bare word for a single row under a card that
 * already carries the ⓘ.
 */
export function ResourcesNotNamed({ note, print, compact }: { note?: string; print?: boolean; compact?: boolean }) {
  if (print) {
    return (
      <span className="text-2xs font-normal text-ink-300">
        Resources not named{note ? "*" : ""}
      </span>
    );
  }
  if (compact) {
    // Per-row status word; the card header carries the one ⓘ.
    return <span className="text-2xs text-ink-300">Resources not named</span>;
  }
  return (
    <span className="inline-flex items-center gap-1 text-xs text-ink-300">
      Resources not named
      {note && <InfoTip label="About resource data">{note}</InfoTip>}
    </span>
  );
}

/** The printed footnote for ResourcesNotNamed in its print variant. */
export function ResourceFootnote({ note }: { note: string }) {
  return <p className="mt-1.5 text-[10px] text-ink-300">* {note}</p>;
}

/** "≈" marks a figure that rests on an estimated team or region split. */
export function Estimated() {
  return (
    <span className="inline-flex items-center">
      <span aria-hidden="true" className="mr-0.5 text-ink-300">
        ≈
      </span>
      <span className="sr-only">estimated, </span>
      <InfoTip label="Estimated split">Estimated split: the source could not read this cut directly, so it is shared in proportion.</InfoTip>
    </span>
  );
}

/** Status is always a word plus an icon. */
export function StatusWord({ status }: { status?: string }) {
  if (!status) return null;
  const word = STATUS_WORDS[status] ?? status;
  const Icon = status === "open" ? CircleDot : CheckCircle2;
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1 text-xs",
        status === "open" ? "font-medium text-ink-50" : "text-ink-300",
      )}
    >
      <Icon size={12} aria-hidden="true" />
      {word}
    </span>
  );
}

/** Up / Down / About the same, with a sign icon so colour is never alone. */
export function ChangeBadge({ direction, word }: { direction?: string; word: string }) {
  const tone = changeTone(direction);
  const Icon = tone === "up" ? ArrowUpRight : tone === "down" ? ArrowDownRight : Equal;
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-0.5 rounded px-1.5 py-0.5 text-2xs font-semibold",
        tone === "up" && "bg-sev-high/15 text-sev-high",
        tone === "down" && "bg-sev-ok/15 text-sev-ok",
        tone === "same" && "bg-ink-600 text-ink-200",
      )}
    >
      <Icon size={11} aria-hidden="true" />
      {word}
    </span>
  );
}

/** The Why cell: the change found, or the named gap with its action. */
export function WhyCell({ why, mono = true }: { why?: FinOpsWhy; mono?: boolean }) {
  const kind = whyKind(why);
  if (!kind) return null;
  const text = whyText(why);
  if (kind === "found") {
    return <span className={clsx("text-ink-200", mono && "font-mono text-2xs")}>{text}</span>;
  }
  if (kind === "not_connected") {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs text-ink-300">
        <Plug size={12} aria-hidden="true" />
        {text}
        <Link to="/aws/settings" className="text-link hover:underline" onClick={(e) => e.stopPropagation()}>
          Connect ›
        </Link>
      </span>
    );
  }
  if (kind === "incomplete") {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs text-ink-300">
        <CircleSlash size={12} aria-hidden="true" />
        {text}
        <InfoTip label="About change history">Some change history for that window is missing, so no change is named.</InfoTip>
      </span>
    );
  }
  if (kind === "pending") {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs text-ink-300">
        <CircleSlash size={12} aria-hidden="true" />
        {text}
        <Link to="/aws/settings" className="text-link hover:underline" onClick={(e) => e.stopPropagation()}>
          Collectors ›
        </Link>
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-ink-300">
      <CircleSlash size={12} aria-hidden="true" />
      {text}
    </span>
  );
}

/**
 * One line cut to its box with an ellipsis. Only when the text is actually
 * cut does it become a button: a tap, click or Enter shows the whole text
 * wrapped in place and a second one cuts it again, so the full value is
 * reachable without a hover. Text that fits is plain text, no tab stop.
 */
export function ClippedText({ text, className }: { text: string; className?: string }) {
  const [whole, setWhole] = useState(false);
  const [cut, setCut] = useState(false);
  const ref = useRef<HTMLElement | null>(null);
  useLayoutEffect(() => {
    const el = ref.current;
    // While the whole text is shown it wraps, so there is nothing to measure.
    if (!el || whole) return;
    const check = () => setCut(el.scrollWidth > el.clientWidth);
    check();
    window.addEventListener("resize", check);
    const observer = typeof ResizeObserver === "function" ? new ResizeObserver(check) : null;
    observer?.observe(el);
    return () => {
      window.removeEventListener("resize", check);
      observer?.disconnect();
    };
  }, [text, whole, cut]);
  const shape = clsx("block w-full min-w-0 text-left", whole ? "whitespace-normal break-all" : "truncate", className);
  if (!cut && !whole) {
    return (
      <span ref={ref} className={shape}>
        {text}
      </span>
    );
  }
  return (
    <button
      ref={(el) => {
        ref.current = el;
      }}
      type="button"
      title={text}
      aria-expanded={whole}
      onClick={(e) => {
        e.stopPropagation();
        setWhole((v) => !v);
      }}
      className={clsx(shape, "rounded focus:outline-none focus-visible:ring-2 focus-visible:ring-accent")}
    >
      {text}
    </button>
  );
}

export function Card({
  title,
  aside,
  children,
  className,
  labelledBy,
}: {
  title: React.ReactNode;
  aside?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  labelledBy?: string;
}) {
  const fallback = useId();
  const id = labelledBy ?? fallback;
  return (
    <section aria-labelledby={id} className={clsx("card min-w-0", className)}>
      <div className="flex flex-wrap items-center justify-between gap-2 px-4 pt-3">
        <h2 id={id} className="card-title">
          {title}
        </h2>
        {aside}
      </div>
      <div className="px-4 pb-4 pt-3">{children}</div>
    </section>
  );
}
