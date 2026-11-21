package common

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"testing"
	"text/template"

	"github.com/VersusControl/versus-incident/pkg/utils"
)

// H4 (B219): the delivery ID is added to the data of every notification, so
// a shipped template must render exactly the same message with and without
// it. Only a custom template that names .DeliveryID shows it.
func TestShippedTemplatesRenderTheSameWithADeliveryID(t *testing.T) {
	paths, err := filepath.Glob("../../config/*.tmpl")
	if err != nil || len(paths) == 0 {
		t.Fatalf("shipped templates not found: %v", err)
	}
	contents := map[string]map[string]interface{}{
		"kubernetes issue": {
			"AlertKind": "kubernetes_issue", "AlertName": "Deployment api is crash looping", "Severity": "critical",
			"KubernetesSeverity": "critical", "Cluster": "alpha", "Namespace": "shop", "WorkloadKind": "Deployment",
			"Workload": "api", "Rule": "pod.crash_loop", "RuleLabel": "is crash looping", "Affected": "1 of 3 pods",
			"BrokeAt": "2026-11-21T09:00:00Z", "BrokeAgo": "1m0s ago", "Evidence": []string{"3 restarts"},
			"Changes": []string{"image changed 2m0s before break"}, "HistoryIncomplete": false,
			"ServiceName": "shop/api", "Service": "shop/api", "Source": "kubernetes:alpha", "Status": "firing",
			"Links": []map[string]string{{"label": "Open", "url": "https://versus.example/incidents/1"}},
		},
		"agent finding": {
			"AlertKind": "agent_finding", "AlertName": "Error spike", "Severity": "high", "ServiceName": "checkout",
			"Service": "checkout", "Source": "elasticsearch", "Summary": "errors rose", "Status": "firing",
		},
		"plain alert": {"Logs": "boom", "ServiceName": "checkout", "Status": "firing"},
	}
	render := func(path string, content map[string]interface{}) string {
		tmpl, err := template.New(filepath.Base(path)).Funcs(utils.GetTemplateFuncMaps()).ParseFiles(path)
		if err != nil {
			return "parse: " + err.Error()
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, content); err != nil {
			return out.String() + "\nexecute: " + err.Error()
		}
		return out.String()
	}
	for _, path := range paths {
		for name, content := range contents {
			t.Run(fmt.Sprintf("%s/%s", filepath.Base(path), name), func(t *testing.T) {
				with := make(map[string]interface{}, len(content)+1)
				for key, value := range content {
					with[key] = value
				}
				with["DeliveryID"] = "episode-delivery-id-1"
				// A template that falls back to the current time renders it to the
				// second; that is not what is compared.
				plain := renderedTime.ReplaceAllString(render(path, content), "<time>")
				delivered := renderedTime.ReplaceAllString(render(path, with), "<time>")
				if plain != delivered {
					t.Fatalf("the delivery ID changed the shipped message:\nwithout: %s\nwith:    %s", plain, delivered)
				}
			})
		}
	}
}

var renderedTime = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}`)
