package controllers

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/runbook"

	"github.com/gofiber/fiber/v2"
)

// maxRunbookUploadBytes bounds a single uploaded runbook file so a
// malicious or accidental large upload cannot exhaust memory.
const maxRunbookUploadBytes = 1 << 20 // 1 MiB

// RunbookAdminController exposes upload/list/get/delete for the runbook
// corpus that backs the find_runbook tool. Same X-Gateway-Secret guard
// as the other /api/agent/* admin controllers. Runbooks are managed by
// uploading `.md` files (multipart) — there is no free-text editor; to
// change a runbook, re-upload a file with the same name.
type RunbookAdminController struct {
	mgr *runbook.Manager
}

// NewRunbookAdminController returns a controller backed by the runbook
// manager. Pass nil to disable the endpoints entirely (every request
// returns 503) — e.g. when storage is unavailable.
func NewRunbookAdminController(mgr *runbook.Manager) *RunbookAdminController {
	return &RunbookAdminController{mgr: mgr}
}

// Register mounts the runbook admin routes:
//
//	GET    /api/agent/runbooks         list (metadata only)
//	POST   /api/agent/runbooks         upload one or more `.md` files (field "files")
//	GET    /api/agent/runbooks/*       get one (full body)
//	DELETE /api/agent/runbooks/*       delete one
//
// The wildcard `*` carries the runbook ID, which may itself contain `/`
// (corpus-relative paths), so a `:id` param would not match nested IDs.
func (c *RunbookAdminController) Register(router fiber.Router) {
	g := router.Group("/agent/runbooks", c.authMiddleware, c.requireManager)
	g.Get("/", c.list)
	g.Post("/", c.upload)
	g.Get("/*", c.get)
	g.Delete("/*", c.delete)
}

func (c *RunbookAdminController) authMiddleware(ctx *fiber.Ctx) error {
	cfg := config.GetConfig()
	expected := cfg.GatewaySecret
	got := ctx.Get("X-Gateway-Secret")
	if expected == "" || !secureEqual(got, expected) {
		return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	return ctx.Next()
}

func (c *RunbookAdminController) requireManager(ctx *fiber.Ctx) error {
	if c.mgr == nil {
		return ctx.Status(fiber.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "runbooks unavailable: no storage backend configured"})
	}
	return ctx.Next()
}

// runbookView is the list/metadata shape — it deliberately OMITS the
// large embedding vector and the full body to keep list responses small.
type runbookView struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Services  []string  `json:"services,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	Source    string    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	HasVector bool      `json:"has_vector"`
}

// runbookDetail adds the full markdown body for the single-runbook view.
type runbookDetail struct {
	runbookView
	Body string `json:"body"`
}

func toRunbookView(r runbook.Record) runbookView {
	return runbookView{
		ID:        r.ID,
		Title:     r.Title,
		Services:  r.Services,
		Tags:      r.Tags,
		Source:    r.Source,
		UpdatedAt: r.UpdatedAt,
		HasVector: len(r.Vector) > 0,
	}
}

func (c *RunbookAdminController) list(ctx *fiber.Ctx) error {
	recs := c.mgr.List()
	views := make([]runbookView, 0, len(recs))
	for _, r := range recs {
		views = append(views, toRunbookView(r))
	}
	return ctx.JSON(fiber.Map{
		"runbooks":   views,
		"embeddings": c.mgr.HasEmbedder(),
	})
}

func (c *RunbookAdminController) get(ctx *fiber.Ctx) error {
	id := ctx.Params("*")
	rec, err := c.mgr.Get(id)
	if err != nil {
		return mapRunbookErr(ctx, err)
	}
	return ctx.JSON(runbookDetail{runbookView: toRunbookView(rec), Body: rec.Body})
}

func (c *RunbookAdminController) delete(ctx *fiber.Ctx) error {
	id := ctx.Params("*")
	if err := c.mgr.Delete(id); err != nil {
		return mapRunbookErr(ctx, err)
	}
	return ctx.SendStatus(fiber.StatusNoContent)
}

func (c *RunbookAdminController) upload(ctx *fiber.Ctx) error {
	form, err := ctx.MultipartForm()
	if err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid multipart form"})
	}
	headers := form.File["files"]
	if len(headers) == 0 {
		return ctx.Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"error": `no files uploaded (use multipart field "files")`})
	}

	files := make([]runbook.UploadFile, 0, len(headers))
	for _, fh := range headers {
		if !strings.EqualFold(filepath.Ext(fh.Filename), ".md") {
			return ctx.Status(fiber.StatusBadRequest).
				JSON(fiber.Map{"error": fmt.Sprintf("%q is not a .md file", fh.Filename)})
		}
		if fh.Size > maxRunbookUploadBytes {
			return ctx.Status(fiber.StatusRequestEntityTooLarge).
				JSON(fiber.Map{"error": fmt.Sprintf("%q exceeds the %d-byte limit", fh.Filename, maxRunbookUploadBytes)})
		}
		f, openErr := fh.Open()
		if openErr != nil {
			return ctx.Status(fiber.StatusInternalServerError).
				JSON(fiber.Map{"error": fmt.Sprintf("open %q: %v", fh.Filename, openErr)})
		}
		data, readErr := io.ReadAll(io.LimitReader(f, maxRunbookUploadBytes+1))
		_ = f.Close()
		if readErr != nil {
			return ctx.Status(fiber.StatusInternalServerError).
				JSON(fiber.Map{"error": fmt.Sprintf("read %q: %v", fh.Filename, readErr)})
		}
		if len(data) > maxRunbookUploadBytes {
			return ctx.Status(fiber.StatusRequestEntityTooLarge).
				JSON(fiber.Map{"error": fmt.Sprintf("%q exceeds the %d-byte limit", fh.Filename, maxRunbookUploadBytes)})
		}
		files = append(files, runbook.UploadFile{Name: fh.Filename, Content: data})
	}

	n, err := c.mgr.Upload(ctx.UserContext(), files, "")
	if err != nil {
		return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	return ctx.Status(fiber.StatusCreated).JSON(fiber.Map{
		"ingested":   n,
		"embeddings": c.mgr.HasEmbedder(),
	})
}

// mapRunbookErr maps manager errors onto HTTP status codes.
func mapRunbookErr(ctx *fiber.Ctx, err error) error {
	if errors.Is(err, runbook.ErrNotFound) {
		return ctx.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "runbook not found"})
	}
	return ctx.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
}
