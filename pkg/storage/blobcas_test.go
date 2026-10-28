package storage_test

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func runBlobCASContract(t *testing.T, providers ...storage.Provider) {
	t.Helper()
	if len(providers) != 2 {
		t.Fatal("blob CAS contract requires two provider instances")
	}
	first, ok := providers[0].(storage.BlobCAS)
	if !ok {
		t.Fatalf("backend %T must implement storage.BlobCAS", providers[0])
	}
	second, ok := providers[1].(storage.BlobCAS)
	if !ok {
		t.Fatalf("backend %T must implement storage.BlobCAS", providers[1])
	}
	if swapped, err := first.CompareAndSwapBlob("cas/session", nil, []byte("zero")); err != nil || !swapped {
		t.Fatalf("initial swap=%v err=%v", swapped, err)
	}
	if swapped, err := second.CompareAndSwapBlob("cas/session", nil, []byte("stale")); err != nil || swapped {
		t.Fatalf("absent compare over existing swap=%v err=%v", swapped, err)
	}
	if swapped, err := second.CompareAndSwapBlob("cas/session", []byte("zero"), nil); err != nil || !swapped {
		t.Fatalf("delete swap=%v err=%v", swapped, err)
	}
	if data, err := providers[0].ReadBlob("cas/session"); err != nil || data != nil {
		t.Fatalf("read after delete=%q err=%v, want missing", data, err)
	}
	if swapped, err := first.CompareAndSwapBlob("cas/session", nil, []byte("recreated")); err != nil || !swapped {
		t.Fatalf("recreate swap=%v err=%v", swapped, err)
	}

	const writers = 20
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			provider := providers[index%len(providers)]
			cas := provider.(storage.BlobCAS)
			for {
				current, err := provider.ReadBlob("cas/counter")
				if err != nil {
					t.Errorf("read counter: %v", err)
					return
				}
				next := fmt.Appendf(nil, "%s,%d", current, index)
				swapped, err := cas.CompareAndSwapBlob("cas/counter", current, next)
				if err != nil {
					t.Errorf("swap counter: %v", err)
					return
				}
				if swapped {
					return
				}
			}
		}(index)
	}
	wait.Wait()
	got, err := providers[0].ReadBlob("cas/counter")
	if err != nil {
		t.Fatal(err)
	}
	commas := 0
	for _, value := range got {
		if value == ',' {
			commas++
		}
	}
	if commas != writers {
		t.Fatalf("successful updates = %d, want %d: %q", commas, writers, got)
	}
}

func TestMemoryBlobCAS(t *testing.T) {
	provider := storage.NewMemory()
	runBlobCASContract(t, provider, provider)
}

func TestIndependentFileProvidersBlobCAS(t *testing.T) {
	directory := t.TempDir()
	first, err := storage.NewFile(storage.FileOptions{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.NewFile(storage.FileOptions{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	runBlobCASContract(t, first, second)
}

func TestPostgresBlobCAS(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres blob CAS test")
	}
	first, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	prefix := fmt.Sprintf("cas-%d/", time.Now().UnixNano())
	wrappedFirst := &prefixedProvider{Provider: first, prefix: prefix}
	wrappedSecond := &prefixedProvider{Provider: second, prefix: prefix}
	t.Run("contract", func(t *testing.T) {
		t.Cleanup(func() {
			blobs, listErr := first.ListBlobs(prefix)
			if listErr != nil {
				t.Errorf("list postgres CAS blobs for cleanup: %v", listErr)
				return
			}
			cas := first.(storage.BlobCAS)
			for _, blob := range blobs {
				swapped, cleanupErr := cas.CompareAndSwapBlob(blob.Name, blob.Data, nil)
				if cleanupErr != nil || !swapped {
					t.Errorf("clean up postgres CAS blob %q: swapped=%v err=%v", blob.Name, swapped, cleanupErr)
				}
			}
		})
		runBlobCASContract(t, wrappedFirst, wrappedSecond)
	})
	remaining, err := first.ListBlobs(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("postgres CAS cleanup left %d blobs", len(remaining))
	}
}

type prefixedProvider struct {
	storage.Provider
	prefix string
}

func (provider *prefixedProvider) ReadBlob(name string) ([]byte, error) {
	return provider.Provider.ReadBlob(provider.prefix + name)
}

func (provider *prefixedProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(provider.prefix+name, expected, replacement)
}
