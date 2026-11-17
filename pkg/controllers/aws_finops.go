package controllers

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/aws/cost"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

const finopsBodyLimit = 64 << 10

// refreshInProgressRetryAfter is the Retry-After, in seconds, of every
// refresh_in_progress answer: FinOps "Refresh now" and drill-down, and an
// AWS settings write that met a running cost refresh or a concurrent account
// change.
const refreshInProgressRetryAfter = "5"

// FinOpsBackend is what the FinOps routes need: the service over the cost
// cache and the paid operations only these admin routes may start.
type FinOpsBackend interface {
	Service() *finops.Service
	Estimate(ctx context.Context, kind cost.Kind) (finops.Plan, error)
	RefreshNow(ctx context.Context) (cost.RefreshResult, error)
	DrillDown(ctx context.Context, account, awsService string) error
}

// AWSFinOpsController adapts finops.Service to the admin API. Reads are the
// same service methods the agent tools call; writes and paid calls are
// admin-only and audited, never tools.
type AWSFinOpsController struct {
	backend FinOpsBackend
}

// NewAWSFinOpsController builds the controller. A nil backend (the AWS
// connector is off) answers every read with enabled=false.
func NewAWSFinOpsController(backend FinOpsBackend) *AWSFinOpsController {
	return &AWSFinOpsController{backend: backend}
}

// Register mounts the FinOps routes behind the shared AWS admin chain: the
// gateway guard, the infrastructure view permission and, for writes, the
// settings-write permission with an admin audit record.
func (c *AWSFinOpsController) Register(router fiber.Router) {
	g := router.Group("/admin/aws/finops", adminGatewayGuard, requireFinOpsView)
	g.Get("/summary", c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Summary(ctx.UserContext(), q)
	}))
	g.Get("/bridge", c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Drivers(ctx.UserContext(), q)
	}))
	g.Get("/trend", c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Trend(ctx.UserContext(), q)
	}))
	g.Get("/breakdown", c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Breakdown(ctx.UserContext(), q)
	}))
	g.Get("/increases", c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Increases(ctx.UserContext(), q)
	}))
	g.Get("/increases/:id", c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Increase(ctx.UserContext(), q, strings.Clone(ctx.Params("id")))
	}))
	g.Get("/savings", c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Savings(ctx.UserContext(), q)
	}))
	g.Get("/accounts", c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Accounts(ctx.UserContext(), q)
	}))
	g.Get("/budgets", c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Budget(ctx.UserContext(), q)
	}))
	g.Get("/reports", c.read(func(s *finops.Service, ctx *fiber.Ctx, _ finops.Query) (any, error) {
		refs, err := s.Reports(ctx.UserContext())
		if refs == nil {
			refs = []finops.ReportRef{}
		}
		return fiber.Map{"enabled": true, "reports": refs}, err
	}))
	g.Get("/report", auditFinOps("aws.finops.report.viewed"), c.read(func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error) {
		return s.Report(ctx.UserContext(), q)
	}))
	g.Get("/export.csv", auditFinOps("aws.finops.exported"), c.exportCSV)

	g.Post("/increases/:id/status", auditFinOps("aws.finops.increase.status_changed"), requireFinOpsWrite, c.setIncreaseStatus)
	g.Post("/savings/:id/dismiss", auditFinOps("aws.finops.saving.dismissed"), requireFinOpsWrite, c.dismissSaving)
	g.Put("/budgets", auditFinOps("aws.finops.budgets.changed"), requireFinOpsWrite, c.putBudgets)
	g.Post("/reports", auditFinOps("aws.finops.report.frozen"), requireFinOpsWrite, c.freezeReport)
	g.Post("/refresh", auditFinOps("aws.finops.refreshed"), requireFinOpsWrite, c.refresh)
	g.Post("/drilldown", auditFinOps("aws.finops.drilled_down"), requireFinOpsWrite, c.drillDown)
}

func requireFinOpsView(ctx *fiber.Ctx) error {
	if !callerCanViewInfrastructure(ctx) {
		return refuseCallerInfrastructureView(ctx)
	}
	return ctx.Next()
}

func requireFinOpsWrite(ctx *fiber.Ctx) error {
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite))
	if !explicit || !allowed {
		return ctx.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "infrastructure:settings:write permission is required"})
	}
	return ctx.Next()
}

// finopsAuditActionKey lets a handler name the action its outcome was
// (an estimate instead of a refresh, a repeat freeze).
const finopsAuditActionKey = "finops_audit_action"

