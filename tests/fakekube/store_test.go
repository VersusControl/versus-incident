package fakekube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTrafficMetricsDeterministicCounters(t *testing.T) {
	for _, elapsed := range []time.Duration{0, 30 * time.Second, 60 * time.Second} {
		body := trafficMetrics(elapsed)
		seconds := max(1, elapsed.Seconds())
		for _, expected := range []string{
			fmt.Sprintf("response_code=\"200\"} %.3f", 196*seconds),
			fmt.Sprintf("response_code=\"500\"} %.3f", 4*seconds),
			fmt.Sprintf("le=\"+Inf\"} %.3f", 200*seconds),
			`source_workload_namespace="shop"`,
			`destination_workload_namespace="payments"`,
			"# TYPE istio_request_duration_milliseconds histogram",
		} {
			if !strings.Contains(body, expected) {
				t.Fatalf("metric missing %q", expected)
			}
		}
		if body != trafficMetrics(elapsed) {
			t.Fatal("traffic counters are not deterministic")
		}
	}
}

func TestPopulatedScenarioMetadataAndDiscovery(t *testing.T) {
	store := NewStore()
	if err := SeedScenario(store, "populated", 1); err != nil {
		t.Fatal(err)
	}
	for resource, count := range map[string]int{"namespaces": 5, "applications": 1, "kustomizations": 1, "rollouts": 1, "pods": 2, "ingresses": 1} {
		if actual := store.Count(resource); actual != count {
			t.Fatalf("%s count=%d want=%d", resource, actual, count)
		}
	}
	for _, name := range []string{"sh.helm.release.v1.checkout.v1", "sh.helm.release.v1.checkout.v2"} {
		body, found := store.Get("secrets", "shop", name)
		var object map[string]any
		if !found || json.Unmarshal(body, &object) != nil || object["data"] != nil {
			t.Fatalf("release metadata missing or contains payload: %s", name)
		}
		labels := object["metadata"].(map[string]any)["labels"].(map[string]any)
		if labels["owner"] != "helm" || labels["name"] != "checkout" || labels["version"] == "" || labels["status"] == "" {
			t.Fatalf("release labels=%v", labels)
		}
	}
	for _, path := range []string{"/apis/argoproj.io/v1alpha1", "/apis/kustomize.toolkit.fluxcd.io/v1"} {
		if len(discoveryForPath(path, "populated")["resources"].([]map[string]any)) == 0 {
			t.Fatalf("discovery empty for %s", path)
		}
	}
	groups, err := json.Marshal(apiGroups("populated"))
	if err != nil || !strings.Contains(string(groups), "argoproj.io") {
		t.Fatal("GitOps group discovery missing")
	}
	for _, resource := range []string{"secrets", "configmaps", "pods"} {
		page, err := store.List(resource, "", url.Values{})
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range page.Items {
			if strings.Contains(string(body), "secret-token") || strings.Contains(string(body), "must-not-cross") || strings.Contains(string(body), "canary-never-project") {
				t.Fatalf("%s retained a payload canary", resource)
			}
		}
	}
}

