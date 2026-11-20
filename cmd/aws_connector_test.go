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
	connector := mountAWS(fiber.New(), storage.NewMemory(), cfg)
	if connector.Settings == nil || connector.Collectors == nil || connector.Snapshots == nil {
		t.Fatalf("connector = %+v", connector)
	}
	collector := connector.Collectors.Collector()
	if collector.Limiter() == nil || collector.Limiter() != connector.Settings.Limiter() {
		t.Fatal("probes and collectors use different limiters")
	}
	if collector.Store() != connector.Snapshots {
		t.Fatal("collector commits to a different snapshot store than the one the settings view reads")
	}
}

// TestShutdownWaitsForCollectorsBeforeClosingStorage: once the root context
// is cancelled the collectors and the cost refresh stop, and shutdown waits
// for that before the store is closed, bounded by a grace period.
func TestShutdownWaitsForCollectorsBeforeClosingStorage(t *testing.T) {
	cfg := &c.Config{}
	cfg.Connectors.AWS.Budgets.MaxRequestsPerSecond = 2
	connector := mountAWS(fiber.New(), storage.NewMemory(), cfg)
	ctx, cancel := context.WithCancel(context.Background())
	connector.Start(ctx)
	cancel()
	started := time.Now()
	connector.Wait(5 * time.Second)
	if time.Since(started) >= 5*time.Second {
		t.Fatal("collectors or the cost refresh did not stop after the root context ended")
	}
	select {
	case <-connector.Collectors.Stopped():
	default:
		t.Fatal("collectors still running after Wait")
	}
	select {
	case <-connector.Cost.Stopped():
	default:
		t.Fatal("cost refresh still running after Wait")
	}
}
