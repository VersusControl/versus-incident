package core

import (
	"context"
	"errors"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/redis/go-redis/v9"
	"testing"
	"time"
)

type budgetOnCallProvider struct{}

func (budgetOnCallProvider) TriggerOnCall(ctx context.Context, _ string, _ *config.OnCallConfig) error {
	<-ctx.Done()
	return ctx.Err()
}
func TestOnCallStartContextHonorsBudget(t *testing.T) {
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer redisClient.Close()
	w := NewOnCallWorkflow(redisClient, budgetOnCallProvider{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := w.StartContext(ctx, "incident", config.OnCallConfig{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := w.StartContext(ctx, "incident", config.OnCallConfig{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

// TestIsOnCallWorkflowInitialized verifies the initialization probe so the
// emit path can skip on-call instead of panicking when the singleton was
// never set up at boot.
func TestIsOnCallWorkflowInitialized(t *testing.T) {
	onCallWorkflow = nil
	if IsOnCallWorkflowInitialized() {
		t.Fatal("expected IsOnCallWorkflowInitialized to be false when singleton is nil")
	}

	onCallWorkflow = NewOnCallWorkflow(nil, nil)
	t.Cleanup(func() { onCallWorkflow = nil })
	if !IsOnCallWorkflowInitialized() {
		t.Fatal("expected IsOnCallWorkflowInitialized to be true after the singleton is set")
	}
}

// TestGetOnCallWorkflowPanicsWhenUninitialized documents the contract the
// emit-path guard relies on: a nil singleton still panics, so callers must
// check IsOnCallWorkflowInitialized first.
func TestGetOnCallWorkflowPanicsWhenUninitialized(t *testing.T) {
	onCallWorkflow = nil
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected GetOnCallWorkflow to panic when singleton is nil")
		}
	}()
	_ = GetOnCallWorkflow()
}

// TestSetOnCallWorkflow verifies the direct installer seam both installs a
// workflow (making the initialization probe report true) and clears it back
// to the uninitialized default when passed nil.
func TestSetOnCallWorkflow(t *testing.T) {
	onCallWorkflow = nil
	t.Cleanup(func() { onCallWorkflow = nil })

	w := NewOnCallWorkflow(nil, nil)
	SetOnCallWorkflow(w)
	if !IsOnCallWorkflowInitialized() {
		t.Fatal("expected IsOnCallWorkflowInitialized to be true after SetOnCallWorkflow")
	}
	if GetOnCallWorkflow() != w {
		t.Fatal("expected GetOnCallWorkflow to return the installed workflow")
	}

	SetOnCallWorkflow(nil)
	if IsOnCallWorkflowInitialized() {
		t.Fatal("expected IsOnCallWorkflowInitialized to be false after SetOnCallWorkflow(nil)")
	}
}
