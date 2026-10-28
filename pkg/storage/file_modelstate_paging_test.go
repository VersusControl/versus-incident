package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileModelStateRejectsSymlinkedNamespace(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ModelStateNamespace)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	models := NewModelStore(providerValue)
	if err := models.Put("acme", "health", "key", 1, []byte("outside-write")); err == nil {
		t.Fatal("Put followed a symlinked model namespace")
	}
	outsideArtifact := filepath.Join(outside, "acme", "health", "key.json")
	if _, err := os.Lstat(outsideArtifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside artifact exists after rejected write: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(outsideArtifact), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := []byte(`{"org_id":"acme","agent":"health","key":"key","data":"c2VjcmV0"}`)
	if err := os.WriteFile(outsideArtifact, secret, 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := models.Get("acme", "health", "key")
	if err == nil || state != nil {
		t.Fatalf("Get followed a symlinked model namespace: state=%+v err=%v", state, err)
	}
	got, err := os.ReadFile(outsideArtifact)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("outside artifact changed: data=%q err=%v", got, err)
	}
}

func TestFileModelStateRejectsSymlinkedPathComponents(t *testing.T) {
	orgHex := hex.EncodeToString([]byte("acme"))
	agentHex := hex.EncodeToString([]byte("health"))
	tests := []struct {
		name       string
		parent     []string
		component  string
		outsideRel string
	}{
		{name: "models", component: "models", outsideRel: "acme/health/key.json"},
		{name: "indexes", component: ".indexes", outsideRel: "modelstate/" + orgHex + "/" + agentHex + "/nodes/root.idx"},
		{name: "org", parent: []string{"models"}, component: "acme", outsideRel: "health/key.json"},
		{name: "agent", parent: []string{"models", "acme"}, component: "health", outsideRel: "key.json"},
		{name: "nodes", parent: []string{".indexes", "modelstate", orgHex, agentHex}, component: "nodes", outsideRel: "root.idx"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			providerValue, err := NewFile(FileOptions{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(append([]string{dir}, test.parent...)...)
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(parent, test.component)); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			models := NewModelStore(providerValue)
			if err := models.Put("acme", "health", "key", 1, []byte("secret")); err == nil {
				t.Fatal("Put accepted a symlinked ModelState path component")
			}
			if state, err := models.Get("acme", "health", "key"); err == nil && state != nil {
				t.Fatalf("Get read through a symlinked path component: %+v", state)
			}
			if _, err := os.Lstat(filepath.Join(outside, filepath.FromSlash(test.outsideRel))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("outside path exists after rejected operations: %v", err)
			}
		})
	}
}

func TestFileModelStateRejectsUnsafeArtifactLeafForAllOperations(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerValue.(*fileProvider)
	models := NewModelStore(provider)
	if err := models.Put("acme", "health", "key", 1, []byte("inside")); err != nil {
		t.Fatal(err)
	}
	artifact := provider.blobPath("models/acme/health/key")
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	outsideArtifact := filepath.Join(outside, "secret.json")
	secret := []byte(`{"org_id":"acme","agent":"health","key":"key","data":"c2VjcmV0"}`)
	if err := os.WriteFile(outsideArtifact, secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideArtifact, artifact); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := models.Get("acme", "health", "key"); err == nil {
		t.Fatal("Get accepted a symlink artifact")
	}
	if err := models.Put("acme", "health", "key", 2, []byte("write")); err == nil {
		t.Fatal("Put accepted a symlink artifact")
	}
	if written, err := provider.CreateBlobIfAbsent("models/acme/health/key", []byte("create")); err == nil || written {
		t.Fatalf("CreateBlobIfAbsent accepted a symlink artifact: written=%v err=%v", written, err)
	}
	if swapped, err := provider.CompareAndSwapBlob("models/acme/health/key", secret, []byte("cas")); err == nil || swapped {
		t.Fatalf("CAS accepted a symlink artifact: swapped=%v err=%v", swapped, err)
	}
	if err := models.Purge("acme", "health", "key"); err == nil {
		t.Fatal("Purge accepted a symlink artifact")
	}
	if _, err := models.ListPageBounded(context.Background(), "acme", "health", "", 10); !errors.Is(err, errUnsafeFile) {
		t.Fatalf("page error=%v, want unsafe-file rejection", err)
	}
	got, err := os.ReadFile(outsideArtifact)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("outside artifact changed: data=%q err=%v", got, err)
	}
}

