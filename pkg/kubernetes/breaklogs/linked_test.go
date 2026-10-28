package breaklogs

import (
	"strings"
	"testing"
	"time"
)

func TestMappingsValidateAllSourceKinds(t *testing.T) {
	for _, kind := range []string{"loki", "elasticsearch", "cloudwatchlogs", "graylog", "splunk", "signoz"} {
		if fields, err := NormalizeFields(kind, Fields{}); err != nil || fields.Namespace == "" || fields.Pod == "" || fields.Container == "" {
			t.Fatal(kind, err)
		}
		for _, bad := range []string{"namespace\" OR *", "namespace\n", "a b", "9bad", strings.Repeat("a", 129)} {
			if _, err := NormalizeFields(kind, Fields{Namespace: bad}); err == nil {
				t.Fatalf("%s accepted mapping", kind)
			}
		}
	}
	if _, err := NormalizeFields("file", Fields{}); err == nil {
		t.Fatal("unknown kind")
	}
	for _, field := range []string{"@namespace", "kube-namespace", "kube/namespace", "kube@namespace", strings.Repeat("a", 65), strings.Repeat("a", 128)} {
		for _, fields := range []Fields{{Namespace: field}, {Pod: field}, {Container: field}, {Time: field}, {Message: field}} {
			if _, err := NormalizeFields("splunk", fields); err == nil {
				t.Errorf("Splunk accepted unprojectable mapping: %+v", fields)
			}
		}
		if _, err := NormalizeFields("elasticsearch", Fields{Namespace: field}); err != nil {
			t.Errorf("non-Splunk mapping narrowed: %q", field)
		}
	}
	if _, err := NormalizeFields("splunk", Fields{Namespace: strings.Repeat("a", 64), Pod: "k8s.pod", Container: "_container"}); err != nil {
		t.Fatal("valid Splunk projection boundary rejected", err)
	}
	now := time.Now()
	query := LinkedQuery{Namespace: "shop", From: now.Add(-time.Hour), To: now, Limit: 2000}
	if query.Validate() != nil {
		t.Fatal("valid query rejected")
	}
	query.Limit = 2001
	if query.Validate() == nil {
		t.Fatal("limit")
	}
}
