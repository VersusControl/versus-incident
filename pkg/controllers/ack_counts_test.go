package controllers

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/incidentcounts"

	"github.com/gofiber/fiber/v2"
)

// An ack through the signed on-call link is the operator's own action, so the
// next poll recounts once and shows the incident as acked instead of serving
// the cached count until it ages out.
func TestSignedLinkAckRecountsTheCachedCounts(t *testing.T) {
	installAckKey(t)
	ackCalls := 0
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(ackRedis{delCalls: &ackCalls}, nil))
	t.Cleanup(func() { core.SetOnCallWorkflow(nil) })
	store, clock := installCountingStore(t, 2)
	app := countsApp(NewIncidentAdminController())

	if got := countsCall(t, app, "GET", "/counts"); openTotal(t, got) != 2 {
		t.Fatalf("first poll = %v, want 2 open", got)
	}

	resp, err := ackTestApp().Test(httptest.NewRequest("GET", signedAckPath("open-0", time.Hour), nil))
	if err != nil {
		t.Fatalf("ack request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated || ackCalls != 1 {
		t.Fatalf("ack status = %d, on-call acks = %d; want 201 and one ack", resp.StatusCode, ackCalls)
	}

	// Past the recount gap but well inside FreshFor: only the ack can make
	// this poll recount.
	clock.Advance(incidentcounts.MinRecountGap)
	after := countsCall(t, app, "GET", "/counts")
	byStatus := after["by_status"].(map[string]any)
	acked := byStatus["acked"].(map[string]any)["total"].(float64)
	if openTotal(t, after) != 1 || acked != 1 {
		t.Fatalf("poll after the signed-link ack = %v, want a recount showing 1 open and 1 acked", after)
	}
	countsCall(t, app, "GET", "/counts")
	if got := store.counts.Load(); got != 2 {
		t.Fatalf("counts reaching storage after one ack and three polls = %d, want 2", got)
	}
}
