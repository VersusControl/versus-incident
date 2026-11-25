package kubernetes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestIdlePodCredentialRotationCancelsWithoutAnotherRequest(t *testing.T) {
	opened := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/log") {
			writeJSON(writer, map[string]any{"items": []any{}})
			return
		}
		writer.WriteHeader(200)
		writer.(http.Flusher).Flush()
		select {
		case opened <- struct{}{}:
		default:
		}
		<-request.Context().Done()
		select {
		case closed <- struct{}{}:
		default:
		}
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
	waitFor(t, opened, "idle source did not open")
	header.Store("Bearer rotated-unit-token")
	waitFor(t, done, "idle Pod survived credential identity rotation")
	waitFor(t, closed, "rotated idle upstream was not closed")
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
	waitFor(t, ctx.Done(), "idle stream access was not revalidated")
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

// For a Pod log the API server relays the kubelet's answer, so a 401 there can
// be one node's kubelet refusing the API server. A viewer's log read that gets
// one fails alone: the credential, recovery and history are left as they are,
// and the cluster's other reads go on.
func TestViewerPodLog401LeavesTheCredentialAlone(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		testViewerPodLog401(t, func(service *Service) error {
			return service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "shop", Pod: "api", Container: "app"}, func(PodLogStreamEvent) error { return nil })
		})
	})
	t.Run("bounded read", func(t *testing.T) {
		testViewerPodLog401(t, func(service *Service) error {
			_, err := service.PodLogs(logStreamTestContext(), "shop", "api", "app", false, 60, 10)
			return err
		})
	})
}

func testViewerPodLog401(t *testing.T, read func(*Service) error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/log"):
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`))
		case strings.HasPrefix(request.URL.Path, "/apis/authentication.k8s.io/"):
			writer.WriteHeader(http.StatusNotFound)
		default:
			writeJSON(writer, map[string]any{"kind": "EventList", "apiVersion": "v1", "metadata": map[string]any{}, "items": []any{}})
		}
	}))
	defer server.Close()
	credentials := &credentialSource{authorization: func(ctx context.Context) (string, error) { return "Bearer viewer-unit-token", ctx.Err() }}
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Credentials: credentials})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetScrubber(replacingScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	service.changes.evidenceEnabled = true
	err = read(service)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("log read error = %v, want unauthorized", err)
	}
	service.identity.mu.Lock()
	ended := service.identity.retired || service.identity.ctx.Err() != nil
	service.identity.mu.Unlock()
	if ended {
		t.Fatal("a 401 on a Pod log ended the credential")
	}
	if service.recovery != nil && (service.recovery.failures != 0 || service.recovery.attemptInFlight()) {
		t.Fatal("a 401 on a Pod log started credential recovery")
	}
	service.changes.mu.Lock()
	denials := len(service.changes.historyDenials)
	service.changes.mu.Unlock()
	if denials != 0 {
		t.Fatal("a 401 on a Pod log was recorded as a history denial")
	}
	var list map[string]any
	if err := service.client.GetJSON(t.Context(), "/api/v1/events", &list); err != nil {
		t.Fatalf("the cluster's other reads stopped after a 401 on a Pod log: %v", err)
	}
}

// Only the Pod log subresource itself gets the relayed-401 rule. A Pod named
// "log", or an object named "log" in a namespace named "pods", is an ordinary
// read whose 401 still ends the credential.
func TestPodLog401RuleMatchesOnlyThePodLogPath(t *testing.T) {
	for path, logPath := range map[string]bool{
		"/api/v1/namespaces/shop/pods/api/log?container=app&tailLines=10": true,
		"/api/v1/namespaces/shop/pods/log":                                false,
		"/api/v1/namespaces/pods/secrets/log":                             false,
		"/api/v1/namespaces/pods/configmaps/log":                          false,
		"/api/v1/namespaces/pods/services/log":                            false,
		"/api/v1/namespaces/shop/pods/api/log/extra":                      false,
		"/apis/v1/namespaces/shop/pods/api/log":                           false,
	} {
		t.Run(path, func(t *testing.T) {
			if got := podLogPath(path); got != logPath {
				t.Fatalf("podLogPath = %v, want %v", got, logPath)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if strings.HasPrefix(request.URL.Path, "/apis/authentication.k8s.io/") {
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				writer.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			credentials := &credentialSource{authorization: func(ctx context.Context) (string, error) { return "Bearer unit-token", ctx.Err() }}
			client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Credentials: credentials})
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(client, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
			ctx, cancel := service.credentialContext(t.Context())
			defer cancel()
			var output any
			if err := service.client.GetJSON(ctx, path, &output); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("read error = %v, want unauthorized", err)
			}
			service.identity.mu.Lock()
			ended := service.identity.retired || service.identity.ctx.Err() != nil
			service.identity.mu.Unlock()
			if ended == logPath {
				t.Fatalf("credential ended = %v after a 401 on %s", ended, path)
			}
		})
	}
}
