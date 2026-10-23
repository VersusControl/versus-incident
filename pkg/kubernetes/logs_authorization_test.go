package kubernetes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdlePodCredentialRotationCancelsWithoutAnotherRequest(t *testing.T) {
	opened := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(200)
		writer.(http.Flusher).Flush()
		opened <- struct{}{}
		<-request.Context().Done()
		closed <- struct{}{}
	}))
	defer server.Close()
	var header atomic.Value
	header.Store("Bearer initial-unit-token")
	credentials := &credentialSource{authorization: func(ctx context.Context) (string, error) { return header.Load().(string), ctx.Err() }}
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Credentials: credentials})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetScrubber(replacingScrubber{})
	ctx, cancel := context.WithCancel(logStreamTestContext())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- service.StreamPodLogs(ctx, PodLogStreamOptions{Namespace: "shop", Pod: "api", Container: "app"}, func(PodLogStreamEvent) error { return nil })
	}()
	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("idle source did not open")
	}
	header.Store("Bearer rotated-unit-token")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("idle Pod survived credential identity rotation")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("rotated idle upstream was not closed")
	}
}

func TestLogStreamAuthorizationInitialDenyAndIdleRevocation(t *testing.T) {
	denied := WithLogStreamAuthorization(context.Background(), func(context.Context) error { return ErrForbidden })
	if _, _, err := logStreamAuthorizationContext(denied); !errors.Is(err, ErrForbidden) {
		t.Fatal("initial revoked access accepted", err)
	}
	var revoke atomic.Bool
	parent := WithLogStreamAuthorization(context.Background(), func(context.Context) error {
		if revoke.Load() {
			return ErrForbidden
		}
		return nil
	})
	ctx, stop, err := logStreamAuthorizationContext(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	revoke.Store(true)
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("idle stream access was not revalidated")
	}
}

func TestLogStreamAuthorizationWatcherJoinsOnCancellation(t *testing.T) {
	var calls atomic.Int32
	ctx, stop, err := logStreamAuthorizationContext(WithLogStreamAuthorization(context.Background(), func(context.Context) error { calls.Add(1); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	stop()
	stop()
	if ctx.Err() == nil || calls.Load() != 1 {
		t.Fatal("watcher did not stop before first idle probe")
	}
}