func TestGeneratedPodMatchesValidatedStoreInsertion(t *testing.T) {
	for _, test := range []struct {
		name     string
		phase    string
		ready    bool
		restarts int
		state    json.RawMessage
	}{
		{name: "running", phase: "Running", ready: true, state: json.RawMessage(`{"running":{}}`)},
		{name: "crashing", phase: "Pending", restarts: 6, state: json.RawMessage(`{"waiting":{"reason":"CrashLoopBackOff"}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			generated, validated := NewStore(), NewStore()
			for _, store := range []*Store{generated, validated} {
				if err := store.Upsert("configmaps", "shop", "existing", json.RawMessage(`{"kind":"ConfigMap"}`)); err != nil {
					t.Fatal(err)
				}
			}
			generated.reserve(10)
			pod := generatedPod{
				Kind:     "Pod",
				Metadata: generatedPodMetadata{Name: "web-0", Namespace: "shop", UID: "uid-web-0", Labels: generatedPodLabels{Application: "web", App: "web"}, OwnerReferences: []generatedPodOwner{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-rs", UID: "uid-web-rs"}}},
				Spec:     generatedPodSpec{NodeName: "node-000", Containers: []generatedPodContainer{{Name: "app", Image: "example.invalid/checkout:v1"}}},
				Status:   generatedPodStatus{Phase: test.phase, ContainerStatuses: []generatedPodContainerStatus{{Name: "app", Ready: test.ready, RestartCount: test.restarts, State: test.state}}},
			}
			body, err := json.Marshal(pod)
			if err != nil {
				t.Fatal(err)
			}
			if err := validated.Upsert("pods", "shop", "web-0", body); err != nil {
				t.Fatal(err)
			}
			if err := generated.upsertGeneratedPod(pod); err != nil {
				t.Fatal(err)
			}
			actual, found := generated.Get("pods", "shop", "web-0")
			if !found || generated.Count("configmaps") != 1 {
				t.Fatal("generated insertion or capacity reservation lost an object")
			}
			expected, _ := validated.Get("pods", "shop", "web-0")
			var actualObject, expectedObject map[string]any
			if err := json.Unmarshal(actual, &actualObject); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(expected, &expectedObject); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actualObject, expectedObject) {
				t.Fatalf("generated object=%v, want %v", actualObject, expectedObject)
			}
			actual[0] = '!'
			retained, _ := generated.Get("pods", "shop", "web-0")
			if bytes.Equal(actual, retained) || !json.Valid(retained) {
				t.Fatal("Get exposed the stored generated body for mutation")
			}
		})
	}
}

func TestStoreListPaginatesAndAppliesSelectors(t *testing.T) {
	store := NewStore()
	for name, body := range map[string]string{
		"api-1": `{"kind":"Pod","metadata":{"labels":{"app":"checkout"}},"spec":{"nodeName":"node-a"}}`,
		"api-2": `{"kind":"Pod","metadata":{"labels":{"app":"checkout"}},"spec":{"nodeName":"node-b"}}`,
		"db-1":  `{"kind":"Pod","metadata":{"labels":{"app":"database"}},"spec":{"nodeName":"node-a"}}`,
	} {
		if err := store.Upsert("pods", "shop", name, json.RawMessage(body)); err != nil {
			t.Fatal(err)
		}
	}
	query := url.Values{"limit": {"1"}, "labelSelector": {"app=checkout"}, "fieldSelector": {"spec.nodeName=node-a"}}
	page, err := store.List("pods", "shop", query)
	if err != nil || len(page.Items) != 1 || page.Continue != "" {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	var object map[string]any
	if err := json.Unmarshal(page.Items[0], &object); err != nil {
		t.Fatal(err)
	}
	metadata := object["metadata"].(map[string]any)
	if metadata["name"] != "api-1" || metadata["resourceVersion"] == "" {
		t.Fatalf("object metadata=%#v", metadata)
	}
	for index := 0; index < 3; index++ {
		if err := store.Upsert("pods", "shop", "checkout-"+string(rune('a'+index)), json.RawMessage(`{"kind":"Pod"}`)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.List("pods", "shop", url.Values{"limit": {"2"}})
	if err != nil || len(first.Items) != 2 || first.Continue == "" {
		t.Fatalf("first page=%#v err=%v", first, err)
	}
	second, err := store.List("pods", "shop", url.Values{"limit": {"2"}, "continue": {first.Continue}})
	if err != nil || len(second.Items) != 2 {
		t.Fatalf("second page=%#v err=%v", second, err)
	}
	if err := store.Upsert("pods", "shop", "new-pod", json.RawMessage(`{"kind":"Pod"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List("pods", "shop", url.Values{"limit": {"2"}, "continue": {first.Continue}}); err != ErrExpiredContinue {
		t.Fatalf("continued list after mutation error=%v", err)
	}
}
