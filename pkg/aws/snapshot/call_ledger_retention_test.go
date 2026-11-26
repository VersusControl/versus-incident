package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The store's retention sweep keeps fourteen days of the call ledger, on
// the inline prune (memory) and the batched sweep (the SQL tables): an
// hour that started fourteen days before now stays, the hour before it
// goes.
func TestRetentionSweepKeepsFourteenDaysOfCallCounts(t *testing.T) {
	file, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	for name, provider := range map[string]storage.Provider{"memory": storage.NewMemory(), "file": file} {
		t.Run(name, func(t *testing.T) {
			ledger, ok := storage.AWSCallCountsOf(provider)
			if !ok {
				t.Fatalf("%s has no call ledger", name)
			}
			now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
			store := New(provider, tenancy.DefaultOrgScope(), WithClock(func() time.Time { return now }))
			cutoff := now.Add(-14 * 24 * time.Hour)
			var counts []storage.AWSCallCount
			for _, hour := range []time.Time{cutoff.Add(-24 * time.Hour), cutoff.Add(-time.Hour), cutoff, cutoff.Add(time.Hour), now} {
				counts = append(counts, storage.AWSCallCount{AccountID: "123400000001", Region: "eu-south-2", Source: "elbv2", Hour: hour, Calls: 10, Runs: 1})
			}
			if err := ledger.AddAWSCallCounts(context.Background(), tenancy.DefaultOrgScope(), counts); err != nil {
				t.Fatal(err)
			}
			var removed int64
			for pass := 0; pass < 100; pass++ {
				result, err := store.SweepRetention(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				removed += result.CallCounts
				if !result.More {
					break
				}
			}
			left, err := ledger.AWSCallCounts(context.Background(), tenancy.DefaultOrgScope(), storage.AWSCallCountQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if removed != 2 || len(left) != 3 || !left[0].Hour.Equal(cutoff) {
				t.Fatalf("sweep removed %d, left %+v; want the two hours before %s gone", removed, left, cutoff)
			}
		})
	}
}