// auditFinOps records one audit event for a write or an audited read, with
// the outcome; a refused caller is recorded as a denial.
func auditFinOps(action string) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		err := ctx.Next()
		status := ctx.Response().StatusCode()
		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) {
			status = fiberErr.Code
		} else if err != nil {
			status = fiber.StatusInternalServerError
		}
		act, result := action, middleware.AdminAuditSuccess
		if outcome, ok := ctx.Locals(finopsAuditActionKey).(string); ok && outcome != "" {
			act = outcome
		}
		switch {
		case status == fiber.StatusUnauthorized || status == fiber.StatusForbidden:
			act, result = "rbac.denied", middleware.AdminAuditDenied
		case status >= fiber.StatusBadRequest:
			result = middleware.AdminAuditDenied
		}
		middleware.RecordAdminAudit(ctx, act, "infrastructure:finops", result)
		return err
	}
}

// finopsDisabled is the answer when the AWS connector is off: 200 with the
// reason, never 404, so the page can explain how to turn it on.
func finopsDisabled(ctx *fiber.Ctx) error {
	return ctx.JSON(fiber.Map{"enabled": false, "reason": cost.ReasonCostDataOff, "links": fiber.Map{"ui": "/aws/finops"}})
}

// finopsQuery reads the shared query parameters: period, compare,
// group_by, depth and status, plus accounts, regions and teams as
// comma-separated lists (accounts=111122223333,444455556666; the singular
// keys account, region and team are accepted too). Values are validated by
// the service against closed vocabularies.
func finopsQuery(ctx *fiber.Ctx) finops.Query {
	list := func(names ...string) []string {
		var out []string
		for _, n := range names {
			if v := ctx.Query(n); v != "" {
				out = append(out, strings.Clone(v))
			}
		}
		return out
	}
	return finops.Query{
		Period: strings.Clone(ctx.Query("period")), Compare: strings.Clone(ctx.Query("compare")),
		Accounts: list("accounts", "account"), Regions: list("regions", "region"), Teams: list("teams", "team"),
		GroupBy: strings.Clone(ctx.Query("group_by")), Depth: strings.Clone(ctx.Query("depth")),
		Status: strings.Clone(ctx.Query("status")),
	}
}

type finopsRead func(s *finops.Service, ctx *fiber.Ctx, q finops.Query) (any, error)

func (c *AWSFinOpsController) read(fn finopsRead) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		if c.backend == nil {
			return finopsDisabled(ctx)
		}
		v, err := fn(c.backend.Service(), ctx, finopsQuery(ctx))
		return writeFinOps(ctx, v, err)
	}
}

// writeFinOps maps service errors onto statuses with closed codes; it
// never echoes free text from the request.
func writeFinOps(ctx *fiber.Ctx, v any, err error) error {
	if err == nil {
		return ctx.JSON(v)
	}
	status, code := fiber.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, finops.ErrBadQuery):
		status, code = fiber.StatusBadRequest, "bad_query"
		if _, field, ok := strings.Cut(err.Error(), "bad query: "); ok && finopsQueryField(field) {
			return ctx.Status(status).JSON(fiber.Map{"error": code, "field": field})
		}
	case errors.Is(err, finops.ErrNotFound):
		status, code = fiber.StatusNotFound, "not_found"
	case errors.Is(err, finops.ErrReasonRequired):
		status, code = fiber.StatusBadRequest, "reason_required"
	case errors.Is(err, finops.ErrUntilInvalid):
		status, code = fiber.StatusBadRequest, "until_invalid"
	case errors.Is(err, finops.ErrBadTransition):
		status, code = fiber.StatusConflict, "status_change_not_allowed"
	case errors.Is(err, finops.ErrBadBudget):
		status, code = fiber.StatusBadRequest, "invalid_budget"
	case errors.Is(err, finops.ErrAlreadyFrozen):
		status, code = fiber.StatusConflict, "already_frozen"
	case errors.Is(err, finops.ErrBillNotClosed):
		status, code = fiber.StatusConflict, "bill_not_closed"
	case errors.Is(err, finops.ErrReportNotMonth):
		status, code = fiber.StatusBadRequest, "report_not_month"
	case errors.Is(err, finops.ErrAmountOverflow), errors.Is(err, storage.ErrAmountOverflow):
		status, code = fiber.StatusUnprocessableEntity, "amount_overflow"
	case errors.Is(err, finops.ErrPeriodTooLarge), errors.Is(err, cost.ErrCostLinesBudgetExceeded):
		status, code = fiber.StatusUnprocessableEntity, "cost_period_too_large"
	case errors.Is(err, finops.ErrNoStore):
		status, code = fiber.StatusServiceUnavailable, "store_unavailable"
	case errors.Is(err, cost.ErrManualUsed):
		status, code = fiber.StatusTooManyRequests, "manual_refresh_used"
	case errors.Is(err, cost.ErrBusy), errors.Is(err, cost.ErrLockLost):
		ctx.Set(fiber.HeaderRetryAfter, refreshInProgressRetryAfter)
		status, code = fiber.StatusConflict, "refresh_in_progress"
	case errors.Is(err, finops.ErrCapReached):
		status, code = fiber.StatusTooManyRequests, "paid_request_cap_reached"
	case errors.Is(err, cost.ErrDisabled):
		status, code = fiber.StatusConflict, cost.ReasonCostDataOff
	case errors.Is(err, cost.ErrUnknownAccount):
		status, code = fiber.StatusNotFound, "account_not_covered"
	case errors.Is(err, cost.ErrUnknownPair):
		status, code = fiber.StatusNotFound, "not_found"
	case errors.Is(err, cost.ErrReservedForRefresh):
		status, code = fiber.StatusTooManyRequests, "reserved_for_scheduled_refresh"
	case errors.Is(err, cost.ErrInvalidAccount):
		status, code = fiber.StatusBadRequest, "invalid_body"
	default:
		var coded *cost.Error
		if errors.As(err, &coded) {
			status, code = fiber.StatusBadGateway, coded.Code
		}
	}
	return ctx.Status(status).JSON(fiber.Map{"error": code})
}

