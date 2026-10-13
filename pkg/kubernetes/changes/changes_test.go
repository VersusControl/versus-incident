package changes

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestUpgradeTimelineVisible(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Now().UTC()
	scope := index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "credential"}
	archive := NewStore(provider, scope)
	old := Change{ID: "shared", Cluster: "cluster", Kind: "Pod", Name: "before", Type: Created, At: now.Add(-time.Minute)}
	if err := archive.Append([]Change{old}, now); err != nil {
		t.Fatal(err)
	}
	if err := archive.MarkGap(Gap{From: now.Add(-time.Hour), To: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	before, err := provider.ReadBlob(archive.key)
	if err != nil {
		t.Fatal(err)
	}
	scope.OrgID = "licensed"
	view, err := NewView(provider, scope, []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.Name = "after"
	added := old
	added.ID = "new"
	added.Name = "new-pod"
	added.At = now
	if err := view.Append([]Change{newer, added}, now); err != nil {
		t.Fatal(err)
	}
	page, err := view.Query(Query{Limit: 1}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "new" || page.Next != "1" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	page, err = view.Query(Query{Limit: 1, Cursor: page.Next}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "after" || page.Next != "" || len(page.Gaps) != 1 {
		t.Fatalf("deduplicated page=%+v err=%v", page, err)
	}
	page, err = view.Query(Query{Name: "before"}, now)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("filter before dedup: %+v %v", page, err)
	}
	after, err := provider.ReadBlob(archive.key)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("archive changed")
	}
	for _, changed := range []index.Scope{{OrgID: "licensed", ClusterID: "renamed", CredentialID: "credential"}, {OrgID: "licensed", ClusterID: "cluster", CredentialID: "different"}} {
		isolated, err := NewView(provider, changed, []string{"default"})
		if err != nil {
			t.Fatal(err)
		}
		page, err := isolated.Query(Query{}, now)
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("identity isolation=%+v %v", page, err)
		}
	}
	if _, err := NewView(provider, scope, []string{"default", "foreign"}); !errors.Is(err, ErrStoreLimit) {
		t.Fatal("third scope accepted")
	}
	if err := provider.WriteBlob(archive.key, []byte("malformed")); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Query(Query{}, now); !errors.Is(err, ErrStoreLimit) {
		t.Fatal("malformed archive was hidden")
	}
}

func TestDetectProjectedChangesWithoutSensitiveFields(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	old := index.Record{UID: "uid-a", Kind: "Deployment", Namespace: "shop", Name: "checkout", Images: []string{"checkout:v1"}, Replicas: 2, Labels: map[string]string{"app.kubernetes.io/name": "checkout"}}
	updated := old
	updated.Images = []string{"checkout:v2"}
	updated.Replicas = 3
	change, ok := Detect("cluster-a", index.Delta{Kind: "Deployment", Op: "upsert", Old: &old, New: &updated}, now)
	if !ok || change.Type != ImageChanged || change.Service != "checkout" || len(change.Fields) != 2 || change.Fields[0].From != "checkout:v1" || change.Fields[1].To != "3" {
		t.Fatalf("detected change=%#v ok=%v", change, ok)
	}
	encoded, err := json.Marshal(change)
	if err != nil || strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "env") {
		t.Fatalf("change contains non-projected data: %s err=%v", encoded, err)
	}
	if _, ok := Detect("cluster-a", index.Delta{Resync: true}, now); ok {
		t.Fatal("resync marker was converted into a data change")
	}
}

