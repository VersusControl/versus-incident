package controllers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// failingLabelStore answers every call with one error.
type failingLabelStore struct{ err error }

func (s failingLabelStore) AWSAccountLabels(context.Context, tenancy.OrgScope) ([]storage.AWSAccountLabel, error) {
	return nil, s.err
}

func (s failingLabelStore) PutAWSAccountLabel(context.Context, tenancy.OrgScope, storage.AWSAccountLabel) (storage.AWSAccountLabel, error) {
	return storage.AWSAccountLabel{}, s.err
}

func (s failingLabelStore) DeleteAWSAccountLabel(context.Context, tenancy.OrgScope, string) (bool, error) {
	return false, s.err
}

// The settings permission alone does not open the routes: the view comes
// first. A caller with no settings permission set at all is refused a
// write, not let through.
func TestAccountLabelWritesNeedViewAndExplicitSettings(t *testing.T) {
	store, _ := storage.AWSAccountLabelsOf(storage.NewMemory())
	settingsOnly := labelsApp(t, map[string]bool{string(core.PermissionInfrastructureView): false,
		string(core.PermissionInfrastructureSettingsWrite): true}, store)
	res, body := awsTestRequest(t, settingsOnly, http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"Datadog"}`)
	refusedFor(t, res, body, core.PermissionInfrastructureView)

	viewUnset := labelsApp(t, map[string]bool{string(core.PermissionInfrastructureView): true}, store)
	res, body = awsTestRequest(t, viewUnset, http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"Datadog"}`)
	refusedFor(t, res, body, core.PermissionInfrastructureSettingsWrite)
	res, body = awsTestRequest(t, viewUnset, http.MethodDelete, "/api/admin/aws/accounts/labels/555500006666", "")
	refusedFor(t, res, body, core.PermissionInfrastructureSettingsWrite)
	if labels, _ := store.AWSAccountLabels(t.Context(), tenancy.DefaultOrgScope()); len(labels) != 0 {
		t.Fatalf("a refused write was stored: %+v", labels)
	}
}

// Validation: a malformed body, a name over the limit (counted in
// characters), a control character, a non-numeric account and a derived
// label are each a 400 and store nothing; a name at the limit is saved.
func TestAccountLabelValidation(t *testing.T) {
	store, _ := storage.AWSAccountLabelsOf(storage.NewMemory())
	app := labelsApp(t, map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureSettingsWrite): true}, store)
	path := "/api/admin/aws/accounts/labels/555500006666"
	for name, request := range map[string][2]string{
		"malformed body":     {path, `{"name":`},
		"name over limit":    {path, `{"name":"` + strings.Repeat("é", storage.MaxAWSAccountLabelName+1) + `"}`},
		"control character":  {path, `{"name":"a\u0007b"}`},
		"tab in name":        {path, `{"name":"a\tb"}`},
		"label unknown":      {path, `{"label":"unknown"}`},
		"label same_org":     {path, `{"label":"same_org"}`},
		"letters in account": {"/api/admin/aws/accounts/labels/55550000666a", `{"name":"x"}`},
		"13 digit account":   {"/api/admin/aws/accounts/labels/5555000066660", `{"name":"x"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if res, body := awsTestRequest(t, app, http.MethodPut, request[0], request[1]); res.StatusCode != fiber.StatusBadRequest {
				t.Fatalf("status = %d %s", res.StatusCode, body)
			}
		})
	}
	if labels, _ := store.AWSAccountLabels(t.Context(), tenancy.DefaultOrgScope()); len(labels) != 0 {
		t.Fatalf("an invalid write was stored: %+v", labels)
	}
	if res, body := awsTestRequest(t, app, http.MethodDelete, "/api/admin/aws/accounts/labels/55550000666a", ""); res.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("delete malformed account = %d %s", res.StatusCode, body)
	}
	atLimit := strings.Repeat("é", storage.MaxAWSAccountLabelName)
	if res, body := awsTestRequest(t, app, http.MethodPut, path, `{"label":"known","name":"`+atLimit+`"}`); res.StatusCode != fiber.StatusOK {
		t.Fatalf("name at the limit = %d %s", res.StatusCode, body)
	}
	if labels, _ := store.AWSAccountLabels(t.Context(), tenancy.DefaultOrgScope()); len(labels) != 1 || labels[0].Name != atLimit {
		t.Fatalf("labels = %+v", labels)
	}
}

// A store failure is a 500 with a fixed message that never echoes the
// store's error; a store that is not ready is a 503.
func TestAccountLabelStoreFailures(t *testing.T) {
	permissions := map[string]bool{string(core.PermissionInfrastructureView): true, string(core.PermissionInfrastructureSettingsWrite): true}
	internal := failingLabelStore{err: errors.New("pq: relation aws_account_labels secret-detail")}
	app := labelsApp(t, permissions, internal)
	for _, request := range [][3]string{
		{http.MethodGet, "/api/admin/aws/accounts/labels", ""},
		{http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"x"}`},
		{http.MethodDelete, "/api/admin/aws/accounts/labels/555500006666", ""},
	} {
		res, body := awsTestRequest(t, app, request[0], request[1], request[2])
		if res.StatusCode != fiber.StatusInternalServerError || strings.Contains(string(body), "secret-detail") || strings.Contains(string(body), "pq:") {
			t.Errorf("%s internal error = %d %s", request[0], res.StatusCode, body)
		}
	}
	unsupported := labelsApp(t, permissions, failingLabelStore{err: storage.ErrUnsupported})
	for _, request := range [][3]string{
		{http.MethodGet, "/api/admin/aws/accounts/labels", ""},
		{http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"x"}`},
		{http.MethodDelete, "/api/admin/aws/accounts/labels/555500006666", ""},
	} {
		if res, body := awsTestRequest(t, unsupported, request[0], request[1], request[2]); res.StatusCode != fiber.StatusServiceUnavailable {
			t.Errorf("%s unsupported = %d %s", request[0], res.StatusCode, body)
		}
	}
	empty := labelsApp(t, permissions, nil)
	for _, request := range [][3]string{
		{http.MethodPut, "/api/admin/aws/accounts/labels/555500006666", `{"name":"x"}`},
		{http.MethodDelete, "/api/admin/aws/accounts/labels/555500006666", ""},
	} {
		if res, body := awsTestRequest(t, empty, request[0], request[1], request[2]); res.StatusCode != fiber.StatusServiceUnavailable {
			t.Errorf("%s with no store = %d %s", request[0], res.StatusCode, body)
		}
	}
}