func finopsQueryField(field string) bool {
	switch field {
	case "period", "compare", "group_by", "depth", "status":
		return true
	}
	return false
}

func decodeFinOpsBody(ctx *fiber.Ctx, v any) error {
	body := ctx.Body()
	if len(body) > finopsBodyLimit {
		return errors.New("body too large")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (c *AWSFinOpsController) setIncreaseStatus(ctx *fiber.Ctx) error {
	if c.backend == nil {
		return finopsDisabled(ctx)
	}
	var ch finops.StatusChange
	if err := decodeFinOpsBody(ctx, &ch); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
	}
	v, err := c.backend.Service().SetIncreaseStatus(ctx.UserContext(), strings.Clone(ctx.Params("id")), ch, requestActor(ctx))
	return writeFinOps(ctx, v, err)
}

type finopsDismissRequest struct {
	Reason string `json:"reason"`
}

func (c *AWSFinOpsController) dismissSaving(ctx *fiber.Ctx) error {
	if c.backend == nil {
		return finopsDisabled(ctx)
	}
	var req finopsDismissRequest
	if err := decodeFinOpsBody(ctx, &req); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
	}
	err := c.backend.Service().DismissSaving(ctx.UserContext(), strings.Clone(ctx.Params("id")), req.Reason, requestActor(ctx))
	return writeFinOps(ctx, fiber.Map{"dismissed": true}, err)
}

type finopsBudgetsRequest struct {
	Budgets []finops.Budget `json:"budgets"`
}

func (c *AWSFinOpsController) putBudgets(ctx *fiber.Ctx) error {
	if c.backend == nil {
		return finopsDisabled(ctx)
	}
	var req finopsBudgetsRequest
	if err := decodeFinOpsBody(ctx, &req); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
	}
	bs, err := c.backend.Service().PutBudgets(ctx.UserContext(), req.Budgets)
	if bs == nil {
		bs = []finops.Budget{}
	}
	return writeFinOps(ctx, fiber.Map{"budgets": bs}, err)
}

type finopsFreezeRequest struct {
	Month string `json:"month"`
}

func (c *AWSFinOpsController) freezeReport(ctx *fiber.Ctx) error {
	if c.backend == nil {
		return finopsDisabled(ctx)
	}
	var req finopsFreezeRequest
	if err := decodeFinOpsBody(ctx, &req); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
	}
	v, err := c.backend.Service().FreezeReport(ctx.UserContext(), req.Month, requestActor(ctx))
	switch {
	case err == nil && v.Enabled:
		return ctx.Status(fiber.StatusCreated).JSON(v)
	case errors.Is(err, finops.ErrAlreadyFrozen):
		// Idempotent per month: the report frozen first is returned as is.
		ctx.Locals(finopsAuditActionKey, "aws.finops.report.already_frozen")
		return ctx.JSON(v)
	}
	return writeFinOps(ctx, v, err)
}

type finopsRefreshRequest struct {
	// Confirm false (the default) only prices the refresh; true runs it.
	Confirm bool `json:"confirm"`
}

