package settings

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(storage.NewMemory(), tenancy.DefaultOrgScope())
}

func TestExternalIDShapeAndMask(t *testing.T) {
	first, err := GenerateExternalID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateExternalID()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("external IDs repeated")
	}
	if !strings.HasPrefix(first, ExternalIDPrefix) {
		t.Fatalf("external id %q has no prefix", first)
	}
	body := strings.TrimPrefix(first, ExternalIDPrefix)
	if len(body) != 24 {
		t.Fatalf("body length = %d, want 24", len(body))
	}
	for _, character := range body {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			t.Fatalf("external id body has non-base32 character %q", character)
		}
	}
	masked := MaskExternalID(first)
	if strings.Contains(masked, body) {
		t.Fatalf("mask %q reveals the full body", masked)
	}
	if !strings.HasSuffix(masked, body[len(body)-4:]) {
		t.Fatalf("mask %q hides the distinguishing tail", masked)
	}
}

func TestStoreUsesExactKeysAndEnforcesScope(t *testing.T) {
	provider := storage.NewMemory()
	store := NewStore(provider, tenancy.NewOrgScope("licensed", "default"))
	if err := store.SaveAccount("licensed", AccountRecord{AccountID: "444455556666", DisplayName: "Staging"}); err != nil {
		t.Fatal(err)
	}
	data, err := provider.ReadBlob(AccountKey("licensed", "444455556666"))
	if err != nil || len(data) == 0 {
		t.Fatalf("record not stored at the exact key: %v", err)
	}
	if err := store.SaveAccount("default", AccountRecord{AccountID: "111122223333"}); err != ErrOutOfScope {
		t.Fatalf("write outside scope = %v, want ErrOutOfScope", err)
	}
	if _, err := store.ListAccounts("default"); err != nil {
		t.Fatalf("read from a scoped organization failed: %v", err)
	}
	if _, err := store.ListAccounts("other-org"); err != ErrOutOfScope {
		t.Fatalf("read outside scope = %v, want ErrOutOfScope", err)
	}
}

func TestDraftExpiresAfterOneHour(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })
	draft := Draft{ID: strings.Repeat("a", 32), ExternalID: "vrs-ext-abcdefghijklmnopqrstuvwx", Status: DraftStarted}
	if err := store.SaveDraft("default", draft); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDraft("default", draft.ID); err != nil {
		t.Fatalf("live draft unreadable: %v", err)
	}
	now = now.Add(DraftTTL + time.Second)
	if _, err := store.GetDraft("default", draft.ID); err != ErrNotFound {
		t.Fatalf("expired draft = %v, want ErrNotFound", err)
	}
}

func TestMergeAccountsConfigWinsOnSameID(t *testing.T) {
	stored := []AccountRecord{
		{AccountID: "111122223333", DisplayName: "Wizard production", Source: SourceWizard},
		{AccountID: "444455556666", DisplayName: "Staging", Source: SourceWizard},
	}
	configured := []config.AWSAccountConfig{{
		AccountID:   "111122223333",
		DisplayName: "Production",
		Auth:        config.AWSAuthConfig{RoleARN: "arn:aws:iam::111122223333:role/versus-reader"},
	}}
	merged := MergeAccounts(stored, configured)
	if len(merged) != 2 {
		t.Fatalf("merged = %d accounts, want 2", len(merged))
	}
	if merged[0].DisplayName != "Production" || merged[0].Source != SourceConfig {
		t.Fatalf("config did not win: %+v", merged[0])
	}
	if merged[1].DisplayName != "Staging" || merged[1].Source != SourceWizard {
		t.Fatalf("wizard account lost: %+v", merged[1])
	}
}

func TestDraftViewNeverCarriesTheFullExternalID(t *testing.T) {
	externalID, err := GenerateExternalID()
	if err != nil {
		t.Fatal(err)
	}
	draft := Draft{ID: strings.Repeat("b", 32), ExternalID: externalID, Status: DraftVerified}
	encoded, err := json.Marshal(draft.View())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), externalID) {
		t.Fatal("draft view contains the full external id")
	}
	if !strings.Contains(string(encoded), MaskExternalID(externalID)) {
		t.Fatal("draft view is missing the masked external id")
	}
}

