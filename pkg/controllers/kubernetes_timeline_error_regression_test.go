package controllers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
)

func TestTimelineAdmissionErrorsKeepDistinctHTTPContracts(t *testing.T) {
	for _, test := range []struct {
		name, code, retryAfter string
		err                    error
		status                 int
		retryable              bool
	}{
		{"storage contention", "timeline_storage_busy", "1", kubernetes.ErrTimelineContention, http.StatusServiceUnavailable, true},
		{"pinned capacity", "stream_busy", "", kubernetes.ErrIndexStreamBusy, http.StatusTooManyRequests, true},
		{"expired or replaced cursor", "invalid_arguments", "", kubernetes.ErrInvalidArguments, http.StatusBadRequest, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, wrapped := range []bool{false, true} {
				t.Run(fmt.Sprintf("wrapped=%t", wrapped), func(t *testing.T) {
					failure := test.err
					if wrapped {
						failure = fmt.Errorf("private upstream context: %w", failure)
					}
					app := fiber.New(fiber.Config{Immutable: true})
					app.Get("/", func(ctx *fiber.Ctx) error {
						return writeKubernetes(ctx, fiber.Map{"next": "must-not-return-on-error"}, failure)
					})
					response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
					if err != nil {
						t.Fatal(err)
					}
					raw, err := io.ReadAll(response.Body)
					if closeErr := response.Body.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					if err != nil {
						t.Fatal(err)
					}
					var body struct {
						Code      string `json:"code"`
						Message   string `json:"error"`
						Action    string `json:"action"`
						Retryable bool   `json:"retryable"`
						Next      string `json:"next"`
					}
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Fatal(err)
					}
					if response.StatusCode != test.status || body.Code != test.code || body.Retryable != test.retryable || response.Header.Get("Retry-After") != test.retryAfter {
						t.Fatalf("status=%d code=%q retryable=%v retry-after=%q; want %d %q %v %q",
							response.StatusCode, body.Code, body.Retryable, response.Header.Get("Retry-After"),
							test.status, test.code, test.retryable, test.retryAfter)
					}
					if body.Message == "" || body.Action == "" || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
						t.Fatal("error response lost its safe JSON message/action contract")
					}
					if body.Next != "" || strings.Contains(string(raw), "must-not-return-on-error") || strings.Contains(string(raw), "private upstream context") {
						t.Fatal("error response exposed a partial page or raw wrapped cause")
					}
				})
			}
		})
	}
}
