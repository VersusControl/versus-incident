package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/VersusControl/versus-incident/pkg/utils"
)

// MaxIncidentsDefault is the default rolling cap for the file backend.
// Older records are dropped on SaveIncident when the cap is exceeded.
const MaxIncidentsDefault = 1000

// FileOptions configures the file backend. Empty fields fall back to
// sensible defaults.
type FileOptions struct {
	DataDir      string // default DefaultDataDir
	MaxIncidents int    // default MaxIncidentsDefault
}

// fileProvider stores blobs as <DataDir>/<name>.json and incidents as
// a single <DataDir>/incidents.json file kept in memory for fast list
// queries. Kubernetes evidence uses a private transactional SQLite image/index;
// other blob writes remain atomic (tmp + rename).
type fileProvider struct {
	*sqlTables
	evidenceMu    sync.Mutex
	localEvidence *localEvidence
	dir           string
	root          *os.Root
	maxIncidents  int

	mu        sync.RWMutex
	incidents []*IncidentRecord // newest last; persisted as is
	loaded    bool

	analysesMu     sync.RWMutex
	analyses       []*AnalysisRecord // newest last
	analysesLoaded bool
}

const fileBlobLockShardCount = 256

const (
	filePageMaxLimit     = 1000
	filePageMaxBlobBytes = 1 << 20
	filePageMaxBytes     = 16 << 20
	fileTrieNodeBytes    = 33
)

var (
	fileBlobLocks                [fileBlobLockShardCount]sync.Mutex
	fileModelStateNamespaceLocks [fileBlobLockShardCount]sync.Mutex
)

func fileBlobLock(path string) *sync.Mutex {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(path))
	return &fileBlobLocks[hash.Sum32()%fileBlobLockShardCount]
}

func fileModelStateNamespaceLock(path string) *sync.Mutex {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(path))
	return &fileModelStateNamespaceLocks[hash.Sum32()%fileBlobLockShardCount]
}

// NewFile returns a Provider backed by the local filesystem.
func NewFile(opts FileOptions) (Provider, error) {
	dir := opts.DataDir
	if dir == "" {
		dir = DefaultDataDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: mkdir %s: %w", dir, err)
	}
	if err := validateSecureStorageRoot(dir); err != nil {
		return nil, fmt.Errorf("storage: validate data directory %s: %w", dir, err)
	}
	// The path to the data directory may pass through links, as macOS /tmp
	// and /var and Kubernetes volume mounts (..data) do. It is resolved once
	// here, and every blob is reached through a handle on the resolved directory.
	resolved, err := filepath.Abs(dir)
	if err == nil {
		resolved, err = filepath.EvalSymlinks(resolved)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: resolve data directory %s: %w", dir, err)
	}
	dir = resolved
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("storage: open data directory %s: %w", dir, err)
	}
	max := opts.MaxIncidents
	if max <= 0 {
		max = MaxIncidentsDefault
	}
	p := &fileProvider{sqlTables: newSQLiteTables(filepath.Join(dir, localTablesFile)), dir: dir, root: root, maxIncidents: max}
	if _, err := os.Stat(filepath.Join(dir, localTablesFile)); err == nil {
		// The tables are opened lazily, so only an existing database is
		// checked here; a fresh deployment creates it with the current schema.
		if err := p.checkTablesSchema(); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	if err := p.loadIncidents(); err != nil {
		return nil, err
	}
	if err := p.loadAnalyses(); err != nil {
		return nil, err
	}
	_, dbErr := os.Lstat(filepath.Join(dir, "kubernetes-evidence.sqlite"))
	_, legacyErr := os.Stat(filepath.Join(dir, "kubernetes", "captured-evidence"))
	if localEvidenceDriverRegistered() && (dbErr == nil || legacyErr == nil) {
		if _, err := p.evidenceIndex(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// checkTablesSchema opens an existing table database and asserts its schema,
// so a file written by an older version fails closed at startup instead of
// starting a store whose writes fail later with an unclassified SQL error.
func (p *fileProvider) checkTablesSchema() error {
	db, err := p.tablesDB()
	if err != nil {
		return err
	}
	return assertTableSchema(context.Background(), db, false)
}

// ---------------------------------------------------------------------------
// Blobs
// ---------------------------------------------------------------------------

// ErrInvalidBlobName is returned for a blob name that would not map to a file
// inside the data directory.
var ErrInvalidBlobName = errors.New("storage: invalid blob name")

// errBlobLink is a symbolic link, or another reparse point, where a blob file
// should be.
var errBlobLink = errors.New("storage: blob path is a link")

// Every blob file is reached through p.root, a handle on the resolved data
// directory: the kernel resolves each name inside it and refuses any path
// that leaves it, also when a directory inside is swapped for a link while a
// call runs. A link that stays inside is followed; a link at the blob file
// itself is refused, so a blob is always a file the store wrote.

// blobRel is the blob file of name relative to the data directory. The name
// rules of blobPath apply.
func (p *fileProvider) blobRel(name string) (string, error) {
	target, err := p.blobPath(name)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(p.dir, target)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidBlobName, name)
	}
	return rel, nil
}

// blobError reports a path the data directory handle refused, because it
// leaves the directory or loops, or a link at the blob file, as
// ErrInvalidBlobName; other errors pass through.
func blobError(name string, err error) error {
	if errors.Is(err, errBlobLink) || errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("%w: %q", ErrInvalidBlobName, name)
	}
	// os.Root does not export its escape error; it is the innermost cause.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if cause.Error() == "path escapes from parent" {
			return fmt.Errorf("%w: %q", ErrInvalidBlobName, name)
		}
	}
	return err
}