func TestFileModelStateRejectsUnsafeIndexLeaf(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	models := NewModelStore(providerValue)
	if err := models.Put("acme", "health", "key", 1, []byte("inside")); err != nil {
		t.Fatal(err)
	}
	rootIndex := filepath.Join(dir, ".indexes", "modelstate", hex.EncodeToString([]byte("acme")), hex.EncodeToString([]byte("health")), "nodes", "root.idx")
	if err := os.Remove(rootIndex); err != nil {
		t.Fatal(err)
	}
	outsideIndex := filepath.Join(outside, "root.idx")
	indexData := make([]byte, fileTrieNodeBytes)
	indexData[0] = 1
	if err := os.WriteFile(outsideIndex, indexData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideIndex, rootIndex); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := models.ListPageBounded(context.Background(), "acme", "health", "", 1); err == nil {
		t.Fatal("page accepted a symlink index leaf")
	}
	if err := models.Put("acme", "health", "other", 1, []byte("write")); err == nil {
		t.Fatal("Put accepted a symlink index leaf")
	}
	if err := models.Purge("acme", "health", "key"); err == nil {
		t.Fatal("Purge accepted a symlink index leaf")
	}
	got, err := os.ReadFile(outsideIndex)
	if err != nil || !bytes.Equal(got, indexData) {
		t.Fatalf("outside index changed: data=%v err=%v", got, err)
	}
}

func TestFileModelStateRejectsHardlinkedLeaves(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerValue.(*fileProvider)
	models := NewModelStore(provider)
	if err := models.Put("acme", "health", "key", 1, []byte("inside")); err != nil {
		t.Fatal(err)
	}
	artifact := provider.blobPath("models/acme/health/key")
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	outsideArtifact := filepath.Join(outside, "secret.json")
	if err := os.WriteFile(outsideArtifact, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outsideArtifact, artifact); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if _, err := models.Get("acme", "health", "key"); err == nil {
		t.Fatal("Get accepted a multiply linked artifact")
	}
	if err := models.Put("acme", "health", "key", 2, []byte("write")); err == nil {
		t.Fatal("Put replaced a multiply linked artifact")
	}
	got, err := os.ReadFile(outsideArtifact)
	if err != nil || string(got) != "secret" {
		t.Fatalf("outside hardlink target changed: data=%q err=%v", got, err)
	}
}

func TestFileModelStateRejectsHardlinkedIndexLeaf(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	models := NewModelStore(providerValue)
	if err := models.Put("acme", "health", "key", 1, []byte("inside")); err != nil {
		t.Fatal(err)
	}
	rootIndex := filepath.Join(dir, ".indexes", "modelstate", hex.EncodeToString([]byte("acme")), hex.EncodeToString([]byte("health")), "nodes", "root.idx")
	if err := os.Remove(rootIndex); err != nil {
		t.Fatal(err)
	}
	outsideIndex := filepath.Join(outside, "root.idx")
	indexData := make([]byte, fileTrieNodeBytes)
	if err := os.WriteFile(outsideIndex, indexData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outsideIndex, rootIndex); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if _, err := models.ListPageBounded(context.Background(), "acme", "health", "", 1); !errors.Is(err, errUnsafeFile) {
		t.Fatalf("page error=%v, want unsafe-file rejection", err)
	}
	if err := models.Put("acme", "health", "other", 1, []byte("write")); !errors.Is(err, errUnsafeFile) {
		t.Fatalf("Put error=%v, want unsafe-file rejection", err)
	}
	got, err := os.ReadFile(outsideIndex)
	if err != nil || !bytes.Equal(got, indexData) {
		t.Fatalf("outside index changed: data=%v err=%v", got, err)
	}
}

