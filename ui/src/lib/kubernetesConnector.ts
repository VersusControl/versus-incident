import { ApiError } from "@/lib/api";

/**
 * Two answers from the Kubernetes controller share a status and, until the
 * server separates them, the same words:
 *
 * - nothing is configured: an operator has never connected a cluster, and the
 *   page owes them a setup card;
 * - a cluster is configured but its credential cannot be resolved: the cluster
 *   published before, the failure must be named, and setup instructions would
 *   hide it.
 *
 * They are never interchangeable, so this module is the one place that decides
 * which answer arrived. A status alone is not evidence, and neither is the
 * message on its own.
 */
export const NOT_CONFIGURED_MESSAGE = "Kubernetes connector is not configured";

/**
 * The controller spells the setup state as a code ending in `not_configured`
 * once it sends one at all. Matching the suffix keeps the check on the server's
 * meaning instead of on a spelling we happened to guess.
 */
const NOT_CONFIGURED_CODE = /not_configured$/;

/** The `code` of a Kubernetes error body, when the answer carries one. */
export function kubernetesErrorCode(body: unknown): string | undefined {
  if (!body || typeof body !== "object") return undefined;
  const code = (body as { code?: unknown }).code;
  return typeof code === "string" && code ? code : undefined;
}

/**
 * True only for the controller's "no connector configured" answer.
 *
 * An answer that names any other code is a fault, whatever its words say: a
 * dead credential and a missing connector were once worded identically, and the
 * code is what now tells them apart. The message is read only when the answer
 * carries no code at all — the shape the controller sends before it separates
 * the two — and even then a caller must corroborate it, because that answer is
 * the same one the dead credential used to produce.
 */
export function isNotConfiguredAnswer(error: unknown): boolean {
  if (!(error instanceof ApiError) || error.status !== 503) return false;
  const code = kubernetesErrorCode(error.body);
  if (code) return NOT_CONFIGURED_CODE.test(code);
  return error.message === NOT_CONFIGURED_MESSAGE;
}

/**
 * Whether the cluster's health report route answered at all.
 *
 * That route is registered ahead of the strict chain and resolves a cluster's
 * configuration without validating its credential, so a cluster whose
 * credential died still gets a report, while a deployment with no connector
 * never gets one. A report is therefore the positive evidence that a cluster is
 * configured — the evidence that tells the two "not configured" answers apart
 * while a server still words them the same.
 */
export function reportProvesConfigured(report: { isSuccess: boolean; isError: boolean; error: unknown }): boolean {
  if (report.isSuccess) return true;
  return report.isError && !isNotConfiguredAnswer(report.error);
}