// refuseBlobLink answers errBlobLink when a link sits at rel. A missing file
// is not an error.
func (p *fileProvider) refuseBlobLink(rel string) error {
	info, err := p.root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if blobLinkMode(info.Mode()) {
		return errBlobLink
	}
	return nil
}

// readBlobRel reads the blob file at rel. A missing file, or a missing
// directory on the way, reads as fs.ErrNotExist.
func (p *fileProvider) readBlobRel(rel string) ([]byte, error) {
	if err := p.refuseBlobLink(rel); err != nil {
		return nil, err
	}
	f, err := p.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// writeBlobRel writes data to the blob file at rel atomically. Missing
// directories are made inside the data directory. The data goes to a new
// temp file under a fresh random name next to the blob, created exclusively
// so nothing already there (a link of any kind) is opened, is synced, and is
// renamed over the blob; on failure the temp file is removed.
func (p *fileProvider) writeBlobRel(rel string, data []byte, mode os.FileMode) error {
	if dir := filepath.Dir(rel); dir != "." {
		if err := p.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := p.refuseBlobLink(rel); err != nil {
		return err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+"."+hex.EncodeToString(random[:])+".tmp")
	f, err := p.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = p.root.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = p.root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = p.root.Remove(tmp)
		return err
	}
	if err := p.root.Rename(tmp, rel); err != nil {
		_ = p.root.Remove(tmp)
		return err
	}
	return nil
}

// Blob names are bounded before any path work, so a crafted name costs
// nothing. The longest names the stores write are under 200 bytes and 8
// path elements: the alert record
// kubernetes/agent-settings/<64 hex>.json.alert-window.<64 hex> (173 bytes),
// the inventory data kubernetes/inventory/<id>/data/<owner>/<slot> (6
// elements), ledger/<org>/runs/<run>/entries/<seq>.json (6 elements), each
// with up to two more elements and the org ID when Enterprise prefixes
// org/<org>/.
const (
	maxBlobNameBytes    = 1024
	maxBlobNameElements = 16
)

// blobPath maps a blob name to its file under the data directory. Names may
// nest with "/" (kubernetes/inventory/<key>), but a name that is empty, too
// long or too deep, absolute, holds a NUL or a ".." element, or otherwise
// resolves outside the data directory is refused, so no name can reach a
// file the store does not own.
func (p *fileProvider) blobPath(name string) (string, error) {
	elements := strings.Count(name, "/") + strings.Count(name, `\`) + 1
	if len(name) > maxBlobNameBytes || elements > maxBlobNameElements {
		return "", fmt.Errorf("%w: %d bytes in %d elements", ErrInvalidBlobName, len(name), elements)
	}
	if name == "" || strings.ContainsRune(name, 0) || filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidBlobName, name)
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "", fmt.Errorf("%w: %q", ErrInvalidBlobName, name)
		}
	}
	target := filepath.Join(p.dir, name+".json")
	rel, err := filepath.Rel(p.dir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q", ErrInvalidBlobName, name)
	}
	return target, nil
}

func (p *fileProvider) ReadBlob(name string) ([]byte, error) {
	if isLocalEvidenceKey(name) {
		index, err := p.evidenceIndex()
		if err != nil {
			return nil, err
		}
		return index.raw(context.Background(), name)
	}
	if _, _, ok := parseModelStateName(name); ok {
		data, err := readSecureModelStateBlob(context.Background(), p.dir, name, -1)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("storage: read blob %s: %w", name, err)
		}
		return data, nil
	}
	rel, err := p.blobRel(name)
	if err != nil {
		return nil, err
	}
	data, err := p.readBlobRel(rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		if err = blobError(name, err); errors.Is(err, ErrInvalidBlobName) {
			return nil, err
		}
		return nil, fmt.Errorf("storage: read blob %s: %w", name, err)
	}
	return data, nil
}

func (p *fileProvider) WriteBlob(name string, data []byte) error {
	if isLocalEvidenceKey(name) {
		index, err := p.evidenceIndex()
		if err != nil {
			return err
		}
		path, err := p.blobPath(name)
		if err != nil {
			return err
		}
		lock := fileBlobLock(path)
		lock.Lock()
		defer lock.Unlock()
		_, err = index.swap(context.Background(), name, nil, data, true)
		return localEvidenceError(err)
	}
	if _, _, ok := parseModelStateName(name); ok {
		if err := writeSecureModelStateBlob(p.dir, name, data, true); err != nil {
			return fmt.Errorf("storage: write blob %s: %w", name, err)
		}
		return nil
	}
	rel, err := p.blobRel(name)
	if err != nil {
		return err
	}
	if err := p.writeBlobRel(rel, data, 0o644); err != nil {
		if err = blobError(name, err); errors.Is(err, ErrInvalidBlobName) {
			return err
		}
		return fmt.Errorf("storage: write blob %s: %w", name, err)
	}
	return nil
}

func (p *fileProvider) WriteModelStateBlob(name string, data []byte) error {
	_, key, indexed := parseModelStateName(name)
	if !indexed || len(key) > 255 {
		return ErrInvalidModelKey
	}
	lock := fileBlobLock(filepath.Join(p.dir, ".indexes", name))
	lock.Lock()
	defer lock.Unlock()
	if err := writeSecureModelStateBlob(p.dir, name, data, true); err != nil {
		return fmt.Errorf("storage: write blob %s: %w", name, err)
	}
	return nil
}

func (p *fileProvider) DeleteBlob(name string) error {
	_, key, indexed := parseModelStateName(name)
	if !indexed || len(key) > 255 {
		return ErrUnsupported
	}
	lock := fileBlobLock(filepath.Join(p.dir, ".indexes", name))
	lock.Lock()
	defer lock.Unlock()
	if err := deleteSecureModelStateBlob(p.dir, name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("storage: remove blob %s: %w", name, err)
	}
	return nil
}

// CreateBlobIfAbsent implements the optional storage.BlobCreator capability
// on the file backend. It is best-effort and coherent only on a
// single node — the kernel's O_CREATE|O_EXCL guarantees exactly one creator
// among concurrent openers on the same filesystem, which is the only place
// file storage is allowed under HA (the guard forces Postgres for
// count>1). An existing blob (created here or via WriteBlob) is left
// untouched and the call returns written==false so the caller re-reads the
// stored bytes via ReadBlob.
func (p *fileProvider) CreateBlobIfAbsent(name string, data []byte) (bool, error) {
	if isLocalEvidenceKey(name) {
		index, err := p.evidenceIndex()
		if err != nil {
			return false, err
		}
		path, err := p.blobPath(name)
		if err != nil {
			return false, err
		}
		lock := fileBlobLock(path)
		lock.Lock()
		defer lock.Unlock()
		return index.swap(context.Background(), name, nil, data, false)
	}
	if _, _, ok := parseModelStateName(name); ok {
		lock := fileBlobLock(filepath.Join(p.dir, ".indexes", name))
		lock.Lock()
		defer lock.Unlock()
		written, err := compareAndSwapSecureModelStateBlob(p.dir, name, nil, data)
		if err != nil {
			return false, fmt.Errorf("storage: create blob %s: %w", name, err)
		}
		return written, nil
	}
	target, err := p.blobRel(name)
	if err != nil {
		return false, err
	}
	if dir := filepath.Dir(target); dir != "." {
		if err := p.root.MkdirAll(dir, 0o755); err != nil {
			if err = blobError(name, err); errors.Is(err, ErrInvalidBlobName) {
				return false, err
			}
			return false, fmt.Errorf("storage: mkdir blob dir: %w", err)
		}
	}
	// O_EXCL never opens anything already at the name, a link included.
	f, err := p.root.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		if err := p.refuseBlobLink(target); err != nil {
			return false, blobError(name, err)
		}
		return false, nil // key already present — caller re-reads via ReadBlob
	}
	if err != nil {
		if err = blobError(name, err); errors.Is(err, ErrInvalidBlobName) {
			return false, err
		}
		return false, fmt.Errorf("storage: create blob %s: %w", name, err)
	}
	// We are the sole creator. Write + fsync so the bytes survive a crash,
	// mirroring writeFileAtomicSync's durability. On any failure, remove the
	// file we created so a later create can retry cleanly.
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = p.root.Remove(target)
		return false, fmt.Errorf("storage: create blob %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = p.root.Remove(target)
		return false, fmt.Errorf("storage: create blob %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		_ = p.root.Remove(target)
		return false, fmt.Errorf("storage: create blob %s: %w", name, err)
	}
	return true, nil
}

// CompareAndSwapBlob serializes updates by canonical blob path. File storage
// is restricted to one process in HA configurations, so this coordinates all
// provider instances that can address the same directory.
func (p *fileProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	if isLocalEvidenceKey(name) {
		index, err := p.evidenceIndex()
		if err != nil {
			return false, err
		}
		path, err := p.blobPath(name)
		if err != nil {
			return false, err
		}
		lock := fileBlobLock(path)
		lock.Lock()
		defer lock.Unlock()
		return index.swap(context.Background(), name, expected, replacement, false)
	}
	if _, _, ok := parseModelStateName(name); ok {
		lock := fileBlobLock(filepath.Join(p.dir, ".indexes", name))
		lock.Lock()
		defer lock.Unlock()
		swapped, err := compareAndSwapSecureModelStateBlob(p.dir, name, expected, replacement)
		if err != nil {
			return false, fmt.Errorf("storage: compare-and-swap blob %s: %w", name, err)
		}
		return swapped, nil
	}
	path, err := p.blobPath(name)
	if err != nil {
		return false, err
	}
	target, err := p.blobRel(name)
	if err != nil {
		return false, err
	}
	lock := fileBlobLock(path)
	lock.Lock()
	defer lock.Unlock()
	current, err := p.readBlobRel(target)
	if invalid := blobError(name, err); errors.Is(invalid, ErrInvalidBlobName) {
		return false, invalid
	}
	if errors.Is(err, os.ErrNotExist) {
		if expected != nil {
			return false, nil
		}
		current = nil
	} else if err != nil {
		return false, fmt.Errorf("storage: read blob %s for compare-and-swap: %w", name, err)
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
		if err := p.root.Remove(target); err != nil {
			if err = blobError(name, err); errors.Is(err, ErrInvalidBlobName) {
				return false, err
			}
			return false, fmt.Errorf("storage: remove blob %s: %w", name, err)
		}
		return true, nil
	}
	if err := p.writeBlobRel(target, replacement, 0o644); err != nil {
		if err = blobError(name, err); errors.Is(err, ErrInvalidBlobName) {
			return false, err
		}
		return false, fmt.Errorf("storage: compare-and-swap blob %s: %w", name, err)
	}
	return true, nil
}

// ListBlobs walks the data directory and returns every blob whose logical
// name (the <DataDir>-relative path minus the ".json" suffix) begins with
// prefix. The one-file-per-blob layout means a namespaced model-state name
// like models/<org>/<agent>/<key> maps to a nested directory, so the walk
// naturally enumerates a whole namespace. In-flight ".tmp" files from an
// atomic write are skipped (they don't carry the ".json" suffix). The walk
// and every read go through the data directory handle, so a directory
// swapped for a link during the walk is never followed out of it.
func (p *fileProvider) ListBlobs(prefix string) ([]Blob, error) {
	if prefix == ModelStateNamespace || strings.HasPrefix(prefix, ModelStateNamespace+"/") {
		out, err := listSecureModelStateBlobs(p.dir, prefix)
		if err != nil {
			return nil, fmt.Errorf("storage: list blobs %q: %w", prefix, err)
		}
		return out, nil
	}
	var out []Blob
	err := fs.WalkDir(p.root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that turned into a link leading out of the data
			// directory while the walk ran holds no blob of this store.
			if errors.Is(blobError(path, err), ErrInvalidBlobName) {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return err
		}
		// A link is not a blob this store wrote; it is never followed.
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !strings.HasSuffix(path, ".json") {
			return nil
		}
		rel := filepath.FromSlash(path)
		name := strings.TrimSuffix(path, ".json")
		if isLocalEvidenceKey(name) {
			return nil
		}
		if !strings.HasPrefix(name, prefix) {
			return nil
		}
		data, err := p.readBlobRel(rel)
		if errors.Is(blobError(name, err), ErrInvalidBlobName) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("storage: read blob %s: %w", name, err)
		}
		out = append(out, Blob{Name: name, Data: data})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("storage: list blobs %q: %w", prefix, err)
	}
	if localEvidenceDriverRegistered() && (prefix == "" || strings.HasPrefix(localEvidencePrefix, prefix) || strings.HasPrefix(prefix, localEvidencePrefix)) {
		index, err := p.evidenceIndex()
		if err != nil {
			return nil, err
		}
		blobs, err := index.blobs(context.Background(), prefix)
		if err != nil {
			return nil, err
		}
		out = append(out, blobs...)
	}
	return out, nil
}

func (p *fileProvider) ListBlobsPage(ctx context.Context, prefix, cursor string, limit int) ([]Blob, error) {
	page, err := p.ListBlobsPageBounded(ctx, prefix, cursor, limit)
	return page.Blobs, err
}

func (p *fileProvider) ListBlobsPageBounded(ctx context.Context, prefix, cursor string, limit int) (BlobPage, error) {
	return listSecureModelStatePage(ctx, p.dir, prefix, cursor, limit)
}

var (
	errUnsafeFile   = errors.New("storage: unsafe non-regular file")
	errFileTooLarge = errors.New("storage: file exceeds bounded read")
)

// writeFileAtomicSync writes data to a sibling tmp file, fsyncs it,
// then renames over the target. The fsync between write and rename is
// the load-bearing line: without it, a power loss between rename and
// fs commit can replace a previous good file with a zero-length one.
// (The rename itself is journaled by ext4/xfs; the tmp file's *data*
// isn't unless we sync.)
func writeFileAtomicSync(target string, data []byte, mode os.FileMode) error {
	// The temp file is new, under a fresh random name next to the target and
	// created exclusively, so nothing already there (a link of any kind) is
	// opened or truncated.
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// Cleanup the tmp file on any error path before rename.
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Incidents
// ---------------------------------------------------------------------------

const incidentsFile = "incidents.json"

const detectionEpisodesFile = "detection_episodes.json"

type detectionEpisodesFileSchema struct {
	Version  int                       `json:"version"`
	Episodes []*memoryDetectionEpisode `json:"episodes"`
	Active   map[string]string         `json:"active"`
}

func (p *fileProvider) loadDetectionEpisodesLocked(path string) (*memoryProvider, error) {
	state := &memoryProvider{
		episodes:          make(map[string]*memoryDetectionEpisode),
		activeEpisodes:    make(map[string]string),
		maxClosedEpisodes: p.maxIncidents,
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read detection episodes: %w", err)
	}
	var file detectionEpisodesFileSchema
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("storage: parse detection episodes: %w", err)
	}
	for _, episode := range file.Episodes {
		if episode != nil && episode.EpisodeID != "" {
			state.episodes[episode.EpisodeID] = episode
		}
	}
	for fingerprint, episodeID := range file.Active {
		state.activeEpisodes[fingerprint] = episodeID
	}
	return state, nil
}

func (p *fileProvider) persistDetectionEpisodesLocked(path string, state *memoryProvider) error {
	episodes := make([]*memoryDetectionEpisode, 0, len(state.episodes))
	for _, episode := range state.episodes {
		episodes = append(episodes, episode)
	}
	file := detectionEpisodesFileSchema{Version: 1, Episodes: episodes, Active: state.activeEpisodes}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("storage: marshal detection episodes: %w", err)
	}
	if err := writeFileAtomicSync(path, data, 0o644); err != nil {
		return fmt.Errorf("storage: write detection episodes: %w", err)
	}
	return nil
}

func (p *fileProvider) RecordDetectionOccurrence(occurrence DetectionOccurrence) (DetectionEpisodeDecision, error) {
	path := filepath.Join(p.dir, detectionEpisodesFile)
	lock := fileBlobLock(path)
	lock.Lock()
	defer lock.Unlock()
	state, err := p.loadDetectionEpisodesLocked(path)
	if err != nil {
		return DetectionEpisodeDecision{}, err
	}
	identity := normalizeDetectionIdentity(occurrence.Identity)
	fingerprint, _ := DetectionIdentityFingerprint(identity)
	if active := state.activeDetectionEpisode(fingerprint, identity); active != nil {
		if incident, err := p.GetIncident(active.IncidentID); err == nil {
			if incident.Resolved {
				closed := occurrence.ReceivedAt.UTC()
				if closed.IsZero() {
					closed = time.Now().UTC()
				}
				active.ClosedAt = &closed
				delete(state.activeEpisodes, active.Fingerprint)
			} else {
				state.incidents = append(state.incidents, incident)
			}
		} else if errors.Is(err, ErrNotFound) {
			if active.PendingToken == "" && detectionNotificationDelivered(active.LastNotificationOutcome) {
				closed := occurrence.ReceivedAt.UTC()
				if closed.IsZero() {
					closed = time.Now().UTC()
				}
				active.ClosedAt = &closed
				delete(state.activeEpisodes, active.Fingerprint)
			}
		} else {
			return DetectionEpisodeDecision{}, err
		}
	}
	decision, err := state.RecordDetectionOccurrence(occurrence)
	if err != nil {
		if occurrence.ExistingOnly && errors.Is(err, ErrNotFound) {
			if persistErr := p.persistDetectionEpisodesLocked(path, state); persistErr != nil {
				return DetectionEpisodeDecision{}, persistErr
			}
		}
		return DetectionEpisodeDecision{}, err
	}
	if err := p.persistDetectionEpisodesLocked(path, state); err != nil {
		return DetectionEpisodeDecision{}, err
	}
	if occurrence.ExistingOnly {
		incident, err := p.GetIncident(decision.IncidentID)
		if err == nil {
			applyDetectionEpisodeDecision(incident, decision)
			if err := p.SaveDetectionIncident(incident); err != nil {
				return decision, err
			}
		} else if !errors.Is(err, ErrNotFound) {
			return decision, err
		}
	}
	return decision, nil
}

func (p *fileProvider) CompleteDetectionNotification(completion DetectionNotificationCompletion) error {
	path := filepath.Join(p.dir, detectionEpisodesFile)
	lock := fileBlobLock(path)
	lock.Lock()
	defer lock.Unlock()
	state, err := p.loadDetectionEpisodesLocked(path)
	if err != nil {
		return err
	}
	if err := state.CompleteDetectionNotification(completion); err != nil {
		return err
	}
	if err := p.persistDetectionEpisodesLocked(path, state); err != nil {
		return err
	}
	episode := state.episodes[completion.EpisodeID]
	if episode == nil {
		return nil
	}
	incident, err := p.GetIncident(episode.IncidentID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	applyDetectionEpisodeDecision(incident, state.detectionEpisodeDecision(episode))
	return p.SaveDetectionIncident(incident)
}

func (p *fileProvider) RenewDetectionNotificationClaim(renewal DetectionNotificationRenewal) error {
	path := filepath.Join(p.dir, detectionEpisodesFile)
	lock := fileBlobLock(path)
	lock.Lock()
	defer lock.Unlock()
	state, err := p.loadDetectionEpisodesLocked(path)
	if err != nil {
		return err
	}
	if err := state.RenewDetectionNotificationClaim(renewal); err != nil {
		return err
	}
	return p.persistDetectionEpisodesLocked(path, state)
}

func (p *fileProvider) CloseDetectionEpisodeByIncident(incidentID string, closedAt time.Time) error {
	path := filepath.Join(p.dir, detectionEpisodesFile)
	lock := fileBlobLock(path)
	lock.Lock()
	defer lock.Unlock()
	state, err := p.loadDetectionEpisodesLocked(path)
	if err != nil {
		return err
	}
	if err := state.CloseDetectionEpisodeByIncident(incidentID, closedAt); err != nil {
		return err
	}
	return p.persistDetectionEpisodesLocked(path, state)
}

type incidentsFileSchema struct {
	Version   int               `json:"version"`
	UpdatedAt time.Time         `json:"updated_at"`
	Incidents []*IncidentRecord `json:"incidents"`
}

const incidentsFileVersion = 1

func (p *fileProvider) loadIncidents() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		return nil
	}
	p.loaded = true

	path := filepath.Join(p.dir, incidentsFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("storage: read incidents: %w", err)
	}
	var f incidentsFileSchema
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("storage: parse incidents: %w", err)
	}
	p.incidents = f.Incidents
	// Defensive: sort by CreatedAt ascending so newest is last.
	sort.SliceStable(p.incidents, func(i, j int) bool {
		return p.incidents[i].CreatedAt.Before(p.incidents[j].CreatedAt)
	})
	return nil
}

func (p *fileProvider) persistIncidentsLocked() error {
	f := incidentsFileSchema{
		Version:   incidentsFileVersion,
		UpdatedAt: time.Now().UTC(),
		Incidents: p.incidents,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("storage: marshal incidents: %w", err)
	}
	target := filepath.Join(p.dir, incidentsFile)
	if err := writeFileAtomicSync(target, data, 0o644); err != nil {
		return fmt.Errorf("storage: write incidents: %w", err)
	}
	return nil
}

func (p *fileProvider) SaveIncident(rec *IncidentRecord) error {
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("storage: SaveIncident: missing id")
	}
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	stored := CloneIncidentRecord(rec)
	p.mu.Lock()
	defer p.mu.Unlock()

	// Update in place if the id already exists (e.g. ack flow).
	for i, existing := range p.incidents {
		if existing.ID == rec.ID {
			mergeDetectionIncident(existing, stored)
			p.incidents[i] = stored
			return p.persistIncidentsLocked()
		}
	}

	// Append + trim.
	p.incidents = append(p.incidents, stored)
	if over := len(p.incidents) - p.maxIncidents; over > 0 {
		p.incidents = append([]*IncidentRecord(nil), p.incidents[over:]...)
	}
	return p.persistIncidentsLocked()
}

func (p *fileProvider) SaveDetectionIncident(rec *IncidentRecord) error {
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("storage: SaveDetectionIncident: missing id")
	}
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	p.mu.Lock()
	defer p.mu.Unlock()
	for index, existing := range p.incidents {
		if existing.ID == rec.ID {
			p.incidents[index] = detectionIncidentUpdate(existing, rec)
			return p.persistIncidentsLocked()
		}
	}
	p.incidents = append(p.incidents, CloneIncidentRecord(rec))
	if over := len(p.incidents) - p.maxIncidents; over > 0 {
		p.incidents = append([]*IncidentRecord(nil), p.incidents[over:]...)
	}
	return p.persistIncidentsLocked()
}

func (p *fileProvider) UpdateIncidentAck(id string, ackedAt time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, rec := range p.incidents {
		if rec.ID == id {
			t := ackedAt.UTC()
			rec.AckedAt = &t
			return p.persistIncidentsLocked()
		}
	}
	return ErrNotFound
}

func (p *fileProvider) GetIncident(id string) (*IncidentRecord, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, rec := range p.incidents {
		if rec.ID == id {
			return CloneIncidentRecord(rec), nil
		}
	}
	return nil, ErrNotFound
}

func (p *fileProvider) ListIncidents(limit int) ([]*IncidentRecord, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := len(p.incidents)
	if n == 0 {
		return nil, nil
	}
	// Newest first.
	out := make([]*IncidentRecord, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, CloneIncidentRecord(p.incidents[i]))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CountIncidents implements the optional storage.IncidentPager capability.
// The file backend keeps a rolling in-memory cap, so a linear tally is
// cheap. Counts reflect open work: resolved incidents are skipped so the
// tally matches the unresolved-only counts the SQL backend returns. Legacy
// rows are classified via EffectiveOrigin, matching the SQL backend.
func (p *fileProvider) CountIncidents() (IncidentCounts, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var c IncidentCounts
	for _, rec := range p.incidents {
		if rec.Resolved {
			continue
		}
		if rec.EffectiveOrigin() == OriginAIDetect {
			c.AIDetect++
		} else {
			c.Webhook++
		}
	}
	c.Total = c.AIDetect + c.Webhook
	return c, nil
}

// CountIncidentsByStatus implements the optional storage.IncidentPager
// capability. The file backend keeps a rolling in-memory cap, so a single
// pass over the slice is cheap; the shared StatusCountsOf helper classifies
// each row via EffectiveOrigin and buckets it by stored status, matching the
// SQL backend exactly.
func (p *fileProvider) CountIncidentsByStatus() (IncidentStatusCounts, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return StatusCountsOf(p.incidents), nil
}

// CountIncidentsByStatusSince implements storage.IncidentWindowCounter over the
// same single pass, dropping rows older than the window.
func (p *fileProvider) CountIncidentsByStatusSince(since time.Time) (IncidentStatusCounts, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return StatusCountsSince(p.incidents, since), nil
}

func (p *fileProvider) CountIncidentsByServiceSince(orgID, service string, since time.Time) (int, map[string]int, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	severities := make(map[string]int)
	for _, rec := range p.incidents {
		if NormalizeOrgID(rec.OrgID) != NormalizeOrgID(orgID) || rec.ServiceLabel() != service || rec.CreatedAt.Before(since) {
			continue
		}
		count++
		severities[utils.ExtractSeverity(rec.Content)]++
	}
	return count, severities, nil
}

func (p *fileProvider) ListIncidentsByServiceSince(orgID, service string, since time.Time, limit int) ([]*IncidentRecord, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*IncidentRecord, 0)
	for _, rec := range p.incidents {
		if NormalizeOrgID(rec.OrgID) != NormalizeOrgID(orgID) || rec.ServiceLabel() != service || rec.CreatedAt.Before(since) {
			continue
		}
		out = append(out, CloneIncidentRecord(rec))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListIncidentsPage implements the optional storage.IncidentPager
// capability: one bounded, newest-first page over the in-memory slice,
// skipping offset matches and returning at most limit rows. The origin
// filter is applied while walking so a filtered page stays bounded.
func (p *fileProvider) ListIncidentsPage(origin string, offset, limit int) ([]*IncidentRecord, error) {
	if limit <= 0 {
		limit = DefaultIncidentPageSize
	}
	if offset < 0 {
		offset = 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	filtered := origin == OriginAIDetect || origin == OriginWebhook
	out := make([]*IncidentRecord, 0, limit)
	skipped := 0
	for i := len(p.incidents) - 1; i >= 0; i-- {
		rec := p.incidents[i]
		if filtered && rec.EffectiveOrigin() != origin {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		out = append(out, CloneIncidentRecord(rec))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (p *fileProvider) Close() error {
	tablesErr := p.closeTables()
	p.evidenceMu.Lock()
	defer p.evidenceMu.Unlock()
	var err error
	if p.localEvidence != nil {
		err = p.localEvidence.db.Close()
	}
	// The data directory handle is closed after the store's last use.
	return errors.Join(err, tablesErr, p.root.Close())
}

// ---------------------------------------------------------------------------
// Analyses
// ---------------------------------------------------------------------------

const (
	analysesFile        = "analyses.json"
	analysesFileVersion = 1
	maxAnalysesDefault  = 500
)

type analysesFileSchema struct {
	Version   int               `json:"version"`
	UpdatedAt time.Time         `json:"updated_at"`
	Analyses  []*AnalysisRecord `json:"analyses"`
}

func (p *fileProvider) loadAnalyses() error {
	p.analysesMu.Lock()
	defer p.analysesMu.Unlock()
	if p.analysesLoaded {
		return nil
	}
	p.analysesLoaded = true

	path := filepath.Join(p.dir, analysesFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("storage: read analyses: %w", err)
	}
	var f analysesFileSchema
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("storage: parse analyses: %w", err)
	}
	p.analyses = f.Analyses
	sort.SliceStable(p.analyses, func(i, j int) bool {
		return p.analyses[i].RequestedAt.Before(p.analyses[j].RequestedAt)
	})
	return nil
}

func (p *fileProvider) persistAnalysesLocked() error {
	f := analysesFileSchema{
		Version:   analysesFileVersion,
		UpdatedAt: time.Now().UTC(),
		Analyses:  p.analyses,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("storage: marshal analyses: %w", err)
	}
	target := filepath.Join(p.dir, analysesFile)
	if err := writeFileAtomicSync(target, data, 0o644); err != nil {
		return fmt.Errorf("storage: write analyses: %w", err)
	}
	return nil
}

func (p *fileProvider) SaveAnalysis(rec *AnalysisRecord) error {
	if rec == nil || rec.ID == "" {
		return fmt.Errorf("storage: SaveAnalysis: missing id")
	}
	stored := CloneAnalysisRecord(rec)
	stored.OrgID = NormalizeOrgID(stored.OrgID)
	p.analysesMu.Lock()
	defer p.analysesMu.Unlock()
	for i, existing := range p.analyses {
		if existing.ID == rec.ID {
			p.analyses[i] = stored
			return p.persistAnalysesLocked()
		}
	}
	p.analyses = append(p.analyses, stored)
	if over := len(p.analyses) - maxAnalysesDefault; over > 0 {
		p.analyses = append([]*AnalysisRecord(nil), p.analyses[over:]...)
	}
	return p.persistAnalysesLocked()
}

func (p *fileProvider) GetAnalysis(id string) (*AnalysisRecord, error) {
	p.analysesMu.RLock()
	defer p.analysesMu.RUnlock()
	for _, rec := range p.analyses {
		if rec.ID == id {
			return CloneAnalysisRecord(rec), nil
		}
	}
	return nil, ErrNotFound
}

func (p *fileProvider) ListAnalysesByIncident(incidentID string, limit int) ([]*AnalysisRecord, error) {
	p.analysesMu.RLock()
	defer p.analysesMu.RUnlock()
	n := len(p.analyses)
	if n == 0 {
		return nil, nil
	}
	out := make([]*AnalysisRecord, 0)
	for i := n - 1; i >= 0; i-- {
		if p.analyses[i].IncidentID != incidentID {
			continue
		}
		out = append(out, CloneAnalysisRecord(p.analyses[i]))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (p *fileProvider) ListAnalyses(limit int) ([]*AnalysisRecord, error) {
	p.analysesMu.RLock()
	defer p.analysesMu.RUnlock()
	n := len(p.analyses)
	if n == 0 {
		return nil, nil
	}
	out := make([]*AnalysisRecord, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, CloneAnalysisRecord(p.analyses[i]))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CountAnalyses implements the optional storage.AnalysisPager capability. The
// file backend keeps a rolling in-memory cap, so a len() is the whole count.
func (p *fileProvider) CountAnalyses() (int, error) {
	p.analysesMu.RLock()
	defer p.analysesMu.RUnlock()
	return len(p.analyses), nil
}

// ListAnalysesPage implements the optional storage.AnalysisPager capability:
// one bounded, newest-first page over the in-memory slice, skipping offset
// rows and returning at most limit rows.
func (p *fileProvider) ListAnalysesPage(offset, limit int) ([]*AnalysisRecord, error) {
	if limit <= 0 {
		limit = DefaultAnalysisPageSize
	}
	if offset < 0 {
		offset = 0
	}
	p.analysesMu.RLock()
	defer p.analysesMu.RUnlock()
	out := make([]*AnalysisRecord, 0, limit)
	skipped := 0
	for i := len(p.analyses) - 1; i >= 0; i-- {
		if skipped < offset {
			skipped++
			continue
		}
		out = append(out, CloneAnalysisRecord(p.analyses[i]))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (p *fileProvider) DeleteAnalysis(id string) error {
	p.analysesMu.Lock()
	defer p.analysesMu.Unlock()
	for i, rec := range p.analyses {
		if rec.ID == id {
			p.analyses = append(p.analyses[:i], p.analyses[i+1:]...)
			return p.persistAnalysesLocked()
		}
	}
	return ErrNotFound
}
