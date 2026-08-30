package controllers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/ui"
	"github.com/gofiber/fiber/v2"
)

func TestExcludeFromSPAFallback(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/api", want: true},
		{path: "/api/admin/config/agent", want: true},
		{path: "/enterprise/api", want: true},
		{path: "/enterprise/api/sso/deployment", want: true},
		{path: "/healthz", want: true},
		{path: "/apix/settings", want: false},
		{path: "/enterprise/apix/settings", want: false},
		{path: "/healthz-check", want: false},
		{path: "/healthz/ready", want: false},
		{path: "/agent/chat", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := excludeFromSPAFallback(tt.path); got != tt.want {
				t.Fatalf("excludeFromSPAFallback(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestMountStaticUI_EnterpriseAPIMissReturnsNotFound(t *testing.T) {
	if !uiBuilt(ui.Dist()) {
		t.Skip("UI build is not embedded")
	}

	app := fiber.New()
	MountStaticUI(app)

	request := httptest.NewRequest(http.MethodGet, "/enterprise/api/sso/deployment", nil)
	response, err := app.Test(request, -1)
	if err != nil {
		t.Fatalf("request enterprise SSO deployment: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
}
