package chat

import (
	"encoding/json"
	"errors"
	"sort"
	"time"
)

type continuationRecord struct {
	Session
	Continuation bool `json:"continuation,omitempty"`
}

func (store *SessionStore) isDeleted(id string) (bool, error) {
	index, _, err := store.readIndex()
	if err != nil || index == nil {
		return false, err
	}
	for _, entry := range index.Sessions {
		if entry.ID == id && entry.Deleted {
			return true, nil
		}
	}
	return false, nil
}

func (store *SessionStore) readRecord(id string) (*continuationRecord, error) {
	index, err := store.archiveIndex()
	if err != nil {
		return nil, err
	}
	for _, entry := range index.Sessions {
		if entry.ID == id {
			return store.readStoredRecord(id)
		}
	}
	return nil, ErrSessionNotFound
}

func (store *SessionStore) readStoredRecord(id string) (*continuationRecord, error) {
	data, err := store.provider.ReadBlob(store.sessionBlobName(id))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSessionBlobBytes {
		return nil, ErrSessionTooLarge
	}
	if len(data) > 0 {
		var record continuationRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, ErrSessionCorrupt
		}
		if record.ID != id || !validSessionID(record.ID) || !validSessionStatus(record.Status) {
			return nil, ErrSessionCorrupt
		}
		if len(record.Turns) > MaxTurnsPerSession {
			return nil, ErrSessionTooLarge
		}
		return &record, nil
	}
	data, err = store.provider.ReadBlob(store.blobName)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSessionBlobBytes {
		return nil, ErrSessionTooLarge
	}
	if len(data) > 0 {
		var legacy sessionDocument
		if err := json.Unmarshal(data, &legacy); err != nil {
			return nil, ErrSessionCorrupt
		}
		if len(legacy.Sessions) > MaxSessions {
			return nil, ErrSessionTooLarge
		}
		for _, session := range legacy.Sessions {
			if session == nil || !validSessionID(session.ID) || !validSessionStatus(session.Status) {
				return nil, ErrSessionCorrupt
			}
			if session.ID == id {
				if len(session.Turns) > MaxTurnsPerSession || !validSessionStatus(session.Status) {
					return nil, ErrSessionTooLarge
				}
				return &continuationRecord{Session: *cloneSession(session)}, nil
			}
		}
	}
	return nil, ErrSessionNotFound
}

