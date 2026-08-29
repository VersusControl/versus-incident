package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestToolCallTracePersistsCallID(t *testing.T) {
	encoded, err := json.Marshal(ToolCallTrace{CallID: "call-42", Name: "query_metrics"})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"call_id":"call-42","Name":"query_metrics","Args":"","Output":"","DurationMs":0,"Error":""}` {
		t.Fatalf("encoded trace = %s", encoded)
	}
	var decoded ToolCallTrace
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CallID != "call-42" {
		t.Fatalf("CallID = %q, want call-42", decoded.CallID)
	}
}

type chatCaptureObserver struct{ events []ChatEvent }

func (observer *chatCaptureObserver) OnChatEvent(event ChatEvent) {
	observer.events = append(observer.events, event)
}

func TestChatTaskIsNeverCacheable(t *testing.T) {
	task := ChatTask{SessionID: "session", Message: "what changed?"}
	if task.Kind() != AITaskChat {
		t.Fatalf("Kind() = %q, want %q", task.Kind(), AITaskChat)
	}
	if task.CacheKey() != "" {
		t.Fatalf("CacheKey() = %q, want empty", task.CacheKey())
	}
}

func TestEmitChatEventDeliversAndStampsTime(t *testing.T) {
	observer := &chatCaptureObserver{}
	ctx := WithChatObserver(context.Background(), observer)

	before := time.Now().UTC()
	EmitChatEvent(ctx, ChatEvent{Seq: 3, Kind: ChatEventToolStarted, Tool: "list_services"})
	after := time.Now().UTC()

	if len(observer.events) != 1 {
		t.Fatalf("events = %d, want 1", len(observer.events))
	}
	event := observer.events[0]
	if event.Seq != 3 || event.Kind != ChatEventToolStarted || event.Tool != "list_services" {
		t.Fatalf("event changed in transit: %+v", event)
	}
	if event.At.Before(before) || event.At.After(after) {
		t.Fatalf("At = %s, want within [%s, %s]", event.At, before, after)
	}
}

func TestEmitChatEventWithoutObserverIsNoOp(t *testing.T) {
	EmitChatEvent(context.Background(), ChatEvent{Kind: ChatEventRunStarted})
	if observer := ChatObserverFrom(WithChatObserver(context.Background(), nil)); observer != nil {
		t.Fatalf("nil observer was retained: %#v", observer)
	}
}
