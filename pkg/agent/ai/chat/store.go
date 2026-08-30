package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	MaxSessions           = 100
	MaxTurnsPerSession    = 100
	MaxMessageBytes       = 8192
	MaxToolPayloadBytes   = 8192
	MaxEventsPerTurn      = 128
	MaxCitationsPerTrace  = 4
	MaxEventBytesPerTurn  = 32 * 1024
	MaxToolBytesPerTurn   = 32 * 1024
	MaxSessionBlobBytes   = 5 * 1024 * 1024
	SessionRetention      = 30 * 24 * time.Hour
	chatSessionBlobPrefix = "chat-sessions-"
	maxCASAttempts        = 64
)

var (
	ErrSessionNotFound = errors.New("chat: session not found")
	ErrStoreConflict   = errors.New("chat: storage conflict")
	ErrLeaseFenced     = errors.New("chat: lease fenced")
	ErrSessionTooLarge = errors.New("chat: session exceeds storage limit")
)

type SessionStatus string

const (
	SessionIdle    SessionStatus = "idle"
	SessionRunning SessionStatus = "running"
	SessionFailed  SessionStatus = "failed"
)

type TurnRole string

const (
	TurnUser       TurnRole = "user"
	TurnAssistant  TurnRole = "assistant"
	TurnCompaction TurnRole = "compaction"
)

type Turn struct {
	ID         string               `json:"id"`
	Role       TurnRole             `json:"role"`
	Content    string               `json:"content"`
	CreatedAt  time.Time            `json:"created_at"`
	Attachment *core.ChatAttachment `json:"attachment,omitempty"`
	ToolCalls  []core.ToolCallTrace `json:"tool_calls,omitempty"`
	Citations  []core.ChatCitation  `json:"citations,omitempty"`
	Events     []core.ChatEvent     `json:"events,omitempty"`
}

type Session struct {
	ID        string        `json:"id"`
	Status    SessionStatus `json:"status"`
	Seeded    bool          `json:"seeded"`
	Turns     []Turn        `json:"turns"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type sessionDocument struct {
	Version  int        `json:"version"`
	Sessions []*Session `json:"sessions"`
}

type sessionIndexEntry struct {
	ID        string        `json:"id"`
	Status    SessionStatus `json:"status,omitempty"`
	Seeded    bool          `json:"seeded,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type sessionIndex struct {
	Version  int                 `json:"version"`
	Sessions []sessionIndexEntry `json:"sessions"`
}

type sessionLease struct {
	Owner           string    `json:"owner"`
	Epoch           uint64    `json:"epoch"`
	ExpiresAt       time.Time `json:"expires_at"`
	CancelRequested bool      `json:"cancel_requested,omitempty"`
}

type SessionStore struct {
	provider storage.Provider
	blobName string
	now      func() time.Time
}

func NewSessionStore(provider storage.Provider, scope tenancy.OrgScope, now func() time.Time) *SessionStore {
	if provider == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	org := scope.Normalized().Write
	hash := sha256.Sum256([]byte(org))
	blobName := chatSessionBlobPrefix + hex.EncodeToString(hash[:8])
	return &SessionStore{provider: provider, blobName: blobName, now: now}
}

func (store *SessionStore) indexBlobName() string { return store.blobName + "/index" }
func (store *SessionStore) sessionBlobName(id string) string {
	return store.blobName + "/sessions/" + id
}
func (store *SessionStore) leaseBlobName(id string) string { return store.blobName + "/leases/" + id }

