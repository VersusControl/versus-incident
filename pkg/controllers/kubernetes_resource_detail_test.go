package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/gofiber/fiber/v2"
)

func TestKubernetesResourceDetailReadContractsAndQueryGuards(t *testing.T) {
	gateway := middleware.GatewayAuthEnabled()
	middleware.SetGatewayAuthEnabled(false)
	t.Cleanup(func() { middleware.SetGatewayAuthEnabled(gateway) })
	var upstreamReads atomic.Int32
	streamClosed := make(chan struct{}, 16)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamReads.Add(1)
		write := func(value any) {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(value)
		}
		metadata := func(name, uid string, owner map[string]any) map[string]any {
			value := map[string]any{"name": name, "uid": uid, "namespace": "shop"}
			if owner != nil {
				value["ownerReferences"] = []any{owner}
			}
			return value
		}
		switch request.URL.Path {
		case "/api":
			write(map[string]any{"versions": []string{"v1"}})
		case "/apis":
			write(map[string]any{"groups": []any{map[string]any{"name": "apps", "preferredVersion": map[string]any{"version": "v1"}}}})
		case "/api/v1":
			write(map[string]any{"resources": []any{map[string]any{"name": "pods", "kind": "Pod", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1":
			write(map[string]any{"resources": []any{map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true, "verbs": []string{"get", "list"}}, map[string]any{"name": "replicasets", "kind": "ReplicaSet", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/apis/apps/v1/deployments":
			write(map[string]any{"items": []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata("api", "root", nil)}}})
		case "/apis/apps/v1/replicasets":
			write(map[string]any{"items": []any{map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": metadata("api-rs", "rs", map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "api", "uid": "root", "controller": true})}}})
		case "/api/v1/pods":
			write(map[string]any{"items": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata("api-0", "pod", map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "api-rs", "uid": "rs", "controller": true}), "spec": map[string]any{"containers": []any{map[string]any{"name": "app"}}}}}})
		case "/api/v1/namespaces/shop/pods/api-0":
			write(map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata("api-0", "pod", map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "api-rs", "uid": "rs", "controller": true}), "spec": map[string]any{"containers": []any{map[string]any{"name": "app"}}}})
		case "/api/v1/namespaces/shop/pods/api-0/log":
			writer.Write([]byte("2026-10-01T00:00:00Z secret\n"))
			if request.URL.Query().Get("follow") == "true" {
				writer.(http.Flusher).Flush()
				<-request.Context().Done()
				streamClosed <- struct{}{}
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: server.URL, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, kubernetes.Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetScrubber(streamTestScrubber{})
	app := fiber.New(fiber.Config{Immutable: true})
	app.Use(func(ctx *fiber.Ctx) error {
		middleware.MarkAuthorized(ctx)
		middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), ctx.Get("X-Deny") != "true")
		return ctx.Next()
	})
	NewKubernetesAdminController(service).Register(app.Group("/api"))
	for _, test := range []struct {
		path   string
		deny   bool
		status int
	}{
		{"/usage-history?namespace=shop&pod=api-0", false, 200},
		{"/usage-history?namespace=shop&pod=api-0&unknown=1", false, 400},
		{"/usage-history?namespace=shop&pod=api-0&pod=api-1", false, 400},
		{"/usage-history?namespace=shop&pod=api-0", true, 403},
		{"/workloads/Deployment/api/logs/stream?namespace=shop&uid=root&follow=false", false, 400},
		{"/workloads/Deployment/api/logs/stream?namespace=shop&uid=root&tail_lines=0", false, 400},
		{"/workloads/Deployment/api/logs/stream?namespace=shop&uid=root&container=a&container=b", false, 400},
		{"/workloads/Deployment/api/logs/stream?namespace=shop&uid=root", true, 403},
		{"/workloads/DaemonSet/api/logs/stream?namespace=shop&uid=root", false, 400},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes"+test.path, nil)
		if test.deny {
			request.Header.Set("X-Deny", "true")
		}
		before := upstreamReads.Load()
		response, err := app.Test(request, -1)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("%s: status=%d body=%s", test.path, response.StatusCode, body)
		}
		if upstreamReads.Load() != before {
			t.Fatal("invalid/denied/history-only request reached upstream", test.path)
		}
		if test.status == 200 && (!strings.Contains(string(body), `"samples":[]`) || !strings.Contains(string(body), `"live_since":`)) {
			t.Fatal("history fallback contract missing", string(body))
		}
	}
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/workloads/Deployment/api/logs/stream?namespace=shop&uid=root&previous=true", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(string(body), "event: sources") || !strings.Contains(string(body), "event: line") || !strings.Contains(string(body), "event: end") || strings.Contains(string(body), "secret") {
		t.Fatalf("workload SSE contract status=%d body=%s", response.StatusCode, body)
	}
	t.Run("shared-admission-disconnect-and-reuse", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { defer close(done); _ = app.Listener(listener) }()
		defer func() { _ = app.ShutdownWithTimeout(time.Second); <-done }()
		httpClient := &http.Client{Timeout: 12 * time.Second}
		base := "http://" + listener.Addr().String() + "/api/admin/kubernetes"
		paths := []string{
			"/pods/shop/api-0/logs/stream?container=app",
			"/workloads/Deployment/api/logs/stream?namespace=shop&uid=root",
		}
		open := func(path string, waitForSlot bool) context.CancelFunc {
			t.Helper()
			for attempt := 0; ; attempt++ {
				ctx, cancel := context.WithCancel(context.Background())
				request, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
				response, err := httpClient.Do(request)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
					body, _ := io.ReadAll(response.Body)
					response.Body.Close()
					cancel()
					var detail struct {
						Code      string
						Retryable bool
					}
					if waitForSlot && attempt < 2 && response.StatusCode == http.StatusTooManyRequests && response.Header.Get("Retry-After") == "2" && json.Unmarshal(body, &detail) == nil && detail.Code == "log_stream_busy" && detail.Retryable {
						// Retry only rejected admissions, respecting the server's
						// backoff hint. Keep the first successful response below
						// rather than probing and leaking an admitted viewer.
						time.Sleep(2 * time.Second)
						continue
					}
					t.Fatalf("admitted stream: status=%d body=%s", response.StatusCode, body)
				}
				t.Cleanup(cancel)
				t.Cleanup(func() { response.Body.Close() })
				reader := bufio.NewReader(response.Body)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						t.Fatal(err)
					}
					if line == "event: line\n" {
						return func() { cancel(); response.Body.Close() }
					}
				}
			}
		}
		var cancellations []context.CancelFunc
		defer func() {
			for _, cancel := range cancellations {
				cancel()
			}
			for range cancellations {
				select {
				case <-streamClosed:
				case <-time.After(6 * time.Second):
					t.Error("admitted upstream survived cleanup")
				}
			}
		}()
		for i := 0; i < kubernetes.MaxOrgLogStreams; i++ {
			cancellations = append(cancellations, open(paths[i%len(paths)], false))
		}
		// Both finite and following requests must fail before committing SSE,
		// without consuming another upstream read or overriding authorization.
		for _, path := range paths {
			for _, previous := range []string{"false", "true"} {
				before := upstreamReads.Load()
				response, err := httpClient.Get(base + path + "&previous=" + previous)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				var detail struct {
					Error, Code, Action string
					Retryable           bool
				}
				if json.Unmarshal(body, &detail) != nil || response.StatusCode != http.StatusTooManyRequests || detail.Code != "log_stream_busy" || detail.Error == "" || detail.Action == "" || !detail.Retryable || response.Header.Get("Retry-After") != "2" || response.Header.Get("Content-Type") == "text/event-stream" {
					t.Errorf("busy admission: status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
				}
				// Workload membership validation can read metadata, but a busy
				// Pod request must not open any upstream request.
				if strings.Contains(path, "/pods/") && upstreamReads.Load() != before {
					t.Error("busy Pod admission reached upstream")
				}
				request, _ := http.NewRequest(http.MethodGet, base+path, nil)
				request.Header.Set("X-Deny", "true")
				response, err = httpClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusForbidden || response.Header.Get("Retry-After") != "" {
					t.Fatal("saturation overrode authorization", response.StatusCode)
				}
			}
		}
		for _, path := range paths {
			// Abort one viewer, observe its upstream cancellation, and then
			// reuse its slot. This is eventual cleanup, not instant TCP close.
			cancellations[0]()
			select {
			case <-streamClosed:
			case <-time.After(6 * time.Second):
				t.Fatal("upstream survived client disconnect")
			}
			cancellations[0] = open(path, true)
		}
	})
	response, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/admin/kubernetes/workloads/Deployment/api/logs/stream?namespace=shop&uid=replacement", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 || response.Header.Get("Content-Type") == "text/event-stream" {
		t.Fatal("UID replacement committed SSE headers", response.StatusCode)
	}
}
