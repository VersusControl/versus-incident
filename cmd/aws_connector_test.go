package main

import (
	"context"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestAWSConnectorSharesOneLimiter proves the boot wiring hands the settings
// probes and the collectors the same limiter, so both draw on one budget per
// account and region and both show on one API-use meter.
func TestAWSConnectorSharesOneLimiter(t *testing.T) {
	cfg := &c.Config{}
	cfg.Connectors.AWS.Budgets.MaxRequestsPerSecond = 2
	connector := registerAWSSettingsController(fiber.New(), storage.NewMemory(), cfg)
	if connector.settings == nil || connector.scheduler == nil || connector.snapshots == nil {
		t.Fatalf("connector = %+v", connector)
	}
	collector := connector.scheduler.Collector()
	if collector.Limiter() == nil || collector.Limiter() != connector.settings.Limiter() {
		t.Fatal("probes and collectors use different limiters")
	}
	if collector.Store() != connector.snapshots {
		t.Fatal("collector commits to a different snapshot store than the one the settings view reads")
	}
}

// TestShutdownWaitsForCollectorsBeforeClosingStorage: once the root context
// is cancelled the collectors stop, and shutdown waits for that before the
// store is closed, bounded by a grace period.
func TestShutdownWaitsForCollectorsBeforeClosingStorage(t *testing.T) {
	cfg := &c.Config{}
	cfg.Connectors.AWS.Budgets.MaxRequestsPerSecond = 2
	connector := registerAWSSettingsController(fiber.New(), storage.NewMemory(), cfg)
	ctx, cancel := context.WithCancel(context.Background())
	connector.scheduler.Start(ctx)
	cancel()
	if !waitForStop(connector.scheduler.Stopped(), 5*time.Second) {
		t.Fatal("collectors did not stop after the root context ended")
	}
	never := make(chan struct{})
	started := time.Now()
	if waitForStop(never, 20*time.Millisecond) || time.Since(started) > time.Second {
		t.Fatal("grace period not honoured")
	}
}
