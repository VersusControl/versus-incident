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
  type RunbookDetail,
} from "@/lib/api";
import { TopBar } from "@/components/TopBar";
import { EmptyState, ErrorBox, Spinner } from "@/components/feedback";
import { Pill } from "@/components/Pill";
import { Modal } from "./MembersPage";

// RunbooksPage lets operators manage the runbook corpus that backs the
// find_runbook tool. Runbooks are managed by UPLOADING `.md` files
// (multiple at once) — there is no free-text editor. To change a runbook,
// re-upload a file with the same name; to remove one, delete it. The
// boot-time auto-ingest of the runbooks/ folder and these uploads share
// the same corpus, so files dropped into the folder and files uploaded
// here appear in the same list.
export function RunbooksPage() {
  const qc = useQueryClient();
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
  const unavailable =
    listQ.isError &&
    listQ.error instanceof ApiError &&
    listQ.error.status === 503;

  const [q, setQ] = useState("");
  const [viewing, setViewing] = useState<RunbookDetail | null>(null);
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
    onSuccess: () => qc.invalidateQueries({ queryKey: ["runbooks"] }),
  });

  const del = useMutation({
    mutationFn: (id: string) => api.deleteRunbook(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["runbooks"] }),
  });

  const view = useMutation({
    mutationFn: (id: string) => api.getRunbook(id),
    onSuccess: (rb) => setViewing(rb),
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
            hint="Enable the AI subsystem (AGENT_AI_ENABLE=true with a valid API key and model) to use the runbook corpus."
          />
        </main>
      </>
    );
  }

  return (
    <>
      <TopBar
        title="Runbooks"
        subtitle={`${runbooks.length} in corpus`}
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
          <div className="mb-3 flex items-start gap-2 rounded-md border border-ink-200 bg-ink-50 px-3 py-2 text-xs text-ink-600">
            <AlertTriangle size={14} className="mt-0.5 shrink-0 text-ink-400" />
            <span>
              No embedding model configured — runbooks are stored but not yet
              searchable by the agent. Set{" "}
              <code className="font-mono text-2xs">tools.find_runbook.embedding_model</code>{" "}
              in tools.yaml to enable search.
            </span>
          </div>
        )}

        {upload.isError && <ErrorBox error={upload.error} />}
        {del.isError && <ErrorBox error={del.error} />}

        <div className="mb-3 flex flex-wrap items-center gap-2">
          <div className="relative max-w-md flex-1">
            <Search
              size={12}
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-300"
            />
            <input
              className="input pl-7"
              placeholder="Search by name, title, service, or tag…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
        </div>

        {listQ.isError && <ErrorBox error={listQ.error} />}

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
                {listQ.isLoading && (
                  <tr>
                    <td colSpan={4} className="py-8 text-center">
                      <Spinner />
                    </td>
                  </tr>
                )}
                {!listQ.isLoading && filtered.length === 0 && (
                  <tr>
                    <td colSpan={4}>
                      <EmptyState
                        title="No runbooks yet"
                        hint="Upload one or more .md files, or drop them into the runbooks/ folder under the data directory."
                      />
                    </td>
                  </tr>
                )}
                {filtered.map((r) => (
                  <tr key={r.id}>
                    <td>
                      <div className="flex items-center gap-2">
                        <FileText size={13} className="shrink-0 text-ink-400" />
                        <div>
                          <div className="font-medium text-ink-900">
                            {r.title}
                          </div>
                          <div className="text-2xs text-ink-500">{r.id}</div>
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
                        <span className="text-2xs font-medium text-emerald-600">
                          indexed
                        </span>
                      ) : (
                        <span className="text-2xs text-ink-400">no vector</span>
                      )}
                    </td>
                    <td>
                      <div className="flex items-center justify-end gap-1">
                        <button
                          className="btn"
                          title="View"
                          onClick={() => view.mutate(r.id)}
                        >
                          <Eye size={11} />
                        </button>
                        <button
                          className="btn"
                          title="Delete"
                          disabled={del.isPending}
                          onClick={() => {
                            if (
                              window.confirm(`Delete runbook "${r.title}"?`)
                            ) {
                              del.mutate(r.id);
                            }
                          }}
                        >
                          <Trash2 size={11} />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </main>

      {viewing && (
        <Modal title={viewing.title} onClose={() => setViewing(null)}>
          <RunbookView rb={viewing} />
        </Modal>
      )}
    </>
  );
}

function RunbookView({ rb }: { rb: RunbookDetail }) {
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-2xs text-ink-500">
        <span className="font-mono">{rb.id}</span>
        {(rb.services ?? []).map((s) => (
          <Pill key={`svc-${s}`}>{s}</Pill>
        ))}
        {(rb.tags ?? []).map((t) => (
          <Pill key={`tag-${t}`}>#{t}</Pill>
        ))}
      </div>
      <pre className="max-h-[60vh] overflow-auto whitespace-pre-wrap break-words rounded-md bg-ink-50 p-3 font-mono text-xs leading-snug text-ink-800">
        {rb.body}
      </pre>
    </div>
  );
}
