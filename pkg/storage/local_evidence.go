package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const localEvidencePrefix = "kubernetes/captured-evidence/"

var errInvalidLocalEvidence = errors.New("storage: local evidence image cannot be indexed")

// The CAS image and its indexed projections have one SQLite commit point.
// Raw images remain available to writers and the generic blob API; history
// readers never select them. File storage retains its single-process contract.
type localEvidence struct {
	db           *sql.DB
	rawReads     atomic.Uint64
	payloadReads atomic.Uint64
	countReads   atomic.Uint64
}

func isLocalEvidenceKey(key string) bool {
	if !strings.HasPrefix(key, localEvidencePrefix) {
		return false
	}
	tail := strings.TrimPrefix(key, localEvidencePrefix)
	if len(tail) != 64 {
		return false
	}
	for _, ch := range tail {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

var errLocalEvidenceDriverMissing = errors.New("storage: indexed EvidenceStore requires host registration: import _ \"github.com/VersusControl/versus-incident/pkg/storage/sqlitedriver\"")

func localEvidenceDriverRegistered() bool {
	for _, name := range sql.Drivers() {
		if name == "sqlite" {
			return true
		}
	}
	return false
}

func openLocalEvidence(path string) (*localEvidence, error) {
	if !localEvidenceDriverRegistered() {
		return nil, errLocalEvidenceDriverMissing
	}
	dsn := "file:evidence-" + uuid.NewString() + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)"
	if path != "" {
		// The default data directory is relative to the working directory. A
		// relative path in a file: URI is parsed as a URI authority, which
		// SQLite rejects on every statement, so the URI always carries an
		// absolute path.
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = absolute
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if err := validateLocalEvidenceArtifact(path + suffix); err != nil {
				return nil, err
			}
		}
		if err := prepareLocalEvidenceArtifact(path); err != nil {
			return nil, err
		}
		dsn = (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if path != "" {
		db.SetMaxOpenConns(4)
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS evidence_scopes(
 key TEXT PRIMARY KEY, present INTEGER NOT NULL, valid INTEGER NOT NULL,
 image BLOB, checkpoint BLOB, high INTEGER NOT NULL DEFAULT 0,
 revision INTEGER NOT NULL DEFAULT 0, generation TEXT NOT NULL DEFAULT '0',
 prune TEXT NOT NULL DEFAULT '0', pruned TEXT NOT NULL DEFAULT '', at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS evidence_events(
 key TEXT NOT NULL, sequence INTEGER NOT NULL, id TEXT NOT NULL, type TEXT NOT NULL,
 at TEXT NOT NULL, observed TEXT NOT NULL, namespace TEXT NOT NULL, name TEXT NOT NULL,
 kind TEXT NOT NULL, authority_kind TEXT NOT NULL, uid TEXT NOT NULL, root TEXT NOT NULL,
 pod TEXT NOT NULL, rule TEXT NOT NULL, broken INTEGER NOT NULL, gap_from TEXT NOT NULL,
 gap_to TEXT NOT NULL, superseded INTEGER, raw BLOB NOT NULL,
 PRIMARY KEY(key,sequence)
);
CREATE INDEX IF NOT EXISTS evidence_event_time ON evidence_events(key,at DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_event_observed ON evidence_events(key,observed DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_event_id ON evidence_events(key,id,sequence DESC);
CREATE INDEX IF NOT EXISTS evidence_event_type ON evidence_events(key,type,at DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_event_termination ON evidence_events(key,at DESC,id DESC) WHERE type='crash' AND json_extract(raw,'$.payload.type')='termination';
CREATE INDEX IF NOT EXISTS evidence_event_namespace ON evidence_events(key,namespace,at DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_event_root ON evidence_events(key,root,at DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_event_uid ON evidence_events(key,uid,at DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_event_pod ON evidence_events(key,pod,at DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_event_rule ON evidence_events(key,rule,at DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_event_name ON evidence_events(key,namespace,kind,name,at DESC,id DESC);
CREATE TABLE IF NOT EXISTS evidence_current(
 key TEXT NOT NULL,id TEXT NOT NULL,first TEXT NOT NULL,seen TEXT NOT NULL,closed TEXT,
 root TEXT NOT NULL,namespace TEXT NOT NULL,kind TEXT NOT NULL,authority_kind TEXT NOT NULL,
 rule TEXT NOT NULL,severity TEXT NOT NULL,broken INTEGER NOT NULL,observation_only INTEGER NOT NULL,
 center TEXT,raw BLOB NOT NULL, PRIMARY KEY(key,id)
);
CREATE INDEX IF NOT EXISTS evidence_current_root ON evidence_current(key,root,seen DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_current_center ON evidence_current(key,root,center,seen DESC,id DESC);
CREATE TABLE IF NOT EXISTS evidence_versions(
 key TEXT NOT NULL,version TEXT NOT NULL,id TEXT NOT NULL,revision INTEGER NOT NULL,
 observed TEXT NOT NULL,first TEXT NOT NULL,seen TEXT NOT NULL,closed TEXT,
 root TEXT NOT NULL,namespace TEXT NOT NULL,kind TEXT NOT NULL,authority_kind TEXT NOT NULL,
 rule TEXT NOT NULL,severity TEXT NOT NULL,broken INTEGER NOT NULL,observation_only INTEGER NOT NULL,
 center TEXT,superseded INTEGER,superseded_at TEXT,raw BLOB NOT NULL,PRIMARY KEY(key,version)
);
CREATE INDEX IF NOT EXISTS evidence_version_time ON evidence_versions(key,first DESC,id DESC,revision DESC);
CREATE INDEX IF NOT EXISTS evidence_version_root ON evidence_versions(key,root,first DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_version_namespace ON evidence_versions(key,namespace,first DESC,id DESC);
CREATE INDEX IF NOT EXISTS evidence_version_id ON evidence_versions(key,id,revision DESC);
CREATE INDEX IF NOT EXISTS evidence_version_center ON evidence_versions(key,root,center,first DESC,id DESC);
CREATE TABLE IF NOT EXISTS evidence_observations(
 key TEXT NOT NULL,id TEXT NOT NULL,revision INTEGER NOT NULL,observed TEXT NOT NULL,
 seen TEXT NOT NULL,version TEXT NOT NULL,measurements BLOB,PRIMARY KEY(key,id,revision)
);
CREATE INDEX IF NOT EXISTS evidence_observation_version ON evidence_observations(key,id,version,revision DESC);
`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	columns, err := db.Query(`PRAGMA table_info(evidence_observations)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	found := false
	for columns.Next() {
		var id, notnull, pk int
		var name, kind string
		var defaultValue any
		if err = columns.Scan(&id, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			columns.Close()
			db.Close()
			return nil, err
		}
		found = found || name == "measurements"
	}
	if err = columns.Err(); err != nil {
		columns.Close()
		db.Close()
		return nil, err
	}
	columns.Close()
	if !found {
		if _, err = db.Exec(`ALTER TABLE evidence_observations ADD COLUMN measurements BLOB`); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &localEvidence{db: db}, nil
}

func localEvidenceTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
func localEvidenceNullable(at *time.Time) any {
	if at == nil {
		return nil
	}
	return localEvidenceTime(*at)
}

func (p *memoryProvider) evidenceIndex() (*localEvidence, error) {
	p.evidenceMu.Lock()
	defer p.evidenceMu.Unlock()
	if p.localEvidence == nil {
		var err error
		p.localEvidence, err = openLocalEvidence("")
		if err != nil {
			return nil, err
		}
	}
	return p.localEvidence, nil
}

func (p *fileProvider) evidenceIndex() (*localEvidence, error) {
	p.evidenceMu.Lock()
	defer p.evidenceMu.Unlock()
	if p.localEvidence != nil {
		return p.localEvidence, nil
	}
	index, err := openLocalEvidence(filepath.Join(p.dir, "kubernetes-evidence.sqlite"))
	if err != nil {
		return nil, err
	}
	// Legacy JSON is imported once at provider initialization, never as a
	// history-query fallback. A durable key, including a removed key, prevents
	// reimporting an older image on subsequent opens.
	entries, err := listLegacyLocalEvidence(p.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = index.db.Close()
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		key := localEvidencePrefix + strings.TrimSuffix(entry.Name(), ".json")
		if !isLocalEvidenceKey(key) {
			continue
		}
		lock := fileBlobLock(p.blobPath(key))
		lock.Lock()
		var exists bool
		err = index.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM evidence_scopes WHERE key=?)`, key).Scan(&exists)
		if err == nil && !exists {
			var data []byte
			data, err = readLegacyLocalEvidence(p.dir, key)
			if err == nil {
				_, err = index.swap(context.Background(), key, nil, data, false)
			}
		}
		lock.Unlock()
		if err != nil {
			_ = index.db.Close()
			return nil, err
		}
	}
	p.localEvidence = index
	return index, nil
}

func (index *localEvidence) raw(ctx context.Context, key string) ([]byte, error) {
	index.rawReads.Add(1)
	var raw []byte
	err := index.db.QueryRowContext(ctx, `SELECT image FROM evidence_scopes WHERE key=? AND present=1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil && raw == nil {
		raw = []byte{}
	}
	return raw, err
}

func (index *localEvidence) swap(ctx context.Context, key string, expected, replacement []byte, force bool) (won bool, err error) {
	conn, err := index.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
			err = errors.Join(err, rollbackErr)
		}
	}()
	var current []byte
	var present bool
	err = conn.QueryRowContext(ctx, `SELECT image,present FROM evidence_scopes WHERE key=?`, key).Scan(&current, &present)
	if errors.Is(err, sql.ErrNoRows) {
		current, present, err = nil, false, nil
	}
	if err != nil {
		return false, err
	}
	if !force && ((expected == nil && present) || (expected != nil && !present) || !bytes.Equal(expected, current)) {
		return false, nil
	}
	if !force && replacement == nil && !present {
		return false, nil
	}
	nextPresent := replacement != nil || force
	if err = index.publish(ctx, conn, key, current, replacement, nextPresent, force); err != nil {
		return false, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func (index *localEvidence) publish(ctx context.Context, conn *sql.Conn, key string, previous, next []byte, present, force bool) error {
	var before, after evidenceState
	beforeValid := len(previous) == 0 || json.Unmarshal(previous, &before) == nil
	valid := len(next) == 0 || json.Unmarshal(next, &after) == nil
	if valid {
		for _, observation := range after.Observations {
			if _, ok := after.OccurrenceVersions[observation.Version]; !ok {
				valid = false
				break
			}
		}
	}
	changedImmutable := force
	if !changedImmutable {
		old := map[uint64]EvidenceRecord{}
		for _, record := range before.Records {
			old[record.Sequence] = record
		}
		for _, record := range after.Records {
			if prior, ok := old[record.Sequence]; ok && !reflect.DeepEqual(prior, record) {
				changedImmutable = true
				break
			}
		}
		for version, issue := range after.OccurrenceVersions {
			if prior, ok := before.OccurrenceVersions[version]; ok && !sameLocalIssue(prior, issue) {
				changedImmutable = true
				break
			}
		}
	}
	if changedImmutable || !beforeValid || !valid || !present || before.Checkpoint.Generation != after.Checkpoint.Generation {
		for _, table := range []string{"evidence_events", "evidence_current", "evidence_versions", "evidence_observations"} {
			if _, err := conn.ExecContext(ctx, "DELETE FROM "+table+" WHERE key=?", key); err != nil {
				return err
			}
		}
		before = evidenceState{}
	}
	checkpoint := after.Checkpoint
	checkpoint.HighSequence = max(checkpoint.HighSequence, after.Sequence)
	cp, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO evidence_scopes(key,present,valid,image,checkpoint,high,revision,generation,prune,pruned,at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(key) DO UPDATE SET present=excluded.present,valid=excluded.valid,image=excluded.image,
 checkpoint=excluded.checkpoint,high=excluded.high,revision=excluded.revision,generation=excluded.generation,prune=excluded.prune,pruned=excluded.pruned,at=excluded.at`,
		key, present, valid, next, cp, after.Sequence, after.Checkpoint.OccurrenceRevision, strconv.FormatUint(after.Checkpoint.Generation, 10), strconv.FormatUint(after.Checkpoint.PruneRevision, 10), localEvidenceTime(after.Checkpoint.PrunedThrough), localEvidenceTime(after.Checkpoint.At)); err != nil {
		return err
	}
	if !valid || !present {
		return nil
	}
	oldRecords := map[uint64]bool{}
	for _, record := range before.Records {
		oldRecords[record.Sequence] = true
	}
	retained := map[uint64]bool{}
	for _, record := range after.Records {
		retained[record.Sequence] = true
		if oldRecords[record.Sequence] {
			continue
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO evidence_events(key,sequence,id,type,at,observed,namespace,name,kind,authority_kind,uid,root,pod,rule,broken,gap_from,gap_to,raw,superseded)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,(SELECT MIN(sequence) FROM evidence_events WHERE key=? AND id=? AND sequence>?))`, key, record.Sequence, record.ID, record.Type, localEvidenceTime(record.At), localEvidenceTime(record.ObservedAt),
			record.Namespace, record.Name, record.Kind, record.AuthorizationKind, record.UID, record.RootUID, record.PodUID, record.Rule, record.Broken, localEvidenceTime(record.GapFrom), localEvidenceTime(record.GapTo), raw, key, record.ID, record.Sequence); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `UPDATE evidence_events SET superseded=? WHERE key=? AND id=? AND sequence<? AND(superseded IS NULL OR superseded>?)`, record.Sequence, key, record.ID, record.Sequence, record.Sequence); err != nil {
			return err
		}
	}
	for sequence := range oldRecords {
		if !retained[sequence] {
			if _, err = conn.ExecContext(ctx, `DELETE FROM evidence_events WHERE key=? AND sequence=?`, key, sequence); err != nil {
				return err
			}
		}
	}
	first := map[string]occurrenceObservation{}
	for _, observation := range after.Observations {
		old, ok := first[observation.Version]
		if !ok || observation.Revision < old.Revision {
			first[observation.Version] = observation
		}
	}
	for version, issue := range after.OccurrenceVersions {
		if _, exists := before.OccurrenceVersions[version]; exists {
			continue
		}
		observation, ok := first[version]
		if !ok {
			continue
		}
		severity, center, err := localIssueFacts(issue)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		var nextRevision sql.NullInt64
		var nextAt sql.NullString
		err = conn.QueryRowContext(ctx, `SELECT revision,observed FROM evidence_versions WHERE key=? AND id=? AND revision>? ORDER BY revision LIMIT 1`, key, issue.OccurrenceID, observation.Revision).Scan(&nextRevision, &nextAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO evidence_versions(key,version,id,revision,observed,first,seen,closed,root,namespace,kind,authority_kind,rule,severity,broken,observation_only,center,superseded,superseded_at,raw)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, key, version, issue.OccurrenceID, observation.Revision, localEvidenceTime(observation.At), localEvidenceTime(issue.FirstSeen), localEvidenceTime(issue.LastSeen),
			localEvidenceNullable(issue.ClosedAt), issue.UID, issue.Namespace, issue.Kind, issue.AuthorizationKind, issue.Rule, severity, issue.Broken, issue.ObservationOnly, center, nextRevision, nextAt, raw); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `UPDATE evidence_versions SET superseded=?,superseded_at=? WHERE key=? AND id=? AND revision<? AND(superseded IS NULL OR superseded>?)`,
			observation.Revision, localEvidenceTime(observation.At), key, issue.OccurrenceID, observation.Revision, observation.Revision); err != nil {
			return err
		}
	}
	oldObservations := map[string]map[uint64]bool{}
	for _, observation := range before.Observations {
		if oldObservations[observation.OccurrenceID] == nil {
			oldObservations[observation.OccurrenceID] = map[uint64]bool{}
		}
		oldObservations[observation.OccurrenceID][observation.Revision] = true
	}
	observations := map[string]map[uint64]bool{}
	for _, observation := range after.Observations {
		if observations[observation.OccurrenceID] == nil {
			observations[observation.OccurrenceID] = map[uint64]bool{}
		}
		observations[observation.OccurrenceID][observation.Revision] = true
		if oldObservations[observation.OccurrenceID][observation.Revision] {
			continue
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO evidence_observations(key,id,revision,observed,seen,version,measurements) VALUES(?,?,?,?,?,?,?)`, key, observation.OccurrenceID, observation.Revision, localEvidenceTime(observation.At), localEvidenceTime(observation.LastSeen), observation.Version, []byte(observation.Measurements)); err != nil {
			return err
		}
	}
	for id, revisions := range oldObservations {
		for revision := range revisions {
			if !observations[id][revision] {
				if _, err = conn.ExecContext(ctx, `DELETE FROM evidence_observations WHERE key=? AND id=? AND revision=?`, key, id, revision); err != nil {
					return err
				}
			}
		}
	}
	for version := range before.OccurrenceVersions {
		if _, exists := after.OccurrenceVersions[version]; !exists {
			if _, err = conn.ExecContext(ctx, `DELETE FROM evidence_versions WHERE key=? AND version=?`, key, version); err != nil {
				return err
			}
		}
	}
	current := after.Occurrences
	if current == nil {
		current = map[string]EvidenceIssue{}
		for _, observation := range after.Observations {
			issue, ok := after.OccurrenceVersions[observation.Version]
			if !ok {
				continue
			}
			prior := current[issue.OccurrenceID]
			if prior.OccurrenceID == "" || !observation.LastSeen.Before(prior.LastSeen) {
				issue.LastSeen = observation.LastSeen
				current[issue.OccurrenceID] = issue
			}
		}
	}
	for id, issue := range current {
		old, exists := before.Occurrences[id]
		if exists && sameLocalIssue(old, issue) {
			continue
		}
		severity, center, err := localIssueFacts(issue)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO evidence_current(key,id,first,seen,closed,root,namespace,kind,authority_kind,rule,severity,broken,observation_only,center,raw)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(key,id) DO UPDATE SET first=excluded.first,seen=excluded.seen,closed=excluded.closed,root=excluded.root,
 namespace=excluded.namespace,kind=excluded.kind,authority_kind=excluded.authority_kind,rule=excluded.rule,severity=excluded.severity,broken=excluded.broken,
 observation_only=excluded.observation_only,center=excluded.center,raw=excluded.raw`, key, id, localEvidenceTime(issue.FirstSeen), localEvidenceTime(issue.LastSeen), localEvidenceNullable(issue.ClosedAt),
			issue.UID, issue.Namespace, issue.Kind, issue.AuthorizationKind, issue.Rule, severity, issue.Broken, issue.ObservationOnly, center, raw); err != nil {
			return err
		}
	}
	for id := range before.Occurrences {
		if _, exists := current[id]; !exists {
			if _, err = conn.ExecContext(ctx, `DELETE FROM evidence_current WHERE key=? AND id=?`, key, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameLocalIssue(a, b EvidenceIssue) bool {
	return a.Key == b.Key && a.OccurrenceID == b.OccurrenceID && a.Namespace == b.Namespace && a.Kind == b.Kind && a.UID == b.UID && a.Rule == b.Rule &&
		a.AuthorizationKind == b.AuthorizationKind && a.Broken == b.Broken && a.ObservationOnly == b.ObservationOnly && a.Epoch == b.Epoch &&
		a.FirstSeen.Equal(b.FirstSeen) && a.LastSeen.Equal(b.LastSeen) && localEvidenceNullable(a.ClosedAt) == localEvidenceNullable(b.ClosedAt) && bytes.Equal(a.Payload, b.Payload)
}
func localIssueFacts(issue EvidenceIssue) (string, any, error) {
	var value struct {
		Severity string     `json:"severity"`
		BrokeAt  *time.Time `json:"broke_at"`
	}
	if err := json.Unmarshal(issue.Payload, &value); err != nil {
		// An individual malformed protected payload occupies an unavailable
		// slot at the service projection; it must not poison unrelated rows.
		return "", nil, nil
	}
	return value.Severity, localEvidenceNullable(value.BrokeAt), nil
}

func (index *localEvidence) checkpoint(ctx context.Context, key string) (EvidenceCheckpoint, error) {
	var cp EvidenceCheckpoint
	var raw []byte
	var valid bool
	err := index.db.QueryRowContext(ctx, `SELECT checkpoint,valid FROM evidence_scopes WHERE key=? AND present=1`, key).Scan(&raw, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return cp, nil
	}
	if err != nil {
		return cp, err
	}
	if !valid {
		return cp, errInvalidLocalEvidence
	}
	err = json.Unmarshal(raw, &cp)
	return cp, err
}

func (index *localEvidence) blobs(ctx context.Context, prefix string) ([]Blob, error) {
	rows, err := index.db.QueryContext(ctx, `SELECT key,image FROM evidence_scopes WHERE present=1 AND substr(key,1,?)=? ORDER BY key`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blobs []Blob
	for rows.Next() {
		var blob Blob
		if err = rows.Scan(&blob.Name, &blob.Data); err != nil {
			return nil, err
		}
		blobs = append(blobs, blob)
	}
	return blobs, rows.Err()
}

func localEvidenceError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("storage: local evidence index: %w", err)
}
