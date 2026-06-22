import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { AlertCircle, Eye, EyeOff, Loader2, Triangle } from "lucide-react";
import {
  AUTH_EXPIRED_EVENT,
  ApiError,
  api,
  getSecret,
  setSecret,
} from "./api";
import { Modal } from "@/components/Modal";

interface Props {
  children: React.ReactNode;
}

// AuthGate prompts once for the X-Gateway-Secret and verifies it against
// /api/agent/status up front — a bad secret fails here, not as a 401 deep
// inside a page. Transient network errors deliberately do NOT trap the
// user (kept behavior). Mid-session rotation is handled by <ReauthModal>.
export function AuthGate({ children }: Props) {
  const [ready, setReady] = useState<"checking" | "needs-secret" | "ok">(
    "checking",
  );

  useEffect(() => {
    if (!getSecret()) {
      setReady("needs-secret");
      return;
    }
    api
      .status()
      .then(() => setReady("ok"))
      .catch((err) => {
        if (err instanceof ApiError && err.status === 401) {
          setReady("needs-secret");
        } else {
          setReady("ok");
        }
      });
  }, []);

  if (ready === "checking") {
    return (
      <div className="flex h-full items-center justify-center gap-2 text-ink-300">
        <span aria-hidden className="sk h-2 w-2 rounded-full" />
        Connecting…
      </div>
    );
  }

  if (ready === "needs-secret") {
    return (
      // No bg here — body paints surface-sunken + the accent page glow,
      // which this screen wants more than any other.
      <div className="flex h-full items-center justify-center p-4">
        <SecretForm
          standalone
          onSuccess={() => setReady("ok")}
        />
      </div>
    );
  }

  return <>{children}</>;
}

// ReauthModal — mounted once in AppShell. When any request 401s with a
// stored secret (rotation), it opens OVER the current page; successful
// re-entry invalidates every query so the page recovers in place.
export function ReauthModal() {
  const [open, setOpen] = useState(false);
  const qc = useQueryClient();

  useEffect(() => {
    const onExpired = () => setOpen(true);
    window.addEventListener(AUTH_EXPIRED_EVENT, onExpired);
    return () => window.removeEventListener(AUTH_EXPIRED_EVENT, onExpired);
  }, []);

  if (!open) return null;

  return (
    <Modal title="Session expired" onClose={() => setOpen(false)} size="sm">
      <p className="mb-3 text-xs text-ink-300">
        The gateway secret was rejected — it may have been rotated on the
        server. Enter the current secret to continue where you left off.
      </p>
      <SecretForm
        onSuccess={() => {
          setOpen(false);
          qc.invalidateQueries();
        }}
      />
    </Modal>
  );
}

function SecretForm({
  onSuccess,
  standalone,
}: {
  onSuccess: () => void;
  standalone?: boolean;
}) {
  const [input, setInput] = useState("");
  const [show, setShow] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const form = (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        setError(null);
        setBusy(true);
        setSecret(input.trim());
        try {
          await api.status();
          onSuccess();
        } catch (err) {
          if (err instanceof ApiError && err.status === 401) {
            setError("Secret rejected by the agent.");
          } else if (err instanceof Error) {
            setError(err.message);
          } else {
            setError("Unable to reach the agent.");
          }
        } finally {
          setBusy(false);
        }
      }}
      className={standalone ? "card w-full p-6" : undefined}
    >
      {standalone && (
        <div className="mb-5">
          <h1 className="text-base font-semibold text-ink-50">Sign in</h1>
          <p className="mt-1 text-xs leading-relaxed text-ink-300">
            Paste this deployment's gateway secret to open the console.
          </p>
        </div>
      )}
      <label className="field-label" htmlFor="gateway-secret">
        Gateway secret
      </label>
      {/* Eye toggle INSIDE the field — the detached "Show" button read as a
          second action competing with submit. */}
      <div className="relative mb-3">
        <input
          id="gateway-secret"
          autoFocus
          type={show ? "text" : "password"}
          autoComplete="current-password"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          className="input h-10 pr-10 text-sm"
        />
        <button
          type="button"
          aria-label={show ? "Hide secret" : "Show secret"}
          aria-pressed={show}
          className="absolute right-1 top-1/2 -translate-y-1/2 rounded-control p-2 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
          onClick={() => setShow((s) => !s)}
        >
          {show ? (
            <EyeOff size={14} aria-hidden />
          ) : (
            <Eye size={14} aria-hidden />
          )}
        </button>
      </div>
      {error && (
        <p
          role="alert"
          className="mb-3 flex items-start gap-1.5 text-xs text-sev-critical"
        >
          <AlertCircle size={13} className="mt-px shrink-0" aria-hidden />
          {error}
        </p>
      )}
      <button
        type="submit"
        className="btn btn-primary h-10 w-full justify-center text-sm"
        disabled={busy}
      >
        {busy ? (
          <>
            <Loader2 size={14} className="animate-spin" aria-hidden />
            Verifying…
          </>
        ) : (
          "Sign in"
        )}
      </button>
    </form>
  );

  if (!standalone) return form;

  // Standalone screen: brand block above the card — the first screen
  // anyone sees leads with the product, not a YAML path.
  return (
    <div className="w-full max-w-[380px] animate-[modal-in_200ms_ease-out]">
      <div className="mb-6 flex flex-col items-center gap-3">
        <div className="flex h-12 w-12 items-center justify-center rounded-xl border border-accent/30 bg-accent-subtle shadow-[0_0_24px_rgb(var(--accent)/0.25)]">
          <Triangle
            size={22}
            className="rotate-180 text-link"
            fill="currentColor"
            aria-hidden
          />
        </div>
        <div className="text-center">
          <div className="text-sm font-semibold uppercase tracking-[0.18em] text-ink-50">
            Versus Incident
          </div>
          <div className="mt-0.5 text-2xs text-ink-300">
            Incident Console
          </div>
        </div>
      </div>
      {form}
    </div>
  );
}