func (store *SessionStore) Create(id string) (*Session, error) {
	if !validSessionID(id) {
		return nil, fmt.Errorf("chat: invalid session id")
	}
	now := store.now().UTC()
	created := &Session{ID: id, Status: SessionIdle, CreatedAt: now, UpdatedAt: now}
	encoded, err := json.Marshal(created)
	if err != nil {
		return nil, fmt.Errorf("chat: encode session: %w", err)
	}
	if err := store.replaceBlob(store.sessionBlobName(id), encoded, true); err != nil {
		return nil, err
	}
	var evicted []string
	err = store.updateIndex(func(index *sessionIndex) error {
		for _, entry := range index.Sessions {
			if entry.ID == id {
				return fmt.Errorf("chat: session already exists")
			}
		}
		evicted = pruneIndex(index, now, 1)
		index.Sessions = append(index.Sessions, summaryEntry(created))
		return nil
	})
	if err != nil {
		rollbackErr := store.replaceBlob(store.sessionBlobName(id), nil, false)
		if rollbackErr != nil {
			return nil, errors.Join(err, fmt.Errorf("chat: roll back session: %w", rollbackErr))
		}
		return nil, err
	}
	for _, evictedID := range evicted {
		if err := store.deleteSessionArtifacts(evictedID); err != nil {
			log.Printf("chat storage cleanup failed: operation=evict session_id=%q code=backend_failure", evictedID)
		}
	}
	return cloneSession(created), nil
}

