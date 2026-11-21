//go:build darwin || linux

package storage_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// A local writer that swaps a directory inside the data directory for a link
// out of it, in one step and over and over while blob calls run, never gets
// a blob call to read, write, create or delete anything outside: every call
// reaches its file through the data directory handle.
func TestFileBlobsStayInsideWhileADirectoryIsSwappedForALink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	data := filepath.Join(root, "data")
	race := filepath.Join(data, "race")
	for _, dir := range []string{outside, race} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outsideVictim, outsideDoomed := []byte(`{"outside":true}`), []byte(`{"doomed":true}`)
	if err := os.WriteFile(filepath.Join(outside, "victim.json"), outsideVictim, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "doomed.json"), outsideDoomed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(race, "victim.json"), []byte(`{"inside":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "swap-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if err := exchangePaths(race, link); err != nil {
		t.Skipf("atomic exchange unavailable: %v", err)
	}
	if err := exchangePaths(race, link); err != nil {
		t.Fatal(err)
	}
	provider, err := storage.NewFile(storage.FileOptions{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	creator := provider.(storage.BlobCreator)
	swapper := provider.(storage.BlobCAS)

	stop := make(chan struct{})
	var swaps atomic.Int64
	var swapping sync.WaitGroup
	swapping.Add(1)
	go func() {
		defer swapping.Done()
		for {
			select {
			case <-stop:
				if info, err := os.Lstat(race); err == nil && info.Mode()&os.ModeSymlink != 0 {
					_ = exchangePaths(race, link)
				}
				return
			default:
			}
			if exchangePaths(race, link) == nil {
				swaps.Add(1)
			}
		}
	}()
	var outsideReads, outsideDeletes, outsideReplaces, calls atomic.Int64
	deadline := time.Now().Add(2 * time.Second)
	var workers sync.WaitGroup
	for worker := range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				calls.Add(1)
				switch i % 5 {
				case 0:
					if got, err := provider.ReadBlob("race/victim"); err == nil && bytes.Equal(got, outsideVictim) {
						outsideReads.Add(1)
					}
				case 1:
					_ = provider.WriteBlob(fmt.Sprintf("race/w%d-%d", worker, i), []byte(`{}`))
				case 2:
					_, _ = creator.CreateBlobIfAbsent(fmt.Sprintf("race/c%d-%d/blob", worker, i), []byte(`{}`))
				case 3:
					if ok, err := swapper.CompareAndSwapBlob("race/doomed", outsideDoomed, nil); err == nil && ok {
						outsideDeletes.Add(1)
					}
				case 4:
					if ok, err := swapper.CompareAndSwapBlob("race/victim", outsideVictim, []byte(`{"replaced":true}`)); err == nil && ok {
						outsideReplaces.Add(1)
					}
				}
			}
		}()
	}
	workers.Wait()
	close(stop)
	swapping.Wait()
	if swaps.Load() < 100 {
		t.Skipf("only %d swaps ran; the race was not exercised", swaps.Load())
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(outside, "victim.json"))
	doomed, doomedErr := os.ReadFile(filepath.Join(outside, "doomed.json"))
	if outsideReads.Load() != 0 || outsideDeletes.Load() != 0 || outsideReplaces.Load() != 0 || len(entries) != 2 || !bytes.Equal(got, outsideVictim) || doomedErr != nil || !bytes.Equal(doomed, outsideDoomed) {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("blob calls reached outside the data directory over %d swaps and %d calls: reads=%d deletes=%d replaces=%d outside=%v victim=%q doomed=%v",
			swaps.Load(), calls.Load(), outsideReads.Load(), outsideDeletes.Load(), outsideReplaces.Load(), names, got, doomedErr)
	}
	t.Logf("%d swaps, %d blob calls, nothing outside touched", swaps.Load(), calls.Load())
}

// A hard link planted where a temp file used to go, next to a blob or the
// incident file, is never opened: writes create a new temp file under a fresh
// name, so the linked file outside keeps its bytes.
func TestFileWritesNeverOpenAPlantedTempFileHardLink(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	provider, err := storage.NewFile(storage.FileOptions{DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	outside := filepath.Join(root, "outside.json")
	original := []byte(`{"outside":true}`)
	if err := os.WriteFile(outside, original, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, planted := range []string{"blob.json.tmp", "incidents.json.tmp"} {
		if err := os.Link(outside, filepath.Join(data, planted)); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
	}
	if err := provider.WriteBlob("blob", []byte(`{"first":true}`)); err != nil {
		t.Fatal(err)
	}
	if ok, err := provider.(storage.BlobCAS).CompareAndSwapBlob("blob", []byte(`{"first":true}`), []byte(`{"second":true}`)); err != nil || !ok {
		t.Fatalf("compare-and-swap = %t, %v", ok, err)
	}
	if err := provider.SaveIncident(&storage.IncidentRecord{ID: "incident", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(outside); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("a write went through a planted hard link: %q, %v", got, err)
	}
	if got, err := provider.ReadBlob("blob"); err != nil || string(got) != `{"second":true}` {
		t.Fatalf("blob = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(data, "blob.json")); errors.Is(err, os.ErrNotExist) {
		t.Fatal("the blob file was not written")
	}
}
