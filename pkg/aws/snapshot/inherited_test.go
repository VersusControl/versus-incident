package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Under a licence the store hides what the inherited default organization
// keeps for an account that is no longer connected, in the reads whose
// rows carry no organization too (changes by ID, history depth), and keeps
// the write organization's own rows for that account.
func TestInheritedRowsOfADisconnectedAccountStayHidden(t *testing.T) {
	const inherited, own = "111100000001", "111100000002"
	provider := storage.NewMemory()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for org, account := range map[string]string{tenancy.DefaultOrgID: inherited, "acme": own} {
		writer := New(provider, tenancy.NewOrgScope(org))
		unit := model.Unit{Account: account, Region: "us-east-1", Service: "ec2"}
		if _, err := writer.Commit(unit, Candidate{Resources: []model.Resource{{ARN: "arn:aws:ec2:us-east-1:" + account + ":instance/i-1", Service: "ec2", Type: "instance", Account: account, Region: "us-east-1"}}}); err != nil {
			t.Fatal(err)
		}
		if err := writer.AppendChanges(account, "us-east-1", []model.Change{{ID: "event-" + account, Time: at, Account: account, Region: "us-east-1", Service: "ec2", EventName: "RunInstances"}}, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	scope := tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
	connected := map[string]bool{}
	known := true
	store := New(provider, scope, WithConnectedAccounts(func(context.Context) (map[string]bool, bool) { return connected, known }))
	feeds := store.inventory.(storage.AWSChangeFeedStore)
	ctx := context.Background()

	read := func() (changes, history []string) {
		rows, err := store.changesByID(ctx, feeds, []string{"event-" + inherited, "event-" + own})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			changes = append(changes, row.Account)
		}
		units, err := store.UnitHistory(ctx, Query{})
		if err != nil {
			t.Fatal(err)
		}
		for _, unit := range units {
			history = append(history, unit.Unit.Account)
		}
		return changes, history
	}
	changes, history := read()
	if len(changes) != 1 || changes[0] != own || len(history) != 1 || history[0] != own {
		t.Fatalf("with neither account connected: changes %v, history %v; want only the write organization's %s", changes, history, own)
	}
	connected[inherited] = true
	if changes, history := read(); len(changes) != 2 || len(history) != 2 {
		t.Fatalf("with the inherited account connected: changes %v, history %v; want both accounts", changes, history)
	}
	delete(connected, inherited)
	known = false
	if changes, history := read(); len(changes) != 2 || len(history) != 2 {
		t.Fatalf("with the connected list unknown: changes %v, history %v; want both accounts", changes, history)
	}
}
