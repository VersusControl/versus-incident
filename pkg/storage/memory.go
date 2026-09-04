package storage

import (
	"bytes"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/utils"
	"github.com/google/uuid"
)

// memoryProvider is an in-memory backend used by tests. It is concurrency-safe
// and never returns transport errors. Not exposed as a config option.
type memoryProvider struct {
	mu                sync.RWMutex
	blobs             map[string][]byte
	blobAt            map[string]time.Time // per-blob updated_at, for Lifecycle purge
	incidents         []*IncidentRecord
	analyses          []*AnalysisRecord
	episodes          map[string]*memoryDetectionEpisode
	activeEpisodes    map[string]string
	maxClosedEpisodes int
}

type memoryDetectionEpisode struct {
	Identity                DetectionIdentity
	Fingerprint             string
	EpisodeID               string
	IncidentID              string
	OccurrenceCount         int64
	FirstSeen               time.Time
	LastSeen                time.Time
	HighestObservedSeverity string
	HighestHandledSeverity  string
	HighestNotifiedSeverity string
	ClosedAt                *time.Time
	PendingKind             DetectionNotificationKind
	PendingSeverity         string
	PendingToken            string
	PendingOwner            string
	PendingExpiresAt        time.Time
	LastCompletedToken      string
	LastNotificationOutcome string
}

// NewMemory returns a Provider that keeps all state in memory. Intended
// for tests; never select via the config factory.
func NewMemory() Provider {
	return &memoryProvider{
		blobs:             make(map[string][]byte),
		blobAt:            make(map[string]time.Time),
		episodes:          make(map[string]*memoryDetectionEpisode),
		activeEpisodes:    make(map[string]string),
		maxClosedEpisodes: MaxClosedDetectionEpisodesDefault,
	}
}

func (m *memoryProvider) RecordDetectionOccurrence(occurrence DetectionOccurrence) (DetectionEpisodeDecision, error) {
	fingerprint, err := DetectionIdentityFingerprint(occurrence.Identity)
	if err != nil || occurrence.Frequency <= 0 {
		return DetectionEpisodeDecision{}, ErrInvalidDetectionOccurrence
	}
	now := occurrence.ReceivedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	quietPeriod := occurrence.QuietPeriod
	if quietPeriod <= 0 {
		quietPeriod = DefaultDetectionEpisodeQuietPeriod
	}
	lease := occurrence.ClaimLease
	if lease <= 0 {
		lease = DefaultDetectionNotificationLease
	}
	severity := canonicalDetectionSeverity(occurrence.Severity)

	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.pruneClosedDetectionEpisodesLocked()

	episode := m.activeDetectionEpisode(fingerprint, normalizeDetectionIdentity(occurrence.Identity))
	if occurrence.ExistingOnly && episode == nil {
		return DetectionEpisodeDecision{}, ErrNotFound
	}
	reopened := episode != nil
	if !reopened {
		for _, previous := range m.episodes {
			if previous.Fingerprint == fingerprint {
				reopened = true
				break
			}
		}
	}
	if episode != nil {
		if exists, resolved := m.incidentStateLocked(episode.IncidentID); exists && resolved {
			closed := now
			episode.ClosedAt = &closed
			delete(m.activeEpisodes, episode.Fingerprint)
			episode = nil
			reopened = true
		}
	}
	if episode != nil && episode.PendingToken == "" && detectionNotificationDelivered(episode.LastNotificationOutcome) && !m.incidentExistsLocked(episode.IncidentID) {
		closed := now
		episode.ClosedAt = &closed
		delete(m.activeEpisodes, fingerprint)
		episode = nil
		reopened = true
	}
	if episode != nil && (episode.ClosedAt != nil || !now.Before(episode.LastSeen.Add(quietPeriod))) {
		closed := now
		episode.ClosedAt = &closed
		delete(m.activeEpisodes, fingerprint)
		episode = nil
	}
	if episode == nil {
		if occurrence.ExistingOnly {
			return DetectionEpisodeDecision{}, ErrNotFound
		}
		episode = &memoryDetectionEpisode{
			Identity:                normalizeDetectionIdentity(occurrence.Identity),
			Fingerprint:             fingerprint,
			EpisodeID:               uuid.NewString(),
			IncidentID:              uuid.NewString(),
			OccurrenceCount:         occurrence.Frequency,
			FirstSeen:               now,
			LastSeen:                now,
			HighestObservedSeverity: severity,
		}
		m.episodes[episode.EpisodeID] = episode
		m.activeEpisodes[fingerprint] = episode.EpisodeID
		decision := m.claimDetectionNotification(episode, occurrence, now, lease, DetectionNotificationInitial)
		if reopened {
			decision.Action = DetectionEpisodeReopened
		} else {
			decision.Action = DetectionEpisodeOpened
		}
		decision.OccurrenceDelta = occurrence.Frequency
		return decision, nil
	}

	episode.OccurrenceCount += occurrence.Frequency
	episode.LastSeen = now
	if detectionSeverityHigher(severity, episode.HighestObservedSeverity) {
		episode.HighestObservedSeverity = severity
	}
	if occurrence.ExistingOnly {
		m.updateIncidentFromEpisodeLocked(episode)
		decision := m.detectionEpisodeDecision(episode)
		decision.Action = DetectionEpisodeCoalesced
		decision.OccurrenceDelta = occurrence.Frequency
		return decision, nil
	}
	action := DetectionEpisodeCoalesced
	claimKind := DetectionNotificationKind("")
	if episode.PendingToken != "" && !now.Before(episode.PendingExpiresAt) {
		claimKind = episode.PendingKind
		if claimKind == DetectionNotificationEscalation {
			action = DetectionEpisodeEscalated
		}
		episode.PendingToken = ""
		episode.PendingOwner = ""
		episode.PendingExpiresAt = time.Time{}
	} else if episode.PendingToken == "" {
		switch {
		case episode.HighestHandledSeverity == "":
			claimKind = DetectionNotificationInitial
		case detectionSeverityHigher(episode.HighestObservedSeverity, episode.HighestHandledSeverity):
			claimKind = DetectionNotificationEscalation
			action = DetectionEpisodeEscalated
		}
	}

	decision := m.detectionEpisodeDecision(episode)
	decision.Action = action
	decision.OccurrenceDelta = occurrence.Frequency
	if claimKind != "" {
		decision = m.claimDetectionNotification(episode, occurrence, now, lease, claimKind)
		decision.Action = action
		decision.OccurrenceDelta = occurrence.Frequency
	}
	return decision, nil
}

