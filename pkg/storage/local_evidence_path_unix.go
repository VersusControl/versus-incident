//go:build darwin || linux

package storage

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func validateLocalEvidenceArtifact(path string) error {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return errUnsafeFile
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errUnsafeFile
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err = unix.Fstat(fd, &opened); err != nil {
		return err
	}
	if opened.Dev != stat.Dev || opened.Ino != stat.Ino || opened.Nlink != 1 || opened.Uid != uint32(os.Geteuid()) || opened.Mode&unix.S_IFMT != unix.S_IFREG {
		return errUnsafeFile
	}
	if err = unix.Fchmod(fd, 0600); err != nil {
		return err
	}
	return nil
}

func prepareLocalEvidenceArtifact(path string) error {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return validateLocalEvidenceArtifact(path)
}

func openLegacyEvidenceDirectory(rootPath string) (*os.File, error) {
	root, err := openSecureFileRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.close()
	parent := int(root.file.Fd())
	var opened []int
	defer func() {
		for _, fd := range opened {
			unix.Close(fd)
		}
	}()
	for _, name := range []string{"kubernetes", "captured-evidence"} {
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		opened = append(opened, fd)
		parent = fd
	}
	fd, err := unix.Dup(parent)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "captured-evidence"), nil
}

func listLegacyLocalEvidence(root string) ([]os.DirEntry, error) {
	dir, err := openLegacyEvidenceDirectory(root)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadDir(-1)
}

func readLegacyLocalEvidence(root, key string) ([]byte, error) {
	dir, err := openLegacyEvidenceDirectory(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	name := strings.TrimPrefix(key, localEvidencePrefix) + ".json"
	if filepath.Base(name) != name {
		return nil, errUnsafeFile
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return nil, errUnsafeFile
	}
	return io.ReadAll(file)
}
