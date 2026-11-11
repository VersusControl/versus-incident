import { useQueryClient } from "@tanstack/react-query";
import { api, finopsErrorFrom, finopsExportCsvUrl, finopsKnownError, type FinOpsCsvTable, type FinOpsHeader, type FinOpsQuery } from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { fmtDay, isNotFinal } from "@/lib/finops";
import { useToast } from "@/components/toastContext";
import type { FinOpsExportHandlers } from "./exportHandlers";

const MONTH_KEY = /^\d{4}-(0[1-9]|1[0-2])$/;

/** Opens a download without leaving the page. */
export function downloadUrl(url: string) {
  const a = document.createElement("a");
  a.href = url;
  a.download = "";
  a.rel = "noopener";
  document.body.appendChild(a);
  a.click();
  a.remove();
}

/**
 * Why "Save as monthly report" is not available yet, or undefined when it
 * is. Only a whole, closed month whose bill AWS has closed can be frozen.
 */
export function saveBlockedReason(h: FinOpsHeader | undefined): string | undefined {
  const period = h?.period;
  if (period?.open) return "Available when the month ends";
  if (!period || !MONTH_KEY.test(period.key)) return "Pick a closed month";
  if (isNotFinal(h)) {
    const closes = h?.sync?.bill_closes_on;
    return closes ? `Available after ${fmtDay(closes)}` : "Available after the bill closes";
  }
  return undefined;
}

/**
 * The Export menu's actions. PDF opens the print route and prints; CSV
 * downloads the current table with the active filters; Copy summary copies
 * the server's plain-text summary; Save freezes the month (admins only).
 */
export function useFinOpsExport({
  q,
  header,
  csvTable = "breakdown",
  csvGroupBy,
}: {
  q: FinOpsQuery;
  header: FinOpsHeader | undefined;
  csvTable?: FinOpsCsvTable;
  csvGroupBy?: FinOpsQuery["group_by"];
}): FinOpsExportHandlers {
  const toast = useToast();
  const qc = useQueryClient();
  const periodKey = header?.period?.key ?? q.period ?? "mtd";
  const scope: FinOpsQuery = { ...q, period: periodKey };

  return {
    pdf: () => {
      const params = new URLSearchParams({ period: periodKey, print: "1" });
      if (q.compare) params.set("compare", q.compare);
      window.open(`/aws/finops/report?${params}`, "_blank", "noopener");
    },
    csv: () => downloadUrl(finopsExportCsvUrl({ ...scope, group_by: csvGroupBy }, csvTable)),
    copy_summary: async () => {
      try {
        const report = await api.finopsReport(scope);
        const text = report.copy_summary || report.summary.answer;
        const ok = text !== "" && (await copyText(text));
        toast.push({ tone: ok ? "ok" : "error", title: ok ? "Summary copied" : "Copy failed" });
      } catch (e) {
        toast.push({ tone: "error", title: "Copy failed", description: finopsKnownError(e)?.title });
      }
    },
    save_monthly_report: async () => {
      try {
        await api.finopsFreezeReport(periodKey);
        void qc.invalidateQueries({ queryKey: ["finops"] });
        toast.push({ tone: "ok", title: "Monthly report saved" });
      } catch (e) {
        const info = finopsErrorFrom(e, "local");
        toast.push({ tone: "error", title: info.title, description: info.detail });
      }
    },
  };
}
