package fakekube

import (
	"encoding/json"
	"net/url"
	"testing"
)

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
