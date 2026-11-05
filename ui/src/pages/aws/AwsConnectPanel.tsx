import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import clsx from "clsx";
import {
  AlertTriangle,
  Check,
  Download,
  ExternalLink,
  Loader2,
  Lock,
  X,
  XCircle,
} from "lucide-react";
import { CopyField } from "@/components/CopyField";
import { Pill } from "@/components/Pill";
import { SecretReveal } from "@/components/SecretReveal";
import { Stepper } from "@/components/Stepper";
import { EnterpriseLockedBody } from "@/components/EnterpriseLocked";
import { useToast } from "@/components/toastContext";
import {
  api,
  awsErrorFrom,
  awsErrorText,
  type AwsConnectDraft,
  type AwsConnectMethod,
  type AwsDraftService,
  type AwsErrorInfo,
  type AwsRegionOption,
  type AwsSettings,
} from "@/lib/api";
import {
  CONNECT_METHODS,
  CONNECT_STEPS,
  accountIdFromRoleArn,
  assumeWords,
  defaultAccountName,
  deniedFixes,
  deriveDraftCounts,
  draftIsRunning,
  methodLabel,
  regionLabel,
} from "@/lib/awsSettings";
import { AwsDeniedFix } from "./AwsDeniedFix";
import { AwsServiceAccessGrid } from "./AwsServiceAccessGrid";

const ROLE_ARN_PATTERN = /^arn:aws[a-z-]*:iam::\d{12}:role\/.+$/;

function maskedParts(masked: string): { prefix: string; suffix: string } {
  const match = masked.match(/^(.*?)([•*·…]+)(.*)$/);
  if (!match) return { prefix: "", suffix: "" };
  return { prefix: match[1], suffix: match[3] };
}

function quickCreateUrl(templateUrl: string): string {
  const params = new URLSearchParams({
    templateURL: templateUrl,
    stackName: "versus-reader",
  });
  return `https://console.aws.amazon.com/cloudformation/home#/stacks/quickcreate?${params.toString()}`;
}

