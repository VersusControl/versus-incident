package common

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	c "github.com/VersusControl/versus-incident/pkg/config"
)

// fakeSQS implements sqsClient and lets tests script the receive
// queue + assert which messages got deleted.
type fakeSQS struct {
	mu sync.Mutex

	// receiveQueue is a list of batches to return on successive
	// ReceiveMessage calls. After the script is exhausted, further
	// calls return an empty batch (simulating "no messages").
	receiveQueue [][]types.Message
	receiveErr   error // returned every call when non-nil
	calls        int32

	deleted []string // receipt handles passed to DeleteMessage
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	atomic.AddInt32(&f.calls, 1)
	if f.receiveErr != nil {
		return nil, f.receiveErr
	}
	if len(f.receiveQueue) == 0 {
		return &sqs.ReceiveMessageOutput{}, nil
	}
	batch := f.receiveQueue[0]
	f.receiveQueue = f.receiveQueue[1:]
	return &sqs.ReceiveMessageOutput{Messages: batch}, nil
}

func (f *fakeSQS) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, aws.ToString(in.ReceiptHandle))
	return &sqs.DeleteMessageOutput{}, nil
}

func msg(id, body, receipt string) types.Message {
	return types.Message{
		MessageId:     aws.String(id),
		Body:          aws.String(body),
		ReceiptHandle: aws.String(receipt),
	}
}

func newListenerWithFake(t *testing.T, fake *fakeSQS) *SQSListener {
	t.Helper()
	return &SQSListener{
		client:                   fake,
		queueURL:                 "https://sqs.test/queue/x",
		maxNumberOfMessages:      10,
		waitTimeSeconds:          20,
		visibilityTimeoutSeconds: 30,
		receiveErrorBackoff:      1 * time.Millisecond,
	}
}

// startInGoroutine runs StartListening in a goroutine. Returns a
// cancel function the test calls when ready to stop. StartListening
// loops forever; the test relies on Go's goroutine leak being fine
// for unit-test lifetime and asserts only on observable side-effects.
func startInGoroutine(t *testing.T, l *SQSListener, handler func(*map[string]interface{}) error) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// StartListening never returns nil; we don't wait for it.
		_ = l.StartListening(handler)
	}()
	// We can't cleanly stop the loop without context — but tests
	// finish in <100ms and process teardown reaps the goroutine.
	t.Cleanup(func() { /* no-op; goroutine dies with process */ })
}

// TestSQS_HappyPathReceiveAndDelete: a single JSON message arrives,
// handler returns nil, the message is deleted.
func TestSQS_HappyPathReceiveAndDelete(t *testing.T) {
	fake := &fakeSQS{
		receiveQueue: [][]types.Message{
			{msg("m1", `{"alertname":"test","severity":"high"}`, "receipt-1")},
		},
	}
	l := newListenerWithFake(t, fake)

	var got map[string]interface{}
	gotCh := make(chan struct{}, 1)
	handler := func(c *map[string]interface{}) error {
		got = *c
		select {
		case gotCh <- struct{}{}:
		default:
		}
		return nil
	}
	startInGoroutine(t, l, handler)

	select {
	case <-gotCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handler never invoked")
	}

	if got["alertname"] != "test" || got["severity"] != "high" {
		t.Fatalf("handler got wrong content: %+v", got)
	}

	// Wait briefly for the DeleteMessage that follows handler success.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := len(fake.deleted)
		fake.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleted) != 1 || fake.deleted[0] != "receipt-1" {
		t.Fatalf("expected delete of receipt-1, got %v", fake.deleted)
	}
}

// TestSQS_HandlerErrorLeavesMessageForRetry: when handler returns
// non-nil, DeleteMessage MUST NOT be called — SQS visibility timeout
// + redrive policy handle the retry.
func TestSQS_HandlerErrorLeavesMessageForRetry(t *testing.T) {
	fake := &fakeSQS{
		receiveQueue: [][]types.Message{
			{msg("m1", `{"x":1}`, "receipt-1")},
		},
	}
	l := newListenerWithFake(t, fake)

	called := make(chan struct{}, 1)
	handler := func(c *map[string]interface{}) error {
		select {
		case called <- struct{}{}:
		default:
		}
		return errors.New("downstream broken")
	}
	startInGoroutine(t, l, handler)

	select {
	case <-called:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handler never invoked")
	}

	// Give the loop time to NOT call DeleteMessage.
	time.Sleep(50 * time.Millisecond)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleted) != 0 {
		t.Fatalf("handler error must not trigger delete; got deletes=%v", fake.deleted)
	}
}

