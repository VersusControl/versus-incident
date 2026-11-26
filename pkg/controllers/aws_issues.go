package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
)

// awsIssuesBodyLimit bounds a write's body: a bulk reject names at most
// 200 IDs of 64 characters and one reason.
const awsIssuesBodyLimit = 64 << 10

// AWSIssuesCoverage reads the coverage the summary and the report show,
// for one account or (empty) every account. enabled is false when the AWS
// connector is off.
type AWSIssuesCoverage func(ctx context.Context, account string) (coverage issues.CoverageRead, enabled bool, err error)

// AWSIssuesNotAssessed says, at read time, which rules cannot be assessed
// on which accounts.
type AWSIssuesNotAssessed func(ctx context.Context, accounts []string) ([]issues.AccountNotAssessed, error)

// AWSIssuesController serves the Issues page, its report and the user's
// writes on issues. Reads and the report need the infrastructure view; the
// writes also need the issues write permission. It reads and writes the
// issue store and reads the coverage, never AWS.
type AWSIssuesController struct {
	actions     *issues.Actions
	coverage    AWSIssuesCoverage
	notAssessed AWSIssuesNotAssessed
	now         func() time.Time
}

// NewAWSIssuesController builds the controller. Nil actions answer reads
// with enabled=false and refuse writes as not ready.
func NewAWSIssuesController(actions *issues.Actions, coverage AWSIssuesCoverage, notAssessed AWSIssuesNotAssessed, now func() time.Time) *AWSIssuesController {
	if now == nil {
		now = time.Now
	}
	return &AWSIssuesController{actions: actions, coverage: coverage, notAssessed: notAssessed, now: now}
}

// Register mounts the issue routes behind the admin chain: the gateway
// guard and the infrastructure view, plus the issues write permission on
// writes. Each write records one audit row with its own action and the
// issue it acted on; a refused one records rbac.denied on the issues write
// permission.
func (c *AWSIssuesController) Register(router fiber.Router) {
	write := core.PermissionInfrastructureIssuesWrite
	g := router.Group("/admin/aws/issues", auditAWSRefusal(write), adminGatewayGuard, requireChangesView)
	g.Get("/", c.list)
	g.Get("/report", c.report)
	g.Post("/reject", auditAWSWrite("aws.issue.bulk_rejected", write, auditFixed("0 issues")), requireAWSIssuesWrite, c.rejectMany)
	g.Get("/:id", c.detail)
	g.Post("/:id/acknowledge", auditAWSWrite("aws.issue.acknowledged", write, auditParam("id")), requireAWSIssuesWrite, c.acknowledge)
	g.Post("/:id/reject", auditAWSWrite("aws.issue.rejected", write, auditParam("id")), requireAWSIssuesWrite, c.reject)
	g.Post("/:id/reopen", auditAWSWrite("aws.issue.reopened", write, auditParam("id")), requireAWSIssuesWrite, c.reopen)
}

// bulkRejectAuditTarget names a bulk reject in the audit trail: how many
// distinct issues it named, and the first MaxBulkReject of their IDs, each
// cut to the length of an issue ID.
func bulkRejectAuditTarget(ids []string) string {
	seen := map[string]bool{}
	named := make([]string, 0, min(len(ids), issues.MaxBulkReject))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(named) < issues.MaxBulkReject {
			id = auditTargetValue(id)
			if len(id) > 64 {
				id = id[:64]
			}
			named = append(named, id)
		}
	}
	return strconv.Itoa(len(seen)) + " issues: " + strings.Join(named, ",")
}

// requireAWSIssuesWrite admits only a caller with the issues write
// permission.
func requireAWSIssuesWrite(ctx *fiber.Ctx) error {
	allowed, explicit := middleware.RequestPermission(ctx, string(core.PermissionInfrastructureIssuesWrite))
	if !explicit || !allowed {
		return refusePermission(ctx, core.PermissionInfrastructureIssuesWrite)
	}
	return ctx.Next()
}

// awsIssuesPage is the list answer: the summary, the warning line, the
// listed issues, the rejected count and where rules could not be assessed.
type awsIssuesPage struct {
	Enabled bool `json:"enabled"`
	issues.Page
	NotAssessed []issues.AccountNotAssessed `json:"not_assessed"`
}

// awsIssueImpact names where the issue sits on the topology.
type awsIssueImpact struct {
	Account string `json:"account"`
	Region  string `json:"region,omitempty"`
	Owner   string `json:"owner"`
	GroupID string `json:"group_id,omitempty"`
}

