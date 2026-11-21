package storage_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// File storage maps a blob name to a file under the data directory. A name
// that would leave it (absolute, a ".." element, a NUL) is refused by every
// blob operation and writes nothing anywhere; a nested name stays inside.
func TestFileBlobNamesStayInsideTheDataDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "data")
	provider, err := storage.NewFile(storage.FileOptions{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	creator := provider.(storage.BlobCreator)
	swapper := provider.(storage.BlobCAS)
	for _, name := range []string{
		"settings.alert-window.x/../../../ESCAPED/planted",
		"settings.alert-window.x/../other",
		"../outside",
		"..",
		"/absolute/name",
		"nul\x00name",
		"",
	} {
		if err := provider.WriteBlob(name, []byte(`{}`)); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("WriteBlob(%q) = %v, want ErrInvalidBlobName", name, err)
		}
		if _, err := provider.ReadBlob(name); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("ReadBlob(%q) = %v, want ErrInvalidBlobName", name, err)
		}
		if _, err := creator.CreateBlobIfAbsent(name, []byte(`{}`)); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("CreateBlobIfAbsent(%q) = %v, want ErrInvalidBlobName", name, err)
		}
		if _, err := swapper.CompareAndSwapBlob(name, nil, []byte(`{}`)); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("CompareAndSwapBlob(%q) = %v, want ErrInvalidBlobName", name, err)
		}
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !strings.HasPrefix(path, directory+string(filepath.Separator)) {
			t.Errorf("a refused name wrote %s outside the data directory", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := provider.WriteBlob("kubernetes/inventory/nested", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("nested blob name refused: %v", err)
	}
	if data, err := provider.ReadBlob("kubernetes/inventory/nested"); err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("nested blob read = %q, %v", data, err)
	}
}

