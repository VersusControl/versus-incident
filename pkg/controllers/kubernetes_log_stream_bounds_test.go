package controllers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
)

// An oversized stream request is answered explicitly, before anything is parsed
// and without echoing the input, so a proxy's header limit can never answer in
// silence instead.
func TestLogStreamOversizedQueryIsRefusedExplicitly(t *testing.T) {
	controller := &KubernetesAdminController{}
	app := fiber.New(fiber.Config{Immutable: true, ReadBufferSize: 16 << 10})
	app.Use(func(ctx *fiber.Ctx) error {
		ctx.Locals(kubernetesServiceContextKey, kubernetes.NewService(nil, kubernetes.Scope{OrgID: "org", ClusterID: "cluster"}, 0))
		return ctx.Next()
	})
	app.Get("/workloads/:kind/:name/logs/stream", controller.workloadLogStream)
	app.Get("/pods/:namespace/:name/logs/stream", controller.podLogStream)

	padding := strings.Repeat("x", kubernetes.MaxStreamRequestQueryBytes+256)
	for _, path := range []string{"/workloads/Deployment/api/logs/stream", "/pods/shop/api/logs/stream"} {
		t.Run(path, func(t *testing.T) {
			oversized := path + "?cluster=one&namespace=shop&unknown=" + padding
			if len(oversized) <= kubernetes.MaxStreamRequestQueryBytes {
				t.Fatal("fixture does not exceed the bound")
			}
			response, err := app.Test(httptest.NewRequest(http.MethodGet, oversized, nil))
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
				Code    string `json:"code"`
				Message string `json:"error"`
				Action  string `json:"action"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("body=%q err=%v", raw, err)
			}
			if response.StatusCode != http.StatusBadRequest || body.Code != "request_too_large" {
				t.Fatalf("status=%d code=%q", response.StatusCode, body.Code)
			}
			if strings.Contains(string(raw), "xxxx") || strings.Contains(body.Message, "unknown") || strings.Contains(body.Action, padding) {
				t.Fatalf("refusal echoed the request: %s", raw)
			}
			// Control: a request inside the bound reaches the ordinary parameter
			// validation, so the bound is a size rule and nothing else.
			small, err := app.Test(httptest.NewRequest(http.MethodGet, path+"?cluster=one&namespace=shop&unknown=value", nil))
			if err != nil {
				t.Fatal(err)
			}
			smallRaw, err := io.ReadAll(small.Body)
			if closeErr := small.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			var smallBody struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(smallRaw, &smallBody); err != nil {
				t.Fatal(err)
			}
			if small.StatusCode != http.StatusBadRequest || smallBody.Code != "invalid_arguments" {
				t.Fatalf("small request status=%d code=%q", small.StatusCode, smallBody.Code)
			}
		})
	}
}