// TestSQS_NonJSONBodyIsDeleted: malformed bodies are deleted (not
// retried). Otherwise a single bad publisher could pin a message in
// the queue forever.
func TestSQS_NonJSONBodyIsDeleted(t *testing.T) {
	fake := &fakeSQS{
		receiveQueue: [][]types.Message{
			{msg("m1", "this is not json", "receipt-1")},
		},
	}
	l := newListenerWithFake(t, fake)
	startInGoroutine(t, l, func(*map[string]interface{}) error {
		t.Fatal("handler should not be invoked for invalid JSON")
		return nil
	})

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := len(fake.deleted)
		fake.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleted) != 1 {
		t.Fatalf("malformed message must be deleted; got %v", fake.deleted)
	}
}

// TestSQS_ReceiveErrorBackoffsAndRetries: when ReceiveMessage fails,
// the loop should sleep + retry — not exit. After the error clears,
// the next batch should still be processed.
func TestSQS_ReceiveErrorBackoffsAndRetries(t *testing.T) {
	fake := &fakeSQS{
		receiveErr: errors.New("auth expired"),
	}
	l := newListenerWithFake(t, fake)

	var handled int32
	startInGoroutine(t, l, func(*map[string]interface{}) error {
		atomic.AddInt32(&handled, 1)
		return nil
	})

	// Let the loop retry a few times.
	time.Sleep(20 * time.Millisecond)
	calls := atomic.LoadInt32(&fake.calls)
	if calls < 2 {
		t.Fatalf("expected >=2 ReceiveMessage attempts during error window, got %d", calls)
	}

	// Clear the error and queue a real message.
	fake.mu.Lock()
	fake.receiveErr = nil
	fake.receiveQueue = [][]types.Message{
		{msg("m1", `{"ok":true}`, "receipt-1")},
	}
	fake.mu.Unlock()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&handled) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&handled) == 0 {
		t.Fatal("after error cleared, listener should resume processing")
	}
}

// TestSQS_BatchOfMessagesAllProcessed: a single ReceiveMessage can
// return up to 10 messages; the listener must process each.
func TestSQS_BatchOfMessagesAllProcessed(t *testing.T) {
	batch := []types.Message{
		msg("m1", `{"id":1}`, "r1"),
		msg("m2", `{"id":2}`, "r2"),
		msg("m3", `{"id":3}`, "r3"),
	}
	fake := &fakeSQS{receiveQueue: [][]types.Message{batch}}
	l := newListenerWithFake(t, fake)

	var seen sync.Map
	gotN := make(chan struct{}, 3)
	startInGoroutine(t, l, func(c *map[string]interface{}) error {
		seen.Store((*c)["id"], true)
		gotN <- struct{}{}
		return nil
	})

	for i := 0; i < 3; i++ {
		select {
		case <-gotN:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("only saw %d/3 messages", i)
		}
	}

	for _, id := range []float64{1, 2, 3} {
		if _, ok := seen.Load(id); !ok {
			t.Fatalf("missing id %v", id)
		}
	}

	// All three should be deleted.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		n := len(fake.deleted)
		fake.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleted) != 3 {
		t.Fatalf("expected 3 deletes, got %d (%v)", len(fake.deleted), fake.deleted)
	}
}

// TestNewSQSListener_RequiresQueueURL guards the config validation:
// an empty queue_url must fail construction so a misconfigured deploy
// can't silently sit in StartListening forever.
func TestNewSQSListener_RequiresQueueURL(t *testing.T) {
	_, err := NewSQSListener(c.SQSConfig{})
	if err == nil {
		t.Fatal("NewSQSListener(empty) must return an error")
	}
}
