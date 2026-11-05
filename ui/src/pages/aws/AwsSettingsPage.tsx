import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import clsx from "clsx";
import { RefreshCw, ShieldAlert } from "lucide-react";
import { TopBar } from "@/components/TopBar";
import { Breadcrumb } from "@/components/Breadcrumb";
import { FreshnessIndicator } from "@/components/FreshnessIndicator";
import { PeekPanel } from "@/components/PeekPanel";
import { AdminAccessNotice } from "@/components/AdminAccessNotice";
import { CopyField } from "@/components/CopyField";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { adminGateState } from "@/lib/role";
import { ApiError, api, type AwsAccount, type AwsSettings } from "@/lib/api";
import { awsSettingsAnswer, type AwsAnswerTone } from "@/lib/awsSettings";
import { AwsAccountsCard } from "./AwsAccountsCard";
import { AwsPrivacyCard } from "./AwsPrivacyCard";
import { AwsAdvanced } from "./AwsAdvanced";
import { AwsConnectPanel } from "./AwsConnectPanel";

const ANSWER_TONE: Record<AwsAnswerTone, string> = {
  plain: "text-ink-100",
  ok: "text-sev-ok",
  warn: "text-sev-warn",
  critical: "text-sev-critical",
  muted: "text-ink-300",
};

const CONFIG_KEY = "connectors.aws.enable";
const ENV_VAR = "VERSUS_CONNECTORS_AWS_ENABLE";

type ConnectState =
  | { mode: "new" }
  | { mode: "reconnect"; account: AwsAccount };

function useMinWidth(query: string): boolean {
  const [matches, setMatches] = useState(() =>
    typeof window !== "undefined" && typeof window.matchMedia === "function"
      ? window.matchMedia(query).matches
      : true,
  );
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const mql = window.matchMedia(query);
    const onChange = () => setMatches(mql.matches);
    onChange();
    mql.addEventListener?.("change", onChange);
    return () => mql.removeEventListener?.("change", onChange);
  }, [query]);
  return matches;
}

// Age of the most recent successful health check, for the header indicator.
function latestCheck(settings: AwsSettings): {
  ageSeconds?: number;
  disconnected: boolean;
} {
  let newest: number | undefined;
  let disconnected = false;
  for (const account of settings.accounts) {
    if (account.status === "disconnected") disconnected = true;
    if (account.checked_at) {
      const t = Date.parse(account.checked_at);
      if (Number.isFinite(t) && (newest === undefined || t > newest)) newest = t;
    }
  }
  return {
    ageSeconds:
      newest === undefined ? undefined : Math.max(0, (Date.now() - newest) / 1000),
    disconnected,
  };
}

