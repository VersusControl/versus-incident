package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// The persisted-history read joins the storage error to the Kubernetes one.
// Both shapes it returns for a page link answer 400 with their own code, and
// the storage error text never reaches the client.
func TestPersistedHistoryCursorErrorsAnswer400(t *testing.T) {
	for _, test := range []struct {
		name, code string
		err        error
	}{
		{"retention made the link stale", "cursor_expired", errors.Join(kubernetes.ErrCursorExpired, storage.ErrEvidenceCursorExpired)},
		{"malformed link", "invalid_arguments", errors.Join(kubernetes.ErrInvalidArguments, storage.ErrInvalidEvidence)},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := fiber.New()
			app.Get("/", func(ctx *fiber.Ctx) error { return writeKubernetes(ctx, nil, test.err) })
			response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusBadRequest || body.Code != test.code {
				t.Fatalf("status=%d code=%q, want 400 %q", response.StatusCode, body.Code, test.code)
			}
			if strings.Contains(string(raw), "storage:") || strings.Contains(string(raw), "no longer matches the stored history") {
				t.Fatalf("response exposed the storage error: %s", raw)
			}
		})
	}
}