func (m *memoryProvider) activeDetectionEpisode(fingerprint string, identity DetectionIdentity) *memoryDetectionEpisode {
	if episode := m.episodes[m.activeEpisodes[fingerprint]]; episode != nil {
		return episode
	}
	for legacyFingerprint, episodeID := range m.activeEpisodes {
		episode := m.episodes[episodeID]
		if episode == nil || normalizeDetectionIdentity(episode.Identity) != identity {
			continue
		}
		delete(m.activeEpisodes, legacyFingerprint)
		m.activeEpisodes[fingerprint] = episodeID
		episode.Identity = identity
		episode.Fingerprint = fingerprint
		return episode
	}
	return nil
}

func (m *memoryProvider) claimDetectionNotification(episode *memoryDetectionEpisode, occurrence DetectionOccurrence, now time.Time, lease time.Duration, kind DetectionNotificationKind) DetectionEpisodeDecision {
	owner := strings.TrimSpace(occurrence.ClaimOwner)
	if owner == "" {
		owner = uuid.NewString()
	}
	episode.PendingKind = kind
	episode.PendingSeverity = episode.HighestObservedSeverity
	episode.PendingToken = uuid.NewString()
	episode.PendingOwner = owner
	episode.PendingExpiresAt = now.Add(lease)
	decision := m.detectionEpisodeDecision(episode)
	decision.NotificationClaimed = true
	decision.NotificationKind = kind
	decision.ClaimToken = episode.PendingToken
	decision.ClaimExpiresAt = episode.PendingExpiresAt
	return decision
}

func (m *memoryProvider) detectionEpisodeDecision(episode *memoryDetectionEpisode) DetectionEpisodeDecision {
	return DetectionEpisodeDecision{
		Fingerprint:             episode.Fingerprint,
		EpisodeID:               episode.EpisodeID,
		IncidentID:              episode.IncidentID,
		OccurrenceCount:         episode.OccurrenceCount,
		FirstSeen:               episode.FirstSeen,
		LastSeen:                episode.LastSeen,
		HighestObservedSeverity: episode.HighestObservedSeverity,
		HighestNotifiedSeverity: episode.HighestNotifiedSeverity,
	}
}