func TestFileModelStateRejectsUnsafeNamespaceLockLeaf(t *testing.T) {
	for _, hardlink := range []bool{false, true} {
		name := "symlink"
		if hardlink {
			name = "hardlink"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			providerValue, err := NewFile(FileOptions{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			models := NewModelStore(providerValue)
			if err := models.Put("acme", "health", "seed", 1, []byte("seed")); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(dir, ".indexes", "modelstate", hex.EncodeToString([]byte("acme")), hex.EncodeToString([]byte("health")), "nodes", ".namespace.lock")
			if err := os.Remove(lockPath); err != nil {
				t.Fatal(err)
			}
			outsideLock := filepath.Join(outside, "lock")
			if err := os.WriteFile(outsideLock, []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if hardlink {
				err = os.Link(outsideLock, lockPath)
			} else {
				err = os.Symlink(outsideLock, lockPath)
			}
			if err != nil {
				t.Skipf("links unavailable: %v", err)
			}
			if err := models.Put("acme", "health", "other", 1, []byte("write")); err == nil {
				t.Fatal("Put accepted an unsafe namespace lock leaf")
			}
			if _, err := models.ListPageBounded(context.Background(), "acme", "health", "", 10); err == nil {
				t.Fatal("page accepted an unsafe namespace lock leaf")
			}
			got, err := os.ReadFile(outsideLock)
			if err != nil || string(got) != "outside" {
				t.Fatalf("outside lock changed: data=%q err=%v", got, err)
			}
		})
	}
}

func TestFileModelStateRejectsSymlinkedConfiguredRoot(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	root := filepath.Join(parent, "data")
	if err := os.Symlink(outside, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := NewFile(FileOptions{DataDir: root}); err == nil {
		t.Fatal("NewFile accepted a symlinked configured root")
	}
}

func TestFileModelStateAncestorReplacementRaceDoesNotEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	models := NewModelStore(providerValue)
	if err := models.Put("acme", "health", "seed", 1, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(dir, "models", "acme", "health")
	parkedDir := filepath.Join(dir, "models", "acme", "health-parked")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(agentDir, parkedDir); err != nil {
				continue
			}
			if err := os.Symlink(outside, agentDir); err == nil {
				_ = os.Remove(agentDir)
			}
			_ = os.Rename(parkedDir, agentDir)
		}
	}()
	for index := 0; index < 40; index++ {
		key := fmt.Sprintf("race-%03d", index)
		_ = models.Put("acme", "health", key, 1, []byte("secret"))
		_, _ = models.Get("acme", "health", key)
	}
	close(stop)
	<-done
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("ancestor replacement escaped storage root: %v", entries)
	}
}

type cancelAfterContext struct {
	context.Context
	mu        sync.Mutex
	remaining int
}

