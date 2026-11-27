package controllers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/grouping"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// AWSGroupInventory reads what grouping needs from the stores: the stored
// resources and the connected accounts. ok is false when the AWS connector
// is off, and corrections are then listed without a status.
type AWSGroupInventory func(ctx context.Context) (resources []model.Resource, accounts []string, ok bool, err error)

// AWSGroupCorrectionsController serves the renames, merges and moves a user
// makes to the Topology groups. Reads need the infrastructure view; writes
// need the infrastructure settings permission. It reads and writes the
// correction store and reads the inventory store, never AWS.
type AWSGroupCorrectionsController struct {
	store     storage.AWSGroupCorrectionStore
	scope     tenancy.OrgScope
	inventory AWSGroupInventory
	now       func() time.Time
}

// NewAWSGroupCorrectionsController builds the controller. A nil store
// answers reads with enabled=false and refuses writes as not ready; a nil
// inventory lists corrections without a status.
func NewAWSGroupCorrectionsController(store storage.AWSGroupCorrectionStore, scope tenancy.OrgScope, inventory AWSGroupInventory) *AWSGroupCorrectionsController {
	return &AWSGroupCorrectionsController{store: store, scope: scope, inventory: inventory, now: time.Now}
}

// Register mounts the correction routes behind the admin chain: the
// gateway guard and the infrastructure view, plus the settings permission
// on writes. GET filters by the account_id query parameter; DELETE names
// the correction with account_id, kind, and group_id or member_arn.
func (c *AWSGroupCorrectionsController) Register(router fiber.Router) {
	write := core.PermissionInfrastructureSettingsWrite
	g := router.Group("/admin/aws/groups/corrections", auditAWSRefusal(write), adminGatewayGuard, requireChangesView)
	g.Get("/", c.list)
	g.Post("/", auditAWSWrite("aws.group.correction.saved", write, auditFixed("group correction")), requireAWSSettingsWrite, c.create)
	g.Delete("/", auditAWSWrite("aws.group.correction.deleted", write, correctionAuditQuery), requireAWSSettingsWrite, c.delete)
}

// awsGroupCorrectionView is one stored correction and what became of it on
// the current read: applied, or no_longer_matches when a group or the
// member it names is gone. Status is empty when the inventory cannot be
// read.
type awsGroupCorrectionView struct {
	storage.AWSGroupCorrection
	Status grouping.CorrectionStatus `json:"status,omitempty"`
}

type awsGroupCorrectionsResponse struct {
	Enabled     bool                     `json:"enabled"`
	Corrections []awsGroupCorrectionView `json:"corrections"`
}

type awsGroupCorrectionRequest struct {
	AccountID     string `json:"account_id"`
	Kind          string `json:"kind"`
	GroupID       string `json:"group_id"`
	MemberARN     string `json:"member_arn"`
	TargetGroupID string `json:"target_group_id"`
	Name          string `json:"name"`
	// ExpectedCreatedAt is the created_at of the correction with this key
	// the caller read; when set, the save fails with correction_conflict
	// if that correction changed or is gone.
	ExpectedCreatedAt *time.Time `json:"expected_created_at"`
}

func groupCorrectionStoreNotReady(ctx *fiber.Ctx) error {
	return ctx.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "aws group correction storage not ready"})
}

func badGroupCorrection(ctx *fiber.Ctx, message string) error {
	return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": message})
}

// groupingCorrections converts stored corrections for the grouping.
func groupingCorrections(stored []storage.AWSGroupCorrection) []grouping.Correction {
	out := make([]grouping.Correction, 0, len(stored))
	for _, correction := range stored {
		out = append(out, grouping.Correction{Account: correction.AccountID, Kind: correction.Kind, GroupID: correction.GroupID,
			MemberARN: correction.MemberARN, TargetGroupID: correction.TargetGroupID, Name: correction.Name})
	}
	return out
}

func (c *AWSGroupCorrectionsController) list(ctx *fiber.Ctx) error {
	if c.store == nil {
		return ctx.JSON(awsGroupCorrectionsResponse{Enabled: false, Corrections: []awsGroupCorrectionView{}})
	}
	account := strings.Clone(ctx.Query("account_id"))
	if account != "" && !coverageAccountPattern.MatchString(account) {
		return badGroupCorrection(ctx, "account_id must be a 12-digit AWS account ID")
	}
	stored, err := c.store.AWSGroupCorrections(ctx.UserContext(), c.scope, "")
	if err != nil {
		if errors.Is(err, storage.ErrUnsupported) {
			return groupCorrectionStoreNotReady(ctx)
		}
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "aws group corrections could not be read"})
	}
	// Every account's corrections are applied, in order, so the status of
	// one never depends on the filter.
	var statuses []grouping.CorrectionStatus
	if c.inventory != nil {
		resources, accounts, ok, err := c.inventory(ctx.UserContext())
		if err != nil {
			return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "aws inventory could not be read"})
		}
		if ok {
			statuses = grouping.Build(resources, nil, groupingCorrections(stored), grouping.Options{Accounts: accounts}).Corrections
		}
	}
	out := make([]awsGroupCorrectionView, 0, len(stored))
	for index, correction := range stored {
		if account != "" && correction.AccountID != account {
			continue
		}
		view := awsGroupCorrectionView{AWSGroupCorrection: correction}
		if statuses != nil {
			view.Status = statuses[index]
		}
		out = append(out, view)
	}
	return ctx.JSON(awsGroupCorrectionsResponse{Enabled: true, Corrections: out})
}

