package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReleasesUseMetadataOnlyAndGroupLatestRevision(t *testing.T) {
	var metadataRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true, "verbs": []string{"get", "list"}}}})
		case "/api/v1/namespaces/ops/secrets":
			metadataRequests.Add(1)
			if accept := request.Header.Get("Accept"); accept != "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1" {
				t.Errorf("Secret list Accept = %q", accept)
			}
			if request.URL.Query().Get("labelSelector") != "owner=helm" && request.URL.Query().Get("labelSelector") != "owner=helm,name=checkout" {
				t.Errorf("label selector = %q", request.URL.Query().Get("labelSelector"))
			}
			if request.URL.Query().Get("continue") == "releases-page-2" {
				writeJSON(writer, map[string]any{"items": []any{helmMetadataFixture("api", "1", "deployed")}})
				return
			}
			writeJSON(writer, map[string]any{"metadata": map[string]any{"continue": "releases-page-2"}, "items": []any{
				helmMetadataFixture("checkout", "1", "superseded"),
				helmMetadataFixture("checkout", "2", "deployed"),
				helmMetadataFixture("checkout", "3", "failed"),
				map[string]any{"metadata": map[string]any{"namespace": "ops", "labels": map[string]string{"owner": "other", "name": "ignored", "version": "1", "status": "failed"}}},
			}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster-a"})
	releases, err := service.Releases(context.Background(), HelmReleaseOptions{Namespace: "ops"})
	if err != nil || len(releases.Items) != 2 || releases.Items == nil {
		t.Fatalf("releases = %#v err=%v", releases, err)
	}
	if releases.Items[0].Current.Revision != 2 || releases.Items[0].History[0].Revision != 3 || releases.Items[0].Health != "warning" {
		t.Fatalf("release = %#v", releases.Items[0])
	}
	encoded, err := json.Marshal(releases)
	if err != nil || strings.Contains(string(encoded), "SensitiveCanary") {
		t.Fatalf("release output contains secret payload: %s err=%v", encoded, err)
	}
	releasePage, err := service.Releases(context.Background(), HelmReleaseOptions{Namespace: "ops", Limit: 1})
	if err != nil || len(releasePage.Items) != 1 || releasePage.Items[0].Name != "checkout" || releasePage.Next != "1" || !releasePage.Truncated {
		t.Fatalf("first release page = %#v err=%v", releasePage, err)
	}
	releasePage, err = service.Releases(context.Background(), HelmReleaseOptions{Namespace: "ops", Limit: 1, Cursor: releasePage.Next})
	if err != nil || len(releasePage.Items) != 1 || releasePage.Items[0].Name != "api" || releasePage.Next != "" || releasePage.Truncated {
		t.Fatalf("second release page = %#v err=%v", releasePage, err)
	}
	detail, err := service.Release(context.Background(), "ops", "checkout")
	if err != nil || detail.Current.Revision != 2 || metadataRequests.Load() != 7 {
		t.Fatalf("detail = %#v metadata requests=%d err=%v", detail, metadataRequests.Load(), err)
	}
}

func helmMetadataFixture(name, version, status string) map[string]any {
	return map[string]any{"metadata": map[string]any{
		"name":      "sh.helm.release.v1." + name + ".v" + version,
		"namespace": "ops",
		"labels":    map[string]string{"owner": "helm", "name": name, "version": version, "status": status},
	}}
}
