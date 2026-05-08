import { useEffect, useState } from "react";
import { KeyRound } from "lucide-react";
import { getSecret, setSecret, api, ApiError } from "./api";

interface Props {
  children: React.ReactNode;
}

// AuthGate prompts the user once for the X-Gateway-Secret. The value is
// stored in localStorage. We verify it by hitting /api/agent/status before
// granting access — otherwise an invalid secret would only show its 401
// later, deep inside a page.
export function AuthGate({ children }: Props) {
  const [ready, setReady] = useState<"checking" | "needs-secret" | "ok">(
    "checking",
  );
  const [input, setInput] = useState("");
  const [error, setError] = useState<string | null>(null);

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
          // Don't trap the user behind a transient network error.
          setReady("ok");
        }
      });
  }, []);

  if (ready === "checking") {
    return (
      <div className="flex h-full items-center justify-center text-ink-400">
        Connecting…
      </div>
    );
  }

  if (ready === "needs-secret") {
    return (
      <div className="flex h-full items-center justify-center bg-ink-900">
        <form
          onSubmit={async (e) => {
            e.preventDefault();
            setError(null);
            setSecret(input.trim());
            try {
              await api.status();
              setReady("ok");
            } catch (err) {
              if (err instanceof ApiError && err.status === 401) {
                setError("Secret rejected by the agent.");
              } else if (err instanceof Error) {
                setError(err.message);
              } else {
                setError("Unable to reach the agent.");
              }
            }
          }}
          className="w-[380px] rounded-md border border-ink-700 bg-ink-800 p-6 shadow-2xl"
        >
          <div className="mb-4 flex items-center gap-2 text-ink-100">
            <KeyRound size={18} className="text-accent" />
            <h1 className="text-base font-semibold">Versus Admin</h1>
          </div>
          <p className="mb-4 text-xs text-ink-300">
            Enter the gateway secret configured at the root of{" "}
            <code className="rounded bg-ink-700 px-1 py-0.5 text-ink-100">
              gateway_secret
            </code>{" "}
            (env <code>GATEWAY_SECRET</code>).
          </p>
          <input
            autoFocus
            type="password"
            placeholder="X-Gateway-Secret"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            className="mb-3 h-9 w-full rounded-md border border-ink-700 bg-ink-900
                       px-3 text-xs text-ink-100 placeholder:text-ink-500
                       focus:border-accent focus:outline-none
                       focus:ring-2 focus:ring-accent/20"
          />
          {error && (
            <p className="mb-3 text-xs text-bad">{error}</p>
          )}
          <button type="submit" className="btn btn-primary w-full justify-center">
            Continue
          </button>
        </form>
      </div>
    );
  }

  return <>{children}</>;
}
