package fakekube

import (
	"encoding/json"
	"testing"
)

func TestTriageAndPopulatedPodsUseControllerUIDChains(t *testing.T) {
	for _, scenario := range []string{"triage", "populated"} {
		t.Run(scenario, func(t *testing.T) {
			store := NewStore()
			if err := SeedScenario(store, scenario, 1); err != nil {
				t.Fatal(err)
			}
			expected := []struct{ resource, name, uid, ownerKind, ownerUID string }{
				{"deployments", "checkout-api", "deployment-checkout", "", ""},
				{"replicasets", "checkout-api-rs", "replicaset-checkout", "Deployment", "deployment-checkout"},
				{"pods", "checkout-api-0", "pod-checkout-0", "ReplicaSet", "replicaset-checkout"},
			}
			for _, want := range expected {
				raw, exists := store.Get(want.resource, "payments", want.name)
				if !exists {
					t.Fatalf("missing %s", want.name)
				}
				var object struct {
					Metadata struct {
						UID    string `json:"uid"`
						Owners []struct {
							Kind       string `json:"kind"`
							UID        string `json:"uid"`
							Controller bool   `json:"controller"`
						} `json:"ownerReferences"`
					} `json:"metadata"`
				}
				if json.Unmarshal(raw, &object) != nil || object.Metadata.UID != want.uid {
					t.Fatalf("fixture UID lost: %s", raw)
				}
				if want.ownerKind != "" && (len(object.Metadata.Owners) != 1 || object.Metadata.Owners[0].Kind != want.ownerKind || object.Metadata.Owners[0].UID != want.ownerUID || !object.Metadata.Owners[0].Controller) {
					t.Fatalf("fixture does not prove controller ownership: %s", raw)
				}
			}
		})
	}
}

func TestGeneratedFleetOwnersAreControllers(t *testing.T) {
	store := NewStore()
	if err := Generate(store, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	for _, object := range []struct{ resource, name string }{{"replicasets", "app-00000-rs"}, {"pods", "app-00000-rs-00"}} {
		raw, exists := store.Get(object.resource, "load-0000", object.name)
		if !exists {
			t.Fatal("missing generated object", object.name)
		}
		var value map[string]any
		if json.Unmarshal(raw, &value) != nil {
			t.Fatal("invalid object")
		}
		metadata := value["metadata"].(map[string]any)
		owners := metadata["ownerReferences"].([]any)
		if owners[0].(map[string]any)["controller"] != true {
			t.Fatalf("generated owner is not a controller: %s", raw)
		}
	}
}