func (store *SessionStore) archivedSession(id string) (*Session, error) {
	for _, archive := range store.archives {
		record, err := archive.readRecord(id)
		if errors.Is(err, ErrSessionNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if record.Continuation {
			return nil, ErrStoreConflict
		}
		if record.Status == SessionRunning {
			record.Status = SessionFailed
		}
		return cloneSession(&record.Session), nil
	}
	return nil, ErrSessionNotFound
}

func (store *SessionStore) getUnion(id string) (*Session, error) {
	if !validSessionID(id) {
		return nil, ErrSessionNotFound
	}
	for _, archive := range store.archives {
		if _, err := archive.archiveIndex(); err != nil {
			return nil, err
		}
	}
	deleted, err := store.isDeleted(id)
	if err != nil {
		return nil, err
	}
	if deleted {
		return nil, ErrSessionNotFound
	}
	record, err := store.readRecord(id)
	if errors.Is(err, ErrSessionNotFound) && !store.readOnly {
		return store.archivedSession(id)
	}
	if err != nil {
		return nil, err
	}
	if record.Continuation {
		archive, err := store.archivedSession(id)
		if err != nil && !errors.Is(err, ErrSessionNotFound) {
			return nil, err
		}
		if err == nil {
			record.Turns = mergeTurns(record.Turns, archive.Turns)
		}
	}
	if record.Status == SessionRunning {
		if store.readOnly {
			record.Status = SessionFailed
		} else {
			active, err := store.LeaseActive(id)
			if err != nil {
				return nil, err
			}
			if !active {
				if err := store.SetStatus(id, SessionFailed, false); err != nil {
					return nil, err
				}
				record.Status = SessionFailed
			}
		}
	}
	if _, err := compactSessionDocument(&record.Session, store.now().UTC()); err != nil {
		return nil, err
	}
	return cloneSession(&record.Session), nil
}

func mergeTurns(write, archive []Turn) []Turn {
	merged := make([]Turn, 0, len(write)+len(archive))
	seen := map[string]bool{}
	for _, turns := range [][]Turn{write, archive} {
		for _, turn := range turns {
			if turn.ID != "" && seen[turn.ID] {
				continue
			}
			if turn.ID != "" {
				seen[turn.ID] = true
			}
			merged = append(merged, turn)
		}
	}
	sort.SliceStable(merged, func(left, right int) bool {
		if merged[left].CreatedAt.Equal(merged[right].CreatedAt) {
			return merged[left].ID < merged[right].ID
		}
		return merged[left].CreatedAt.Before(merged[right].CreatedAt)
	})
	if len(merged) > MaxTurnsPerSession {
		merged = merged[len(merged)-MaxTurnsPerSession:]
	}
	return merged
}

func (store *SessionStore) summaryIndex() (*sessionIndex, error) {
	index, _, err := store.readIndex()
	if err != nil {
		return nil, err
	}
	if index == nil {
		data, err := store.provider.ReadBlob(store.blobName)
		if err != nil {
			return nil, err
		}
		if len(data) > MaxSessionBlobBytes {
			return nil, ErrSessionTooLarge
		}
		index = &sessionIndex{}
		if len(data) > 0 {
			var legacy sessionDocument
			if err := json.Unmarshal(data, &legacy); err != nil {
				return nil, ErrSessionCorrupt
			}
			if len(legacy.Sessions) > MaxSessions {
				return nil, ErrSessionTooLarge
			}
			for _, session := range legacy.Sessions {
				if session == nil || !validSessionID(session.ID) {
					return nil, ErrSessionCorrupt
				}
				index.Sessions = append(index.Sessions, summaryEntry(session))
			}
		}
	}
	for position, entry := range index.Sessions {
		if !entry.Deleted && !validSessionStatus(entry.Status) {
			record, err := store.readStoredRecord(entry.ID)
			if err != nil {
				return nil, err
			}
			index.Sessions[position] = summaryEntry(&record.Session)
		}
	}
	return index, nil
}

func (store *SessionStore) archiveIndex() (*sessionIndex, error) {
	index, err := store.summaryIndex()
	if err != nil {
		return nil, err
	}
	kept := index.Sessions[:0]
	for _, entry := range index.Sessions {
		if !entry.Deleted {
			kept = append(kept, entry)
		}
	}
	index.Sessions = kept
	pruneIndex(index, store.now().UTC(), 0)
	return index, nil
}

func (store *SessionStore) listUnion() ([]*Session, error) {
	writeIndex, err := store.summaryIndex()
	if store.readOnly && err == nil {
		writeIndex, err = store.archiveIndex()
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	result := []*Session{}
	for _, source := range append([]*SessionStore{store}, store.archives...) {
		index := writeIndex
		if source != store {
			index, err = source.archiveIndex()
			if err != nil {
				return nil, err
			}
		}
		for _, entry := range index.Sessions {
			if seen[entry.ID] {
				continue
			}
			seen[entry.ID] = true
			if entry.Deleted || entry.UpdatedAt.Before(store.now().Add(-SessionRetention)) {
				continue
			}
			status := entry.Status
			if status == SessionRunning {
				status = SessionFailed
				if source == store && !store.readOnly {
					active, err := store.LeaseActive(entry.ID)
					if err != nil {
						return nil, err
					}
					if active {
						status = SessionRunning
					}
				}
			}
			result = append(result, &Session{ID: entry.ID, Status: status, Seeded: entry.Seeded, CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt})
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].UpdatedAt.Equal(result[right].UpdatedAt) {
			return result[left].ID < result[right].ID
		}
		return result[left].UpdatedAt.After(result[right].UpdatedAt)
	})
	if len(result) > MaxSessions {
		result = result[:MaxSessions]
	}
	return result, nil
}

func suppressionMarker(index *sessionIndex, id string) *sessionIndexEntry {
	marker, ok := index.retainedArchives[id]
	if !ok {
		return nil
	}
	return &marker
}

func (store *SessionStore) retainedArchiveEntries() (map[string]sessionIndexEntry, error) {
	retained := make(map[string]sessionIndexEntry)
	for _, archive := range store.archives {
		index, err := archive.archiveIndex()
		if err != nil {
			return nil, err
		}
		for _, entry := range index.Sessions {
			if _, exists := retained[entry.ID]; !exists {
				retained[entry.ID] = sessionIndexEntry{ID: entry.ID, Deleted: true, UpdatedAt: entry.UpdatedAt}
			}
		}
	}
	if len(retained) > MaxSessions {
		return nil, ErrSessionTooLarge
	}
	return retained, nil
}

func (store *SessionStore) pruneSuppressionMarkers(index *sessionIndex) error {
	retained, err := store.retainedArchiveEntries()
	if err != nil {
		return err
	}
	index.retainedArchives = retained
	kept := make([]sessionIndexEntry, 0, len(index.Sessions))
	for _, entry := range index.Sessions {
		if entry.Deleted {
			marker, exists := retained[entry.ID]
			if !exists {
				continue
			}
			entry = marker
		}
		kept = append(kept, entry)
	}
	index.Sessions = kept
	return nil
}

func (store *SessionStore) pruneIndex(index *sessionIndex, now time.Time, reserve int) ([]string, error) {
	evicted := pruneIndex(index, now, reserve)
	for _, id := range evicted {
		if marker := suppressionMarker(index, id); marker != nil {
			index.Sessions = append(index.Sessions, *marker)
		}
	}
	markers := 0
	for _, entry := range index.Sessions {
		if entry.Deleted {
			markers++
		}
	}
	if markers > MaxSessions {
		return nil, ErrSessionTooLarge
	}
	return evicted, nil
}