// AWS connector settings. The page states the worst problem in one sentence,
// then shows the accounts, the privacy contract and the connect wizard. A
// viewer sees the same facts with every write control removed.
export function AwsSettingsPage() {
  const access = useEffectiveRole();
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });
  const settings = useQuery({
    queryKey: ["aws", "settings"],
    queryFn: api.awsSettings,
    refetchInterval: 15_000,
    retry: 1,
  });
  const wide = useMinWidth("(min-width: 1280px)");
  const [connect, setConnect] = useState<ConnectState | null>(null);

  const data = settings.data;
  // The server's capability is authoritative; the role gate removes a
  // signed-in viewer or a session-less operator. The OSS "locked" gate is the
  // gateway-secret operator, who may write when the server says so.
  const canWrite =
    data?.capabilities.can_write === true &&
    gate !== "loading" &&
    gate !== "sign-in" &&
    gate !== "read-only";

  // First run: with no accounts, open the wizard at step one so the empty
  // screen offers the one action that matters.
  useEffect(() => {
    if (!data || !data.enabled || !canWrite) return;
    if (data.accounts.length === 0) {
      setConnect((currentConnect) => currentConnect ?? { mode: "new" });
    }
  }, [data, canWrite]);

  // Escape closes the panel when it sits in the desktop column. The PeekPanel
  // route below owns its own Escape handling.
  const panelOpen = connect !== null;
  useEffect(() => {
    if (!panelOpen || !wide) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || document.body.dataset.modalDepth) return;
      e.stopPropagation();
      setConnect(null);
    };
    document.addEventListener("keydown", onKey, true);
    return () => document.removeEventListener("keydown", onKey, true);
  }, [panelOpen, wide]);

  const openConnect = () => setConnect({ mode: "new" });
  const reconnect = (account: AwsAccount) =>
    setConnect({ mode: "reconnect", account });
  const closeConnect = () => setConnect(null);

  const freshness = data ? latestCheck(data) : undefined;
  const headerActions =
    data?.enabled && data.accounts.length > 0 ? (
      <>
        <FreshnessIndicator
          state={freshness?.disconnected ? "disconnected" : "live"}
          ageSeconds={freshness?.disconnected ? undefined : freshness?.ageSeconds}
        />
        <button
          type="button"
          className="btn-icon"
          aria-label="Refresh AWS settings"
          title="Refresh"
          onClick={() => void settings.refetch()}
        >
          <RefreshCw
            size={14}
            aria-hidden="true"
            className={
              settings.isFetching
                ? "animate-spin motion-reduce:animate-none"
                : undefined
            }
          />
        </button>
      </>
    ) : undefined;

  return (
    <main className="flex min-h-0 flex-1 flex-col overflow-auto">
      <TopBar title="Settings" subtitle="AWS" actions={headerActions} />
      <div className="mx-auto w-full max-w-2xl p-4 lg:p-6">
        <Breadcrumb
          items={[{ label: "AWS", to: "/aws" }, { label: "Settings" }]}
          className="mb-3"
        />

        {settings.isPending ? (
          <SkCard lines={5} />
        ) : settings.isError ? (
          <SettingsError
            error={settings.error}
            onRetry={() => void settings.refetch()}
            retrying={settings.isFetching}
          />
        ) : !data ? (
          <SkCard lines={3} />
        ) : !data.enabled ? (
          <OffNotice data={data} />
        ) : (
          <>
            <AnswerLine data={data} />

            {gate === "read-only" && (
              <div className="mt-3">
                <AdminAccessNotice reason="role" />
              </div>
            )}
            {gate === "sign-in" && (
              <div className="mt-3">
                <AdminAccessNotice reason="sign-in" />
              </div>
            )}
            {gate !== "read-only" && gate !== "sign-in" && !canWrite && (
              <p className="mt-3 flex items-center gap-1.5 text-2xs text-ink-300">
                <ShieldAlert size={12} aria-hidden="true" />
                Read-only: you can view AWS settings but not change them.
              </p>
            )}

            <div
              className={clsx(
                "mt-4 grid min-w-0 gap-4",
                panelOpen && wide && "xl:grid-cols-[minmax(0,1fr)_minmax(26rem,32rem)]",
              )}
            >
              <div className="min-w-0 space-y-4">
                <AwsAccountsCard
                  settings={data}
                  canWrite={canWrite}
                  onConnect={openConnect}
                  onReconnect={reconnect}
                />
                <AwsPrivacyCard privacy={data.privacy} />
                <AwsAdvanced settings={data} canWrite={canWrite} />
              </div>

              {panelOpen && wide && (
                <div className="hidden min-w-0 xl:block">
                  <div className="sticky top-0">
                    <AwsConnectPanel
                      key={
                        connect?.mode === "reconnect"
                          ? connect.account.account_id
                          : "new"
                      }
                      settings={data}
                      canWrite={canWrite}
                      mode={connect?.mode ?? "new"}
                      reconnectRoleArn={
                        connect?.mode === "reconnect"
                          ? connect.account.role_arn
                          : undefined
                      }
                      onClose={closeConnect}
                      onCommitted={closeConnect}
                      layout="column"
                    />
                  </div>
                </div>
              )}
            </div>
          </>
        )}
      </div>

      {panelOpen && !wide && data && (
        <PeekPanel
          open
          onClose={closeConnect}
          title={<span id="aws-connect-title">Connect account</span>}
          headingLabel="Connect account"
          ariaLabel="Connect an AWS account"
          size="wide"
          appearance="resource"
        >
          <AwsConnectPanel
            key={
              connect?.mode === "reconnect" ? connect.account.account_id : "new"
            }
            settings={data}
            canWrite={canWrite}
            mode={connect?.mode ?? "new"}
            reconnectRoleArn={
              connect?.mode === "reconnect" ? connect.account.role_arn : undefined
            }
            onClose={closeConnect}
            onCommitted={closeConnect}
            layout="peek"
          />
        </PeekPanel>
      )}
    </main>
  );
}

function AnswerLine({ data }: { data: AwsSettings }) {
  const answer = awsSettingsAnswer(data);
  return (
    <p
      role="status"
      aria-live="polite"
      data-answer-kind={answer.kind}
      className="text-sm leading-6"
    >
      {answer.segments.map((segment, index) => (
        <span key={index} className={ANSWER_TONE[segment.tone]}>
          {segment.text}
        </span>
      ))}
    </p>
  );
}

function OffNotice({ data }: { data: AwsSettings }) {
  const configKey = data.enable_hint?.config_key ?? CONFIG_KEY;
  const envVar = data.enable_hint?.env_var ?? ENV_VAR;
  return (
    <>
      <AnswerLine data={data} />
      <section className="card mt-4" aria-labelledby="aws-off-title">
        <div className="card-header">
          <h2 id="aws-off-title" className="card-title">
            AWS connector is off
          </h2>
        </div>
        <div className="card-body space-y-3 text-xs text-ink-200">
          <p>
            Enable the connector in the server configuration, then restart
            Versus. The AWS Harness reads nothing while it is off.
          </p>
          <CopyField label="Config key" value={`${configKey}: true`} />
          <CopyField label="Environment variable" value={`${envVar}=true`} />
          <p className="text-2xs text-ink-300">
            The environment variable overrides the config file. Both are read at
            startup.
          </p>
        </div>
      </section>
    </>
  );
}

function SettingsError({
  error,
  onRetry,
  retrying,
}: {
  error: unknown;
  onRetry: () => void;
  retrying: boolean;
}) {
  if (error instanceof ApiError && error.status === 404) {
    return (
      <section className="card" aria-labelledby="aws-unavailable-title">
        <div className="card-header">
          <h2 id="aws-unavailable-title" className="card-title">
            AWS settings are not available
          </h2>
        </div>
        <div className="card-body text-xs text-ink-200">
          This server build does not expose the AWS settings API. Upgrade Versus
          to a build with the AWS Harness.
        </div>
      </section>
    );
  }
  if (error instanceof ApiError && error.status === 403) {
    return (
      <section className="card" aria-labelledby="aws-denied-title">
        <div className="card-header">
          <h2 id="aws-denied-title" className="card-title">
            AWS settings are restricted
          </h2>
        </div>
        <div className="card-body text-xs text-ink-200">
          Your role cannot view the AWS connector. An owner or admin can grant
          access.
        </div>
      </section>
    );
  }
  return (
    <RetryableError
      error={error}
      onRetry={onRetry}
      retrying={retrying}
      context="Couldn't load AWS settings"
    />
  );
}
