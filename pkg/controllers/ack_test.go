package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
)

// ackTestKey is the signing key installed for the ack handler tests so they can
// mint valid tokens the same way the running service does.
var ackTestKey = []byte("ack-test-signing-key-0123456789ab")

// installAckKey installs a known signing key for the duration of a test and
// restores the previous one on cleanup.
func installAckKey(t *testing.T) {
	t.Helper()
	prev := services.AckSigningKey()
	services.SetAckSigningKey(ackTestKey)
	t.Cleanup(func() { services.SetAckSigningKey(prev) })
}

// ackTestApp mounts HandleAck on the public ack route the same way the router
// registers it, so requests exercise the real handler path.
func ackTestApp() *fiber.App {
	app := fiber.New()
	app.Get("/api/ack/:incidentID", HandleAck)
	return app
}

// signedAckPath builds the ack URL path (with exp + sig query) for id, valid
// for the given TTL from now, using the installed test key.
func signedAckPath(id string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	sig := services.SignAckToken(ackTestKey, id, exp)
	return fmt.Sprintf("/api/ack/%s?exp=%d&sig=%s", id, exp, sig)
}

type ackStorage struct {
	storage.Provider
	checkErr  error
	updateErr error
}

func (s *ackStorage) CheckIncidentMutable(string) error {
	return s.checkErr
}

func (s *ackStorage) UpdateIncidentAck(string, time.Time) error {
	return s.updateErr
}

type ackRedis struct {
	redis.UniversalClient
	delCalls *int
}

func (ackRedis) Exists(ctx context.Context, _ ...string) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(1)
	return cmd
}

func (r ackRedis) Del(ctx context.Context, _ ...string) *redis.IntCmd {
	if r.delCalls != nil {
		*r.delCalls++
	}
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(1)
	return cmd
}

func TestHandleAck_StorageSentinelResponses(t *testing.T) {
	installAckKey(t)
	t.Cleanup(func() {
		core.SetOnCallWorkflow(nil)
		services.SetStorage(nil)
	})

	tests := []struct {
		name       string
		checkErr   error
		updateErr  error
		wantStatus int
		wantError  string
		wantAcked  bool
	}{
		{"pre-check not found", storage.ErrNotFound, nil, fiber.StatusNotFound, "incident not found", false},
		{"pre-check read only", storage.ErrReadOnlyIncident, nil, fiber.StatusConflict, storage.ErrReadOnlyIncident.Error(), false},
		{"update not found", nil, storage.ErrNotFound, fiber.StatusCreated, "", true},
		{"update read only", nil, storage.ErrReadOnlyIncident, fiber.StatusCreated, "", true},
		{"update backend failure", nil, fmt.Errorf("disk path /secret/store"), fiber.StatusCreated, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ackCalls := 0
			core.SetOnCallWorkflow(core.NewOnCallWorkflow(ackRedis{delCalls: &ackCalls}, nil))
			services.SetStorage(&ackStorage{
				Provider:  storage.NewMemory(),
				checkErr:  tc.checkErr,
				updateErr: tc.updateErr,
			})

			resp, err := ackTestApp().Test(httptest.NewRequest("GET", signedAckPath("i-123", time.Hour), nil))
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			var body struct {
				Error  string `json:"error"`
				Status string `json:"status"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Error != tc.wantError {
				t.Fatalf("error = %q, want %q", body.Error, tc.wantError)
			}
			if tc.wantAcked {
				if ackCalls != 1 || body.Status != "success" {
					t.Fatalf("ack calls = %d, status = %q; want one call and success", ackCalls, body.Status)
				}
			} else if ackCalls != 0 {
				t.Fatalf("ack calls = %d, want 0", ackCalls)
			}
		})
	}
}

// TestHandleAck_UninitializedReturns503 proves the DoS fix: an ack request with
// a VALID token but on-call never initialized (the default) returns a clean 503
// instead of panicking and taking down the process.
func TestHandleAck_UninitializedReturns503(t *testing.T) {
	installAckKey(t)
	core.SetOnCallWorkflow(nil)

	app := ackTestApp()
	resp, err := app.Test(httptest.NewRequest("GET", signedAckPath("anything", time.Hour), nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("expected %d when on-call uninitialized, got %d", fiber.StatusServiceUnavailable, resp.StatusCode)
	}
}

// TestHandleAck_InitializedProceeds proves that when the workflow IS installed
// and the token is valid the handler skips the 503 guard and delegates to Ack.
// With a workflow that has no Redis client, Ack reports it isn't fully wired,
// so the handler returns the existing 500 error path — the key point being it
// is NOT the 503 guard, NOT the 401 auth guard, and it does NOT panic.
func TestHandleAck_InitializedProceeds(t *testing.T) {
	installAckKey(t)
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(nil, nil))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })

	app := ackTestApp()
	resp, err := app.Test(httptest.NewRequest("GET", signedAckPath("i-123", time.Hour), nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == fiber.StatusServiceUnavailable {
		t.Fatalf("did not expect the 503 uninitialized guard once the workflow is installed")
	}
	if resp.StatusCode == fiber.StatusUnauthorized {
		t.Fatalf("did not expect the 401 auth guard for a valid token")
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("expected %d from the Ack error path, got %d", fiber.StatusInternalServerError, resp.StatusCode)
	}
}

// TestHandleAck_MissingTokenRejected proves the auth check runs FIRST: a
// request with no token is rejected 401 BEFORE the on-call 503 guard, even
// though on-call is uninitialized.
func TestHandleAck_MissingTokenRejected(t *testing.T) {
	installAckKey(t)
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(nil, nil))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })

	app := ackTestApp()
	resp, err := app.Test(httptest.NewRequest("GET", "/api/ack/i-123", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("expected %d for a missing token, got %d", fiber.StatusUnauthorized, resp.StatusCode)
	}
}

// TestHandleAck_ExpiredTokenRejected proves an expired but correctly-signed
// token is rejected 403 without acking.
func TestHandleAck_ExpiredTokenRejected(t *testing.T) {
	installAckKey(t)
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(nil, nil))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })

	app := ackTestApp()
	resp, err := app.Test(httptest.NewRequest("GET", signedAckPath("i-123", -time.Hour), nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("expected %d for an expired token, got %d", fiber.StatusForbidden, resp.StatusCode)
	}
}

// TestHandleAck_TamperedTokenRejected proves a token whose id was swapped (a
// valid signature for a DIFFERENT incident) is rejected 401 — the IDOR fix.
func TestHandleAck_TamperedTokenRejected(t *testing.T) {
	installAckKey(t)
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(nil, nil))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })

	// Sign for one incident, then request a different one with that signature.
	exp := time.Now().Add(time.Hour).Unix()
	sig := services.SignAckToken(ackTestKey, "other-incident", exp)
	path := fmt.Sprintf("/api/ack/i-123?exp=%d&sig=%s", exp, sig)

	app := ackTestApp()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("expected %d for a tampered token, got %d", fiber.StatusUnauthorized, resp.StatusCode)
	}
}
