package mount

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
)

// The mounted correction routes read each correction's status from the
// stored inventory of the connected accounts: a rename of a group the
// inventory makes is applied, one of a group it does not make no longer
// matches. With the connector off the corrections list without a status.
func TestMountedGroupCorrectionsReadTheInventory(t *testing.T) {
	const account = "123400000002"
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureSettingsWrite), true)
		return ctx.Next()
	})
	config := &c.Config{}
	config.Connectors.AWS = c.AWSConnectorConfig{Enable: true, Accounts: []c.AWSAccountConfig{{AccountID: account, Regions: []string{"eu-south-2"}}}}
	mounted := mountForTest(t, app, Deps{Config: config})
	var resources []model.Resource
	for _, name := range []string{"a", "b"} {
		resources = append(resources, model.Resource{ARN: "arn:aws:lambda:eu-south-2:" + account + ":function:" + name, Service: "lambda",
			Type: "function", Account: account, Region: "eu-south-2", Name: name, Attributes: map[string]string{model.AttrStackName: "shop"}})
	}
	if _, err := mounted.Snapshots.Commit(model.Unit{Account: account, Region: "eu-south-2", Service: "lambda"},
		awssnapshot.Candidate{Resources: resources}); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) (int, string) {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		res, err := app.Test(request, -1)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(data)
	}
	for _, body := range []string{
		`{"account_id":"` + account + `","kind":"rename","group_id":"stack:shop","name":"Shop"}`,
		`{"account_id":"` + account + `","kind":"rename","group_id":"stack:gone","name":"Gone"}`,
	} {
		if status, text := call(http.MethodPost, "/api/admin/aws/groups/corrections", body); status != fiber.StatusOK {
			t.Fatalf("post = %d %s", status, text)
		}
	}
	read := func() map[string]string {
		status, text := call(http.MethodGet, "/api/admin/aws/groups/corrections", "")
		var list struct {
			Enabled     bool `json:"enabled"`
			Corrections []struct {
				GroupID string `json:"group_id"`
				Status  string `json:"status"`
			} `json:"corrections"`
		}
		if status != fiber.StatusOK || json.Unmarshal([]byte(text), &list) != nil || !list.Enabled {
			t.Fatalf("list = %d %s", status, text)
		}
		out := map[string]string{}
		for _, correction := range list.Corrections {
			out[correction.GroupID] = correction.Status
		}
		return out
	}
	if got := read(); got["stack:shop"] != "applied" || got["stack:gone"] != "no_longer_matches" || len(got) != 2 {
		t.Fatalf("statuses = %v", got)
	}
	config.Connectors.AWS.Enable = false
	if got := read(); got["stack:shop"] != "" || got["stack:gone"] != "" || len(got) != 2 {
		t.Fatalf("statuses with the connector off = %v", got)
	}
}
