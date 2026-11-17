// The Export menu's actions: PDF, CSV, Copy summary, Save as monthly report.

export type FinOpsExportAction = "pdf" | "csv" | "copy_summary" | "save_monthly_report";

export type FinOpsExportHandlers = Record<FinOpsExportAction, () => void | Promise<void>>;