type awsIssueDetail struct {
	Issue  issues.Issue   `json:"issue"`
	Events []issues.Event `json:"events"`
	Impact awsIssueImpact `json:"impact"`
}

type awsIssueRejectRequest struct {
	Reason   string `json:"reason"`
	ReviewOn string `json:"review_on,omitempty"`
}

type awsIssueReopenRequest struct {
	Reason string `json:"reason,omitempty"`
}

type awsIssueBulkRejectRequest struct {
	IDs      []string `json:"ids"`
	Reason   string   `json:"reason"`
	ReviewOn string   `json:"review_on,omitempty"`
}

type awsIssueBulkRejectResponse struct {
	Results []issues.BulkResult `json:"results"`
}

func awsIssuesDisabled(ctx *fiber.Ctx) error {
	return ctx.JSON(fiber.Map{"enabled": false})
}

func awsIssuesNotReady(ctx *fiber.Ctx) error {
	return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "aws issue storage not ready", "code": "not_ready"})
}

// awsIssueError maps an issue error onto its answer: a bad input is a 400,
// an unknown issue a 404, an issue in the wrong state or changed under the
// write a 409.
func awsIssueError(ctx *fiber.Ctx, err error) error {
	status, code := fiber.StatusInternalServerError, "internal"
	message := "aws issues could not be read or written"
	switch {
	case errors.Is(err, issues.ErrInvalidFilter):
		status, code, message = fiber.StatusBadRequest, "invalid_filter", "kind, severity, account, state and group must be values the queue knows"
	case errors.Is(err, issues.ErrInvalidReason):
		status, code, message = fiber.StatusBadRequest, "invalid_reason", issues.ErrInvalidReason.Error()
	case errors.Is(err, issues.ErrInvalidReviewDate):
		status, code, message = fiber.StatusBadRequest, "invalid_review_date", issues.ErrInvalidReviewDate.Error()
	case errors.Is(err, issues.ErrTooMany):
		status, code, message = fiber.StatusBadRequest, "too_many", "a bulk reject names at most 200 issues"
	case errors.Is(err, issues.ErrInvalidActor):
		status, code, message = fiber.StatusBadRequest, "no_actor", "the request names no user"
	case errors.Is(err, issues.ErrNotFound):
		status, code, message = fiber.StatusNotFound, "not_found", "issue not found"
	case errors.Is(err, issues.ErrAlreadyRejected):
		status, code, message = fiber.StatusConflict, "already_rejected", "the issue is already rejected"
	case errors.Is(err, issues.ErrNotActive):
		status, code, message = fiber.StatusConflict, "not_open", "the issue is not open"
	case errors.Is(err, issues.ErrNotRejected):
		status, code, message = fiber.StatusConflict, "not_rejected", "the issue is not rejected"
	case errors.Is(err, issues.ErrConflict):
		status, code, message = fiber.StatusConflict, "conflict", "the issue changed; read it again"
	}
	return ctx.Status(status).JSON(fiber.Map{"error": message, "code": code})
}

func issueFilter(ctx *fiber.Ctx) issues.Filter {
	return issues.Filter{
		Kind: strings.Clone(ctx.Query("kind")), Severity: strings.Clone(ctx.Query("severity")),
		Account: strings.Clone(ctx.Query("account")), State: strings.Clone(ctx.Query("state")),
		Group: strings.Clone(ctx.Query("group")),
	}
}

// coverageOf reads the coverage the summary and the report show; empty
// when the connector is off.
func (c *AWSIssuesController) coverageOf(ctx *fiber.Ctx, filter issues.Filter) (issues.CoverageRead, error) {
	if c.coverage == nil {
		return issues.CoverageRead{}, nil
	}
	read, enabled, err := c.coverage(ctx.UserContext(), filter.Account)
	if err != nil || !enabled {
		return issues.CoverageRead{}, err
	}
	return read, nil
}

func (c *AWSIssuesController) list(ctx *fiber.Ctx) error {
	if c.actions == nil {
		return awsIssuesDisabled(ctx)
	}
	filter := issueFilter(ctx)
	if err := issues.ValidFilter(filter); err != nil {
		return awsIssueError(ctx, err)
	}
	coverage, err := c.coverageOf(ctx, filter)
	if err != nil {
		return awsIssueError(ctx, err)
	}
	read, err := c.actions.Page(ctx.UserContext(), filter, coverage)
	if err != nil {
		return awsIssueError(ctx, err)
	}
	page := awsIssuesPage{Enabled: true, Page: read, NotAssessed: []issues.AccountNotAssessed{}}
	if c.notAssessed != nil {
		var accounts []string
		if filter.Account != "" {
			accounts = []string{filter.Account}
		}
		missing, err := c.notAssessed(ctx.UserContext(), accounts)
		if err != nil {
			return awsIssueError(ctx, err)
		}
		page.NotAssessed = missing
	}
	return ctx.JSON(page)
}

