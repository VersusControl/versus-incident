// Package cost is the AWS adapter of the cloud-neutral FinOps core. It reads
// AWS Cost Explorer, Cost Optimization Hub, AWS Budgets and the cost
// allocation tag list through pinned, read-only clients, maps what it reads
// into finops.CostLine and the neutral savings and budget inputs, and keeps
// the result in the FinOps tables (storage.FinOpsStore): cost lines per
// payer and day, increases with their status history, frozen reports and
// saving decisions. The paid-request meter, per-payer refresh state, UI
// budgets and budget-alert levels stay small blobs.
//
// Paid Cost Explorer requests are planned and priced before the first call
// and every request, each page included, is taken from one daily meter that
// enforces the deployment's cap. A day is written only when every required
// call for it succeeded; otherwise the day keeps its previous data and the
// source is marked stale. Errors are reported in a closed vocabulary and
// never echo provider text.
//
// Source implements finops.CostSource over the cache only, so the FinOps
// service and every agent tool built on it never call AWS. SignalSource
// emits new cost increases through the existing alert path.
package cost
