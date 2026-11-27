package mount

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	awssnapshot "github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// An account whose only reads failed carries the cause class the
// collectors' closed error codes map to, on Overview and its Topology card;
// a code outside that vocabulary reads not checked, never a denial.
func TestFailedAccountReasonNamesTheCollectorCause(t *testing.T) {
	accounts := map[string]string{"123400000501": collect.CodeDenied, "123400000502": "throttled", "123400000503": "SomethingElse"}
	want := map[string]string{"123400000501": string(model.GapDenied), "123400000502": string(model.GapThrottled),
		"123400000503": string(model.GapNotChecked)}
	ids := []string{"123400000501", "123400000502", "123400000503"}
	app, mounted, _ := estateApp(t, storage.NewMemory(), tenancy.OrgScope{}, ids...)
	for account, code := range accounts {
		if err := mounted.Snapshots.RecordFailure(model.Unit{Account: account, Region: estateRegion, Service: "lambda"}, code); err != nil {
			t.Fatal(err)
		}
	}
	var overview overviewAnswer
	getJSON(t, app, "/api/admin/aws/overview", &overview)
	cards := topologyNodes(t, app, "/api/admin/aws/topology?level=accounts")
	for _, row := range overview.Accounts {
		if row.Read != "not_read" || row.NotReadReason != want[row.ID] || row.NotReadReason == awssnapshot.AccountNotReadYet {
			t.Errorf("account %s (code %s) = read %q reason %q; want not_read, %q", row.ID, accounts[row.ID], row.Read, row.NotReadReason, want[row.ID])
		}
		if card := cards["account:"+row.ID]; card.Read != "not_read" {
			t.Errorf("card %s = %+v", row.ID, card)
		}
	}
}
