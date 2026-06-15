package core

import "testing"

// TestIsOnCallWorkflowInitialized verifies the initialization probe so the
// emit path can skip on-call instead of panicking when the singleton was
// never set up at boot (QA-003).
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
