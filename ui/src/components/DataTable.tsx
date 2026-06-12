import { useNavigate } from "react-router-dom";
import clsx from "clsx";

export { useTableKeys } from "@/lib/hooks";

// ClickableRow makes the WHOLE row the navigation affordance (audit D4: the
// only link was the truncated title text while tr:hover promised more).
// Clicks on nested interactive elements (links, buttons, inputs, labels)
// pass through untouched; plain row clicks navigate. Rows are ≥44px on
// touch via py-2.5 + the 16px mobile base font.
export function ClickableRow({
  to,
  onOpen,
  className,
  children,
  ...rest
}: {
  to?: string;
  onOpen?: () => void;
  className?: string;
  children: React.ReactNode;
} & React.HTMLAttributes<HTMLTableRowElement>) {
  const navigate = useNavigate();
  const open = () => {
    if (onOpen) onOpen();
    else if (to) navigate(to);
  };
  return (
    <tr
      {...rest}
      className={clsx("cursor-pointer", className)}
      onClick={(e) => {
        const el = e.target as HTMLElement;
        if (el.closest("a,button,input,select,textarea,label,summary")) return;
        open();
      }}
    >
      {children}
    </tr>
  );
}

// SortHeader — a th with aria-sort + click affordance for sortable columns.
export function SortHeader({
  label,
  dir,
  onClick,
  className,
}: {
  label: string;
  dir?: "asc" | "desc" | null;
  onClick: () => void;
  className?: string;
}) {
  return (
    <th
      aria-sort={dir === "asc" ? "ascending" : dir === "desc" ? "descending" : "none"}
      className={className}
    >
      <button
        className="inline-flex items-center gap-1 uppercase tracking-wider hover:text-ink-100"
        onClick={onClick}
      >
        {label}
        <span aria-hidden className="text-ink-400">
          {dir === "asc" ? "↑" : dir === "desc" ? "↓" : ""}
        </span>
      </button>
    </th>
  );
}
