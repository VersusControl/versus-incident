//go:build darwin || linux

package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type secureFileRoot struct {
	file *os.File
}

func validateSecureStorageRoot(path string) error {
	root, err := openSecureFileRoot(path)
	if err != nil {
		return err
	}
	return root.close()
}

func openSecureFileRoot(path string) (*secureFileRoot, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(abs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &secureFileRoot{file: os.NewFile(uintptr(fd), abs)}, nil
}

func (root *secureFileRoot) close() error {
	if root == nil || root.file == nil {
		return nil
	}
	return root.file.Close()
}

func validSecureComponent(component string) bool {
	return component != "" && component != "." && component != ".." && !strings.ContainsAny(component, `/\\`)
}

func (root *secureFileRoot) openDir(parts []string, create bool) (*os.File, error) {
	fd, err := unix.Dup(int(root.file.Fd()))
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), root.file.Name())
	for _, component := range parts {
		if !validSecureComponent(component) {
			_ = current.Close()
			return nil, errUnsafeFile
		}
		nextFD, openErr := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			if mkdirErr := unix.Mkdirat(int(current.Fd()), component, 0o755); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = current.Close()
				return nil, mkdirErr
			}
			nextFD, openErr = unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			_ = current.Close()
			return nil, openErr
		}
		next := os.NewFile(uintptr(nextFD), component)
		_ = current.Close()
		current = next
	}
	return current, nil
}

func parseModelStateName(name string) ([]string, string, bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != ModelStateNamespace {
		return nil, "", false
	}
	for _, component := range parts[1:] {
		if !validSecureComponent(component) || strings.Contains(component, "..") {
			return nil, "", false
		}
	}
	return parts[1:3], parts[3], true
}

func modelIndexParts(namespace []string) []string {
	return []string{".indexes", "modelstate", hex.EncodeToString([]byte(namespace[0])), hex.EncodeToString([]byte(namespace[1])), "nodes"}
}

func modelStateNamespaceProcessLock(rootPath string, namespace []string) *sync.Mutex {
	return fileModelStateNamespaceLock(filepath.Join(append([]string{rootPath}, modelIndexParts(namespace)...)...))
}

func regularFileStatAt(dir *os.File, name string) (*unix.Stat_t, error) {
	if !validSecureComponent(name) {
		return nil, errUnsafeFile
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return nil, errUnsafeFile
	}
	return &stat, nil
}