func (ctx *cancelAfterContext) Err() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestFileModelPagingBoundedAndStable(t *testing.T) {
	dir := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerValue.(*fileProvider)
	models := NewModelStore(provider)
	for index := 0; index < 200; index++ {
		if err := models.Put("acme", "health", fmt.Sprintf("key-%04d", index), 1, []byte("payload")); err != nil {
			t.Fatal(err)
		}
	}
	irrelevant := filepath.Join(dir, "irrelevant")
	if err := os.MkdirAll(irrelevant, 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 10000; index++ {
		path := filepath.Join(irrelevant, fmt.Sprintf("noise-%05d.json", index))
		file, createErr := os.Create(path)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}

	cursor := ""
	seen := make(map[string]struct{})
	for len(seen) < 200 {
		page, pageErr := models.ListPageBounded(context.Background(), "acme", "health", cursor, 7)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		if page.Scanned > 44 || len(page.States) > 7 || page.Bytes > filePageMaxBytes {
			t.Fatalf("unbounded page: %+v", page)
		}
		for _, state := range page.States {
			if _, duplicate := seen[state.Key]; duplicate {
				t.Fatalf("duplicate key %q", state.Key)
			}
			seen[state.Key] = struct{}{}
		}
		if page.Next <= cursor {
			t.Fatalf("cursor did not advance: %q <= %q", page.Next, cursor)
		}
		cursor = page.Next
	}
}

func TestFileModelPagingRestartLegacyCancellationAndScope(t *testing.T) {
	dir := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	models := NewModelStore(providerValue)
	for _, org := range []string{"acme", "globex"} {
		for index := 0; index < 4; index++ {
			if err := models.Put(org, "health", fmt.Sprintf("key-%02d", index), 1, []byte(org)); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = providerValue.Close()
	restarted, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	page, err := NewModelStore(restarted).ListPageBounded(context.Background(), "acme", "health", "", 10)
	if err != nil || len(page.States) != 4 {
		t.Fatalf("restart page=%+v err=%v", page, err)
	}
	for _, state := range page.States {
		if state.OrgID != "acme" || string(state.Data) != "acme" {
			t.Fatalf("cross-org result: %+v", state)
		}
	}
	canceling := &cancelAfterContext{Context: context.Background(), remaining: 3}
	if _, err := NewModelStore(restarted).ListPageBounded(canceling, "acme", "health", "", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("during-page cancellation error=%v", err)
	}

	legacyDir := filepath.Join(dir, "models", "legacy", "health")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "old.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewModelStore(restarted).ListPageBounded(context.Background(), "legacy", "health", "", 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("legacy page error=%v", err)
	}
}

func TestFileModelPagingRejectsSymlinkAndConcurrentWrite(t *testing.T) {
	dir := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerValue.(*fileProvider)
	models := NewModelStore(provider)
	if err := models.Put("acme", "health", "a", 1, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := models.Put("acme", "health", "b", 1, []byte("b")); err != nil {
		t.Fatal(err)
	}
	target := provider.blobPath("models/acme/health/a")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "outside-secret"), target); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < 50; index++ {
			_ = models.Put("acme", "health", fmt.Sprintf("z-%03d", index), 1, []byte("z"))
		}
	}()
	_, err = models.ListPageBounded(context.Background(), "acme", "health", "", 10)
	<-done
	if !errors.Is(err, errUnsafeFile) {
		t.Fatalf("page error=%v, want unsafe-file rejection", err)
	}

	deadline, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()
	if _, err := models.ListPageBounded(deadline, "acme", "health", "", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired deadline error=%v", err)
	}
}

func TestFileModelPagingMalformedProgressAndIndexedPurge(t *testing.T) {
	dir := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	models := NewModelStore(providerValue)
	for index := 0; index < 30; index++ {
		key := fmt.Sprintf("bad-%02d", index)
		if err := models.Put("acme", "health", key, 1, []byte("before-corruption")); err != nil {
			t.Fatal(err)
		}
		if err := providerValue.WriteBlob("models/acme/health/"+key, []byte("not-json")); err != nil {
			t.Fatal(err)
		}
	}
	if err := models.Put("acme", "health", "z-valid", 1, []byte("valid")); err != nil {
		t.Fatal(err)
	}
	cursor := ""
	found := false
	for call := 0; call < 40 && !found; call++ {
		page, pageErr := models.ListPageBounded(context.Background(), "acme", "health", cursor, 1)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		if page.Scanned > 10 {
			t.Fatalf("page scanned %d malformed entries", page.Scanned)
		}
		if page.Next <= cursor {
			t.Fatalf("malformed page did not advance: %q <= %q", page.Next, cursor)
		}
		cursor = page.Next
		found = len(page.States) == 1 && page.States[0].Key == "z-valid"
	}
	if !found {
		t.Fatal("valid artifact starved behind malformed entries")
	}
	if err := models.Purge("acme", "health", "z-valid"); err != nil {
		t.Fatal(err)
	}
	page, err := models.ListPageBounded(context.Background(), "acme", "health", cursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.States) != 0 {
		t.Fatalf("purged artifact remained indexed: %+v", page.States)
	}
}