func (store *SessionStore) List() ([]*Session, error) {
	index, _, err := store.readIndex()
	if err != nil {
		return nil, err
	}
	if index == nil {
		if err := store.migrateLegacy(); err != nil {
			return nil, err
		}
		index, _, err = store.readIndex()
		if err != nil || index == nil {
			return nil, err
		}
	}
	if indexNeedsHydration(index) {
		if err := store.hydrateIndex(); err != nil {
			return nil, err
		}
		index, _, err = store.readIndex()
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(index.Sessions, func(i, j int) bool {
		if index.Sessions[i].UpdatedAt.Equal(index.Sessions[j].UpdatedAt) {
			return index.Sessions[i].ID < index.Sessions[j].ID
		}
		return index.Sessions[i].UpdatedAt.After(index.Sessions[j].UpdatedAt)
	})
	activeLeases := make(map[string]bool)
	for _, entry := range index.Sessions {
		if entry.Status == SessionRunning {
			leaseBlobs, listErr := store.provider.ListBlobs(store.blobName + "/leases/")
			if listErr != nil {
				return nil, fmt.Errorf("chat: list lease blobs: %w", listErr)
			}
			for _, blob := range leaseBlobs {
				var lease sessionLease
				if json.Unmarshal(blob.Data, &lease) == nil && lease.ExpiresAt.After(store.now().UTC()) {
					activeLeases[strings.TrimPrefix(blob.Name, store.blobName+"/leases/")] = true
				}
			}
			break
		}
	}
	out := make([]*Session, 0, len(index.Sessions))
	staleRunning := make(map[string]struct{})
	for _, entry := range index.Sessions {
		status := entry.Status
		if status == SessionRunning && !activeLeases[entry.ID] {
			status = SessionFailed
			staleRunning[entry.ID] = struct{}{}
		}
		out = append(out, &Session{ID: entry.ID, Status: status, Seeded: entry.Seeded, CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt})
	}
	if len(staleRunning) > 0 {
		if err := store.updateIndex(func(current *sessionIndex) error {
			for position := range current.Sessions {
				if _, stale := staleRunning[current.Sessions[position].ID]; stale && current.Sessions[position].Status == SessionRunning {
					current.Sessions[position].Status = SessionFailed
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (store *SessionStore) Get(id string) (*Session, error) {
	if !validSessionID(id) {
		return nil, ErrSessionNotFound
	}
	data, err := store.provider.ReadBlob(store.sessionBlobName(id))
	if err != nil {
		return nil, fmt.Errorf("chat: read session: %w", err)
	}
	if len(data) == 0 {
		if err := store.migrateLegacy(); err != nil {
			return nil, err
		}
		data, err = store.provider.ReadBlob(store.sessionBlobName(id))
		if err != nil {
			return nil, fmt.Errorf("chat: read session: %w", err)
		}
	}
	if len(data) == 0 {
		return nil, ErrSessionNotFound
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("chat: decode session: %w", err)
	}
	if session.Status == SessionRunning {
		active, leaseErr := store.LeaseActive(id)
		if leaseErr != nil {
			return nil, leaseErr
		}
		if !active {
			if err := store.SetStatus(id, SessionFailed, false); err != nil {
				return nil, err
			}
			session.Status = SessionFailed
		}
	}
	return cloneSession(&session), nil
}

func (store *SessionStore) Delete(id string) error {
	if !validSessionID(id) {
		return ErrSessionNotFound
	}
	found := false
	if err := store.updateIndex(func(index *sessionIndex) error {
		for position, entry := range index.Sessions {
			if entry.ID == id {
				found = true
				index.Sessions = append(index.Sessions[:position], index.Sessions[position+1:]...)
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if !found {
		data, err := store.provider.ReadBlob(store.sessionBlobName(id))
		if err != nil {
			return fmt.Errorf("chat: read session for delete: %w", err)
		}
		if len(data) == 0 {
			return nil
		}
	}
	return store.deleteSessionArtifacts(id)
}

func (store *SessionStore) deleteSessionArtifacts(id string) error {
	if err := store.replaceBlob(store.leaseBlobName(id), nil, false); err != nil {
		return fmt.Errorf("delete lease: %w", err)
	}
	if err := store.replaceBlob(store.sessionBlobName(id), nil, false); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (store *SessionStore) Append(id string, turns ...Turn) (*Session, error) {
	var updated *Session
	err := store.updateSession(id, func(session *Session, now time.Time) error {
		for _, turn := range turns {
			turn.Content = capString(turn.Content, MaxMessageBytes)
			turn.ToolCalls = boundToolCalls(turn.ToolCalls)
			turn.Citations = boundCitations(turn.Citations)
			turn.Events = boundEvents(turn.Events)
			turn.Attachment = cloneAttachment(turn.Attachment)
			session.Turns = append(session.Turns, turn)
		}
		if len(session.Turns) > MaxTurnsPerSession {
			dropped := len(session.Turns) - MaxTurnsPerSession + 1
			kept := append([]Turn(nil), session.Turns[dropped:]...)
			marker := Turn{Role: TurnCompaction, Content: fmt.Sprintf("%d older turns were compacted from model context", dropped), CreatedAt: now}
			session.Turns = append([]Turn{marker}, kept...)
		}
		session.UpdatedAt = now
		updated = session
		return nil
	})
	return cloneSession(updated), err
}

func (store *SessionStore) SetStatus(id string, status SessionStatus, seeded bool) error {
	return store.updateSession(id, func(session *Session, now time.Time) error {
		session.Status = status
		session.Seeded = session.Seeded || seeded
		session.UpdatedAt = now
		return nil
	})
}

func (store *SessionStore) updateSession(id string, change func(*Session, time.Time) error) error {
	cas, ok := store.provider.(storage.BlobCAS)
	if !ok {
		return storage.ErrUnsupported
	}
	name := store.sessionBlobName(id)
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		current, err := store.provider.ReadBlob(name)
		if err != nil {
			return fmt.Errorf("chat: read session: %w", err)
		}
		if len(current) == 0 {
			return ErrSessionNotFound
		}
		var session Session
		if err := json.Unmarshal(current, &session); err != nil {
			return fmt.Errorf("chat: decode session: %w", err)
		}
		if err := change(&session, store.now().UTC()); err != nil {
			return err
		}
		replacement, err := compactSessionDocument(&session, store.now().UTC())
		if err != nil {
			return err
		}
		swapped, err := cas.CompareAndSwapBlob(name, current, replacement)
		if err != nil {
			return fmt.Errorf("chat: persist session: %w", err)
		}
		if swapped {
			return store.touchIndex(&session)
		}
	}
	return ErrStoreConflict
}

func (store *SessionStore) readIndex() (*sessionIndex, []byte, error) {
	data, err := store.provider.ReadBlob(store.indexBlobName())
	if err != nil {
		return nil, nil, fmt.Errorf("chat: read session index: %w", err)
	}
	if len(data) == 0 {
		return nil, data, nil
	}
	var index sessionIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, nil, fmt.Errorf("chat: decode session index: %w", err)
	}
	return &index, data, nil
}

func (store *SessionStore) updateIndex(change func(*sessionIndex) error) error {
	cas, ok := store.provider.(storage.BlobCAS)
	if !ok {
		return storage.ErrUnsupported
	}
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		index, current, err := store.readIndex()
		if err != nil {
			return err
		}
		if index == nil {
			index = &sessionIndex{Version: 1}
		}
		if err := change(index); err != nil {
			return err
		}
		index.Version++
		replacement, err := json.Marshal(index)
		if err != nil {
			return fmt.Errorf("chat: encode session index: %w", err)
		}
		swapped, err := cas.CompareAndSwapBlob(store.indexBlobName(), current, replacement)
		if err != nil {
			return fmt.Errorf("chat: persist session index: %w", err)
		}
		if swapped {
			return nil
		}
	}
	return ErrStoreConflict
}

func (store *SessionStore) touchIndex(session *Session) error {
	var evicted []string
	err := store.updateIndex(func(index *sessionIndex) error {
		for position := range index.Sessions {
			if index.Sessions[position].ID == session.ID {
				index.Sessions[position] = summaryEntry(session)
				return nil
			}
		}
		evicted = pruneIndex(index, store.now().UTC(), 1)
		index.Sessions = append(index.Sessions, summaryEntry(session))
		return nil
	})
	if err != nil {
		return err
	}
	for _, evictedID := range evicted {
		if err := store.deleteSessionArtifacts(evictedID); err != nil {
			log.Printf("chat storage cleanup failed: operation=evict session_id=%q code=backend_failure", evictedID)
		}
	}
	return nil
}

func compactSessionDocument(session *Session, now time.Time) ([]byte, error) {
	encoded, err := json.Marshal(session)
	if err != nil {
		return nil, fmt.Errorf("chat: encode session: %w", err)
	}
	if len(encoded) <= MaxSessionBlobBytes {
		return encoded, nil
	}

	latestUser, latestAssistant, latestTerminal := -1, -1, -1
	for index := range session.Turns {
		turn := &session.Turns[index]
		if turn.Role == TurnUser {
			latestUser = index
		}
		if turn.Role == TurnAssistant {
			latestAssistant = index
		}
		if terminalEvents(turn.Events) != nil {
			latestTerminal = index
		}
	}
	protected := func(index int) bool {
		return index == latestUser || index == latestAssistant || index == latestTerminal
	}
	for index := range session.Turns {
		turn := &session.Turns[index]
		if len(turn.ToolCalls) == 0 && len(turn.Events) == 0 {
			continue
		}
		terminals := terminalEvents(turn.Events)
		turn.ToolCalls = nil
		turn.Events = append([]core.ChatEvent{{Kind: "trace_compacted", Output: "older tool and event traces were compacted"}}, terminals...)
		encoded, err = json.Marshal(session)
		if err != nil {
			return nil, fmt.Errorf("chat: encode compacted session: %w", err)
		}
		if len(encoded) <= MaxSessionBlobBytes {
			return encoded, nil
		}
	}

	type retainedTurn struct {
		turn      Turn
		protected bool
	}
	retained := make([]retainedTurn, len(session.Turns))
	for index, turn := range session.Turns {
		retained[index] = retainedTurn{turn: turn, protected: protected(index)}
	}
	for dropped := 1; ; dropped++ {
		remove := -1
		for index := range retained {
			if !retained[index].protected {
				remove = index
				break
			}
		}
		if remove < 0 {
			return compactProtectedTurns(session, now)
		}
		retained = append(retained[:remove], retained[remove+1:]...)
		session.Turns = []Turn{{Role: TurnCompaction, Content: fmt.Sprintf("%d older turns were compacted to stay within the session storage limit", dropped), CreatedAt: now}}
		for _, item := range retained {
			session.Turns = append(session.Turns, item.turn)
		}
		encoded, err = json.Marshal(session)
		if err != nil {
			return nil, fmt.Errorf("chat: encode compacted session: %w", err)
		}
		if len(encoded) <= MaxSessionBlobBytes {
			return encoded, nil
		}
	}
}

func compactProtectedTurns(session *Session, now time.Time) ([]byte, error) {
	latest := make(map[TurnRole]Turn)
	var terminal *Turn
	for index := range session.Turns {
		turn := session.Turns[index]
		if turn.Role == TurnUser || turn.Role == TurnAssistant {
			latest[turn.Role] = turn
		}
		if terminalEvents(turn.Events) != nil {
			copy := turn
			terminal = &copy
		}
	}
	turns := []Turn{{Role: TurnCompaction, Content: "Older turns and traces were compacted to stay within the session storage limit.", CreatedAt: now}}
	for _, role := range []TurnRole{TurnUser, TurnAssistant} {
		if turn, ok := latest[role]; ok {
			turn.ToolCalls = nil
			turn.Events = terminalEvents(turn.Events)
			turns = append(turns, turn)
		}
	}
	if terminal != nil && terminal.ID != latest[terminal.Role].ID {
		terminal.ToolCalls = nil
		terminal.Events = terminalEvents(terminal.Events)
		turns = append(turns, *terminal)
	}
	session.Turns = turns
	encoded, err := json.Marshal(session)
	if err != nil {
		return nil, fmt.Errorf("chat: encode compacted session: %w", err)
	}
	if len(encoded) > MaxSessionBlobBytes {
		return nil, fmt.Errorf("%w: encoded bytes=%d limit=%d", ErrSessionTooLarge, len(encoded), MaxSessionBlobBytes)
	}
	return encoded, nil
}

func terminalEvents(events []core.ChatEvent) []core.ChatEvent {
	var terminals []core.ChatEvent
	for _, event := range events {
		if event.Kind == core.ChatEventRunFinished || event.Kind == core.ChatEventRunFailed || event.Kind == core.ChatEventRunCancelled || event.Kind == "run_throttled" {
			terminals = append(terminals, event)
		}
	}
	return terminals
}

func summaryEntry(session *Session) sessionIndexEntry {
	return sessionIndexEntry{
		ID:        session.ID,
		Status:    session.Status,
		Seeded:    session.Seeded,
		CreatedAt: session.CreatedAt,
		UpdatedAt: session.UpdatedAt,
	}
}

func validSessionStatus(status SessionStatus) bool {
	return status == SessionIdle || status == SessionRunning || status == SessionFailed
}

func indexNeedsHydration(index *sessionIndex) bool {
	for _, entry := range index.Sessions {
		if !validSessionStatus(entry.Status) {
			return true
		}
	}
	return false
}

func (store *SessionStore) hydrateIndex() error {
	return store.updateIndex(func(index *sessionIndex) error {
		hydrated := make([]sessionIndexEntry, 0, len(index.Sessions))
		for _, entry := range index.Sessions {
			if validSessionStatus(entry.Status) {
				hydrated = append(hydrated, entry)
				continue
			}
			data, err := store.provider.ReadBlob(store.sessionBlobName(entry.ID))
			if err != nil {
				return fmt.Errorf("chat: hydrate session summary: %w", err)
			}
			if len(data) == 0 {
				continue
			}
			var session Session
			if err := json.Unmarshal(data, &session); err != nil {
				return fmt.Errorf("chat: decode session summary: %w", err)
			}
			hydrated = append(hydrated, summaryEntry(&session))
		}
		index.Sessions = hydrated
		return nil
	})
}

func pruneIndex(index *sessionIndex, now time.Time, reserve int) []string {
	cutoff := now.Add(-SessionRetention)
	kept := index.Sessions[:0]
	evicted := make([]string, 0)
	for _, entry := range index.Sessions {
		if entry.UpdatedAt.Before(cutoff) {
			evicted = append(evicted, entry.ID)
			continue
		}
		kept = append(kept, entry)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].UpdatedAt.Equal(kept[j].UpdatedAt) {
			return kept[i].ID < kept[j].ID
		}
		return kept[i].UpdatedAt.Before(kept[j].UpdatedAt)
	})
	for len(kept) > MaxSessions-reserve {
		evicted = append(evicted, kept[0].ID)
		kept = kept[1:]
	}
	index.Sessions = kept
	return evicted
}

func (store *SessionStore) replaceBlob(name string, replacement []byte, requireMissing bool) error {
	cas, ok := store.provider.(storage.BlobCAS)
	if !ok {
		return storage.ErrUnsupported
	}
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		current, err := store.provider.ReadBlob(name)
		if err != nil {
			return fmt.Errorf("chat: read blob: %w", err)
		}
		if requireMissing && len(current) > 0 {
			return fmt.Errorf("chat: session already exists")
		}
		if !requireMissing && len(current) == 0 && replacement == nil {
			return nil
		}
		swapped, err := cas.CompareAndSwapBlob(name, current, replacement)
		if err != nil {
			return fmt.Errorf("chat: persist blob: %w", err)
		}
		if swapped {
			return nil
		}
	}
	return ErrStoreConflict
}

func (store *SessionStore) migrateLegacy() error {
	if index, _, err := store.readIndex(); err != nil || index != nil {
		return err
	}
	data, err := store.provider.ReadBlob(store.blobName)
	if err != nil {
		return fmt.Errorf("chat: read legacy sessions: %w", err)
	}
	var document sessionDocument
	if len(data) > 0 {
		if err := json.Unmarshal(data, &document); err != nil {
			return fmt.Errorf("chat: decode legacy sessions: %w", err)
		}
	}
	index := sessionIndex{Version: 1}
	for _, session := range document.Sessions {
		if session == nil || !validSessionID(session.ID) {
			continue
		}
		encoded, err := json.Marshal(session)
		if err != nil {
			return fmt.Errorf("chat: encode migrated session: %w", err)
		}
		if err := store.replaceBlob(store.sessionBlobName(session.ID), encoded, true); err != nil && !strings.Contains(err.Error(), "already exists") {
			return err
		}
		index.Sessions = append(index.Sessions, summaryEntry(session))
	}
	pruneIndex(&index, store.now().UTC(), 0)
	encoded, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("chat: encode migrated index: %w", err)
	}
	cas := store.provider.(storage.BlobCAS)
	_, err = cas.CompareAndSwapBlob(store.indexBlobName(), nil, encoded)
	return err
}

func (store *SessionStore) AcquireLease(id, owner string, ttl time.Duration) (uint64, bool, error) {
	if owner == "" || ttl <= 0 {
		return 0, false, fmt.Errorf("chat: invalid lease")
	}
	cas, ok := store.provider.(storage.BlobCAS)
	if !ok {
		return 0, false, storage.ErrUnsupported
	}
	name := store.leaseBlobName(id)
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		current, err := store.provider.ReadBlob(name)
		if err != nil {
			return 0, false, fmt.Errorf("chat: read lease: %w", err)
		}
		now := store.now().UTC()
		var epoch uint64 = 1
		if len(current) > 0 {
			var lease sessionLease
			if err := json.Unmarshal(current, &lease); err != nil {
				return 0, false, fmt.Errorf("chat: decode lease: %w", err)
			}
			if lease.Owner != owner && lease.ExpiresAt.After(now) {
				return 0, false, nil
			}
			epoch = lease.Epoch + 1
		}
		replacement, _ := json.Marshal(sessionLease{Owner: owner, Epoch: epoch, ExpiresAt: now.Add(ttl)})
		swapped, err := cas.CompareAndSwapBlob(name, current, replacement)
		if err != nil {
			return 0, false, fmt.Errorf("chat: acquire lease: %w", err)
		}
		if swapped {
			return epoch, true, nil
		}
	}
	return 0, false, ErrStoreConflict
}

func (store *SessionStore) RenewLease(id, owner string, epoch uint64, ttl time.Duration) (bool, bool, error) {
	return store.mutateLease(id, owner, epoch, true, func(lease sessionLease, now time.Time) *sessionLease {
		lease.ExpiresAt = now.Add(ttl)
		return &lease
	})
}

func (store *SessionStore) ReleaseLease(id, owner string, epoch uint64) error {
	_, _, err := store.mutateLease(id, owner, epoch, false, func(sessionLease, time.Time) *sessionLease { return nil })
	return err
}

func (store *SessionStore) RequestCancel(id string) (bool, error) {
	cas, ok := store.provider.(storage.BlobCAS)
	if !ok {
		return false, storage.ErrUnsupported
	}
	name := store.leaseBlobName(id)
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		current, err := store.provider.ReadBlob(name)
		if err != nil || len(current) == 0 {
			return false, err
		}
		var lease sessionLease
		if err := json.Unmarshal(current, &lease); err != nil {
			return false, fmt.Errorf("chat: decode lease: %w", err)
		}
		if !lease.ExpiresAt.After(store.now().UTC()) {
			return false, nil
		}
		lease.CancelRequested = true
		replacement, _ := json.Marshal(lease)
		swapped, err := cas.CompareAndSwapBlob(name, current, replacement)
		if err != nil || swapped {
			return swapped, err
		}
	}
	return false, ErrStoreConflict
}

func (store *SessionStore) mutateLease(id, owner string, epoch uint64, stopOnCancel bool, replacement func(sessionLease, time.Time) *sessionLease) (bool, bool, error) {
	cas, ok := store.provider.(storage.BlobCAS)
	if !ok {
		return false, false, storage.ErrUnsupported
	}
	name := store.leaseBlobName(id)
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		current, err := store.provider.ReadBlob(name)
		if err != nil {
			return false, false, err
		}
		if len(current) == 0 {
			return false, false, nil
		}
		var lease sessionLease
		if err := json.Unmarshal(current, &lease); err != nil {
			return false, false, err
		}
		if lease.Owner != owner || lease.Epoch != epoch {
			return false, false, ErrLeaseFenced
		}
		if stopOnCancel && lease.CancelRequested {
			return true, true, nil
		}
		next := replacement(lease, store.now().UTC())
		var encoded []byte
		if next != nil {
			encoded, _ = json.Marshal(next)
		}
		swapped, err := cas.CompareAndSwapBlob(name, current, encoded)
		if err != nil || swapped {
			return swapped, false, err
		}
	}
	return false, false, ErrStoreConflict
}

func (store *SessionStore) LeaseActive(id string) (bool, error) {
	data, err := store.provider.ReadBlob(store.leaseBlobName(id))
	if err != nil || len(data) == 0 {
		return false, err
	}
	var lease sessionLease
	if err := json.Unmarshal(data, &lease); err != nil {
		return false, fmt.Errorf("chat: decode lease: %w", err)
	}
	return lease.ExpiresAt.After(store.now().UTC()), nil
}

func validSessionID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, char := range id {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func capString(value string, limit int) string {
	return capRawString(strings.TrimSpace(value), limit)
}

func capRawString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

func boundToolCalls(calls []core.ToolCallTrace) []core.ToolCallTrace {
	if len(calls) > MaxEventsPerTurn {
		calls = calls[:MaxEventsPerTurn]
	}
	out := append([]core.ToolCallTrace(nil), calls...)
	for index := range out {
		out[index].CallID = capString(out[index].CallID, 256)
		out[index].Name = capString(out[index].Name, 256)
		out[index].Args = capString(out[index].Args, MaxToolPayloadBytes/2)
		out[index].Output = capString(out[index].Output, MaxToolPayloadBytes/2)
		out[index].Error = capString(out[index].Error, 256)
	}
	encoded, _ := json.Marshal(out)
	if len(encoded) <= MaxToolBytesPerTurn {
		return out
	}
	if len(out) > 2 {
		out = []core.ToolCallTrace{
			out[0],
			{Name: "tool_calls_elided", Error: fmt.Sprintf("%d tool calls omitted", len(out)-2)},
			out[len(out)-1],
		}
	}
	encoded, _ = json.Marshal(out)
	if len(encoded) > MaxToolBytesPerTurn {
		for index := range out {
			out[index].Args = ""
			out[index].Output = ""
			out[index].Error = ""
		}
	}
	encoded, _ = json.Marshal(out)
	if len(encoded) > MaxToolBytesPerTurn {
		return []core.ToolCallTrace{{Name: "tool_calls_elided"}}
	}
	return out
}

func boundEvents(events []core.ChatEvent) []core.ChatEvent {
	if len(events) > MaxEventsPerTurn {
		head := MaxEventsPerTurn / 2
		tail := MaxEventsPerTurn - head - 1
		bounded := make([]core.ChatEvent, 0, MaxEventsPerTurn)
		bounded = append(bounded, events[:head]...)
		bounded = append(bounded, core.ChatEvent{Kind: "events_elided", Output: fmt.Sprintf("%d events omitted", len(events)-head-tail)})
		bounded = append(bounded, events[len(events)-tail:]...)
		events = bounded
	}
	out := append([]core.ChatEvent(nil), events...)
	for index := range out {
		out[index].Kind = capString(out[index].Kind, 64)
		out[index].Tool = capString(out[index].Tool, 256)
		out[index].CallID = capString(out[index].CallID, 256)
		out[index].ToolDisplay = capString(out[index].ToolDisplay, 256)
		out[index].Delta = capRawString(out[index].Delta, MaxMessageBytes/2)
		out[index].Args = capString(out[index].Args, MaxToolPayloadBytes/2)
		out[index].Output = capString(out[index].Output, MaxToolPayloadBytes/2)
		out[index].Error = capString(out[index].Error, 256)
		out[index].Citations = boundCitations(out[index].Citations)
	}
	encoded, _ := json.Marshal(out)
	if len(encoded) <= MaxEventBytesPerTurn {
		return out
	}
	if len(out) > 2 {
		out = []core.ChatEvent{
			out[0],
			{Kind: "events_elided", Output: fmt.Sprintf("%d events omitted", len(out)-2)},
			out[len(out)-1],
		}
	}
	encoded, _ = json.Marshal(out)
	if len(encoded) > MaxEventBytesPerTurn {
		for index := range out {
			out[index].Tool = ""
			out[index].ToolDisplay = ""
			out[index].Delta = ""
			out[index].Args = ""
			out[index].Output = ""
			out[index].Error = ""
			out[index].Citations = nil
		}
	}
	encoded, _ = json.Marshal(out)
	if len(encoded) > MaxEventBytesPerTurn {
		terminal := core.ChatEvent{Kind: out[len(out)-1].Kind, Seq: out[len(out)-1].Seq}
		out = []core.ChatEvent{{Kind: "events_elided"}, terminal}
	}
	encoded, _ = json.Marshal(out)
	if len(encoded) > MaxEventBytesPerTurn {
		return []core.ChatEvent{{Kind: "events_elided"}}
	}
	return out
}

func boundCitations(citations []core.ChatCitation) []core.ChatCitation {
	if len(citations) > MaxCitationsPerTrace {
		citations = citations[:MaxCitationsPerTrace]
	}
	out := append([]core.ChatCitation(nil), citations...)
	for index := range out {
		out[index].Tool = capString(out[index].Tool, 128)
		out[index].Label = capString(out[index].Label, 256)
		out[index].Locator = capString(out[index].Locator, 512)
	}
	return out
}

func cloneSession(session *Session) *Session {
	if session == nil {
		return nil
	}
	encoded, _ := json.Marshal(session)
	var out Session
	_ = json.Unmarshal(encoded, &out)
	return &out
}

func cloneAttachment(attachment *core.ChatAttachment) *core.ChatAttachment {
	if attachment == nil {
		return nil
	}
	encoded, _ := json.Marshal(attachment)
	var out core.ChatAttachment
	_ = json.Unmarshal(encoded, &out)
	return &out
}
