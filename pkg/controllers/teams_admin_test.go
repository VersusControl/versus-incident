package controllers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/teams"
	"github.com/gofiber/fiber/v2"
)

type assignStorage struct {
	storage.Provider
	saveErr error
}

func (s *assignStorage) SaveIncident(rec *storage.IncidentRecord) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.Provider.SaveIncident(rec)
}

func TestAssignIncident_ReadOnlyConflictAndWritableSuccess(t *testing.T) {
	base := storage.NewMemory()
	for _, rec := range []*storage.IncidentRecord{
		{ID: "archived", OrgID: storage.DefaultOrgID},
		{ID: "licensed", OrgID: "licensed"},
	} {
		if err := base.SaveIncident(rec); err != nil {
			t.Fatalf("seed incident %q: %v", rec.ID, err)
		}
	}
	teamStore, err := teams.NewStore(base)
	if err != nil {
		t.Fatalf("new teams store: %v", err)
	}
	team, err := teamStore.CreateTeam(teams.Team{Name: "Platform"})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	controller := NewTeamsAdminController(teamStore)
	app := fiber.New()
	app.Post("/incidents/:id/assign", controller.assignIncident)
	t.Cleanup(func() { services.SetStorage(nil) })

	services.SetStorage(&assignStorage{Provider: base, saveErr: storage.ErrReadOnlyIncident})
	conflict := assignRequest(t, app, "archived", team.ID)
	defer conflict.Body.Close()
	if conflict.StatusCode != fiber.StatusConflict {
		t.Fatalf("read-only status = %d, want %d", conflict.StatusCode, fiber.StatusConflict)
	}
	var conflictBody struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(conflict.Body).Decode(&conflictBody); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if conflictBody.Error != storage.ErrReadOnlyIncident.Error() {
		t.Fatalf("conflict error = %q, want %q", conflictBody.Error, storage.ErrReadOnlyIncident)
	}

	services.SetStorage(&assignStorage{Provider: base})
	success := assignRequest(t, app, "licensed", team.ID)
	defer success.Body.Close()
	if success.StatusCode != fiber.StatusOK {
		t.Fatalf("writable status = %d, want %d", success.StatusCode, fiber.StatusOK)
	}
	updated, err := base.GetIncident("licensed")
	if err != nil {
		t.Fatalf("get writable incident: %v", err)
	}
	if updated.AssignedTeamID != team.ID {
		t.Fatalf("assigned team = %q, want %q", updated.AssignedTeamID, team.ID)
	}
}

func assignRequest(t *testing.T, app *fiber.App, incidentID, teamID string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"team_id": teamID})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request := httptest.NewRequest("POST", "/incidents/"+incidentID+"/assign", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := app.Test(request)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return response
}
