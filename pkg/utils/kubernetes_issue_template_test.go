package utils

import (
	"bytes"
	htmltemplate "html/template"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func TestKubernetesIssueTemplatesFullAndMinimal(t *testing.T) {
	if IsKubernetesIssueIncident(nil) || IsKubernetesIssueIncident(map[string]interface{}{"PatternID": "x"}) || !IsKubernetesIssueIncident(map[string]interface{}{"AlertKind": "kubernetes_issue"}) {
		t.Fatal("discriminator")
	}
	if KubernetesIssueTemplatePath("custom", "default") != "custom" || KubernetesIssueTemplatePath("", "default") != "default" {
		t.Fatal("override")
	}
	for _, path := range []string{KubernetesIssueSlackTemplatePath, KubernetesIssueTelegramTemplatePath, KubernetesIssueMSTeamsTemplatePath, KubernetesIssueLarkTemplatePath, KubernetesIssueViberTemplatePath, KubernetesIssueEmailTemplatePath} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var tpl interface{ Execute(io.Writer, any) error }
			var err error
			if path == KubernetesIssueEmailTemplatePath {
				tpl, err = htmltemplate.New(filepath.Base(path)).Funcs(htmltemplate.FuncMap(GetTemplateFuncMaps())).ParseFiles(filepath.Join("../..", path))
			} else {
				tpl, err = template.New(filepath.Base(path)).Funcs(GetTemplateFuncMaps()).ParseFiles(filepath.Join("../..", path))
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, content := range []map[string]interface{}{
				{"AlertKind": "kubernetes_issue"},
				{"AlertKind": "kubernetes_issue", "Severity": "critical", "AlertName": "Deployment checkout crash looping", "Cluster": "prod", "Namespace": "shop", "WorkloadKind": "Deployment", "Workload": "checkout", "Affected": "3 of 3 pods", "BrokeAt": "2026-10-19T15:00:00Z", "Evidence": []string{"Exit 137 OOMKilled 4 times"}, "Changes": []string{"Image changed 4m before break"}, "HistoryIncomplete": true, "Links": []map[string]string{{"id": "timeline", "label": "Open timeline", "url": "https://versus.invalid/agent?x=1&y=2"}, {"id": "agent", "label": "Investigate with DevOps Agent", "url": "https://versus.invalid/agent/chat"}}},
			} {
				var out bytes.Buffer
				if err := tpl.Execute(&out, content); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), "Kubernetes issue") || strings.Contains(out.String(), "<no value>") {
					t.Fatal(out.String())
				}
				if content["Evidence"] != nil && (!strings.Contains(out.String(), "What we saw") || !strings.Contains(out.String(), "Changes before the break") || !strings.Contains(out.String(), "Open timeline") || !strings.Contains(out.String(), "Investigate with DevOps Agent")) {
					t.Fatal(out.String())
				}
			}

		})
	}
}

func TestKubernetesIssueNonSlackActionOrder(t *testing.T) {
	for _, channel := range []string{"telegram", "msteams", "lark", "viber", "email"} {
		t.Run(channel, func(t *testing.T) {
			path := filepath.Join("../../config", "kubernetes_issue_"+channel+".tmpl")
			var tpl interface{ Execute(io.Writer, any) error }
			var err error
			if channel == "email" {
				tpl, err = htmltemplate.New(filepath.Base(path)).Funcs(htmltemplate.FuncMap(GetTemplateFuncMaps())).ParseFiles(path)
			} else {
				tpl, err = template.New(filepath.Base(path)).Funcs(GetTemplateFuncMaps()).ParseFiles(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, ack := range []bool{false, true} {
				content := map[string]interface{}{"Links": []map[string]string{{"label": "Open timeline", "url": "https://versus.invalid/timeline"}, {"label": "Investigate with DevOps Agent", "url": "https://versus.invalid/chat"}}}
				if ack {
					content["AckURL"] = "https://versus.invalid/ack"
				}
				var rendered bytes.Buffer
				if err := tpl.Execute(&rendered, content); err != nil {
					t.Fatal(err)
				}
				text := rendered.String()
				first, second := strings.Index(text, "Open timeline"), strings.Index(text, "Investigate with DevOps Agent")
				if first < 0 || second < first {
					t.Fatal(text)
				}
				position := strings.Index(text, "Acknowledge")
				if (ack && (position < 0 || position > first)) || (!ack && position >= 0) {
					t.Fatal(text)
				}
			}
		})
	}
}
