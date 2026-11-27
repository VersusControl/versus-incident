package utils

import (
	"encoding/json"
	"testing"
)

// Content a webhook sends cannot pass for an alert Versus built: naming the
// kind and setting the mark field to true in the body does not select the
// Kubernetes issue template.
func TestWebhookContentIsNotAnInternalKubernetesIssue(t *testing.T) {
	var content map[string]interface{}
	if err := json.Unmarshal([]byte(`{"AlertKind":"kubernetes_issue","InternalAlert":true}`), &content); err != nil {
		t.Fatal(err)
	}
	if IsInternalAlert(content) || IsKubernetesIssueIncident(content) {
		t.Fatalf("webhook content %v was taken for a Kubernetes issue Versus built", content)
	}
	built := map[string]interface{}{"AlertKind": "kubernetes_issue"}
	MarkInternalAlert(built)
	if !IsKubernetesIssueIncident(built) {
		t.Fatal("content marked by Versus is not a Kubernetes issue")
	}
}

// The mark does not survive JSON on purpose: a stored incident reads back with
// the field set to true, which is what a webhook body can hold, so content
// read back from storage or an API is never trusted as built by Versus.
func TestInternalAlertMarkIsLostInAJSONRoundTrip(t *testing.T) {
	built := map[string]interface{}{"AlertKind": "kubernetes_issue"}
	MarkInternalAlert(built)
	encoded, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"AlertKind":"kubernetes_issue","InternalAlert":true}` {
		t.Fatalf("stored form = %s", encoded)
	}
	var stored map[string]interface{}
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatal(err)
	}
	if IsInternalAlert(stored) || IsKubernetesIssueIncident(stored) {
		t.Fatalf("content read back from JSON %v kept the internal mark", stored)
	}
}
