package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

func fakeService(t *testing.T, scenario string) (*Service, *Store, *fakeaws.Server) {
	t.Helper()
	fake := fakeaws.New(t, scenario)
	store := NewStore(storage.NewMemory(), tenancy.DefaultOrgScope())
	connector := NewService(store,
		WithConfig(enabledConfig()),
		WithHTTPClient(fake.HTTPClient()),
		WithBaseProvider(staticBase),
		WithLimiter(limit.NewManager(100)),
	)
	return connector, store, fake
}

func waitForVerified(t *testing.T, connector *Service, draftID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		view, err := connector.Draft("default", draftID)
		if err != nil {
			t.Fatal(err)
		}
		if view.Status == DraftVerified {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("draft did not finish verifying")
}

func TestNewAccountConnectEndToEnd(t *testing.T) {
	connector, _, _ := fakeService(t, "new-account-connect")
	ctx := context.Background()

	start, err := connector.CreateDraft(ctx, "default", "cloudformation")
	if err != nil {
		t.Fatal(err)
	}
	if start.DraftID == "" || start.ExternalIDMasked == "" || start.TemplateURL == "" {
		t.Fatalf("start view = %+v", start)
	}
	fullExternalID, err := connector.RevealExternalID("default", start.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fullExternalID, ExternalIDPrefix) || strings.Contains(start.ExternalIDMasked, strings.TrimPrefix(fullExternalID, ExternalIDPrefix)) {
		t.Fatalf("external id masking failed")
	}

	probeID, err := connector.Verify("default", start.DraftID, "arn:aws:iam::444455556666:role/versus-reader")
	if err != nil {
		t.Fatal(err)
	}
	if probeID == "" {
		t.Fatal("verify returned no probe id")
	}
	waitForVerified(t, connector, start.DraftID)

	view, err := connector.Draft("default", start.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Assume.OK {
		t.Fatalf("assume failed: %+v", view.Assume)
	}
	if view.AccountID != "444455556666" {
		t.Fatalf("account id = %q", view.AccountID)
	}
	if len(view.Services) != 24 {
		t.Fatalf("services = %d", len(view.Services))
	}
	if !containsValue(view.Counts.Denied, "elasticache") {
		t.Fatalf("denied = %v", view.Counts.Denied)
	}
	if !containsValue(view.Counts.NotOnPlan, "aws-health") {
		t.Fatalf("not on plan = %v", view.Counts.NotOnPlan)
	}
	if view.Counts.NotChecked != 0 {
		t.Fatalf("not checked = %d %v", view.Counts.NotChecked, view.Counts.NotCheckedServices)
	}
	if view.FixPolicy == "" || !strings.Contains(view.FixPolicy, "elasticache:Describe*") {
		t.Fatalf("fix policy = %q", view.FixPolicy)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), fullExternalID) {
		t.Fatal("draft response revealed the full external id")
	}

	regions, err := connector.DiscoverRegions(ctx, "default", start.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 4 {
		t.Fatalf("regions = %+v", regions)
	}
	found := false
	for _, region := range regions {
		if region.ResourcesFound {
			found = true
		}
	}
	if !found {
		t.Fatal("region discovery found no resources")
	}

	account, err := connector.Commit(ctx, "default", start.DraftID, "Staging", []string{"us-east-1", "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if account.Source != SourceWizard || account.DisplayName != "Staging" {
		t.Fatalf("committed account = %+v", account)
	}
	if len(account.Services.Denied) != 1 || account.Services.Denied[0] != "elasticache" {
		t.Fatalf("account services = %+v", account.Services)
	}
	if account.Regions.Enabled != 2 || account.APIUse.BudgetRPS != 200 {
		t.Fatalf("account regions/api use = %+v / %+v", account.Regions, account.APIUse)
	}

	settingsView, err := connector.View(ctx, "default", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(settingsView.Accounts) != 1 {
		t.Fatalf("settings accounts = %+v", settingsView.Accounts)
	}
	row := settingsView.Accounts[0]
	if row.AccountID != "444455556666" || row.Services.Denied[0] != "elasticache" || row.Status != StatusConnected {
		t.Fatalf("settings row = %+v", row)
	}
	if settingsView.Capabilities.Organizations {
		t.Fatal("OSS reported the Organizations capability")
	}
	if _, err := connector.Draft("default", start.DraftID); err != ErrNotFound {
		t.Fatalf("committed draft still readable: %v", err)
	}
}

func TestAssumeDeniedMapsToClosedError(t *testing.T) {
	connector, _, fake := fakeService(t, "assume-denied")
	fake.SetExternalID("vrs-ext-deliberatelydifferent")
	ctx := context.Background()
	start, err := connector.CreateDraft(ctx, "default", "cloudformation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Verify("default", start.DraftID, "arn:aws:iam::444455556666:role/versus-reader"); err != nil {
		t.Fatal(err)
	}
	waitForVerified(t, connector, start.DraftID)
	view, err := connector.Draft("default", start.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Assume.OK {
		t.Fatal("assume unexpectedly succeeded")
	}
	if view.Assume.ErrorCode != "external_id_mismatch" {
		t.Fatalf("assume error = %q", view.Assume.ErrorCode)
	}
	if view.Counts.NotChecked != 24 {
		t.Fatalf("assume failure did not report every connector as not checked: %+v", view.Counts)
	}
	if len(view.Counts.Denied) != 0 {
		t.Fatalf("assume failure produced a denial: %v", view.Counts.Denied)
	}
}

func TestThrottledProbesAreNotCheckedNeverDenied(t *testing.T) {
	connector, _, fake := fakeService(t, "throttled-api")
	fake.SetThrottle(2)
	ctx := context.Background()
	start, err := connector.CreateDraft(ctx, "default", "cloudformation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Verify("default", start.DraftID, "arn:aws:iam::111122223333:role/versus-reader"); err != nil {
		t.Fatal(err)
	}
	waitForVerified(t, connector, start.DraftID)
	view, err := connector.Draft("default", start.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Counts.NotChecked == 0 {
		t.Fatalf("throttled run produced no not-checked cells: %+v", view.Counts)
	}
	if len(view.Counts.Denied) != 0 {
		t.Fatalf("throttled run produced a false denial: %v", view.Counts.Denied)
	}
	notChecked := make(map[string]bool, len(view.Counts.NotCheckedServices))
	for _, connector := range view.Counts.NotCheckedServices {
		notChecked[connector] = true
	}
	for _, row := range view.Services {
		if notChecked[row.Service] {
			if row.Status != string(probe.StatusNotChecked) || row.Items != 0 {
				t.Fatalf("throttled row = %+v", row)
			}
		}
	}
}