func readRegularFileAt(ctx context.Context, dir *os.File, name string, maxBytes int64) ([]byte, error) {
	stat, err := regularFileStatAt(dir, name)
	if err != nil {
		return nil, err
	}
	if maxBytes >= 0 && stat.Size > maxBytes {
		return nil, errFileTooLarge
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return nil, err
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Nlink != 1 || opened.Dev != stat.Dev || opened.Ino != stat.Ino {
		return nil, errUnsafeFile
	}
	capacity := int64(32 * 1024)
	if maxBytes >= 0 && maxBytes < capacity {
		capacity = maxBytes
	}
	data := make([]byte, 0, int(capacity))
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := file.Read(buffer)
		if maxBytes >= 0 && int64(len(data)+count) > maxBytes {
			return nil, errFileTooLarge
		}
		data = append(data, buffer[:count]...)
		if errors.Is(readErr, io.EOF) {
			return data, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func validateReplaceTarget(dir *os.File, name string) error {
	_, err := regularFileStatAt(dir, name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

func writeRegularFileAtomicAt(dir *os.File, name string, data []byte, mode fs.FileMode) error {
	if err := validateReplaceTarget(dir, name); err != nil {
		return err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".tmp-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(dir.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), tmp)
	cleanup := func() {
		_ = file.Close()
		_ = unix.Unlinkat(int(dir.Fd()), tmp, 0)
	}
	if _, err := file.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		_ = unix.Unlinkat(int(dir.Fd()), tmp, 0)
		return err
	}
	if err := unix.Renameat(int(dir.Fd()), tmp, int(dir.Fd()), name); err != nil {
		_ = unix.Unlinkat(int(dir.Fd()), tmp, 0)
		return err
	}
	return unix.Fsync(int(dir.Fd()))
}

func readSecureModelStateBlob(ctx context.Context, rootPath, name string, maxBytes int64) ([]byte, error) {
	namespace, key, ok := parseModelStateName(name)
	if !ok {
		return nil, ErrInvalidModelKey
	}
	root, err := openSecureFileRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.close()
	dir, err := root.openDir(append([]string{ModelStateNamespace}, namespace...), false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return readRegularFileAt(ctx, dir, key+".json", maxBytes)
}

func openModelStateDirs(rootPath string, namespace []string, create bool) (*secureFileRoot, *os.File, *os.File, error) {
	root, err := openSecureFileRoot(rootPath)
	if err != nil {
		return nil, nil, nil, err
	}
	artifacts, err := root.openDir(append([]string{ModelStateNamespace}, namespace...), create)
	if err != nil {
		root.close()
		return nil, nil, nil, err
	}
	nodes, err := root.openDir(modelIndexParts(namespace), create)
	if err != nil {
		artifacts.Close()
		root.close()
		return nil, nil, nil, err
	}
	return root, artifacts, nodes, nil
}

func closeModelStateDirs(root *secureFileRoot, artifacts, nodes *os.File) {
	if nodes != nil {
		_ = nodes.Close()
	}
	if artifacts != nil {
		_ = artifacts.Close()
	}
	_ = root.close()
}

func lockModelStateNamespace(ctx context.Context, nodes *os.File) (*os.File, error) {
	const lockName = ".namespace.lock"
	fd, err := unix.Openat(int(nodes.Fd()), lockName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), lockName)
	closeLock := func(err error) (*os.File, error) {
		_ = lock.Close()
		return nil, err
	}
	stat, err := regularFileStatAt(nodes, lockName)
	if err != nil {
		return closeLock(err)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return closeLock(err)
	}
	if opened.Dev != stat.Dev || opened.Ino != stat.Ino {
		return closeLock(errUnsafeFile)
	}
	for {
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return lock, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) {
			return closeLock(err)
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return closeLock(ctx.Err())
		case <-timer.C:
		}
	}
}

func unlockModelStateNamespace(lock *os.File) {
	if lock == nil {
		return
	}
	_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	_ = lock.Close()
}

const modelStatePendingName = ".pending.json"

type modelStatePendingOperation struct {
	Key    string `json:"key"`
	Delete bool   `json:"delete,omitempty"`
}

func writeModelStatePendingAt(nodes *os.File, key string, deleteArtifact bool) error {
	data, err := json.Marshal(modelStatePendingOperation{Key: key, Delete: deleteArtifact})
	if err != nil {
		return err
	}
	return writeRegularFileAtomicAt(nodes, modelStatePendingName, data, 0o600)
}

func clearModelStatePendingAt(nodes *os.File) error {
	if err := unix.Unlinkat(int(nodes.Fd()), modelStatePendingName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(int(nodes.Fd()))
}

func repairModelStatePendingAt(artifacts, nodes *os.File) error {
	data, err := readRegularFileAt(context.Background(), nodes, modelStatePendingName, 1024)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var pending modelStatePendingOperation
	if err := json.Unmarshal(data, &pending); err != nil {
		return err
	}
	if !validSecureComponent(pending.Key) || strings.Contains(pending.Key, "..") || len(pending.Key) > 255 {
		return ErrInvalidModelKey
	}
	artifactName := pending.Key + ".json"
	if pending.Delete {
		if _, err := readTrieNodeAt(nodes, []byte(pending.Key)); err == nil {
			if err := clearTrieKeyAt(nodes, []byte(pending.Key)); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := unix.Unlinkat(int(artifacts.Fd()), artifactName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		if err := unix.Fsync(int(artifacts.Fd())); err != nil {
			return err
		}
	} else if _, err := regularFileStatAt(artifacts, artifactName); err == nil {
		if err := writeTrieKeyAt(nodes, []byte(pending.Key)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return clearModelStatePendingAt(nodes)
}

func trieNodeName(prefix []byte) string {
	if len(prefix) == 0 {
		return "root.idx"
	}
	return hex.EncodeToString(prefix) + ".idx"
}

func readTrieNodeAt(nodes *os.File, prefix []byte) ([]byte, error) {
	data, err := readRegularFileAt(context.Background(), nodes, trieNodeName(prefix), fileTrieNodeBytes)
	if err != nil {
		return nil, err
	}
	if len(data) != fileTrieNodeBytes {
		return nil, fmt.Errorf("invalid trie node size %d", len(data))
	}
	return data, nil
}

func prepareModelIndexAt(artifacts, nodes *os.File) error {
	if _, err := readTrieNodeAt(nodes, nil); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: inspect model index: %w", err)
	}
	entries, err := artifacts.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("storage: inspect model namespace: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("storage: legacy model namespace has no bounded index: %w", ErrUnsupported)
	}
	return writeRegularFileAtomicAt(nodes, trieNodeName(nil), make([]byte, fileTrieNodeBytes), 0o644)
}

func writeTrieKeyAt(nodes *os.File, key []byte) error {
	for depth := 0; depth <= len(key); depth++ {
		node, err := readTrieNodeAt(nodes, key[:depth])
		if errors.Is(err, os.ErrNotExist) {
			node = make([]byte, fileTrieNodeBytes)
		} else if err != nil {
			return err
		}
		changed := false
		if depth == len(key) {
			if node[0] == 0 {
				node[0] = 1
				changed = true
			}
		} else {
			child := key[depth]
			index := 1 + int(child)/8
			mask := byte(1 << (child % 8))
			if node[index]&mask == 0 {
				node[index] |= mask
				changed = true
			}
		}
		if !changed {
			continue
		}
		if err := writeRegularFileAtomicAt(nodes, trieNodeName(key[:depth]), node, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func clearTrieKeyAt(nodes *os.File, key []byte) error {
	node, err := readTrieNodeAt(nodes, key)
	if err != nil {
		return err
	}
	node[0] = 0
	return writeRegularFileAtomicAt(nodes, trieNodeName(key), node, 0o644)
}

func writeSecureModelStateBlob(rootPath, name string, data []byte, indexed bool) error {
	namespace, key, ok := parseModelStateName(name)
	if !ok || len(key) > 255 {
		return ErrInvalidModelKey
	}
	if !indexed {
		root, err := openSecureFileRoot(rootPath)
		if err != nil {
			return err
		}
		defer root.close()
		dir, err := root.openDir(append([]string{ModelStateNamespace}, namespace...), true)
		if err != nil {
			return err
		}
		defer dir.Close()
		return writeRegularFileAtomicAt(dir, key+".json", data, 0o644)
	}
	processLock := modelStateNamespaceProcessLock(rootPath, namespace)
	processLock.Lock()
	defer processLock.Unlock()
	root, artifacts, nodes, err := openModelStateDirs(rootPath, namespace, true)
	if err != nil {
		return err
	}
	defer closeModelStateDirs(root, artifacts, nodes)
	lock, err := lockModelStateNamespace(context.Background(), nodes)
	if err != nil {
		return err
	}
	defer unlockModelStateNamespace(lock)
	if err := prepareModelIndexAt(artifacts, nodes); err != nil {
		return err
	}
	if err := repairModelStatePendingAt(artifacts, nodes); err != nil {
		return err
	}
	previous, previousErr := readRegularFileAt(context.Background(), artifacts, key+".json", -1)
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		return previousErr
	}
	if err := writeModelStatePendingAt(nodes, key, false); err != nil {
		return err
	}
	if err := writeRegularFileAtomicAt(artifacts, key+".json", data, 0o644); err != nil {
		_ = clearModelStatePendingAt(nodes)
		return err
	}
	// Under the namespace lock, artifact bytes exist before their trie terminal is published.
	if err := writeTrieKeyAt(nodes, []byte(key)); err != nil {
		var rollbackErr error
		if previousErr == nil {
			rollbackErr = writeRegularFileAtomicAt(artifacts, key+".json", previous, 0o644)
		} else {
			rollbackErr = unix.Unlinkat(int(artifacts.Fd()), key+".json", 0)
			if rollbackErr == nil {
				rollbackErr = unix.Fsync(int(artifacts.Fd()))
			}
		}
		if rollbackErr == nil {
			rollbackErr = clearModelStatePendingAt(nodes)
		}
		return errors.Join(err, rollbackErr)
	}
	return clearModelStatePendingAt(nodes)
}

func deleteSecureModelStateBlob(rootPath, name string) error {
	namespace, key, ok := parseModelStateName(name)
	if !ok {
		return ErrInvalidModelKey
	}
	processLock := modelStateNamespaceProcessLock(rootPath, namespace)
	processLock.Lock()
	defer processLock.Unlock()
	root, artifacts, nodes, err := openModelStateDirs(rootPath, namespace, false)
	if err != nil {
		return err
	}
	defer closeModelStateDirs(root, artifacts, nodes)
	lock, err := lockModelStateNamespace(context.Background(), nodes)
	if err != nil {
		return err
	}
	defer unlockModelStateNamespace(lock)
	if _, err := readTrieNodeAt(nodes, nil); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrUnsupported
		}
		return err
	}
	if err := repairModelStatePendingAt(artifacts, nodes); err != nil {
		return err
	}
	if _, err := regularFileStatAt(artifacts, key+".json"); err != nil {
		return err
	}
	if err := writeModelStatePendingAt(nodes, key, true); err != nil {
		return err
	}
	if err := clearTrieKeyAt(nodes, []byte(key)); err != nil {
		_ = clearModelStatePendingAt(nodes)
		return err
	}
	if err := unix.Unlinkat(int(artifacts.Fd()), key+".json", 0); err != nil {
		restoreErr := writeTrieKeyAt(nodes, []byte(key))
		if restoreErr == nil {
			restoreErr = clearModelStatePendingAt(nodes)
		}
		return errors.Join(err, restoreErr)
	}
	if err := unix.Fsync(int(artifacts.Fd())); err != nil {
		return err
	}
	return clearModelStatePendingAt(nodes)
}

func compareAndSwapSecureModelStateBlob(rootPath, name string, expected, replacement []byte) (bool, error) {
	namespace, key, ok := parseModelStateName(name)
	if !ok || len(key) > 255 {
		return false, ErrInvalidModelKey
	}
	processLock := modelStateNamespaceProcessLock(rootPath, namespace)
	processLock.Lock()
	defer processLock.Unlock()
	create := replacement != nil
	root, artifacts, nodes, err := openModelStateDirs(rootPath, namespace, create)
	if errors.Is(err, os.ErrNotExist) {
		if expected != nil || replacement == nil {
			return false, nil
		}
	}
	if err != nil {
		return false, err
	}
	defer closeModelStateDirs(root, artifacts, nodes)
	lock, err := lockModelStateNamespace(context.Background(), nodes)
	if err != nil {
		return false, err
	}
	defer unlockModelStateNamespace(lock)
	if replacement != nil {
		if err := prepareModelIndexAt(artifacts, nodes); err != nil {
			return false, err
		}
	}
	if err := repairModelStatePendingAt(artifacts, nodes); err != nil {
		return false, err
	}
	current, err := readRegularFileAt(context.Background(), artifacts, key+".json", -1)
	if errors.Is(err, os.ErrNotExist) {
		if expected != nil {
			return false, nil
		}
		current = nil
	} else if err != nil {
		return false, err
	} else if expected == nil {
		return false, nil
	}
	if expected != nil && !bytes.Equal(current, expected) {
		return false, nil
	}
	if replacement == nil {
		if expected == nil {
			return false, nil
		}
		if _, err := readTrieNodeAt(nodes, nil); err != nil {
			return false, ErrUnsupported
		}
		if err := writeModelStatePendingAt(nodes, key, true); err != nil {
			return false, err
		}
		if err := clearTrieKeyAt(nodes, []byte(key)); err != nil {
			_ = clearModelStatePendingAt(nodes)
			return false, err
		}
		if err := unix.Unlinkat(int(artifacts.Fd()), key+".json", 0); err != nil {
			restoreErr := writeTrieKeyAt(nodes, []byte(key))
			if restoreErr == nil {
				restoreErr = clearModelStatePendingAt(nodes)
			}
			return false, errors.Join(err, restoreErr)
		}
		if err := unix.Fsync(int(artifacts.Fd())); err != nil {
			return false, err
		}
		return true, clearModelStatePendingAt(nodes)
	}
	if err := writeModelStatePendingAt(nodes, key, false); err != nil {
		return false, err
	}
	if err := writeRegularFileAtomicAt(artifacts, key+".json", replacement, 0o644); err != nil {
		_ = clearModelStatePendingAt(nodes)
		return false, err
	}
	if err := writeTrieKeyAt(nodes, []byte(key)); err != nil {
		var rollbackErr error
		if current == nil {
			rollbackErr = unix.Unlinkat(int(artifacts.Fd()), key+".json", 0)
			if rollbackErr == nil {
				rollbackErr = unix.Fsync(int(artifacts.Fd()))
			}
		} else {
			rollbackErr = writeRegularFileAtomicAt(artifacts, key+".json", current, 0o644)
		}
		if rollbackErr == nil {
			rollbackErr = clearModelStatePendingAt(nodes)
		}
		return false, errors.Join(err, rollbackErr)
	}
	return true, clearModelStatePendingAt(nodes)
}

func listSecureModelStateBlobs(rootPath, prefix string) ([]Blob, error) {
	root, err := openSecureFileRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.close()
	models, err := root.openDir([]string{ModelStateNamespace}, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer models.Close()
	var out []Blob
	if err := walkSecureModelDir(models, ModelStateNamespace, prefix, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func walkSecureModelDir(dir *os.File, logicalDir, prefix string, out *[]Blob) error {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !validSecureComponent(name) {
			return errUnsafeFile
		}
		fd, openErr := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return openErr
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(fd, &stat); statErr != nil {
			unix.Close(fd)
			return statErr
		}
		logical := logicalDir + "/" + name
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			child := os.NewFile(uintptr(fd), name)
			err = walkSecureModelDir(child, logical, prefix, out)
			_ = child.Close()
			if err != nil {
				return err
			}
		case unix.S_IFREG:
			unix.Close(fd)
			if stat.Nlink != 1 || !strings.HasSuffix(name, ".json") {
				return errUnsafeFile
			}
			blobName := strings.TrimSuffix(logical, ".json")
			if !strings.HasPrefix(blobName, prefix) {
				continue
			}
			data, readErr := readRegularFileAt(context.Background(), dir, name, -1)
			if readErr != nil {
				return readErr
			}
			*out = append(*out, Blob{Name: blobName, Data: data})
		default:
			unix.Close(fd)
			return errUnsafeFile
		}
	}
	return nil
}

func listSecureModelStatePage(ctx context.Context, rootPath, prefix, cursor string, limit int) (BlobPage, error) {
	if err := ctx.Err(); err != nil {
		return BlobPage{}, err
	}
	if limit <= 0 {
		return BlobPage{Done: true}, nil
	}
	if limit > filePageMaxLimit {
		limit = filePageMaxLimit
	}
	name := strings.TrimSuffix(prefix, "/") + "/x"
	namespace, _, ok := parseModelStateName(name)
	if !ok || !strings.HasSuffix(prefix, "/") {
		return BlobPage{}, ErrUnsupported
	}
	if cursor != "" && !strings.HasPrefix(cursor, prefix) {
		return BlobPage{}, ErrInvalidModelKey
	}
	processLock := modelStateNamespaceProcessLock(rootPath, namespace)
	processLock.Lock()
	defer processLock.Unlock()
	root, err := openSecureFileRoot(rootPath)
	if err != nil {
		return BlobPage{}, err
	}
	defer root.close()
	artifacts, artifactErr := root.openDir(append([]string{ModelStateNamespace}, namespace...), false)
	if artifactErr != nil && !errors.Is(artifactErr, os.ErrNotExist) {
		return BlobPage{}, artifactErr
	}
	if artifacts != nil {
		defer artifacts.Close()
	}
	nodes, nodeErr := root.openDir(modelIndexParts(namespace), false)
	if nodeErr != nil && !errors.Is(nodeErr, os.ErrNotExist) {
		return BlobPage{}, nodeErr
	}
	if nodes != nil {
		defer nodes.Close()
	}
	if nodes == nil {
		if artifacts == nil {
			return BlobPage{Done: true}, nil
		}
		entries, readErr := artifacts.ReadDir(1)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return BlobPage{}, fmt.Errorf("storage: inspect blob namespace %q: %w", prefix, readErr)
		}
		if len(entries) == 0 {
			return BlobPage{Done: true}, nil
		}
		return BlobPage{}, fmt.Errorf("storage: page blob names %q: legacy namespace has no bounded index: %w", prefix, ErrUnsupported)
	}
	if artifacts == nil {
		return BlobPage{}, fmt.Errorf("storage: page blob names %q: index has no namespace: %w", prefix, ErrUnsupported)
	}
	lock, err := lockModelStateNamespace(ctx, nodes)
	if err != nil {
		return BlobPage{}, err
	}
	defer unlockModelStateNamespace(lock)
	if _, err := readTrieNodeAt(nodes, nil); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return BlobPage{}, fmt.Errorf("storage: read blob index %q: %w", prefix, err)
		}
		entries, readErr := artifacts.ReadDir(1)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return BlobPage{}, fmt.Errorf("storage: inspect blob namespace %q: %w", prefix, readErr)
		}
		if len(entries) == 0 {
			return BlobPage{Done: true}, nil
		}
		return BlobPage{}, fmt.Errorf("storage: page blob names %q: legacy namespace has no bounded index: %w", prefix, ErrUnsupported)
	}
	if err := repairModelStatePendingAt(artifacts, nodes); err != nil {
		return BlobPage{}, fmt.Errorf("storage: repair blob index %q: %w", prefix, err)
	}
	after := strings.TrimPrefix(cursor, prefix)
	scanLimit := limit*2 + 8
	out := make([]Blob, 0, limit)
	next := cursor
	var bytesRead int64
	scanned := 0
	done := false
	for len(out) < limit && scanned < scanLimit {
		if err := ctx.Err(); err != nil {
			return BlobPage{}, err
		}
		key, found, err := nextTrieKeyAt(ctx, nodes, []byte(after))
		if err != nil {
			return BlobPage{}, fmt.Errorf("storage: page blob index %q: %w", prefix, err)
		}
		if !found {
			done = true
			break
		}
		after = string(key)
		name := prefix + after
		next = name
		scanned++
		data, err := readRegularFileAt(ctx, artifacts, after+".json", filePageMaxBlobBytes)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errFileTooLarge) {
			continue
		}
		if err != nil {
			return BlobPage{}, fmt.Errorf("storage: read blob %s: %w", name, err)
		}
		if bytesRead+int64(len(data)) > filePageMaxBytes {
			continue
		}
		bytesRead += int64(len(data))
		out = append(out, Blob{Name: name, Data: data})
	}
	return BlobPage{Blobs: out, Next: next, Scanned: scanned, Bytes: bytesRead, Done: done}, nil
}

func nextTrieKeyAt(ctx context.Context, nodes *os.File, after []byte) ([]byte, bool, error) {
	return seekTrieKeyAt(ctx, nodes, nil, after, 0)
}

func seekTrieKeyAt(ctx context.Context, nodes *os.File, prefix, after []byte, depth int) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	node, err := readTrieNodeAt(nodes, prefix)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if depth > len(after) && node[0] == 1 {
		return append([]byte(nil), prefix...), true, nil
	}
	start := 0
	if depth < len(after) {
		start = int(after[depth])
	}
	for child := start; child < 256; child++ {
		if node[1+child/8]&(1<<byte(child%8)) == 0 {
			continue
		}
		candidate := append(append([]byte(nil), prefix...), byte(child))
		if depth < len(after) && child == int(after[depth]) {
			if found, ok, err := seekTrieKeyAt(ctx, nodes, candidate, after, depth+1); err != nil || ok {
				return found, ok, err
			}
			continue
		}
		if found, ok, err := firstTrieKeyAt(ctx, nodes, candidate); err != nil || ok {
			return found, ok, err
		}
	}
	return nil, false, nil
}

func firstTrieKeyAt(ctx context.Context, nodes *os.File, prefix []byte) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	node, err := readTrieNodeAt(nodes, prefix)
	if err != nil {
		return nil, false, err
	}
	if node[0] == 1 {
		return append([]byte(nil), prefix...), true, nil
	}
	for child := 0; child < 256; child++ {
		if node[1+child/8]&(1<<byte(child%8)) != 0 {
			found, ok, err := firstTrieKeyAt(ctx, nodes, append(append([]byte(nil), prefix...), byte(child)))
			if err != nil || ok {
				return found, ok, err
			}
		}
	}
	return nil, false, nil
}