func (m *memoryProvider) RenewDetectionNotificationClaim(renewal DetectionNotificationRenewal) error {
	now := renewal.RenewedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	lease := renewal.ClaimLease
	if lease <= 0 {
		lease = DefaultDetectionNotificationLease
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	episode := m.episodes[renewal.EpisodeID]
	if episode == nil {
		return ErrNotFound
	}
	if renewal.ClaimToken == "" || renewal.ClaimToken != episode.PendingToken || !now.Before(episode.PendingExpiresAt) {
		return ErrDetectionClaimLost
	}
	episode.PendingExpiresAt = now.Add(lease)
	return nil
}

func (m *memoryProvider) CompleteDetectionNotification(completion DetectionNotificationCompletion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	episode := m.episodes[completion.EpisodeID]
	if episode == nil {
		return ErrNotFound
	}
	if episode.LastCompletedToken == completion.ClaimToken && completion.ClaimToken != "" {
		return nil
	}
	if completion.ClaimToken == "" || completion.ClaimToken != episode.PendingToken {
		return ErrDetectionClaimLost
	}
	if detectionNotificationTerminal(completion.Outcome) {
		episode.HighestHandledSeverity = episode.PendingSeverity
		if detectionNotificationDelivered(completion.Outcome) {
			episode.HighestNotifiedSeverity = episode.PendingSeverity
		}
	}
	episode.LastCompletedToken = completion.ClaimToken
	episode.LastNotificationOutcome = completion.Outcome
	episode.PendingKind = ""
	episode.PendingSeverity = ""
	episode.PendingToken = ""
	episode.PendingOwner = ""
	episode.PendingExpiresAt = time.Time{}
	for _, incident := range m.incidents {
		if incident.ID != episode.IncidentID {
			continue
		}
		applyDetectionEpisodeDecision(incident, m.detectionEpisodeDecision(episode))
		break
	}
	return nil
}

func (m *memoryProvider) incidentExistsLocked(incidentID string) bool {
	exists, _ := m.incidentStateLocked(incidentID)
	return exists
}

func (m *memoryProvider) incidentStateLocked(incidentID string) (bool, bool) {
	for _, incident := range m.incidents {
		if incident.ID == incidentID {
			return true, incident.Resolved
		}
	}
	return false, false
}

func (m *memoryProvider) updateIncidentFromEpisodeLocked(episode *memoryDetectionEpisode) {
	for _, incident := range m.incidents {
		if incident.ID != episode.IncidentID {
			continue
		}
		applyDetectionEpisodeDecision(incident, m.detectionEpisodeDecision(episode))
		return
	}
}

func (m *memoryProvider) CloseDetectionEpisodeByIncident(incidentID string, closedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.pruneClosedDetectionEpisodesLocked()
	for _, episode := range m.episodes {
		if episode.IncidentID != incidentID || episode.ClosedAt != nil {
			continue
		}
		closed := closedAt.UTC()
		if closed.IsZero() {
			closed = time.Now().UTC()
		}
		episode.ClosedAt = &closed
		delete(m.activeEpisodes, episode.Fingerprint)
		return nil
	}
	return ErrNotFound
}

func (m *memoryProvider) pruneClosedDetectionEpisodesLocked() {
	limit := m.maxClosedEpisodes
	if limit <= 0 {
		limit = MaxClosedDetectionEpisodesDefault
	}
	closed := make([]*memoryDetectionEpisode, 0)
	for _, episode := range m.episodes {
		if episode.ClosedAt != nil {
			closed = append(closed, episode)
		}
	}
	if len(closed) <= limit {
		return
	}
	sort.Slice(closed, func(left, right int) bool {
		return closed[left].ClosedAt.Before(*closed[right].ClosedAt)
	})
	for _, episode := range closed[:len(closed)-limit] {
		delete(m.episodes, episode.EpisodeID)
	}
}

func (m *memoryProvider) ReadBlob(name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if data, ok := m.blobs[name]; ok {
		// return a copy so callers can't mutate state through the slice
		out := make([]byte, len(data))
		copy(out, data)
		return out, nil
	}
	return nil, nil
}

func (m *memoryProvider) WriteBlob(name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.blobs[name] = cp
	m.blobAt[name] = time.Now().UTC()
	return nil
}

// CreateBlobIfAbsent implements the optional storage.BlobCreator capability.
// The write is atomic under the provider mutex, so N concurrent
// creators serialize and exactly one observes written==true; every other
// caller observes written==false and reads the survivor's bytes via
// ReadBlob. An existing key (created here or via WriteBlob) is left
// untouched.
func (m *memoryProvider) CreateBlobIfAbsent(name string, data []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.blobs[name]; ok {
		return false, nil
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.blobs[name] = cp
	m.blobAt[name] = time.Now().UTC()
	return true, nil
}

func (m *memoryProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.blobs[name]
	if expected == nil {
		if exists {
			return false, nil
		}
	} else if !exists || !bytes.Equal(current, expected) {
		return false, nil
	}
	if replacement == nil {
		if !exists {
			return false, nil
		}
		delete(m.blobs, name)
		delete(m.blobAt, name)
		return true, nil
	}
	stored := append([]byte(nil), replacement...)
	m.blobs[name] = stored
	m.blobAt[name] = time.Now().UTC()
	return true, nil
}

func (m *memoryProvider) ListBlobs(prefix string) ([]Blob, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Blob, 0)
	for name, data := range m.blobs {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Return a copy so callers can't mutate state through the slice.
		cp := make([]byte, len(data))
		copy(cp, data)
		out = append(out, Blob{Name: name, Data: cp})
	}
	return out, nil
}

func (m *memoryProvider) SaveIncident(rec *IncidentRecord) error {
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	stored := CloneIncidentRecord(rec)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, existing := range m.incidents {
		if existing.ID == rec.ID {
			mergeDetectionIncident(existing, stored)
			m.incidents[i] = stored
			return nil
		}
	}
	m.incidents = append(m.incidents, stored)
	return nil
}

func (m *memoryProvider) SaveDetectionIncident(rec *IncidentRecord) error {
	if rec == nil || rec.ID == "" {
		return ErrInvalidDetectionOccurrence
	}
	rec.OrgID = NormalizeOrgID(rec.OrgID)
	m.mu.Lock()
	defer m.mu.Unlock()
	for index, existing := range m.incidents {
		if existing.ID == rec.ID {
			m.incidents[index] = detectionIncidentUpdate(existing, rec)
			return nil
		}
	}
	m.incidents = append(m.incidents, CloneIncidentRecord(rec))
	return nil
}

func (m *memoryProvider) UpdateIncidentAck(id string, ackedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range m.incidents {
		if rec.ID == id {
			t := ackedAt.UTC()
			rec.AckedAt = &t
			return nil
		}
	}
	return ErrNotFound
}

func (m *memoryProvider) GetIncident(id string) (*IncidentRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rec := range m.incidents {
		if rec.ID == id {
			return CloneIncidentRecord(rec), nil
		}
	}
	return nil, ErrNotFound
}

func (m *memoryProvider) ListIncidents(limit int) ([]*IncidentRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := len(m.incidents)
	if n == 0 {
		return nil, nil
	}
	out := make([]*IncidentRecord, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, CloneIncidentRecord(m.incidents[i]))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CountIncidents implements the optional storage.IncidentPager capability.
// The in-memory history is already capped, so a linear tally is cheap.
// Counts reflect open work: resolved incidents are skipped so the tally
// matches the unresolved-only counts the SQL backend returns. Legacy rows
// are classified via EffectiveOrigin, matching the SQL backend.
func (m *memoryProvider) CountIncidents() (IncidentCounts, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var c IncidentCounts
	for _, rec := range m.incidents {
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
// capability. The in-memory history is already capped, so a single pass over
// the slice is cheap; the shared StatusCountsOf helper classifies each row via
// EffectiveOrigin and buckets it by stored status, matching the SQL backend
// exactly.
func (m *memoryProvider) CountIncidentsByStatus() (IncidentStatusCounts, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return StatusCountsOf(m.incidents), nil
}

// CountIncidentsByStatusSince implements storage.IncidentWindowCounter over the
// same single pass, dropping rows older than the window.
func (m *memoryProvider) CountIncidentsByStatusSince(since time.Time) (IncidentStatusCounts, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return StatusCountsSince(m.incidents, since), nil
}

func (m *memoryProvider) CountIncidentsByServiceSince(orgID, service string, since time.Time) (int, map[string]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	severities := make(map[string]int)
	for _, rec := range m.incidents {
		if NormalizeOrgID(rec.OrgID) != NormalizeOrgID(orgID) || rec.ServiceLabel() != service || rec.CreatedAt.Before(since) {
			continue
		}
		count++
		severities[utils.ExtractSeverity(rec.Content)]++
	}
	return count, severities, nil
}

func (m *memoryProvider) ListIncidentsByServiceSince(orgID, service string, since time.Time, limit int) ([]*IncidentRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*IncidentRecord, 0)
	for _, rec := range m.incidents {
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
func (m *memoryProvider) ListIncidentsPage(origin string, offset, limit int) ([]*IncidentRecord, error) {
	if limit <= 0 {
		limit = DefaultIncidentPageSize
	}
	if offset < 0 {
		offset = 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	filtered := origin == OriginAIDetect || origin == OriginWebhook
	out := make([]*IncidentRecord, 0, limit)
	skipped := 0
	for i := len(m.incidents) - 1; i >= 0; i-- {
		rec := m.incidents[i]
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

func (m *memoryProvider) Close() error { return nil }

func (m *memoryProvider) SaveAnalysis(rec *AnalysisRecord) error {
	if rec == nil || rec.ID == "" {
		return ErrNotFound
	}
	stored := CloneAnalysisRecord(rec)
	stored.OrgID = NormalizeOrgID(stored.OrgID)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, existing := range m.analyses {
		if existing.ID == rec.ID {
			m.analyses[i] = stored
			return nil
		}
	}
	m.analyses = append(m.analyses, stored)
	return nil
}

func (m *memoryProvider) GetAnalysis(id string) (*AnalysisRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rec := range m.analyses {
		if rec.ID == id {
			return CloneAnalysisRecord(rec), nil
		}
	}
	return nil, ErrNotFound
}

func (m *memoryProvider) ListAnalysesByIncident(incidentID string, limit int) ([]*AnalysisRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := len(m.analyses)
	if n == 0 {
		return nil, nil
	}
	out := make([]*AnalysisRecord, 0)
	for i := n - 1; i >= 0; i-- {
		if m.analyses[i].IncidentID != incidentID {
			continue
		}
		out = append(out, CloneAnalysisRecord(m.analyses[i]))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memoryProvider) ListAnalyses(limit int) ([]*AnalysisRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := len(m.analyses)
	if n == 0 {
		return nil, nil
	}
	out := make([]*AnalysisRecord, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, CloneAnalysisRecord(m.analyses[i]))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CountAnalyses implements the optional storage.AnalysisPager capability. The
// in-memory history is small, so a len() is the whole count.
func (m *memoryProvider) CountAnalyses() (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.analyses), nil
}

// ListAnalysesPage implements the optional storage.AnalysisPager capability:
// one bounded, newest-first page over the in-memory slice, skipping offset
// rows and returning at most limit rows.
func (m *memoryProvider) ListAnalysesPage(offset, limit int) ([]*AnalysisRecord, error) {
	if limit <= 0 {
		limit = DefaultAnalysisPageSize
	}
	if offset < 0 {
		offset = 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*AnalysisRecord, 0, limit)
	skipped := 0
	for i := len(m.analyses) - 1; i >= 0; i-- {
		if skipped < offset {
			skipped++
			continue
		}
		out = append(out, CloneAnalysisRecord(m.analyses[i]))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memoryProvider) DeleteAnalysis(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, rec := range m.analyses {
		if rec.ID == id {
			m.analyses = append(m.analyses[:i], m.analyses[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// ---------------------------------------------------------------------------
// Lifecycle (implements the optional storage.Lifecycle capability)
// ---------------------------------------------------------------------------

func (m *memoryProvider) PurgeOlderThan(domain string, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch domain {
	case DomainIncidents:
		kept := make([]*IncidentRecord, 0, len(m.incidents))
		n := 0
		for _, rec := range m.incidents {
			if rec.CreatedAt.Before(cutoff) {
				n++
				continue
			}
			kept = append(kept, rec)
		}
		m.incidents = kept
		return n, nil
	case DomainAnalyses:
		kept := make([]*AnalysisRecord, 0, len(m.analyses))
		n := 0
		for _, rec := range m.analyses {
			if rec.RequestedAt.Before(cutoff) {
				n++
				continue
			}
			kept = append(kept, rec)
		}
		m.analyses = kept
		return n, nil
	case DomainBlobs:
		n := 0
		for name, at := range m.blobAt {
			if at.Before(cutoff) {
				delete(m.blobs, name)
				delete(m.blobAt, name)
				n++
			}
		}
		return n, nil
	default:
		return 0, ErrUnknownDomain
	}
}

func (m *memoryProvider) DeleteByID(domain, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch domain {
	case DomainIncidents:
		for i, rec := range m.incidents {
			if rec.ID == id {
				m.incidents = append(m.incidents[:i], m.incidents[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	case DomainAnalyses:
		for i, rec := range m.analyses {
			if rec.ID == id {
				m.analyses = append(m.analyses[:i], m.analyses[i+1:]...)
				return nil
			}
		}
		return ErrNotFound
	case DomainBlobs:
		if _, ok := m.blobs[id]; !ok {
			return ErrNotFound
		}
		delete(m.blobs, id)
		delete(m.blobAt, id)
		return nil
	default:
		return ErrUnknownDomain
	}
}
