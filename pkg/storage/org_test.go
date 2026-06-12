package storage_test

// org_test.go — covers the OrgID defaulting seam (X2-T1). Records saved
// without an explicit OrgID must come back stamped with the default org
// so single-tenant OSS users never have to think about orgs, while
// explicit orgs are preserved verbatim for enterprise multi-tenant use.

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestNormalizeOrgID(t *testing.T) {
	if got := storage.NormalizeOrgID(""); got != storage.DefaultOrgID {
		t.Fatalf("NormalizeOrgID(\"\") = %q, want %q", got, storage.DefaultOrgID)
	}
	if got := storage.NormalizeOrgID("acme"); got != "acme" {
		t.Fatalf("NormalizeOrgID(\"acme\") = %q, want acme", got)
	}
}

func TestSaveIncidentDefaultsOrgID(t *testing.T) {
	p := storage.NewMemory()
	defer p.Close()

	// No OrgID set — must default to "default".
	if err := p.SaveIncident(&storage.IncidentRecord{ID: "a", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}
	got, err := p.GetIncident("a")
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if got.OrgID != storage.DefaultOrgID {
		t.Fatalf("OrgID = %q, want %q", got.OrgID, storage.DefaultOrgID)
	}

	// Explicit OrgID — preserved.
	if err := p.SaveIncident(&storage.IncidentRecord{ID: "b", OrgID: "acme", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}
	got, err = p.GetIncident("b")
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if got.OrgID != "acme" {
		t.Fatalf("OrgID = %q, want acme", got.OrgID)
	}
}

func TestSaveAnalysisDefaultsOrgID(t *testing.T) {
	p := storage.NewMemory()
	defer p.Close()

	if err := p.SaveAnalysis(&storage.AnalysisRecord{ID: "an-1", IncidentID: "a", Status: "ok"}); err != nil {
		t.Fatalf("SaveAnalysis: %v", err)
	}
	got, err := p.GetAnalysis("an-1")
	if err != nil {
		t.Fatalf("GetAnalysis: %v", err)
	}
	if got.OrgID != storage.DefaultOrgID {
		t.Fatalf("OrgID = %q, want %q", got.OrgID, storage.DefaultOrgID)
	}
}
