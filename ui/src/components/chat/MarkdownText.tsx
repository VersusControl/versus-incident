import { isValidElement, memo, useState, type ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { Check, Copy } from "lucide-react";
import { copyText } from "@/lib/clipboard";
import { capChatMessage, safeMarkdownUrl } from "@/lib/markdownPolicy";

function CodeBlock({ children }: { children?: ReactNode }) {
  const [copied, setCopied] = useState(false);
  const codeElement = isValidElement<{ className?: string; children?: ReactNode }>(
    children,
  )
    ? children
    : null;
  const className = codeElement?.props.className ?? "";
  const language = /language-([\w-]+)/.exec(className)?.[1] ?? "text";
  const code = String(codeElement?.props.children ?? "").replace(/\n$/, "");

  const copy = async () => {
    if (!(await copyText(code))) return;
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1600);
  };

  return (
    <div className="my-3 overflow-hidden rounded-control border border-ink-500/60 bg-ink-900">
      <div className="flex min-h-9 items-center justify-between border-b border-ink-500/40 px-3 text-2xs text-ink-300">
        <span>{language}</span>
        <button
          type="button"
          onClick={copy}
          className="inline-flex min-h-8 min-w-8 items-center justify-center rounded-control hover:bg-ink-700"
          aria-label={copied ? "Copied" : "Copy code"}
          title={copied ? "Copied" : "Copy code"}
        >
          {copied ? <Check size={14} aria-hidden /> : <Copy size={14} aria-hidden />}
        </button>
      </div>
      <pre className="max-w-full overflow-x-auto p-3 text-xs leading-relaxed text-ink-100">
        <code>{code}</code>
      </pre>
    </div>
  );
}

function MarkdownTextImpl({ children }: { children: string }) {
  return (
    <div className="markdown-content min-w-0 text-sm leading-6 text-ink-100">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        urlTransform={safeMarkdownUrl}
        components={{
          img: () => null,
          a: ({ children: linkChildren, ...props }) => (
            <a
              {...props}
              target="_blank"
              rel="noopener noreferrer"
              className="text-link underline decoration-link/50 underline-offset-2 hover:decoration-link"
            >
              {linkChildren}
            </a>
          ),
          h1: (props) => <h1 {...props} className="mb-2 mt-5 text-lg font-semibold text-ink-50" />,
          h2: (props) => <h2 {...props} className="mb-2 mt-5 text-base font-semibold text-ink-50" />,
          h3: (props) => <h3 {...props} className="mb-1.5 mt-4 text-sm font-semibold text-ink-50" />,
          p: (props) => <p {...props} className="my-2 first:mt-0 last:mb-0" />,
          ul: (props) => <ul {...props} className="my-2 ml-5 list-disc space-y-1" />,
          ol: (props) => <ol {...props} className="my-2 ml-5 list-decimal space-y-1" />,
          blockquote: (props) => (
            <blockquote {...props} className="my-3 border-l-2 border-ink-500 pl-3 text-ink-300" />
          ),
          hr: () => <hr className="my-4 border-ink-500/50" />,
          table: ({ children: tableChildren }) => (
            <div className="my-3 max-w-full overflow-x-auto">
              <table className="w-full min-w-max border-collapse text-left text-xs">
                {tableChildren}
              </table>
            </div>
          ),
          th: (props) => <th {...props} className="border border-ink-500/50 bg-surface-raised px-3 py-2 font-semibold" />,
          td: (props) => <td {...props} className="border border-ink-500/50 px-3 py-2 align-top" />,
          pre: CodeBlock,
          code: ({ className, children: codeChildren, ...props }) => (
            <code
              {...props}
              className={
                className ??
                "rounded-control bg-ink-700 px-1.5 py-0.5 font-mono text-[0.9em] text-ink-50"
              }
            >
              {codeChildren}
            </code>
          ),
        }}
      >
        {capChatMessage(children)}
      </ReactMarkdown>
    </div>
  );
}

export const MarkdownText = memo(MarkdownTextImpl);