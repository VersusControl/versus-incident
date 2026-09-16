//go:build darwin || linux

package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const fileModelStateSubprocessEnv = "VERSUS_TEST_MODELSTATE_SUBPROCESS"

func waitForModelStateMarker(ctx context.Context, path string) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func writeModelStateTestMarker(path string) error {
	return os.WriteFile(path, []byte("ready"), 0o600)
}

func TestFileModelStateSubprocessHelper(t *testing.T) {
	if os.Getenv(fileModelStateSubprocessEnv) == "" {
		t.Skip("subprocess helper")
	}
	dir := os.Getenv("VERSUS_TEST_MODELSTATE_DIR")
	processID, err := strconv.Atoi(os.Getenv("VERSUS_TEST_MODELSTATE_PROCESS"))
	if err != nil || processID < 0 || processID > 1 {
		t.Fatalf("invalid subprocess id %q", os.Getenv("VERSUS_TEST_MODELSTATE_PROCESS"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := writeModelStateTestMarker(filepath.Join(dir, fmt.Sprintf("ready-%d", processID))); err != nil {
		t.Fatal(err)
	}
	if err := waitForModelStateMarker(ctx, filepath.Join(dir, "start")); err != nil {
		t.Fatal(err)
	}
	provider, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	models := NewModelStore(provider)
	for index := 0; index < 12; index++ {
		key := fmt.Sprintf("process-%d-%02d", processID, index)
		if err := models.Put("acme", "health", key, 1, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	for version := 0; version < 8; version++ {
		if err := models.Put("acme", "health", "shared", processID*100+version, []byte(strconv.Itoa(processID))); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeModelStateTestMarker(filepath.Join(dir, fmt.Sprintf("contention-ready-%d", processID))); err != nil {
		t.Fatal(err)
	}
	other := 1 - processID
	if err := waitForModelStateMarker(ctx, filepath.Join(dir, fmt.Sprintf("contention-ready-%d", other))); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		for index := 0; index < 6; index++ {
			key := fmt.Sprintf("purged-%02d", index)
			if processID == 0 {
				err = models.Put("acme", "health", key, round+1, []byte("writer"))
			} else {
				err = models.Purge("acme", "health", key)
				if errors.Is(err, ErrNotFound) {
					err = nil
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			runtime.Gosched()
		}
	}
	if processID == 0 {
		if err := writeModelStateTestMarker(filepath.Join(dir, "writer-done")); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := waitForModelStateMarker(ctx, filepath.Join(dir, "writer-done")); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 6; index++ {
		err := models.Purge("acme", "health", fmt.Sprintf("purged-%02d", index))
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
}

type modelStateChild struct {
	command *exec.Cmd
	output  bytes.Buffer
}

type modelStateChildResult struct {
	index int
	err   error
}

func runModelStateChildren(t *testing.T, dir string) {
	t.Helper()
	children := make([]*modelStateChild, 2)
	for index := range children {
		child := &modelStateChild{}
		child.command = exec.Command(os.Args[0], "-test.run=^TestFileModelStateSubprocessHelper$", "-test.v")
		child.command.Env = append(os.Environ(),
			fileModelStateSubprocessEnv+"=1",
			"VERSUS_TEST_MODELSTATE_DIR="+dir,
			fmt.Sprintf("VERSUS_TEST_MODELSTATE_PROCESS=%d", index),
		)
		child.command.Stdout = &child.output
		child.command.Stderr = &child.output
		if err := child.command.Start(); err != nil {
			for previous := 0; previous < index; previous++ {
				_ = children[previous].command.Process.Kill()
				_ = children[previous].command.Wait()
			}
			t.Fatalf("start child %d: %v", index, err)
		}
		children[index] = child
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for index := range children {
		if err := waitForModelStateMarker(ctx, filepath.Join(dir, fmt.Sprintf("ready-%d", index))); err != nil {
			for _, child := range children {
				_ = child.command.Process.Kill()
			}
			for _, child := range children {
				_ = child.command.Wait()
			}
			t.Fatalf("children did not reach start barrier: %v", err)
		}
	}
	if err := writeModelStateTestMarker(filepath.Join(dir, "start")); err != nil {
		for _, child := range children {
			_ = child.command.Process.Kill()
		}
		for _, child := range children {
			_ = child.command.Wait()
		}
		t.Fatal(err)
	}
	results := make(chan modelStateChildResult, len(children))
	for index, child := range children {
		go func(index int, child *modelStateChild) {
			results <- modelStateChildResult{index: index, err: child.command.Wait()}
		}(index, child)
	}
	remaining := len(children)
	var failures []string
	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			if result.err != nil {
				failures = append(failures, fmt.Sprintf("child %d: %v\n%s", result.index, result.err, children[result.index].output.String()))
				cancel()
				for _, child := range children {
					if child.command.ProcessState == nil {
						_ = child.command.Process.Kill()
					}
				}
			}
		case <-ctx.Done():
			failures = append(failures, "subprocess deadline exceeded")
			for _, child := range children {
				if child.command.ProcessState == nil {
					_ = child.command.Process.Kill()
				}
			}
			for remaining > 0 {
				<-results
				remaining--
			}
		}
	}
	if len(failures) != 0 {
		t.Fatalf("ModelState subprocess contention failed after all children exited:\n%s", strings.Join(failures, "\n"))
	}
}

func TestFileModelStateSubprocessNamespaceIntegrity(t *testing.T) {
	dir := t.TempDir()
	provider, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	models := NewModelStore(provider)
	if err := models.Put("acme", "health", "recover-purge", 1, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	nodesPath := filepath.Join(dir, ".indexes", "modelstate", hex.EncodeToString([]byte("acme")), hex.EncodeToString([]byte("health")), "nodes")
	pending, err := json.Marshal(modelStatePendingOperation{Key: "recover-purge", Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodesPath, modelStatePendingName), pending, 0o600); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	runModelStateChildren(t, dir)
	t.Logf("two-process durable ModelState contention completed in %s", time.Since(started).Round(time.Millisecond))

	restarted, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	seen := make(map[string]int)
	cursor := ""
	for {
		page, err := NewModelStore(restarted).ListPageBounded(context.Background(), "acme", "health", cursor, 5)
		if err != nil {
			t.Fatal(err)
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
	if len(seen) != 25 {
		t.Fatalf("paged keys=%v, want 24 distinct process keys plus shared", seen)
	}
	if seen["shared"] != 1 {
		t.Fatalf("shared key count=%d, want 1", seen["shared"])
	}
	for processID := 0; processID < 2; processID++ {
		for index := 0; index < 12; index++ {
			key := fmt.Sprintf("process-%d-%02d", processID, index)
			if seen[key] != 1 {
				t.Fatalf("retained key %q count=%d, want 1", key, seen[key])
			}
		}
	}
	root, artifacts, nodes, err := openModelStateDirs(dir, []string{"acme", "health"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeModelStateDirs(root, artifacts, nodes)
	for _, key := range []string{"recover-purge", "purged-00", "purged-01", "purged-02", "purged-03", "purged-04", "purged-05"} {
		if seen[key] != 0 {
			t.Fatalf("purged key %q appeared %d times", key, seen[key])
		}
		node, err := readTrieNodeAt(nodes, []byte(key))
		if err != nil {
			t.Fatal(err)
		}
		if node[0] != 0 {
			t.Fatalf("purged key %q retained a phantom trie terminal", key)
		}
		if _, err := regularFileStatAt(artifacts, key+".json"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("purged key %q artifact remains: %v", key, err)
		}
	}
	if _, err := regularFileStatAt(nodes, modelStatePendingName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending marker remains after subprocess recovery: %v", err)
	}
}