func TestStoreAppendFilterCursorScopeAndRetention(t *testing.T) {
	provider := storage.NewMemory()
	scope := index.Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}
	store := NewStore(provider, scope)
	other := NewStore(provider, index.Scope{OrgID: "org-b", ClusterID: "cluster-a", CredentialID: "cred-a"})
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for sequence := 0; sequence < 3; sequence++ {
		change := Change{ID: strconv.Itoa(sequence), Cluster: "cluster-a", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid-a", Type: SpecChanged, At: now.Add(time.Duration(sequence) * time.Minute)}
		if sequence == 1 {
			change.Kind = "Pod"
		}
		if err := store.Append([]Change{change}, now.Add(time.Duration(sequence)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Query(Query{Kind: "Deployment", Limit: 1}, now.Add(3*time.Minute))
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "2" || page.Next == "" {
		t.Fatalf("filtered page=%#v err=%v", page, err)
	}
	page, err = store.Query(Query{Kind: "Deployment", Limit: 1, Cursor: page.Next}, now.Add(3*time.Minute))
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "0" {
		t.Fatalf("cursor page=%#v err=%v", page, err)
	}
	if page, err := other.Query(Query{}, now.Add(3*time.Minute)); err != nil || len(page.Items) != 0 {
		t.Fatalf("other organization read changes: page=%#v err=%v", page, err)
	}
	store.retention = time.Hour
	page, err = store.Query(Query{}, now.Add(3*time.Hour))
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("expired timeline page=%#v err=%v", page, err)
	}
	if err := store.Append([]Change{{ID: "bad", Cluster: "c", Kind: "Pod", Name: "p", Type: "unknown", At: now}}, now); err != ErrInvalidQuery {
		t.Fatalf("invalid change error=%v", err)
	}
}

type writeCountingProvider struct {
	storage.Provider
	writes int
}

func (provider *writeCountingProvider) WriteBlob(name string, data []byte) error {
	provider.writes++
	return provider.Provider.WriteBlob(name, data)
}

func TestStoreAppendSkipsWritesWithoutChangesOrPruning(t *testing.T) {
	provider := &writeCountingProvider{Provider: storage.NewMemory()}
	store := NewStore(provider, index.Scope{OrgID: "org", ClusterID: "cluster"})
	now := time.Now().UTC()
	change := Change{ID: "one", Cluster: "cluster", Kind: "Pod", Name: "pod", Type: Created, At: now}
	if err := store.Append([]Change{change}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Append([]Change{change}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(nil, now); err != nil {
		t.Fatal(err)
	}
	if provider.writes != 1 {
		t.Fatalf("writes=%d, want one initial snapshot write", provider.writes)
	}
	if err := store.Append(nil, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if provider.writes != 2 {
		t.Fatalf("writes=%d after retention pruning, want 2", provider.writes)
	}
	gap := Gap{From: now.Add(26 * time.Hour), To: now.Add(26*time.Hour + time.Second)}
	if err := store.MarkGap(gap); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkGap(Gap{From: gap.From.Add(time.Millisecond), To: gap.To.Add(-time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	if provider.writes != 3 {
		t.Fatalf("writes=%d after duplicate gap, want 3", provider.writes)
	}
}

func TestStoreCapsRecordCountAndReportsEvictionGap(t *testing.T) {
	store := NewStore(storage.NewMemory(), index.Scope{OrgID: "org", ClusterID: "cluster"})
	now := time.Now().UTC()
	changes := make([]Change, maxChanges+1)
	for sequence := range changes {
		changes[sequence] = Change{ID: strconv.Itoa(sequence), Cluster: "cluster", Kind: "Pod", Name: "pod", UID: "uid", Type: Created, At: now.Add(time.Duration(sequence) * time.Millisecond)}
	}
	if err := store.Append(changes, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	page, err := store.Query(Query{Limit: 500}, now.Add(time.Hour))
	if err != nil || len(page.Items) != 500 || len(page.Gaps) == 0 || page.Next == "" {
		t.Fatalf("bounded page=%#v err=%v", page, err)
	}
}

func TestStoreMarksTimelineGap(t *testing.T) {
	store := NewStore(storage.NewMemory(), index.Scope{OrgID: "org", ClusterID: "cluster"})
	now := time.Now().UTC()
	gap := Gap{From: now, To: now.Add(time.Minute)}
	if err := store.MarkGap(gap); err != nil {
		t.Fatal(err)
	}
	page, err := store.Query(Query{Since: now, Until: now.Add(time.Minute)}, now.Add(time.Minute))
	if err != nil || len(page.Gaps) != 1 || !page.Gaps[0].From.Equal(gap.From) {
		t.Fatalf("gap page=%#v err=%v", page, err)
	}
	if err := store.MarkGap(Gap{From: now.Add(time.Minute), To: now}); err != ErrInvalidQuery {
		t.Fatalf("invalid gap error=%v", err)
	}
}