func (c *AWSIssuesController) report(ctx *fiber.Ctx) error {
	if c.actions == nil {
		return awsIssuesDisabled(ctx)
	}
	filter := issueFilter(ctx)
	if err := errors.Join(issues.ValidFilter(filter), issues.ValidReportFilter(filter)); err != nil {
		return awsIssueError(ctx, err)
	}
	coverage, err := c.coverageOf(ctx, filter)
	if err != nil {
		return awsIssueError(ctx, err)
	}
	report, err := c.actions.Report(ctx.UserContext(), filter, coverage, c.now())
	if err != nil {
		return awsIssueError(ctx, err)
	}
	return ctx.JSON(report)
}

func (c *AWSIssuesController) detail(ctx *fiber.Ctx) error {
	if c.actions == nil {
		return awsIssuesDisabled(ctx)
	}
	issue, events, err := c.actions.Get(ctx.UserContext(), strings.Clone(ctx.Params("id")))
	if err != nil {
		return awsIssueError(ctx, err)
	}
	if events == nil {
		events = []issues.Event{}
	}
	return ctx.JSON(awsIssueDetail{Issue: issue, Events: events,
		Impact: awsIssueImpact{Account: issue.Account, Region: issue.Region, Owner: issue.Owner, GroupID: issue.GroupID}})
}

func decodeIssueBody(ctx *fiber.Ctx, v any, optional bool) error {
	body := ctx.Body()
	if len(body) > awsIssuesBodyLimit {
		return errors.New("body too large")
	}
	if optional && len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}

func badIssueBody(ctx *fiber.Ctx, message string) error {
	return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": message, "code": "invalid_body"})
}

func (c *AWSIssuesController) acknowledge(ctx *fiber.Ctx) error {
	if c.actions == nil {
		return awsIssuesNotReady(ctx)
	}
	issue, err := c.actions.Acknowledge(ctx.UserContext(), strings.Clone(ctx.Params("id")), strings.Clone(requestActor(ctx)))
	if err != nil {
		return awsIssueError(ctx, err)
	}
	return ctx.JSON(fiber.Map{"issue": issue})
}

func (c *AWSIssuesController) reject(ctx *fiber.Ctx) error {
	if c.actions == nil {
		return awsIssuesNotReady(ctx)
	}
	var request awsIssueRejectRequest
	if err := decodeIssueBody(ctx, &request, false); err != nil {
		return badIssueBody(ctx, "body must be a JSON object with reason and an optional review_on")
	}
	issue, err := c.actions.Reject(ctx.UserContext(), strings.Clone(ctx.Params("id")), strings.Clone(requestActor(ctx)), request.Reason, request.ReviewOn)
	if err != nil {
		return awsIssueError(ctx, err)
	}
	return ctx.JSON(fiber.Map{"issue": issue})
}

func (c *AWSIssuesController) reopen(ctx *fiber.Ctx) error {
	if c.actions == nil {
		return awsIssuesNotReady(ctx)
	}
	var request awsIssueReopenRequest
	if err := decodeIssueBody(ctx, &request, true); err != nil {
		return badIssueBody(ctx, "body must be empty or a JSON object with an optional reason")
	}
	issue, err := c.actions.Reopen(ctx.UserContext(), strings.Clone(ctx.Params("id")), strings.Clone(requestActor(ctx)), request.Reason)
	if err != nil {
		return awsIssueError(ctx, err)
	}
	return ctx.JSON(fiber.Map{"issue": issue})
}

func (c *AWSIssuesController) rejectMany(ctx *fiber.Ctx) error {
	if c.actions == nil {
		return awsIssuesNotReady(ctx)
	}
	var request awsIssueBulkRejectRequest
	if err := decodeIssueBody(ctx, &request, false); err != nil {
		return badIssueBody(ctx, "body must be a JSON object with ids, reason and an optional review_on")
	}
	if len(request.IDs) == 0 {
		return badIssueBody(ctx, "ids must name at least one issue")
	}
	ctx.Locals(awsAuditTargetKey, bulkRejectAuditTarget(request.IDs))
	results, err := c.actions.RejectMany(ctx.UserContext(), request.IDs, strings.Clone(requestActor(ctx)), request.Reason, request.ReviewOn)
	if err != nil {
		return awsIssueError(ctx, err)
	}
	return ctx.JSON(awsIssueBulkRejectResponse{Results: results})
}
