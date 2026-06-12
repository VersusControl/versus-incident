import { useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertTriangle,
  Eye,
  FileText,
  Search,
  Trash2,
  Upload,
} from "lucide-react";
import {
  api,
  ApiError,
  type Runbook,
  type RunbookDetail,
} from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { EmptyState, Spinner } from "@/components/feedback";
import { Pill } from "@/components/Pill";
import { Modal } from "@/components/Modal";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { RetryableError } from "@/components/RetryableError";
import { SkRows } from "@/components/Skeleton";
import { useToast } from "@/components/Toast";

// RunbooksPage lets operators manage the runbook corpus that backs the
// find_runbook tool. Runbooks are managed by UPLOADING `.md` files
// (multiple at once) — there is no free-text editor. To change a runbook,
// re-upload a file with the same name; to remove one, delete it. The
// boot-time auto-ingest of the runbooks/ folder and these uploads share
// the same corpus, so files dropped into the folder and files uploaded
// here appear in the same list.
export function RunbooksPage() {
  const qc = useQueryClient();
  const toast = useToast();
  const listQ = useQuery({
    queryKey: ["runbooks"],
    queryFn: api.listRunbooks,
    retry: (count, error) => {
      // Don't retry on 503 (feature not configured) — it won't self-heal.
      if (error instanceof ApiError && error.status === 503) return false;
      return count < 3;
    },
  });

  // Feature not configured — show a clean disabled state, not an error.
  // The sidebar keeps the Runbooks entry visible with a dim hint; this page
  // is where that hint resolves into an explanation.
  const unavailable =
    listQ.isError &&
    listQ.error instanceof ApiError &&
    listQ.error.status === 503;

  const [q, setQ] = useState("");
  const [viewing, setViewing] = useState<RunbookDetail | null>(null);
  const [toDelete, setToDelete] = useState<Runbook | null>(null);
  const fileInput = useRef<HTMLInputElement | null>(null);

  const runbooks = listQ.data?.runbooks ?? [];
  const embeddings = listQ.data?.embeddings ?? false;

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    if (!needle) return runbooks;
    return runbooks.filter(
      (r) =>
        r.id.toLowerCase().includes(needle) ||
        r.title.toLowerCase().includes(needle) ||
        (r.services ?? []).some((s) => s.toLowerCase().includes(needle)) ||
        (r.tags ?? []).some((t) => t.toLowerCase().includes(needle)),
    );
  }, [runbooks, q]);

  const upload = useMutation({
    mutationFn: (files: File[]) => api.uploadRunbooks(files),
    onSuccess: (result) => {
      qc.invalidateQueries({ queryKey: ["runbooks"] });
      toast.push({
        tone: "ok",
        title: `Uploaded ${result.ingested} runbook${result.ingested === 1 ? "" : "s"}`,
        description: result.embeddings
          ? undefined
          : "Stored, but not searchable until an embedding model is configured.",
      });
    },
    onError: (err, files) => {
      toast.push({
        tone: "error",
        title: "Upload failed",
        description: err.message,
        action: { label: "Retry", onClick: () => upload.mutate(files) },
      });
    },
  });

  const del = useMutation({
    mutationFn: (rb: Runbook) => api.deleteRunbook(rb.id),
    onSuccess: (_data, rb) => {
      qc.invalidateQueries({ queryKey: ["runbooks"] });
      setToDelete(null);
      toast.push({ tone: "ok", title: `Deleted "${rb.title}"` });
    },
    onError: (err, rb) => {
      // The confirm dialog stays open with the inline error (its Delete
      // button doubles as Retry); the toast makes the failure unmissable.
      toast.push({
        tone: "error",
        title: `Couldn't delete "${rb.title}"`,
        description: err.message,
      });
    },
  });

  const view = useMutation({
    mutationFn: (rb: Runbook) => api.getRunbook(rb.id),
    onSuccess: (rb) => setViewing(rb),
    onError: (err, rb) => {
      toast.push({
        tone: "error",
        title: `Couldn't open "${rb.title}"`,
        description: err.message,
        action: { label: "Retry", onClick: () => view.mutate(rb) },
      });
    },
  });

  function onPickFiles(e: React.ChangeEvent<HTMLInputElement>) {
    const picked = e.target.files;
    if (!picked || picked.length === 0) return;
    const files = Array.from(picked);
    upload.mutate(files);
    // Reset so re-uploading the same file fires onChange again.
    if (fileInput.current) fileInput.current.value = "";
  }

  if (unavailable) {
    return (
      <>
        <TopBar title="Runbooks" subtitle="Not configured" />
        <main className="flex-1 overflow-auto p-6">
          <EmptyState
            title="Runbooks not available"
            hint="Requires the AI subsystem (agent.ai.enable) and a storage backend — configure both to use the runbook corpus."
          />
        </main>
      </>
    );
  }

  return (
    <>
      <TopBar
        title="Runbooks"
        subtitle={listQ.data ? `${runbooks.length} in corpus` : undefined}
        actions={
          <button
            className="btn"
            onClick={() => fileInput.current?.click()}
            disabled={upload.isPending}
          >
            <Upload size={12} /> {upload.isPending ? "Uploading…" : "Upload .md"}
          </button>
        }
      />

      <input
        ref={fileInput}
        type="file"
        accept=".md,text/markdown"
        multiple
        className="hidden"
        onChange={onPickFiles}
      />

      <main className="flex-1 overflow-auto p-6">
        {!embeddings && runbooks.length > 0 && (
          <div className="mb-3 flex items-start gap-2 rounded-card border border-ink-600 bg-surface-raised px-3 py-2 text-xs text-ink-200">
            <AlertTriangle size={14} className="mt-0.5 shrink-0 text-sev-warn" />
            <span>
              No embedding model configured — runbooks are stored but not yet
              searchable by the agent. Set{" "}
              <code className="font-mono text-2xs text-ink-100">
                tools.find_runbook.embedding_model
              </code>{" "}
              in tools.yaml to enable search.
            </span>
          </div>
        )}

        <div className="mb-3 flex flex-wrap items-center gap-2">
          <div className="relative max-w-md flex-1">
            <Search
              size={12}
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-400"
            />
            <input
              data-page-search
              aria-label="Search runbooks"
              className="input pl-7"
              placeholder="Search by name, title, service, or tag…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
        </div>

        {listQ.isError && (
          <div className="mb-3">
            <RetryableError
              error={listQ.error}
              onRetry={() => listQ.refetch()}
              retrying={listQ.isRefetching}
              context="Couldn't load runbooks"
            />
          </div>
        )}

        {(!listQ.isError || listQ.data) && (
          <div className="card overflow-hidden">
            <div className="max-h-[calc(100vh-220px)] overflow-auto">
              <table className="ddt">
                <thead>
                  <tr>
                    <th>Title</th>
                    <th className="w-48">Services</th>
                    <th className="w-28">Searchable</th>
                    <th className="w-28" />
                  </tr>
                </thead>
                <tbody>
                  {listQ.isLoading && <SkRows rows={6} cols={4} />}
                  {listQ.isSuccess && filtered.length === 0 && (
                    <tr>
                      <td colSpan={4}>
                        {runbooks.length > 0 ? (
                          <EmptyState
                            title="No runbooks match your search."
                            hint="Try clearing the search."
                          />
                        ) : (
                          <EmptyState
                            title="No runbooks yet"
                            hint="Upload one or more .md files, or drop them into the runbooks/ folder under the data directory."
                          />
                        )}
                      </td>
                    </tr>
                  )}
                  {filtered.map((r) => {
                    const viewPending =
                      view.isPending && view.variables?.id === r.id;
                    return (
                      <tr key={r.id}>
                        <td>
                          <div className="flex items-center gap-2">
                            <FileText
                              size={13}
                              className="shrink-0 text-ink-400"
                            />
                            <div>
                              <div className="font-medium text-ink-50">
                                {r.title}
                              </div>
                              <div className="text-2xs text-ink-400">
                                {r.id}
                              </div>
                            </div>
                          </div>
                        </td>
                        <td>
                          <div className="flex flex-wrap gap-1">
                            {(r.services ?? []).map((s) => (
                              <Pill key={s}>{s}</Pill>
                            ))}
                          </div>
                        </td>
                        <td>
                          {r.has_vector ? (
                            <span className="text-2xs font-medium text-sev-ok">
                              indexed
                            </span>
                          ) : (
                            <span className="text-2xs text-ink-400">
                              no vector
                            </span>
                          )}
                        </td>
                        <td>
                          <div className="flex items-center justify-end gap-1">
                            <button
                              className="btn"
                              title="View"
                              aria-label={`View runbook "${r.title}"`}
                              disabled={viewPending}
                              onClick={() => view.mutate(r)}
                            >
                              {viewPending ? <Spinner /> : <Eye size={11} />}
                            </button>
                            <button
                              className="btn"
                              title="Delete"
                              aria-label={`Delete runbook "${r.title}"`}
                              onClick={() => {
                                del.reset();
                                setToDelete(r);
                              }}
                            >
                              <Trash2 size={11} />
                            </button>
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </div>
        )}
      </main>

      {viewing && (
        <Modal title={viewing.title} size="lg" onClose={() => setViewing(null)}>
          <RunbookView rb={viewing} />
        </Modal>
      )}

      {toDelete && (
        <ConfirmDialog
          title="Delete runbook"
          message={
            <>
              Delete runbook{" "}
              <span className="font-medium text-ink-50">
                "{toDelete.title}"
              </span>
              ? The agent will no longer find it.
            </>
          }
          confirmLabel="Delete"
          tone="danger"
          busy={del.isPending}
          error={del.isError ? del.error : null}
          onConfirm={() => del.mutate(toDelete)}
          onClose={() => {
            setToDelete(null);
            del.reset();
          }}
        />
      )}
    </>
  );
}

function RunbookView({ rb }: { rb: RunbookDetail }) {
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-2xs text-ink-300">
        <span className="font-mono">{rb.id}</span>
        {(rb.services ?? []).map((s) => (
          <Pill key={`svc-${s}`}>{s}</Pill>
        ))}
        {(rb.tags ?? []).map((t) => (
          <Pill key={`tag-${t}`}>#{t}</Pill>
        ))}
      </div>
      <pre className="max-h-[60vh] overflow-auto whitespace-pre-wrap break-words rounded-card bg-surface-sunken p-3 font-mono text-xs leading-snug text-ink-100">
        {rb.body}
      </pre>
    </div>
  );
}
