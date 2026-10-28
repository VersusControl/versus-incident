package storage_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func populatedAnalysisRecord() *storage.AnalysisRecord {
	return &storage.AnalysisRecord{
		ID:          "analysis-clone",
		OrgID:       storage.DefaultOrgID,
		IncidentID:  "incident-clone",
		RequestedAt: time.Unix(100, 0).UTC(),
		Status:      "ok",
		ToolCalls: []storage.AnalysisToolCall{{
			Name:   "logs",
			Args:   json.RawMessage(`{"query":"error"}`),
			Output: json.RawMessage(`{"lines":["one"]}`),
		}},
		Finding: &core.AIFinding{
			Title:               "finding",
			Suggestions:         []string{"suggestion"},
			SampleIDs:           []string{"sample"},
			RootCauseHypotheses: []core.RootCauseHypothesis{{Hypothesis: "cause"}},
			Evidence:            []core.EvidenceItem{{Source: "logs"}},
			RelatedPatternIDs:   []string{"pattern"},
			NextSteps:           []string{"next"},
		},
	}
}

func mutateAnalysisRecord(rec *storage.AnalysisRecord) {
	rec.ToolCalls[0].Args[0] = 'X'
	rec.ToolCalls[0].Output[0] = 'Y'
	rec.Finding.Title = "changed"
	rec.Finding.Suggestions[0] = "changed"
	rec.Finding.SampleIDs[0] = "changed"
	rec.Finding.RootCauseHypotheses[0].Hypothesis = "changed"
	rec.Finding.Evidence[0].Source = "changed"
	rec.Finding.RelatedPatternIDs[0] = "changed"
	rec.Finding.NextSteps[0] = "changed"
}

func TestCloneAnalysisRecordDetachesMutableFields(t *testing.T) {
	original := populatedAnalysisRecord()
	clone := storage.CloneAnalysisRecord(original)
	mutateAnalysisRecord(clone)

	if reflect.DeepEqual(original, clone) {
		t.Fatal("mutating clone did not change it")
	}
	if !reflect.DeepEqual(original, populatedAnalysisRecord()) {
		t.Fatalf("clone mutation changed original: %+v", original)
	}
	if storage.CloneAnalysisRecord(nil) != nil {
		t.Fatal("nil clone must remain nil")
	}
}

func TestAnalysisStorageDetachesMutableFields(t *testing.T) {
	fileProvider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	providers := map[string]storage.Provider{
		"memory": storage.NewMemory(),
		"file":   fileProvider,
	}
	for name, provider := range providers {
		t.Run(name, func(t *testing.T) {
			source := populatedAnalysisRecord()
			want := storage.CloneAnalysisRecord(source)
			if err := provider.SaveAnalysis(source); err != nil {
				t.Fatalf("SaveAnalysis: %v", err)
			}
			mutateAnalysisRecord(source)

			got, err := provider.GetAnalysis(want.ID)
			if err != nil {
				t.Fatalf("GetAnalysis: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("saved record aliases caller: got %+v", got)
			}
			mutateAnalysisRecord(got)

			byIncident, err := provider.ListAnalysesByIncident(want.IncidentID, 0)
			if err != nil || len(byIncident) != 1 || !reflect.DeepEqual(byIncident[0], want) {
				t.Fatalf("incident list aliases prior read: records=%+v error=%v", byIncident, err)
			}
			mutateAnalysisRecord(byIncident[0])

			all, err := provider.ListAnalyses(0)
			if err != nil || len(all) != 1 || !reflect.DeepEqual(all[0], want) {
				t.Fatalf("analysis list aliases prior read: records=%+v error=%v", all, err)
			}
			mutateAnalysisRecord(all[0])

			pager, ok := provider.(storage.AnalysisPager)
			if !ok {
				t.Fatal("provider does not implement AnalysisPager")
			}
			page, err := pager.ListAnalysesPage(0, 1)
			if err != nil || len(page) != 1 || !reflect.DeepEqual(page[0], want) {
				t.Fatalf("analysis page aliases prior read: records=%+v error=%v", page, err)
			}
		})
	}
}