// refresh is the paid "Refresh now": it returns the estimate first and runs
// only on confirmation, at most once a day.
func (c *AWSFinOpsController) refresh(ctx *fiber.Ctx) error {
	if c.backend == nil {
		return finopsDisabled(ctx)
	}
	var req finopsRefreshRequest
	if len(ctx.Body()) > 0 {
		if err := decodeFinOpsBody(ctx, &req); err != nil {
			return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
	}
	plan, err := c.backend.Estimate(ctx.UserContext(), cost.KindManual)
	if !req.Confirm {
		ctx.Locals(finopsAuditActionKey, "aws.finops.refresh.estimated")
	}
	if err != nil || !req.Confirm {
		return writeFinOps(ctx, fiber.Map{"estimate": plan, "ran": false}, err)
	}
	res, err := c.backend.RefreshNow(ctx.UserContext())
	return writeFinOps(ctx, fiber.Map{"estimate": plan, "ran": true, "result": res}, err)
}

type finopsDrillRequest struct {
	Account string `json:"account"`
	Service string `json:"service"`
}

// drillDown spends one paid request (or more pages) through the meter to
// read one account and service by usage type; cached for the day.
func (c *AWSFinOpsController) drillDown(ctx *fiber.Ctx) error {
	if c.backend == nil {
		return finopsDisabled(ctx)
	}
	var req finopsDrillRequest
	if err := decodeFinOpsBody(ctx, &req); err != nil || !twelveDigits(req.Account) || req.Service == "" || len(req.Service) > 256 {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
	}
	err := c.backend.DrillDown(ctx.UserContext(), req.Account, req.Service)
	return writeFinOps(ctx, fiber.Map{"drilled": true}, err)
}

// exportCSV writes the breakdown, increases or savings table with the
// active filters. Text cells that a spreadsheet would run as a formula are
// escaped.
func (c *AWSFinOpsController) exportCSV(ctx *fiber.Ctx) error {
	if c.backend == nil {
		return finopsDisabled(ctx)
	}
	s, q := c.backend.Service(), finopsQuery(ctx)
	var rows [][]string
	switch ctx.Query("table", "breakdown") {
	case "breakdown":
		v, err := s.Breakdown(ctx.UserContext(), q)
		if err != nil || !v.Enabled {
			return writeFinOps(ctx, v, err)
		}
		rows = append(rows, []string{"key", "label", "amount", "base", "change", "share_pct", "owner", "estimated"})
		for _, r := range v.Rows {
			owner := ""
			if r.Owner != nil {
				owner = r.Owner.Name
			}
			rows = append(rows, []string{csvText(r.Key), csvText(r.Label), finops.FormatMicros(r.Amount), optMicros(r.Base), optMicros(r.Change),
				strconv.FormatFloat(float64(r.ShareBP)/100, 'f', 2, 64), csvText(owner), strconv.FormatBool(r.Estimated)})
		}
	case "increases":
		v, err := s.Increases(ctx.UserContext(), q)
		if err != nil || !v.Enabled {
			return writeFinOps(ctx, v, err)
		}
		rows = append(rows, []string{"id", "account", "service", "started_on", "daily_before", "daily_now", "monthly_impact", "status", "owner", "owner_estimated"})
		for _, i := range v.Items {
			rows = append(rows, []string{csvText(i.ID), csvText(i.Account), csvText(i.ServiceLabel), i.StartedOn.String(),
				finops.FormatMicros(i.DailyBefore), finops.FormatMicros(i.DailyNow), finops.FormatMicros(i.Monthly),
				csvText(i.Status), csvText(i.Owner.Name), strconv.FormatBool(i.Owner.Estimated)})
		}
	case "savings":
		v, err := s.Savings(ctx.UserContext(), q)
		if err != nil || !v.Enabled {
			return writeFinOps(ctx, v, err)
		}
		rows = append(rows, []string{"id", "kind", "title", "monthly", "effort", "risk", "account", "region", "owner", "status"})
		for _, i := range v.Items {
			rows = append(rows, []string{csvText(i.ID), csvText(i.Kind), csvText(i.Title), finops.FormatMicros(i.Monthly),
				csvText(i.Effort), csvText(i.Risk), csvText(i.Account), csvText(i.Region), csvText(i.Owner.Name), csvText(i.Status)})
		}
	default:
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_query", "field": "table"})
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.WriteAll(rows); err != nil {
		return writeFinOps(ctx, nil, err)
	}
	ctx.Set(fiber.HeaderContentType, "text/csv; charset=utf-8")
	ctx.Set(fiber.HeaderContentDisposition, `attachment; filename="finops-`+ctx.Query("table", "breakdown")+`.csv"`)
	ctx.Set(fiber.HeaderCacheControl, "no-store")
	return ctx.Send(buf.Bytes())
}

// csvText neutralises spreadsheet formulas: a text cell starting with =, +,
// -, @, a tab or a carriage return is prefixed with a single quote.
func csvText(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func optMicros(v *finops.Micros) string {
	if v == nil {
		return ""
	}
	return finops.FormatMicros(*v)
}

func twelveDigits(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