// validGroupCorrectionRequest checks what the store cannot know: the group
// IDs have the grouping's form, and Ungrouped is only ever a move target.
func validGroupCorrectionRequest(request awsGroupCorrectionRequest) string {
	if !coverageAccountPattern.MatchString(request.AccountID) {
		return "account_id must be a 12-digit AWS account ID"
	}
	switch request.Kind {
	case storage.AWSGroupRename, storage.AWSGroupMerge, storage.AWSGroupMove:
	default:
		return "kind must be rename, merge or move"
	}
	if request.Kind != storage.AWSGroupMove && (!grouping.ValidID(request.GroupID) || request.GroupID == grouping.UngroupedID) {
		return "group_id must name a group other than Ungrouped"
	}
	if request.Kind == storage.AWSGroupMerge && (!grouping.ValidID(request.TargetGroupID) || request.TargetGroupID == grouping.UngroupedID) {
		return "target_group_id must name a group other than Ungrouped"
	}
	if request.Kind == storage.AWSGroupMove && !grouping.ValidID(request.TargetGroupID) {
		return "target_group_id must name a group"
	}
	return ""
}

func (c *AWSGroupCorrectionsController) create(ctx *fiber.Ctx) error {
	if c.store == nil {
		return groupCorrectionStoreNotReady(ctx)
	}
	var request awsGroupCorrectionRequest
	if err := ctx.BodyParser(&request); err != nil {
		return badGroupCorrection(ctx, "body must be a JSON object with account_id, kind and the fields the kind takes")
	}
	request = awsGroupCorrectionRequest{AccountID: strings.Clone(request.AccountID), Kind: strings.Clone(request.Kind),
		GroupID: strings.Clone(request.GroupID), MemberARN: strings.Clone(request.MemberARN),
		TargetGroupID: strings.Clone(request.TargetGroupID), Name: strings.Clone(request.Name), ExpectedCreatedAt: request.ExpectedCreatedAt}
	setAWSAuditTarget(ctx, correctionAuditTarget(request.AccountID, request.Kind, request.GroupID, request.MemberARN))
	if message := validGroupCorrectionRequest(request); message != "" {
		return badGroupCorrection(ctx, message)
	}
	correction, err := c.store.PutAWSGroupCorrection(ctx.UserContext(), c.scope, storage.AWSGroupCorrection{
		AccountID: request.AccountID, Kind: request.Kind, GroupID: request.GroupID, MemberARN: request.MemberARN,
		TargetGroupID: request.TargetGroupID, Name: request.Name,
		CreatedBy: strings.Clone(middleware.RequestActor(ctx)), CreatedAt: c.now(), ExpectedCreatedAt: request.ExpectedCreatedAt,
	})
	switch {
	case errors.Is(err, storage.ErrInvalidAWSGroupCorrection):
		return badGroupCorrection(ctx, "a rename takes group_id and a name of at most 128 printable characters; a merge takes group_id and "+
			"target_group_id; a move takes the member_arn of a resource in the account and target_group_id")
	case errors.Is(err, storage.ErrAWSGroupCorrectionConflict):
		return ctx.Status(fiber.StatusConflict).JSON(fiber.Map{"code": "correction_conflict",
			"error": "changed by someone else; reload"})
	case errors.Is(err, storage.ErrAWSGroupCorrectionLimit):
		return ctx.Status(fiber.StatusConflict).JSON(fiber.Map{"code": "correction_limit",
			"error": "this account holds the most corrections allowed; delete one first"})
	case errors.Is(err, storage.ErrUnsupported):
		return groupCorrectionStoreNotReady(ctx)
	case err != nil:
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "aws group correction could not be saved"})
	}
	return ctx.JSON(fiber.Map{"correction": correction})
}

func (c *AWSGroupCorrectionsController) delete(ctx *fiber.Ctx) error {
	if c.store == nil {
		return groupCorrectionStoreNotReady(ctx)
	}
	key := storage.AWSGroupCorrectionKey{AccountID: strings.Clone(ctx.Query("account_id")), Kind: strings.Clone(ctx.Query("kind")),
		GroupID: strings.Clone(ctx.Query("group_id")), MemberARN: strings.Clone(ctx.Query("member_arn"))}
	deleted, err := c.store.DeleteAWSGroupCorrection(ctx.UserContext(), c.scope, key)
	switch {
	case errors.Is(err, storage.ErrInvalidAWSGroupCorrection):
		return badGroupCorrection(ctx, "name the correction with account_id, kind, and group_id for a rename or merge or member_arn for a move")
	case errors.Is(err, storage.ErrAWSGroupCorrectionInherited):
		return ctx.Status(fiber.StatusConflict).JSON(fiber.Map{"code": "correction_inherited",
			"error": "this correction was made before the upgrade and is inherited; make a new correction for the same group or member to replace it"})
	case errors.Is(err, storage.ErrUnsupported):
		return groupCorrectionStoreNotReady(ctx)
	case err != nil:
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "aws group correction could not be deleted"})
	case !deleted:
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no such correction"})
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

// correctionAuditTarget names a correction in the audit trail by its key.
func correctionAuditTarget(account, kind, group, member string) string {
	target := "account=" + account + " kind=" + kind
	if member != "" {
		return target + " member=" + member
	}
	return target + " group=" + group
}

// correctionAuditQuery names the correction a delete asks for.
func correctionAuditQuery(ctx *fiber.Ctx) string {
	return correctionAuditTarget(ctx.Query("account_id"), ctx.Query("kind"), ctx.Query("group_id"), ctx.Query("member_arn"))
}
