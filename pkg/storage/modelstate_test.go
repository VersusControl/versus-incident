package storage_test

// modelstate_test.go — covers the learned-state / model-artifact
// persistence seam: the opaque-bytes ModelStore over a Provider.

import (
	"bytes"
	"errors"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestModelStore_RoundTrip(t *testing.T) {
	ms := storage.NewModelStore(storage.NewMemory())

	payload := []byte{0x00, 0x01, 0xff, 0x42} // arbitrary opaque bytes
	if err := ms.Put("acme", "sre", "baseline:checkout", 3, payload); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := ms.Get("acme", "sre", "baseline:checkout")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil for a persisted artifact")
	}
	if !bytes.Equal(got.Data, payload) {
		t.Fatalf("Data = %v, want %v", got.Data, payload)
	}
	if got.Version != 3 {
		t.Fatalf("Version = %d, want 3", got.Version)
	}
	if got.OrgID != "acme" || got.Agent != "sre" || got.Key != "baseline:checkout" {
		t.Fatalf("metadata mismatch: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt must be stamped")
	}
}

func TestModelStore_DefaultOrg(t *testing.T) {
	ms := storage.NewModelStore(storage.NewMemory())

	// Blank org persists under the default org and is readable both ways.
	if err := ms.Put("", "sre", "k", 1, []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := ms.Get("", "sre", "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.OrgID != storage.DefaultOrgID {
		t.Fatalf("OrgID = %v, want %q", got, storage.DefaultOrgID)
	}
	// Reading with the explicit default org resolves the same artifact.
	got2, err := ms.Get(storage.DefaultOrgID, "sre", "k")
	if err != nil {
		t.Fatalf("Get(default): %v", err)
	}
	if got2 == nil {
		t.Fatal("default-org read should resolve the same artifact")
	}
}

func TestModelStore_MissingReturnsNil(t *testing.T) {
	ms := storage.NewModelStore(storage.NewMemory())
	got, err := ms.Get("acme", "sre", "never-written")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("missing artifact should return nil, got %+v", got)
	}
}

func TestModelStore_PerOrgIsolation(t *testing.T) {
	ms := storage.NewModelStore(storage.NewMemory())

	if err := ms.Put("orgA", "sre", "baseline", 1, []byte("A")); err != nil {
		t.Fatalf("Put orgA: %v", err)
	}
	if err := ms.Put("orgB", "sre", "baseline", 1, []byte("B")); err != nil {
		t.Fatalf("Put orgB: %v", err)
	}

	a, _ := ms.Get("orgA", "sre", "baseline")
	b, _ := ms.Get("orgB", "sre", "baseline")
	if a == nil || b == nil {
		t.Fatal("both orgs should resolve their own artifact")
	}
	if string(a.Data) != "A" || string(b.Data) != "B" {
		t.Fatalf("org artifacts leaked: A=%q B=%q", a.Data, b.Data)
	}

	// Org C, which never wrote anything, must not resolve org A's state.
	c, _ := ms.Get("orgC", "sre", "baseline")
	if c != nil {
		t.Fatalf("orgC must not resolve another org's state, got %q", c.Data)
	}
}

func TestModelStore_Purge(t *testing.T) {
	ms := storage.NewModelStore(storage.NewMemory())

	if err := ms.Put("acme", "sre", "k", 1, []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := ms.Purge("acme", "sre", "k"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	got, _ := ms.Get("acme", "sre", "k")
	if got != nil {
		t.Fatalf("artifact should be purged, got %+v", got)
	}
	// Purging a missing artifact surfaces ErrNotFound from the Lifecycle.
	if err := ms.Purge("acme", "sre", "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("purge of missing artifact = %v, want ErrNotFound", err)
	}
}

// purgeUnsupportedProvider implements only Provider (no Lifecycle) so we
// can assert Purge fails closed with ErrUnsupported.
type purgeUnsupportedProvider struct{ storage.Provider }

func TestModelStore_PurgeUnsupported(t *testing.T) {
	ms := storage.NewModelStore(purgeUnsupportedProvider{storage.NewMemory()})
	if err := ms.Put("acme", "sre", "k", 1, []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := ms.Purge("acme", "sre", "k"); !errors.Is(err, storage.ErrUnsupported) {
		t.Fatalf("Purge on a Lifecycle-less backend = %v, want ErrUnsupported", err)
	}
}

func TestModelStore_InvalidKey(t *testing.T) {
	ms := storage.NewModelStore(storage.NewMemory())
	for _, c := range []struct{ org, agent, key string }{
		{"acme", "sre", "../escape"},
		{"acme", "sre/sub", "k"},
		{"acme", "sre", ""},
		{"acme", "sre", "a/b"},
	} {
		if err := ms.Put(c.org, c.agent, c.key, 1, []byte("x")); !errors.Is(err, storage.ErrInvalidModelKey) {
			t.Fatalf("Put(%q,%q,%q) = %v, want ErrInvalidModelKey", c.org, c.agent, c.key, err)
		}
	}
}

func TestModelStore_List(t *testing.T) {
	ms := storage.NewModelStore(storage.NewMemory())

	// Persist three artifacts under (acme, sre) and one under a different
	// agent + a different org that must NOT show up in the (acme, sre) list.
	if err := ms.Put("acme", "sre", "k1", 1, []byte("one")); err != nil {
		t.Fatalf("Put k1: %v", err)
	}
	if err := ms.Put("acme", "sre", "k2", 2, []byte("two")); err != nil {
		t.Fatalf("Put k2: %v", err)
	}
	if err := ms.Put("acme", "sre", "k3", 3, []byte("three")); err != nil {
		t.Fatalf("Put k3: %v", err)
	}
	if err := ms.Put("acme", "other-agent", "k1", 9, []byte("nope")); err != nil {
		t.Fatalf("Put other-agent: %v", err)
	}
	if err := ms.Put("globex", "sre", "k1", 9, []byte("nope")); err != nil {
		t.Fatalf("Put globex: %v", err)
	}

	got, err := ms.List("acme", "sre")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	byKey := map[string][]byte{}
	for _, m := range got {
		if m.OrgID != "acme" || m.Agent != "sre" {
			t.Fatalf("List returned a foreign artifact: %+v", m)
		}
		byKey[m.Key] = m.Data
	}
	for k, want := range map[string]string{"k1": "one", "k2": "two", "k3": "three"} {
		if string(byKey[k]) != want {
			t.Fatalf("List[%s] = %q, want %q", k, byKey[k], want)
		}
	}
}

func TestModelStore_ListEmptyForUnknownOrg(t *testing.T) {
	ms := storage.NewModelStore(storage.NewMemory())
	if err := ms.Put("acme", "sre", "k1", 1, []byte("one")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// An org that never wrote anything must list nothing — no leakage, no
	// error.
	got, err := ms.List("nobody", "sre")
	if err != nil {
		t.Fatalf("List(unknown org): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List(unknown org) len = %d, want 0", len(got))
	}
}

func TestModelStore_ListInvalidKey(t *testing.T) {
	ms := storage.NewModelStore(storage.NewMemory())
	for _, c := range []struct{ org, agent string }{
		{"acme", "sre/sub"},
		{"acme", "../escape"},
		{"acme", ""},
	} {
		if _, err := ms.List(c.org, c.agent); !errors.Is(err, storage.ErrInvalidModelKey) {
			t.Fatalf("List(%q,%q) = %v, want ErrInvalidModelKey", c.org, c.agent, err)
		}
	}
}
