package issues

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The snapshot source reads each unit per Region, answers a Region from a
// service's global read, and dates the view by its latest committed read.
func TestSnapshotSourceReadsPerRegionWithAsOf(t *testing.T) {
	committed := time.Date(2026, 11, 25, 8, 30, 0, 0, time.UTC)
	now := committed
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope(), snapshot.WithClock(func() time.Time { return now }))
	bucket := model.Resource{ARN: "arn:aws:s3:::docs-site", Service: "s3", Type: "bucket", Account: testAccount, Region: "eu-west-1", Name: "docs-site"}
	if _, err := store.Commit(model.Unit{Account: testAccount, Region: "eu-west-1", Service: "s3"}, snapshot.Candidate{Resources: []model.Resource{bucket}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(model.Unit{Account: testAccount, Region: model.GlobalRegion, Service: "iam"}, snapshot.Candidate{}); err != nil {
		t.Fatal(err)
	}
	now = committed.Add(5 * time.Minute)
	if err := store.RecordFailure(model.Unit{Account: testAccount, Region: "us-east-1", Service: "s3"}, "denied"); err != nil {
		t.Fatal(err)
	}
	view, err := NewSnapshotSource(store).View(context.Background(), testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if !view.AsOf().Equal(committed) {
		t.Fatalf("as of = %s, want the commit time %s", view.AsOf(), committed)
	}
	for _, check := range []struct {
		service, region string
		want            Read
	}{
		{"s3", "eu-west-1", ReadOK},
		{"s3", "us-east-1", ReadNotRead},
		{"s3", "", ReadNotRead},
		{"iam", "eu-west-1", ReadOK},
		{"iam", "", ReadOK},
		{"lambda", "eu-west-1", ReadNotRead},
	} {
		if got := view.ReadIn(check.service, check.region); got != check.want {
			t.Fatalf("read of %s in %q = %s, want %s", check.service, check.region, got, check.want)
		}
	}
	if regions := view.Regions(); len(regions) != 2 || regions[0] != "eu-west-1" || regions[1] != "us-east-1" {
		t.Fatalf("regions = %v", regions)
	}
}