func TestCountResultsKeepsMissingServicesNotChecked(t *testing.T) {
	counts := CountResults([]probe.Result{
		{Service: "ec2", Status: probe.StatusReadable, Items: 3},
		{Service: "elasticache", Status: probe.StatusDenied},
		{Service: "aws-health", Status: probe.StatusNotOnPlan},
	})
	if counts.Total != 24 {
		t.Fatalf("total = %d", counts.Total)
	}
	if counts.Readable != 1 || len(counts.Denied) != 1 || len(counts.NotOnPlan) != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	if counts.NotChecked != counts.Total-3 || len(counts.NotCheckedServices) != counts.NotChecked {
		t.Fatalf("not checked = %d, want %d", counts.NotChecked, counts.Total-3)
	}
	for _, service := range counts.NotCheckedServices {
		if service == "ec2" || service == "elasticache" || service == "aws-health" {
			t.Fatalf("answered service %q reported as not checked", service)
		}
	}
}

func TestSanitizeAllowlistBoundsAndDeduplicates(t *testing.T) {
	clean := SanitizeAllowlist([]string{" App ", "app", "", "TEAM", strings.Repeat("x", 64)})
	if len(clean) != 2 || clean[0] != "app" || clean[1] != "team" {
		t.Fatalf("clean = %v", clean)
	}
}

func TestStoreScopesEveryKeyToItsOrg(t *testing.T) {
	provider := storage.NewMemory()
	store := NewStore(provider, tenancy.NewOrgScope("licensed", "default"))
	accountID := "444455556666"
	draftID := strings.Repeat("d", 32)
	if err := store.SaveAccount("licensed", AccountRecord{AccountID: accountID, DisplayName: "Staging"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveHealth("licensed", HealthRecord{AccountID: accountID, Status: StatusConnected}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDraft("licensed", Draft{ID: draftID, ExternalID: "vrs-ext-abcdefghijklmnopqrstuvwx"}); err != nil {
		t.Fatal(err)
	}
	budget := 9
	if err := store.SaveOptions("licensed", Options{MaxRequestsPerSecond: &budget}); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePrivacy("licensed", privacyRecord{TagValueAllowlist: []string{"team"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelfTest("licensed", SelfTestRecord{Passing: true}); err != nil {
		t.Fatal(err)
	}

	// The owning org reads everything back.
	if _, err := store.GetAccount("licensed", accountID); err != nil {
		t.Fatalf("owner account unreadable: %v", err)
	}
	if _, err := store.GetDraft("licensed", draftID); err != nil {
		t.Fatalf("owner draft unreadable: %v", err)
	}

	// Another in-scope org sees none of it through the same provider.
	if _, err := store.GetAccount("default", accountID); err != ErrNotFound {
		t.Fatalf("read-org account = %v, want ErrNotFound", err)
	}
	accounts, err := store.ListAccounts("default")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("read-org listed another org's accounts: %+v", accounts)
	}
	if _, err := store.GetDraft("default", draftID); err != ErrNotFound {
		t.Fatalf("read-org draft = %v, want ErrNotFound", err)
	}
	health, err := store.GetHealth("default", accountID)
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != StatusUnknown || !health.CheckedAt.IsZero() {
		t.Fatalf("read-org health = %+v, want the empty record", health)
	}
	options, err := store.LoadOptions("default")
	if err != nil {
		t.Fatal(err)
	}
	if options.MaxRequestsPerSecond != nil {
		t.Fatalf("read-org options = %+v", options)
	}
	privacy, err := store.LoadPrivacy("default")
	if err != nil {
		t.Fatal(err)
	}
	if len(privacy.TagValueAllowlist) != 0 {
		t.Fatalf("read-org privacy = %+v", privacy)
	}
	selfTest, err := store.LoadSelfTest("default")
	if err != nil {
		t.Fatal(err)
	}
	if !selfTest.LastRunAt.IsZero() || selfTest.Passing {
		t.Fatalf("read-org self-test = %+v", selfTest)
	}

	// A read org cannot write into the owner's namespace either.
	if err := store.SaveAccount("default", AccountRecord{AccountID: "111122223333"}); err != ErrOutOfScope {
		t.Fatalf("read-org write = %v, want ErrOutOfScope", err)
	}
}
