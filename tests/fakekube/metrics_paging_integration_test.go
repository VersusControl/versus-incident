//go:build integration

package fakekube

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestScaleMetricsAdvertisedInDiscovery(t *testing.T) {
	server, err := NewServer(Config{Scenario: "scale", Pods: 1, Namespaces: 1})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(server)
	defer upstream.Close()
	response, err := upstream.Client().Get(upstream.URL + "/apis")
	if err != nil {
		t.Fatal(err)
	}
	var groups struct {
		Groups []struct{ Name string } `json:"groups"`
	}
	err = json.NewDecoder(response.Body).Decode(&groups)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, group := range groups.Groups {
		found = found || group.Name == "metrics.k8s.io"
	}
	if !found {
		t.Fatal("scale fixture serves metrics but does not advertise their API group")
	}
	response, err = upstream.Client().Get(upstream.URL + "/apis/metrics.k8s.io/v1beta1")
	if err != nil {
		t.Fatal(err)
	}
	var resources struct {
		Resources []struct{ Name, Kind string } `json:"resources"`
	}
	err = json.NewDecoder(response.Body).Decode(&resources)
	response.Body.Close()
	if err != nil || len(resources.Resources) != 2 {
		t.Fatalf("metrics resources: %+v %v", resources, err)
	}
}

func TestGeneratedMetricsPageTheWholeInventory(t *testing.T) {
	server, err := NewServer(Config{Scenario: "scale", Pods: 60000, Namespaces: 27, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(server)
	defer upstream.Close()
	for _, resource := range []string{"pods", "nodes"} {
		path := "/apis/metrics.k8s.io/v1beta1/" + resource
		continuation, count, pages := "", 0, 0
		seen := map[string]bool{}
		for {
			response, err := upstream.Client().Get(upstream.URL + path + "?limit=500&continue=" + url.QueryEscape(continuation))
			if err != nil {
				t.Fatal(err)
			}
			var page struct {
				Metadata struct {
					Continue string `json:"continue"`
				} `json:"metadata"`
				Items []struct {
					Metadata   struct{ Name, Namespace string } `json:"metadata"`
					Containers []json.RawMessage                `json:"containers"`
					Usage      map[string]string                `json:"usage"`
				} `json:"items"`
			}
			err = json.NewDecoder(response.Body).Decode(&page)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("metric page: status %d error %v", response.StatusCode, err)
			}
			for _, item := range page.Items {
				key := item.Metadata.Namespace + "/" + item.Metadata.Name
				if seen[key] || resource == "pods" && len(item.Containers) != 1 || resource == "nodes" && item.Usage["cpu"] == "" {
					t.Fatalf("duplicate or invalid sample: %s", key)
				}
				seen[key] = true
			}
			count += len(page.Items)
			pages++
			continuation = page.Metadata.Continue
			if continuation == "" {
				break
			}
		}
		expected := 60000
		if resource == "nodes" {
			expected = 60
		}
		if count != expected || resource == "pods" && pages != 120 {
			t.Fatalf("%s: count=%d pages=%d", resource, count, pages)
		}
	}
}
