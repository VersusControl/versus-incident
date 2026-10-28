//go:build integration

package kubernetes

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// These tests exercise the inventory publication contract against independent
// real PostgreSQL connections. There is no HTTP server or network substitute.
func TestPostgresInventoryPublicationFencingAndUpgrade(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN required")
	}
	first, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	prefix := "postgres-inventory-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	service := func(provider storage.Provider, scope Scope) *Service {
		value := NewService(nil, scope, time.Minute)
		value.SetChangeStorage(provider)
		t.Cleanup(value.StopCapture)
		return value
	}
	generation := func(name string) inventoryGeneration {
		now := time.Now().UTC()
		return inventoryGeneration{View: kubeindex.Snapshot{
			Records: []kubeindex.Record{{UID: name, Kind: "Pod", Namespace: "shop", Name: name}},
			Kinds:   map[string]kubeindex.KindStatus{"Pod": {State: "ready", Records: 1, ObservedAt: now, CompletedAt: now}},
		}}
	}
	read := func(provider storage.Provider, scope Scope) inventoryGeneration {
		value, _, err := readInventoryGeneration(provider, scope, "")
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Run("real CAS publication renewal takeover stale writer and garbage collection", func(t *testing.T) {
		scope := Scope{OrgID: prefix, ClusterID: "cluster", CredentialID: "epoch"}
		oldService, nextService := service(first, scope), service(second, scope)
		old, owned, err := oldService.claimInventory(t.Context(), first, "old-owner")
		if err != nil || !owned {
			t.Fatalf("initial claim: %v %v", owned, err)
		}
		oldGeneration := generation("last-good")
		for range 2 {
			if err := old.publish(t.Context(), oldService, &oldGeneration); err != nil {
				t.Fatal(err)
			}
		}
		if _, owned, err := nextService.claimInventory(t.Context(), second, "successor"); err != nil || owned {
			t.Fatalf("unexpired owner was not exclusive: owned=%v err=%v", owned, err)
		}
		key, err := inventoryDataKey(scope, old.record)
		if err != nil {
			t.Fatal(err)
		}
		payloadBefore, err := first.ReadBlob(key)
		if err != nil {
			t.Fatal(err)
		}
		db := first.(storage.SQLAccessor).DB()
		var versionBefore, versionAfter string
		if err := db.QueryRow("SELECT xmin::text FROM vs_blobs WHERE name=$1", key).Scan(&versionBefore); err != nil {
			t.Fatal(err)
		}
		if err := old.renew(t.Context(), oldService); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT xmin::text FROM vs_blobs WHERE name=$1", key).Scan(&versionAfter); err != nil {
			t.Fatal(err)
		}
		payloadAfter, err := second.ReadBlob(key)
		if err != nil || !bytes.Equal(payloadBefore, payloadAfter) || versionBefore != versionAfter {
			t.Fatal("renewal rewrote inventory payload")
		}
		raw, err := first.ReadBlob(inventoryKey(scope))
		if err != nil || len(raw) > 1024 {
			t.Fatal("lease is not independently small")
		}
		expired := old.record
		expired.Expires = time.Now().Add(-time.Minute)
		replacement, _ := json.Marshal(expired)
		if won, err := second.(storage.BlobCAS).CompareAndSwapBlob(inventoryKey(scope), raw, replacement); err != nil || !won {
			t.Fatalf("expire owned test lease: %v %v", won, err)
		}
		successor, owned, err := nextService.claimInventory(t.Context(), second, "successor")
		if err != nil || !owned {
			t.Fatalf("successor claim: %v %v", owned, err)
		}
		next := generation("successor-pod")
		if err := successor.publish(t.Context(), nextService, &next); err != nil {
			t.Fatal(err)
		}
		late := generation("late-stale-pod")
		if err := old.publish(t.Context(), oldService, &late); err == nil {
			t.Fatal("stale owner published over successor")
		}
		current := read(first, scope)
		if len(current.View.Records) != 1 || current.View.Records[0].Name != "successor-pod" {
			t.Fatalf("successor generation changed: %+v", current.View)
		}
		leftovers, err := second.ListBlobs(inventoryKey(scope) + "/data/old-owner/")
		if err != nil || len(leftovers) != 0 {
			t.Fatalf("retired/late-stale payloads survived fenced GC: %d %v", len(leftovers), err)
		}
		if err := old.release(); err != nil {
			t.Fatal(err)
		}
		_, record, err := readInventoryGeneration(first, scope, "")
		if err != nil || record.Owner != "successor" {
			t.Fatal("stale graceful release changed successor lease")
		}
		if err := successor.release(); err != nil {
			t.Fatal(err)
		}
		_, record, err = readInventoryGeneration(first, scope, "")
		if err != nil || record.Owner != "" || !record.Expires.IsZero() {
			t.Fatal("graceful owner release did not relinquish lease")
		}
		if read(second, scope).View.Records[0].Name != "successor-pod" {
			t.Fatal("graceful release lost published data")
		}
		t.Logf("real PostgreSQL lease bytes=%d; renewal xmin unchanged; successor and late-writer GC fenced", len(raw))
	})
	t.Run("default archive precedence scoped upgrade and credential isolation", func(t *testing.T) {
		scope := Scope{OrgID: prefix + "-licensed", ClusterID: prefix, CredentialID: "epoch-one"}
		archiveScope := scope
		archiveScope.OrgID = "default"
		archive := generation("archive-pod")
		raw, _ := json.Marshal(archive)
		if err := first.WriteBlob(inventoryKey(archiveScope), raw); err != nil {
			t.Fatal(err)
		}
		licensed := service(second, scope)
		licensed.readOrgs = []string{scope.OrgID, "default"}
		before, err := licensed.readInventory(t.Context())
		if err != nil || len(before.View.Records) != 1 || before.View.Records[0].Name != "archive-pod" {
			t.Fatal("authorized default archive was not readable before upgrade sync")
		}
		lease, owned, err := licensed.claimInventory(t.Context(), second, "licensed-owner")
		if err != nil || !owned {
			t.Fatalf("licensed claim: %v %v", owned, err)
		}
		own := generation("licensed-pod")
		if err := lease.publish(t.Context(), licensed, &own); err != nil {
			t.Fatal(err)
		}
		after, err := licensed.readInventory(t.Context())
		if err != nil || after.View.Records[0].Name != "licensed-pod" {
			t.Fatal("own licensed generation did not take precedence")
		}
		unchanged, err := second.ReadBlob(inventoryKey(archiveScope))
		if err != nil || !bytes.Equal(raw, unchanged) {
			t.Fatal("upgrade mutated default archive")
		}
		for _, isolated := range []Scope{
			{OrgID: prefix + "-other", ClusterID: scope.ClusterID, CredentialID: scope.CredentialID},
			{OrgID: scope.OrgID, ClusterID: scope.ClusterID, CredentialID: "epoch-two"},
			{OrgID: scope.OrgID, ClusterID: "other-cluster", CredentialID: scope.CredentialID},
		} {
			view, err := service(first, isolated).readInventory(t.Context())
			if err != nil || len(view.View.Records) != 0 {
				t.Fatalf("generation crossed org/cluster/credential boundary: %+v %v", isolated, err)
			}
		}
		t.Log("upgrade archive remained byte-identical; own scope won precedence; org/cluster/credential reads isolated")
	})
}