// File storage never follows a link inside its data directory to a file
// outside it: a directory link that resolves outside or a link at the blob
// file itself answers ErrInvalidBlobName and touches nothing outside, and a
// link planted where a temp file might go is never opened. The path to the data directory may pass through a
// link (macOS /tmp, a Kubernetes projected volume's ..data), and a link that
// stays inside the data directory keeps working.
func TestFileBlobsDoNotFollowLinksOutOfTheDataDirectory(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "victim.json")
	if err := os.WriteFile(victim, []byte(`{"secret":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A projected-volume style data directory: ..data links to a dated sibling.
	volume := filepath.Join(root, "volume")
	if err := os.MkdirAll(filepath.Join(volume, "..2026_11_21"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_11_21", filepath.Join(volume, "..data")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	directory := filepath.Join(volume, "..data", "store")
	provider, err := storage.NewFile(storage.FileOptions{DataDir: directory})
	if err != nil {
		t.Fatalf("a linked data directory was refused: %v", err)
	}
	if err := provider.WriteBlob("kubernetes/inventory/ok", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("write under a linked data directory: %v", err)
	}
	if data, err := provider.ReadBlob("kubernetes/inventory/ok"); err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("read under a linked data directory = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(volume, "..2026_11_21", "store", "kubernetes", "inventory", "ok.json")); err != nil {
		t.Fatalf("the blob is not in the resolved data directory: %v", err)
	}

	real := filepath.Join(volume, "..2026_11_21", "store")
	for _, link := range []struct{ path, target string }{
		{filepath.Join(real, "escape"), outside},
		{filepath.Join(real, "victim.json"), victim},
		{filepath.Join(real, "planted.json.tmp"), victim},
		{filepath.Join(real, "inside"), "kubernetes"},
	} {
		if err := os.Symlink(link.target, link.path); err != nil {
			t.Fatal(err)
		}
	}
	creator := provider.(storage.BlobCreator)
	swapper := provider.(storage.BlobCAS)
	for _, name := range []string{"escape/victim", "escape/new/blob", "victim"} {
		if _, err := provider.ReadBlob(name); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("ReadBlob(%q) = %v, want ErrInvalidBlobName", name, err)
		}
		if err := provider.WriteBlob(name, []byte(`{"overwritten":true}`)); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("WriteBlob(%q) = %v, want ErrInvalidBlobName", name, err)
		}
		if _, err := creator.CreateBlobIfAbsent(name, []byte(`{}`)); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("CreateBlobIfAbsent(%q) = %v, want ErrInvalidBlobName", name, err)
		}
		if _, err := swapper.CompareAndSwapBlob(name, nil, []byte(`{}`)); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("CompareAndSwapBlob(%q) = %v, want ErrInvalidBlobName", name, err)
		}
	}
	if err := provider.WriteBlob("planted", []byte(`{"planted":true}`)); err != nil {
		t.Errorf("WriteBlob next to a planted temp-file link = %v", err)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != `{"secret":true}` {
		t.Fatalf("a file outside the data directory changed: %q, %v", data, err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 1 {
		t.Fatalf("something was written outside the data directory: %v, %v", entries, err)
	}
	if data, err := provider.ReadBlob("inside/inventory/ok"); err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("a link that stays inside the data directory = %q, %v", data, err)
	}
	blobs, err := provider.ListBlobs("")
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range blobs {
		if blob.Name == "victim" || strings.Contains(string(blob.Data), "secret") {
			t.Fatalf("listing followed a link out of the data directory: %s", blob.Name)
		}
	}
}

// A blob name past the length or depth bound is refused at once by every
// blob operation, before any path work, while the longest names the stores
// write keep working.
func TestFileBlobNamesAreBounded(t *testing.T) {
	provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	creator := provider.(storage.BlobCreator)
	swapper := provider.(storage.BlobCAS)
	for label, name := range map[string]string{
		"100 kB name":        strings.Repeat("a", 100<<10),
		"1,000-element name": strings.Repeat("a/", 999) + "a",
		"17-element name":    strings.Repeat("a/", 16) + "a",
	} {
		started := time.Now()
		if _, err := provider.ReadBlob(name); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("%s: ReadBlob = %v", label, err)
		}
		if err := provider.WriteBlob(name, []byte(`{}`)); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("%s: WriteBlob = %v", label, err)
		}
		if _, err := creator.CreateBlobIfAbsent(name, []byte(`{}`)); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("%s: CreateBlobIfAbsent = %v", label, err)
		}
		if _, err := swapper.CompareAndSwapBlob(name, nil, []byte(`{}`)); !errors.Is(err, storage.ErrInvalidBlobName) {
			t.Errorf("%s: CompareAndSwapBlob = %v", label, err)
		}
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			t.Errorf("%s: refusing took %s", label, elapsed)
		}
	}
	hex64 := strings.Repeat("0123456789abcdef", 4)
	for _, name := range []string{
		// The alert record, the longest name OSS writes, under an Enterprise org prefix.
		"org/" + strings.Repeat("o", 64) + "/kubernetes/agent-settings/" + hex64 + ".json.alert-window." + hex64,
		// The deepest: a ledger entry under an Enterprise org prefix.
		"org/licensed/ledger/0123456789abcdef/runs/" + strings.Repeat("r", 36) + "/entries/000000000001.json",
		"kubernetes/inventory/" + strings.Repeat("i", 43) + "/data/" + strings.Repeat("o", 64) + "/1",
		// Exactly at both bounds.
		strings.Repeat(strings.Repeat("a", 63)+"/", 15) + strings.Repeat("b", 64),
	} {
		if err := provider.WriteBlob(name, []byte(`{"ok":true}`)); err != nil {
			t.Fatalf("WriteBlob(%d bytes) = %v", len(name), err)
		}
		if data, err := provider.ReadBlob(name); err != nil || string(data) != `{"ok":true}` {
			t.Fatalf("ReadBlob(%d bytes) = %q, %v", len(name), data, err)
		}
	}
}