// AwsConnectPanel — the three-step connect wizard. It can sit in the desktop
// right column or inside a PeekPanel; the steps, data and gating are identical.
//
// The panel never writes for a viewer: it renders the read-only notice instead
// and skips every draft/reveal call.
export function AwsConnectPanel({
  settings,
  canWrite,
  mode = "new",
  reconnectRoleArn,
  onClose,
  onCommitted,
  layout = "column",
}: {
  settings: AwsSettings;
  canWrite: boolean;
  mode?: "new" | "reconnect";
  /** Stored role ARN, injected at step 2 when reconnecting. */
  reconnectRoleArn?: string;
  onClose: () => void;
  onCommitted: () => void;
  layout?: "column" | "peek";
}) {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [step, setStep] = useState(mode === "reconnect" ? 1 : 0);
  const [method, setMethod] = useState<AwsConnectMethod>(
    mode === "reconnect" ? "existing_role" : "cloudformation",
  );
  const [draft, setDraft] = useState<AwsConnectDraft | null>(null);
  const [draftId, setDraftId] = useState<string | null>(null);
  const [roleArn, setRoleArn] = useState(reconnectRoleArn ?? "");
  const [editingArn, setEditingArn] = useState(mode !== "reconnect");
  const [verifying, setVerifying] = useState(false);
  const [error, setError] = useState<AwsErrorInfo | null>(null);
  const [regions, setRegions] = useState<AwsRegionOption[] | null>(null);
  const [selectedRegions, setSelectedRegions] = useState<string[]>([]);
  const [accountName, setAccountName] = useState(
    reconnectRoleArn ? defaultAccountName(reconnectRoleArn) : "",
  );
  const started = useRef(false);

  // One draft per panel open. Reconnect starts it in existing-role mode and
  // verifies the stored ARN at once, which lands the operator on check access.
  useEffect(() => {
    if (!canWrite || started.current) return;
    started.current = true;
    let cancelled = false;
    void (async () => {
      const initialMethod: AwsConnectMethod =
        mode === "reconnect" ? "existing_role" : "cloudformation";
      try {
        const created = await api.createAwsDraft({ method: initialMethod });
        if (cancelled) return;
        setDraft(created);
        setDraftId(created.draft_id);
        if (mode === "reconnect" && reconnectRoleArn) {
          setRoleArn(reconnectRoleArn);
          await api.verifyAwsDraft(created.draft_id, { role_arn: reconnectRoleArn });
          if (cancelled) return;
          setVerifying(true);
          void queryClient.invalidateQueries({
            queryKey: ["aws", "draft", created.draft_id],
          });
        }
      } catch (e) {
        if (!cancelled) setError(awsErrorFrom(e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [canWrite, mode, reconnectRoleArn, queryClient]);

  // While a check runs, poll the draft once a second; stop as soon as every
  // probe has landed so a settled screen is quiet.
  const draftQuery = useQuery<AwsConnectDraft>({
    queryKey: ["aws", "draft", draftId ?? "none"],
    queryFn: () => api.awsDraft(draftId as string),
    enabled: canWrite && Boolean(draftId) && step >= 1,
    retry: 1,
    refetchInterval: (query) => {
      const data = query.state.data;
      if (!data?.assume) return 1000;
      return draftIsRunning(data.services ?? []) ? 1000 : false;
    },
  });

  const current = draftQuery.data ?? draft;
  const services: AwsDraftService[] = current?.services ?? [];
  const counts = deriveDraftCounts(services);
  const assume = current?.assume ?? null;
  const arnValid = ROLE_ARN_PATTERN.test(roleArn.trim());
  const assumeOk = assume?.ok === true;
  const probing = verifying && !assume;
  const checkedAll = assumeOk && !draftIsRunning(services);

  const verify = useMutation({
    mutationFn: () =>
      api.verifyAwsDraft(draftId as string, { role_arn: roleArn.trim() }),
    onSuccess: () => {
      setError(null);
      setVerifying(true);
      // Once the role is assumed the ARN is a copied fact, not a form field.
      setEditingArn(false);
      void queryClient.invalidateQueries({ queryKey: ["aws", "draft", draftId] });
    },
    onError: (e) => setError(awsErrorFrom(e)),
  });

  const discover = useMutation({
    mutationFn: () => api.discoverAwsRegions(draftId as string),
    onSuccess: (options) => {
      setRegions(options);
      const withResources = options
        .filter((r) => r.enabled && r.resources_found > 0)
        .map((r) => r.region);
      const fallback = options.find((r) => r.enabled)?.region;
      setSelectedRegions(
        withResources.length > 0 ? withResources : fallback ? [fallback] : [],
      );
    },
    onError: (e) => setError(awsErrorFrom(e)),
  });

  const discoverRegions = discover.mutate;
  useEffect(() => {
    if (step !== 2 || regions !== null || !draftId || !canWrite) return;
    discoverRegions();
  }, [step, regions, draftId, canWrite, discoverRegions]);

  const commit = useMutation({
    mutationFn: () =>
      api.createAwsAccount({
        draft_id: draftId as string,
        display_name: accountName.trim(),
        regions: selectedRegions,
      }),
    onSuccess: async () => {
      toast.push({ tone: "ok", title: "Account connected" });
      await queryClient.invalidateQueries({ queryKey: ["aws", "settings"] });
      onCommitted();
    },
    onError: (e) => setError(awsErrorFrom(e)),
  });

  // Entering regions names the account from the role ARN unless the operator
  // already typed a name (reconnect starts with one).
  const goToRegions = () => {
    if (!accountName.trim()) setAccountName(defaultAccountName(roleArn));
    setStep(2);
  };

  const ensureDraft = async (next: AwsConnectMethod): Promise<AwsConnectDraft | null> => {
    if (draft && draft.method === next) return draft;
    const created = await api.createAwsDraft({ method: next });
    setDraft(created);
    setDraftId(created.draft_id);
    return created;
  };

  const continueFromMethod = async () => {
    if (!settings.principal_arn) return;
    setError(null);
    try {
      const created = await ensureDraft(method);
      if (created) setStep(1);
    } catch (e) {
      setError(awsErrorFrom(e));
    }
  };

  const organizationsLocked =
    method === "organizations" && !settings.capabilities.organizations;
  const canLeaveMethod =
    Boolean(settings.principal_arn) && !organizationsLocked;

  if (!canWrite) {
    return (
      <section className={clsx(layout === "column" && "card")}>
        <div className="card-header">
          <h2 className="card-title">Connect account</h2>
        </div>
        <div className="card-body text-xs text-ink-300">
          <p className="font-medium text-ink-100">Read-only</p>
          <p className="mt-1">
            Connecting an AWS account requires the admin role. You can still see
            every account and what it can read.
          </p>
          <button type="button" className="btn mt-3" onClick={onClose}>
            Close
          </button>
        </div>
      </section>
    );
  }

  const failures = deniedFixes(services, roleArn);
  const assumeFailed = assume !== null && assume.ok === false;

  return (
    <section
      className={clsx("min-w-0", layout === "column" && "card")}
      aria-labelledby="aws-connect-title"
    >
      {layout === "column" && (
        <div className="flex items-start justify-between gap-2 px-4 pt-3">
          <div className="min-w-0">
            <h2 id="aws-connect-title" className="card-title">
              Connect account
            </h2>
            <p className="mt-0.5 text-2xs text-ink-300">
              Read-only access, three steps.
            </p>
          </div>
          <button
            type="button"
            className="btn-icon"
            aria-label="Close connect panel"
            title="Close"
            onClick={onClose}
          >
            <X size={15} aria-hidden="true" />
          </button>
        </div>
      )}
      <div className="px-4 pt-3">
        <Stepper steps={CONNECT_STEPS} current={step} label="Connect account progress" />
      </div>

      <div className="min-w-0 space-y-4 p-4">
        {error && (
          <div
            role="alert"
            className="rounded-control border border-sev-critical/40 bg-sev-critical/10 p-2.5 text-xs"
          >
            <p className="flex items-center gap-1.5 font-medium text-sev-critical">
              <AlertTriangle size={13} aria-hidden="true" />
              {error.title}
            </p>
            <p className="mt-1 text-ink-200">{error.detail}</p>
            {error.fix && <p className="mt-1 text-ink-200">Fix: {error.fix}</p>}
          </div>
        )}

        {step === 0 && (
          <>
            {!settings.principal_arn && (
              <div className="rounded-control border border-sev-warn/40 bg-sev-warn/10 p-2.5 text-xs">
                <p className="flex items-center gap-1.5 font-medium text-sev-warn">
                  <AlertTriangle size={13} aria-hidden="true" />
                  The Versus server has no AWS identity
                </p>
                <p className="mt-1 text-ink-200">
                  Versus assumes your role from the identity of the machine it
                  runs on. Attach an instance profile, an ECS task role or a
                  shared profile that may call{" "}
                  <code className="font-mono text-2xs text-ink-50">sts:AssumeRole</code>
                  , then reload this page.
                </p>
              </div>
            )}

            <fieldset className="min-w-0">
              <legend className="field-label">How do you want to connect?</legend>
              <div className="space-y-1.5">
                {CONNECT_METHODS.map((option) => {
                  const selected = method === option.id;
                  const locked =
                    option.id === "organizations" &&
                    !settings.capabilities.organizations;
                  return (
                    <label
                      key={option.id}
                      className={clsx(
                        "flex cursor-pointer items-start gap-2 rounded-control border p-2 text-xs",
                        selected
                          ? "border-accent bg-accent-subtle/40"
                          : "border-ink-500 hover:bg-ink-600/40",
                      )}
                    >
                      <input
                        type="radio"
                        name="aws-connect-method"
                        value={option.id}
                        checked={selected}
                        onChange={() => setMethod(option.id)}
                        className="mt-0.5"
                      />
                      <span className="min-w-0">
                        <span className="flex flex-wrap items-center gap-1 font-medium text-ink-100">
                          {option.label}
                          {option.id === "cloudformation" && (
                            <Pill tone="accent">Recommended</Pill>
                          )}
                          {locked && (
                            <span className="inline-flex items-center gap-0.5 text-2xs text-ink-300">
                              <Lock size={11} aria-hidden="true" />
                              Enterprise
                            </span>
                          )}
                        </span>
                        <span className="mt-0.5 block text-2xs text-ink-300">
                          {option.detail}
                        </span>
                      </span>
                    </label>
                  );
                })}
              </div>
            </fieldset>

            {organizationsLocked && (
              <EnterpriseLockedBody title="AWS Organizations is an Enterprise feature">
                Connecting every account from AWS Organizations needs the
                Enterprise edition. A single account can be connected with any
                method above.
              </EnterpriseLockedBody>
            )}

            {settings.principal_arn && (
              <div className="min-w-0">
                <p className="mb-1 text-2xs text-ink-300">
                  The role trusts this Versus principal
                </p>
                <CopyField
                  label="Versus principal ARN"
                  value={settings.principal_arn}
                />
              </div>
            )}

            {method === "cloudformation" && (
              <div className="min-w-0 space-y-1.5 text-xs">
                {current?.template_url ? (
                  <>
                    <a
                      className="btn"
                      href={quickCreateUrl(current.template_url)}
                      target="_blank"
                      rel="noreferrer"
                    >
                      Open CloudFormation quick-create
                      <ExternalLink size={12} aria-hidden="true" />
                    </a>
                    <a
                      className="btn"
                      href={current.template_url}
                      download="versus-reader.yaml"
                    >
                      <Download size={12} aria-hidden="true" />
                      Download template
                    </a>
                  </>
                ) : (
                  <p className="text-2xs text-ink-300">
                    {current
                      ? "The template URL is not configured on this server. Use Terraform or an existing role, or download the template from the release page."
                      : "Preparing the template…"}
                  </p>
                )}
                <p className="text-2xs text-ink-300">
                  The console asks for the ExternalId — continue to Check access
                  to reveal it.
                </p>
              </div>
            )}

            {method === "terraform" && (
              <div className="min-w-0">
                {current?.terraform_snippet ? (
                  <CopyField label="Terraform module" value={current.terraform_snippet} />
                ) : (
                  <p className="text-2xs text-ink-300">Preparing the module…</p>
                )}
              </div>
            )}

            {method === "existing_role" && (
              <p className="text-2xs text-ink-300">
                You will paste the role ARN next. Its trust policy must allow the
                Versus principal above with the ExternalId this draft generates.
              </p>
            )}

            {method === "local_profile" && (
              <p className="text-2xs text-ink-300">
                For development only. Versus reads the profile named in the
                server configuration; production should use a role.
              </p>
            )}
          </>
        )}

        {step === 1 && (
          <>
            {!settings.principal_arn && (
              <div className="rounded-control border border-sev-warn/40 bg-sev-warn/10 p-2.5 text-xs">
                <p className="flex items-center gap-1.5 font-medium text-sev-warn">
                  <AlertTriangle size={13} aria-hidden="true" />
                  The Versus server has no AWS identity
                </p>
                <p className="mt-1 text-ink-200">
                  Give Versus an instance profile, task role or profile first,
                  then reopen this panel.
                </p>
              </div>
            )}

            {editingArn ? (
              <div>
                <label htmlFor="aws-role-arn" className="field-label">
                  Role ARN
                </label>
                <input
                  id="aws-role-arn"
                  className="input font-mono"
                  value={roleArn}
                  spellCheck={false}
                  onChange={(e) => setRoleArn(e.target.value)}
                  placeholder="arn:aws:iam::123456789012:role/versus-reader"
                />
                <p className="mt-1 text-2xs text-ink-300">
                  Paste the ARN of the read-only role Versus should assume.
                </p>
              </div>
            ) : (
              <div className="flex items-end gap-2">
                <CopyField label="Role ARN" value={roleArn} className="flex-1" />
                <button
                  type="button"
                  className="btn mb-0.5"
                  onClick={() => setEditingArn(true)}
                >
                  Change
                </button>
              </div>
            )}

            {draftId && roleArn.trim() && (
              <div className="min-w-0">
                <SecretReveal
                  label="External ID"
                  prefix={maskedParts(current?.external_id_masked ?? "").prefix}
                  suffix={maskedParts(current?.external_id_masked ?? "").suffix}
                  reveal={() => api.revealAwsExternalId(draftId)}
                />
                <p className="mt-1 text-2xs text-ink-300">
                  Add this to the role's trust policy condition (sts:ExternalId).
                </p>
              </div>
            )}

            <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                className="btn"
                onClick={() => {
                  setError(null);
                  verify.mutate();
                }}
                disabled={!arnValid || verify.isPending || probing}
              >
                {verify.isPending || probing ? (
                  <Loader2
                    size={12}
                    aria-hidden="true"
                    className="animate-spin motion-reduce:animate-none"
                  />
                ) : null}
                {verify.isPending || probing ? "Checking access…" : "Check access"}
              </button>
              {!arnValid && roleArn.trim().length > 0 && (
                <span className="text-2xs text-ink-300">
                  That does not look like a role ARN.
                </span>
              )}
            </div>

            {(probing || assume) && (
              <div className="min-w-0 space-y-2">
                {assume?.ok ? (
                  <p
                    role="status"
                    className="flex items-center gap-1.5 text-xs text-sev-ok"
                  >
                    <Check size={13} aria-hidden="true" />
                    {assumeWords(assume)}
                  </p>
                ) : assume && !assume.ok ? (
                  <AssumeFailure errorCode={assume.error_code} />
                ) : (
                  <p
                    role="status"
                    className="flex items-center gap-1.5 text-xs text-ink-300"
                  >
                    <Loader2
                      size={13}
                      aria-hidden="true"
                      className="animate-spin motion-reduce:animate-none"
                    />
                    Assuming the role…
                  </p>
                )}

                <div>
                  <h3 className="mb-1.5 text-xs font-medium text-ink-100">
                    Service access
                  </h3>
                  <AwsServiceAccessGrid
                    services={services}
                    columns={2}
                    assumeFailed={assumeFailed}
                  />
                </div>

                {failures.length > 0 && (
                  <div className="space-y-1.5" aria-live="polite">
                    {failures.map((failure) => (
                      <AwsDeniedFix
                        key={failure.service}
                        label={failure.label}
                        fix={failure.fix}
                        actions={failure.actions}
                        policy={current?.fix_policy}
                      />
                    ))}
                  </div>
                )}

                {assumeOk && !checkedAll && (
                  <p className="text-2xs text-ink-300" aria-live="polite">
                    {counts.pending} of {counts.total} services still checking.
                  </p>
                )}
              </div>
            )}
          </>
        )}

        {step === 2 && (
          <>
            <div>
              <label htmlFor="aws-account-name" className="field-label">
                Account name
              </label>
              <input
                id="aws-account-name"
                className="input"
                value={accountName}
                onChange={(e) => setAccountName(e.target.value)}
                placeholder="Production"
              />
              {accountIdFromRoleArn(roleArn) && (
                <p className="mt-1 font-mono text-2xs text-ink-300">
                  {accountIdFromRoleArn(roleArn)}
                </p>
              )}
            </div>

            <div className="min-w-0">
              <h3 className="mb-1.5 text-xs font-medium text-ink-100">Regions</h3>
              {regions === null ? (
                <p
                  role="status"
                  className="flex items-center gap-1.5 text-xs text-ink-300"
                >
                  <Loader2
                    size={13}
                    aria-hidden="true"
                    className="animate-spin motion-reduce:animate-none"
                  />
                  Discovering regions…
                </p>
              ) : regions.length === 0 ? (
                <p className="text-xs text-ink-300">No regions came back from AWS.</p>
              ) : (
                <ul aria-label="Regions" className="space-y-1 text-xs">
                  {regions.map((option) => {
                    const checked = selectedRegions.includes(option.region);
                    return (
                      <li key={option.region}>
                        <label
                          className={clsx(
                            "flex flex-wrap items-center gap-2",
                            option.enabled ? "cursor-pointer" : "cursor-not-allowed",
                          )}
                        >
                          <input
                            type="checkbox"
                            checked={checked}
                            disabled={!option.enabled}
                            onChange={() =>
                              setSelectedRegions((currentRegions) =>
                                checked
                                  ? currentRegions.filter((r) => r !== option.region)
                                  : [...currentRegions, option.region],
                              )
                            }
                            aria-label={`${regionLabel(option.region)} (${option.region})`}
                          />
                          <span
                            className={clsx(
                              "text-ink-100",
                              !option.enabled && "text-ink-300",
                            )}
                          >
                            {regionLabel(option.region)}
                          </span>
                          <span className="font-mono text-2xs text-ink-300">
                            {option.region}
                          </span>
                          {!option.enabled ? (
                            <Pill tone="warn">Not enabled</Pill>
                          ) : option.resources_found > 0 ? (
                            <span className="text-2xs text-ink-300">
                              {option.resources_found} resources found
                            </span>
                          ) : (
                            <span className="text-2xs text-ink-300">
                              no resources found
                            </span>
                          )}
                        </label>
                      </li>
                    );
                  })}
                </ul>
              )}
              <p className="mt-1.5 text-2xs text-ink-300">
                Only enabled regions can be selected. Regions with resources are
                preselected.
              </p>
            </div>

            <p className="text-2xs text-ink-300">
              {methodLabel(method)} · reads only, nothing is written to the
              account.
            </p>
          </>
        )}
      </div>

      <div className="flex flex-wrap items-center justify-end gap-2 border-t border-ink-500/40 px-4 py-3">
        {step > 0 && (
          <button type="button" className="btn" onClick={() => setStep(step - 1)}>
            Back
          </button>
        )}
        {step === 0 && (
          <>
            <button type="button" className="btn" onClick={onClose}>
              Cancel
            </button>
            <button
              type="button"
              className="btn btn-primary-strong"
              disabled={!canLeaveMethod}
              title={
                !settings.principal_arn
                  ? "The server needs an AWS identity first"
                  : organizationsLocked
                    ? "Organizations needs the Enterprise edition"
                    : undefined
              }
              onClick={() => void continueFromMethod()}
            >
              Continue
            </button>
          </>
        )}
        {step === 1 && (
          <>
            {checkedAll && counts.denied > 0 && (
              <button type="button" className="btn" onClick={goToRegions}>
                You can fix this later
              </button>
            )}
            <button
              type="button"
              className="btn btn-primary-strong"
              disabled={!checkedAll}
              title={!checkedAll ? "Finish checking access first" : undefined}
              onClick={goToRegions}
            >
              Continue to regions
            </button>
          </>
        )}
        {step === 2 && (
          <button
            type="button"
            className="btn btn-primary-strong"
            disabled={
              commit.isPending ||
              regions === null ||
              selectedRegions.length === 0 ||
              accountName.trim().length === 0
            }
            onClick={() => commit.mutate()}
          >
            {commit.isPending ? "Connecting…" : "Connect account"}
          </button>
        )}
      </div>
    </section>
  );
}

// The assume failure in the operator's words: what happened, and the exact
// change that fixes it. AWS's own message never reaches this box.
function AssumeFailure({ errorCode }: { errorCode?: string | null }) {
  const info = awsErrorText(errorCode);
  return (
    <div
      role="alert"
      className="rounded-control border border-sev-critical/40 bg-sev-critical/10 p-2.5 text-xs"
    >
      <p className="flex items-center gap-1.5 font-medium text-sev-critical">
        <XCircle size={13} aria-hidden="true" />
        {info.title}
      </p>
      <p className="mt-1 text-ink-200">{info.detail}</p>
      {info.fix && <p className="mt-1 text-ink-200">Fix: {info.fix}</p>}
    </div>
  );
}
