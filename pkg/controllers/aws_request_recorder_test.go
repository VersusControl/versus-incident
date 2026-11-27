package controllers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// blockingTransport counts the requests it answers and holds each until
// released.
type blockingTransport struct {
	entered  chan struct{}
	release  chan struct{}
	answered atomic.Int64
}

func (transport *blockingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.entered <- struct{}{}
	<-transport.release
	transport.answered.Add(1)
	return httptest.NewRecorder().Result(), nil
}

// The recorder's quiesce cuts requests at one point: it waits for a
// request recorded but not yet answered, and refuses every later one
// without recording it, so the ledger and the fake's count agree.
func TestRequestRecorderQuiesceCutsAtOnePoint(t *testing.T) {
	next := &blockingTransport{entered: make(chan struct{}), release: make(chan struct{})}
	recorder := &awsRequestRecorder{next: next}
	request := httptest.NewRequest(http.MethodGet, "https://sts.amazonaws.com/", nil)
	sent := make(chan error, 1)
	go func() {
		_, err := recorder.RoundTrip(request)
		sent <- err
	}()
	<-next.entered
	quiesced := make(chan struct{})
	go func() {
		recorder.quiesce()
		close(quiesced)
	}()
	// Once the cut is made, quiesce must still be waiting on the request.
	eventuallyWithin(t, awsBudgetObserveGuard, "the recorder to close", func() bool {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		return recorder.closed
	})
	select {
	case <-quiesced:
		t.Fatal("quiesce returned while a recorded request was still in flight")
	case err := <-sent:
		t.Fatalf("the in-flight request finished before it was released: %v", err)
	default:
	}
	close(next.release)
	<-quiesced
	if err := <-sent; err != nil {
		t.Fatalf("in-flight request = %v", err)
	}
	if _, err := recorder.RoundTrip(request); !errors.Is(err, errRecorderClosed) {
		t.Fatalf("request after quiesce = %v; want refused", err)
	}
	if got, answered := len(recorder.snapshot()), next.answered.Load(); got != 1 || answered != 1 {
		t.Fatalf("ledger %d, answered %d; want 1 and 1", got, answered)
	}
}