const (
	fileModelStateWorkloadDeadline = 90 * time.Second
	fileModelStateProgressDeadline = 15 * time.Second
)

func runFileModelStateConcurrentOps(t *testing.T, operationCount, workerCount int, action func(index int) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fileModelStateWorkloadDeadline)
	defer cancel()
	var next atomic.Int64
	next.Store(-1)
	progress := make(chan struct{}, 1)
	errorsCh := make(chan error, operationCount)
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				index := int(next.Add(1))
				if index >= operationCount {
					return
				}
				if err := action(index); err != nil {
					errorsCh <- err
				}
				select {
				case progress <- struct{}{}:
				default:
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()

	started := time.Now()
	// Local race phases complete in under one second. Watch progress with ample
	// headroom instead of asserting filesystem throughput, which varies in CI.
	watchdog := time.NewTimer(fileModelStateProgressDeadline)
	defer watchdog.Stop()
	var failure string
	for failure == "" {
		select {
		case <-done:
			failure = "done"
		case <-progress:
			if !watchdog.Stop() {
				select {
				case <-watchdog.C:
				default:
				}
			}
			watchdog.Reset(fileModelStateProgressDeadline)
		case <-watchdog.C:
			buffer := make([]byte, 1<<20)
			count := runtime.Stack(buffer, true)
			t.Logf("ModelState concurrency stalled for %s after %s; goroutines:\n%s", fileModelStateProgressDeadline, time.Since(started).Round(time.Millisecond), buffer[:count])
			failure = "no durable operation completed before the progress deadline"
			cancel()
		case <-ctx.Done():
			failure = "workload deadline exceeded"
		}
	}
	if failure != "done" {
		cancel()
		<-done
	}
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	t.Logf("completed %d durable ModelState operations with %d workers in %s", operationCount, workerCount, time.Since(started).Round(time.Millisecond))
	if failure != "done" {
		t.Fatalf("concurrent ModelState workload failed after workers joined: %s", failure)
	}
}

func assertRestartedModelStateKeys(t *testing.T, dir string, keyCount int) {
	t.Helper()
	restarted, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	models := NewModelStore(restarted)
	seen := make(map[string]int)
	cursor := ""
	for {
		page, pageErr := models.ListPageBounded(context.Background(), "acme", "health", cursor, 7)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		for _, state := range page.States {
			seen[state.Key]++
		}
		if page.Done {
			break
		}
		if page.Next <= cursor {
			t.Fatalf("cursor did not advance: %q <= %q", page.Next, cursor)
		}
		cursor = page.Next
	}
	if seen["shared"] != 1 {
		t.Fatalf("shared key count=%d, want 1", seen["shared"])
	}
	for index := 0; index < keyCount; index++ {
		key := fmt.Sprintf("key-%04d", index)
		want := 0
		if index%2 == 0 {
			want = 1
		}
		if seen[key] != want {
			t.Fatalf("key %q count=%d, want %d", key, seen[key], want)
		}
	}
	if len(seen) != keyCount/2+1 {
		t.Fatalf("paged %d unique keys, want %d", len(seen), keyCount/2+1)
	}
}

func TestFileModelStateHighCardinalityRestartPaging(t *testing.T) {
	dir := t.TempDir()
	providerValue, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	models := NewModelStore(providerValue)
	const keyCount = 96
	for index := 0; index < keyCount; index++ {
		key := fmt.Sprintf("key-%04d", index)
		if err := models.Put("acme", "health", key, 1, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	if err := models.Put("acme", "health", "shared", 1, []byte("shared")); err != nil {
		t.Fatal(err)
	}
	for index := 1; index < keyCount; index += 2 {
		if err := models.Purge("acme", "health", fmt.Sprintf("key-%04d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := providerValue.Close(); err != nil {
		t.Fatal(err)
	}
	assertRestartedModelStateKeys(t, dir, keyCount)
}

func TestFileModelStateConcurrentNamespaceIntegrityAcrossProviders(t *testing.T) {
	dir := t.TempDir()
	const providerCount = 4
	providers := make([]Provider, 0, providerCount)
	stores := make([]*ModelStore, 0, providerCount)
	for index := 0; index < providerCount; index++ {
		provider, err := NewFile(FileOptions{DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		providers = append(providers, provider)
		stores = append(stores, NewModelStore(provider))
	}

	const keyCount = 24
	runFileModelStateConcurrentOps(t, keyCount, 4, func(index int) error {
		key := fmt.Sprintf("key-%04d", index)
		return stores[index%len(stores)].Put("acme", "health", key, 1, []byte(key))
	})
	runFileModelStateConcurrentOps(t, keyCount, 4, func(index int) error {
		return stores[index%len(stores)].Put("acme", "health", "shared", index+1, []byte(strconv.Itoa(index)))
	})
	runFileModelStateConcurrentOps(t, keyCount, 4, func(index int) error {
		key := fmt.Sprintf("key-%04d", index)
		results := make(chan error, 2)
		go func() {
			results <- stores[index%len(stores)].Put("acme", "health", key, 2, []byte("raced-write"))
		}()
		go func() {
			err := stores[(index+1)%len(stores)].Purge("acme", "health", key)
			if errors.Is(err, ErrNotFound) {
				err = nil
			}
			results <- err
		}()
		for attempt := 0; attempt < 2; attempt++ {
			if err := <-results; err != nil {
				return err
			}
		}
		return nil
	})

	for index := 0; index < keyCount; index++ {
		key := fmt.Sprintf("key-%04d", index)
		store := stores[index%len(stores)]
		if index%2 == 0 {
			if err := store.Put("acme", "health", key, 3, []byte(key)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := store.Purge("acme", "health", key); err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	for _, provider := range providers {
		if err := provider.Close(); err != nil {
			t.Fatal(err)
		}
	}

	restarted, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	assertRestartedModelStateKeys(t, dir, keyCount)
}

func TestFileModelStateRepairsInterruptedNamespaceOperations(t *testing.T) {
	dir := t.TempDir()
	provider, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	models := NewModelStore(provider)
	if err := models.Put("acme", "health", "purged", 1, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}

	nodes := filepath.Join(dir, ".indexes", "modelstate", hex.EncodeToString([]byte("acme")), hex.EncodeToString([]byte("health")), "nodes")
	artifacts := filepath.Join(dir, "models", "acme", "health")
	pending := filepath.Join(nodes, modelStatePendingName)
	created := ModelState{OrgID: "acme", Agent: "health", Key: "created", Version: 1, Data: []byte("new")}
	createdData, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "created.json"), createdData, 0o644); err != nil {
		t.Fatal(err)
	}
	putPending, err := json.Marshal(modelStatePendingOperation{Key: "created"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, putPending, 0o600); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	page, err := NewModelStore(restarted).ListPageBounded(context.Background(), "acme", "health", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]int)
	for _, state := range page.States {
		seen[state.Key]++
	}
	if seen["created"] != 1 || seen["purged"] != 1 {
		t.Fatalf("repaired put page keys=%v", seen)
	}

	deletePending, err := json.Marshal(modelStatePendingOperation{Key: "purged", Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, deletePending, 0o600); err != nil {
		t.Fatal(err)
	}
	page, err = NewModelStore(restarted).ListPageBounded(context.Background(), "acme", "health", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	seen = make(map[string]int)
	for _, state := range page.States {
		seen[state.Key]++
	}
	if seen["created"] != 1 || seen["purged"] != 0 {
		t.Fatalf("repaired purge page keys=%v", seen)
	}
	if _, err := os.Stat(filepath.Join(artifacts, "purged.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending purge artifact still exists: %v", err)
	}
	if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending operation was not cleared: %v", err)
	}
}
